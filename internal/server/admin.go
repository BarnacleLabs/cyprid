package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cyprid/internal/auth"
	"cyprid/internal/install"
	"cyprid/internal/paths"
	"cyprid/internal/recipe"
	"cyprid/internal/reveal"
	"cyprid/internal/supervisor"
	"cyprid/internal/version"
)

// Admin API, used by the CLI and the web UI:
//
//	GET    /admin/status
//	POST   /admin/models/{name}/start[?wait=1] | stop | install | uninstall[?delete_weights=1] | delete-weights
//	GET    /admin/models/{name}/weights      the cached repos deleting its weights would remove, and their sizes
//	GET    /admin/models/{name}/logs?n=      worker log (from its log file, so it spans restarts)
//	GET    /admin/models/{name}/job?n=       latest install/uninstall job and its output
//	GET    /admin/recipes                    GET|PUT|DELETE /admin/recipes/{id}
//	GET    /admin/bench                      GET /admin/bench/{run}
//	GET    /admin/models/{name}/calibration  PUT {"temperature": T | 0 | null}; suggestions fitted from bench runs
//	POST   /admin/login-link                 a one-time sign-in link for the web page
//	POST   /admin/reveal {"what": "bench", "name": ""}  show a named cyprid folder or file in Finder
//	GET    /admin/network                    PUT /admin/network {"enabled": bool}, POST /admin/network/rotate-key
func (s *Server) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/status", s.adminStatus)
	mux.HandleFunc("POST /admin/models/{name}/start", s.adminStart)
	mux.HandleFunc("POST /admin/models/{name}/stop", s.adminStop)
	mux.HandleFunc("POST /admin/models/{name}/install", s.adminInstall)
	mux.HandleFunc("POST /admin/models/{name}/uninstall", s.adminUninstall)
	mux.HandleFunc("GET /admin/models/{name}/weights", s.adminWeights)
	mux.HandleFunc("POST /admin/models/{name}/delete-weights", s.adminDeleteWeights)
	mux.HandleFunc("GET /admin/models/{name}/logs", s.adminLogs)
	mux.HandleFunc("GET /admin/models/{name}/job", s.adminJob)
	mux.HandleFunc("GET /admin/recipes", s.adminRecipes)
	mux.HandleFunc("GET /admin/recipes/{id}", s.adminRecipe)
	mux.HandleFunc("PUT /admin/recipes/{id}", s.adminSaveRecipe)
	mux.HandleFunc("DELETE /admin/recipes/{id}", s.adminResetRecipe)
	mux.HandleFunc("GET /admin/bench", s.adminBenchRuns)
	mux.HandleFunc("GET /admin/bench/{run}", s.adminBenchRun)
	mux.HandleFunc("GET /admin/models/{name}/calibration", s.adminCalibration)
	mux.HandleFunc("PUT /admin/models/{name}/calibration", s.adminSetCalibration)
	mux.HandleFunc("POST /admin/reveal", s.adminReveal)
	mux.HandleFunc("POST /admin/login-link", s.adminLoginLink)
	mux.HandleFunc("GET /admin/network", s.adminNetwork)
	mux.HandleFunc("PUT /admin/network", s.adminSetNetwork)
	mux.HandleFunc("POST /admin/network/rotate-key", s.adminRotateKey)
	return mux
}

// requireAdmin refuses admin requests without cyprid's access token, so on a
// shared Mac other accounts can't drive cyprid even though they can reach localhost.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.Valid(r, s.cfg.AdminToken) {
			fail(w, http.StatusUnauthorized, "cyprid's access token is required: open the web page with `cyprid ui`, "+
				"or send the "+auth.Header+" header (the token is in "+auth.TokenPath()+")")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// signIn exchanges a one-time ?login= code for the sign-in cookie, then
// redirects to the plain address so the code doesn't linger in the address bar.
func (s *Server) signIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("login")
		if code == "" {
			next.ServeHTTP(w, r)
			return
		}
		if s.links.Use(code) {
			auth.SetCookie(w, s.cfg.AdminToken)
		}
		// Valid or not, drop the code from the address; an unsigned-in page explains what to do.
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// LoginLink returns a one-time sign-in link for the web page.
func (s *Server) LoginLink(ttl time.Duration) string {
	return "http://" + s.cfg.Addr + "/?login=" + s.links.New(ttl)
}

func (s *Server) adminLoginLink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"url": s.LoginLink(2 * time.Minute)})
}

// localOnly guards the admin API and UI. Recipes decide which commands the
// server runs, so only this machine may drive these endpoints, and no web page
// may either: the connection must come from loopback (other machines are
// refused even in network mode), requests must be addressed to a loopback host
// name (defeats DNS rebinding), and a browser request must come from the UI's
// own origin (defeats CSRF).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			fail(w, http.StatusForbidden, "the admin API and UI are only available on the machine running cyprid")
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !isLoopbackHost(host) {
			fail(w, http.StatusForbidden, "the admin API and UI only accept requests addressed to localhost")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				fail(w, http.StatusForbidden, "cross-origin requests to the admin API are not allowed")
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			fail(w, http.StatusForbidden, "cross-site requests to the admin API are not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	s.recipes()
	s.mu.Lock()
	recipeErr := s.badError
	s.mu.Unlock()
	uv, _ := exec.LookPath("uv")
	writeJSON(w, http.StatusOK, map[string]any{
		"addr":          s.cfg.Addr,
		"home":          paths.Home(),
		"default_model": s.cfg.DefaultModel,
		"strict":        s.cfg.Strict,
		"max_mem_gb":    s.cfg.MaxMemGB,
		"used_mem_gb":   s.sup.UsedMemGB(),
		"recipe_error":  recipeErr,
		"uv":            uv,
		"network":       s.networkInfo().Enabled,
		"version":       version.Get(),
	})
}

func (s *Server) adminRef(w http.ResponseWriter, r *http.Request) (recipe.Ref, bool) {
	ref, ok := s.recipes().Resolve(r.PathValue("name"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", r.PathValue("name")))
		return ref, false
	}
	if ref.IsRemote() {
		fail(w, http.StatusBadRequest, ref.Name+" is a remote model; there is no local process")
		return ref, false
	}
	return ref, true
}

func (s *Server) adminStart(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	if err := s.sup.Start(ref); err != nil {
		s.acquireFailed(w, ref, err)
		return
	}
	if r.URL.Query().Get("wait") != "" {
		// Reuse Acquire's wait-for-ready logic, then release immediately.
		_, release, err := s.sup.Acquire(r.Context(), ref, ref.Recipe.Launch.Ready.Timeout)
		if err != nil {
			s.acquireFailed(w, ref, err)
			return
		}
		release()
	}
	writeJSON(w, http.StatusOK, s.sup.Status(ref.Name))
}

func (s *Server) adminStop(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	stopped := s.sup.Stop(ref.Name)
	writeJSON(w, http.StatusOK, map[string]any{"stopped": stopped, "state": s.sup.Status(ref.Name).State})
}

func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 200
	}
	lines, err := tailFile(filepath.Join(paths.Logs(), ref.Name+".log"), n)
	if err != nil && !os.IsNotExist(err) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": ref.Name, "lines": lines})
}

// tailFile returns the last n lines of a file, reading at most its last 512KB.
func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const window = 512 << 10
	if info, err := f.Stat(); err == nil && info.Size() > window {
		f.Seek(info.Size()-window, io.SeekStart)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	buf := newLineBuffer(n)
	buf.Write(data)
	return buf.tail(n), nil
}

// ---- install jobs ----

type installJob struct {
	kind     string // install or uninstall
	state    string // running, done or failed
	err      string
	started  time.Time
	finished time.Time
	out      *lineBuffer
	recipeID string
}

type jobStatus struct {
	Kind     string     `json:"kind"`
	State    string     `json:"state"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	Lines    []string   `json:"lines,omitempty"`
}

func (s *Server) jobStatus(name string, lines int) *jobStatus {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j := s.jobs[name]
	if j == nil {
		return nil
	}
	st := &jobStatus{Kind: j.kind, State: j.state, Error: j.err, Started: j.started}
	if !j.finished.IsZero() {
		f := j.finished
		st.Finished = &f
	}
	if lines > 0 {
		st.Lines = j.out.tail(lines)
	}
	return st
}

func (s *Server) adminJob(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 200
	}
	st := s.jobStatus(ref.Name, n)
	if st == nil {
		fail(w, http.StatusNotFound, "no install job for "+ref.Name)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// startJob runs fn in the background, one job per recipe at a time, since
// models of a recipe share one environment.
func (s *Server) startJob(ref recipe.Ref, kind string, fn func(out io.Writer) error) error {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	for name, j := range s.jobs {
		if j.state == "running" && j.recipeID == ref.Recipe.ID {
			return fmt.Errorf("%s of %s is already running (recipe %s)", j.kind, name, ref.Recipe.ID)
		}
	}
	j := &installJob{kind: kind, state: "running", started: time.Now(), out: newLineBuffer(2000), recipeID: ref.Recipe.ID}
	s.jobs[ref.Name] = j
	go func() {
		err := fn(j.out)
		s.jobsMu.Lock()
		defer s.jobsMu.Unlock()
		j.finished = time.Now()
		if err != nil {
			j.state, j.err = "failed", err.Error()
			fmt.Fprintf(j.out, "\nerror: %v\n", err)
		} else {
			j.state = "done"
		}
	}()
	return nil
}

func (s *Server) siblings(ref recipe.Ref) []recipe.Ref {
	var out []recipe.Ref
	for _, r := range s.recipes().Models() {
		if r.Recipe.ID == ref.Recipe.ID {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) running(ref recipe.Ref) bool {
	st := s.sup.Status(ref.Name).State
	return st == supervisor.Starting || st == supervisor.Ready
}

func (s *Server) adminInstall(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	if _, err := exec.LookPath("uv"); err != nil {
		fail(w, http.StatusFailedDependency, "uv not found on the server's PATH; install it with: brew install uv")
		return
	}
	if !install.EnvReady(ref) {
		// Building the environment replaces it, which would break a sibling model running from it.
		for _, sib := range s.siblings(ref) {
			if s.running(sib) {
				fail(w, http.StatusConflict, fmt.Sprintf("installing %s rebuilds the %s environment, which %s is running from; stop it first", ref.Name, ref.Recipe.ID, sib.Name))
				return
			}
		}
	}
	if err := s.startJob(ref, "install", func(out io.Writer) error { return install.Install(ref, out) }); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.jobStatus(ref.Name, 0))
}

func (s *Server) adminUninstall(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	siblings := s.siblings(ref)
	deleteWeights := r.URL.Query().Get("delete_weights") != ""
	all := s.recipes().Models()
	err := s.startJob(ref, "uninstall", func(out io.Writer) error {
		if s.sup.Stop(ref.Name) {
			fmt.Fprintf(out, "stopped %s\n", ref.Name)
		}
		if err := install.Uninstall(ref, siblings, out); err != nil {
			return err
		}
		if deleteWeights {
			_, err := install.DeleteWeights(ref, all, out)
			return err
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.jobStatus(ref.Name, 0))
}

func (s *Server) adminWeights(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	all := s.recipes().Models()
	writeJSON(w, http.StatusOK, map[string]any{
		"model": ref.Name, "repos": install.WeightsPlan(ref, all), "freeable_bytes": install.CachedBytes(ref, all),
	})
}

// adminDeleteWeights deletes the cached weights of a model that is no longer installed.
func (s *Server) adminDeleteWeights(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.adminRef(w, r)
	if !ok {
		return
	}
	if st := install.StatusOf(ref); st != install.NotInstalled {
		fail(w, http.StatusConflict, fmt.Sprintf("%s is %s; uninstall it (with its weights) instead", ref.Name, st))
		return
	}
	all := s.recipes().Models()
	if err := s.startJob(ref, "delete-weights", func(out io.Writer) error {
		_, err := install.DeleteWeights(ref, all, out)
		return err
	}); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.jobStatus(ref.Name, 0))
}

// adminReveal opens one of cyprid's named places in Finder (see package reveal).
func (s *Server) adminReveal(w http.ResponseWriter, r *http.Request) {
	var body struct{ What, Name string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, `body must be {"what": "...", "name": "..."}`)
		return
	}
	path, isFile, err := reveal.Locate(body.What, body.Name, s.recipes())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := reveal.Open(path, isFile); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"opened": path})
}

// ---- recipes ----

type recipeInfo struct {
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"` // builtin or user
	HasBuiltin  bool     `json:"has_builtin"`
	Remote      bool     `json:"remote"`
	Models      []string `json:"models"`
}

func (s *Server) adminRecipes(w http.ResponseWriter, r *http.Request) {
	c := s.recipes()
	var out []recipeInfo
	for _, rec := range c.Recipes {
		_, builtin := recipe.Builtin(rec.ID)
		info := recipeInfo{ID: rec.ID, Description: rec.Description, Source: "user", HasBuiltin: builtin, Remote: rec.Remote != nil}
		if rec.Source == "builtin" {
			info.Source = "builtin"
		}
		for name := range rec.Models {
			info.Models = append(info.Models, name)
		}
		sort.Strings(info.Models)
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"recipes": out})
}

func validRecipeID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return !strings.HasPrefix(id, ".")
}

func (s *Server) adminRecipe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRecipeID(id) {
		fail(w, http.StatusBadRequest, "invalid recipe id")
		return
	}
	builtin, hasBuiltin := recipe.Builtin(id)
	user, err := os.ReadFile(recipe.UserPath(id))
	isUser := err == nil
	if !isUser && !hasBuiltin {
		fail(w, http.StatusNotFound, fmt.Sprintf("unknown recipe %q", id))
		return
	}
	current := builtin
	if isUser {
		current = user
	}
	resp := map[string]any{"id": id, "yaml": string(current), "source": "builtin", "has_builtin": hasBuiltin, "path": recipe.UserPath(id)}
	if isUser {
		resp["source"] = "user"
	}
	if hasBuiltin {
		resp["builtin_yaml"] = string(builtin)
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminSaveRecipe writes a user recipe, keeping it only if the whole catalog still loads.
func (s *Server) adminSaveRecipe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRecipeID(id) {
		fail(w, http.StatusBadRequest, "invalid recipe id (letters, digits, - _ . only)")
		return
	}
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "body must be {\"yaml\": \"...\"}")
		return
	}
	parsed, err := recipe.Parse([]byte(body.YAML), recipe.UserPath(id))
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if parsed.ID != id {
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("the recipe's id is %q but it is being saved as %q", parsed.ID, id))
		return
	}

	path := recipe.UserPath(id)
	previous, prevErr := os.ReadFile(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(path, []byte(body.YAML), 0o644); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := recipe.Load(); err != nil {
		if prevErr == nil {
			os.WriteFile(path, previous, 0o644)
		} else {
			os.Remove(path)
		}
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.recipes()
	var outdated []string
	for _, ref := range s.siblings(recipe.Ref{Recipe: parsed}) {
		if install.StatusOf(ref) == install.Outdated {
			outdated = append(outdated, ref.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": path, "outdated": outdated})
}

func (s *Server) adminResetRecipe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRecipeID(id) {
		fail(w, http.StatusBadRequest, "invalid recipe id")
		return
	}
	if err := os.Remove(recipe.UserPath(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fail(w, http.StatusNotFound, "no user recipe to remove for "+id)
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.recipes()
	_, builtin := recipe.Builtin(id)
	writeJSON(w, http.StatusOK, map[string]any{"removed": recipe.UserPath(id), "now": map[bool]string{true: "builtin", false: "deleted"}[builtin]})
}

// ---- bench runs ----

func benchDir() string { return filepath.Join(paths.Home(), "bench", "runs") }

func (s *Server) adminBenchRuns(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(benchDir())
	type modelSummary struct {
		Model string  `json:"model"`
		Acc   float64 `json:"acc"`
		Brier float64 `json:"brier"`
		N     int     `json:"n"`
	}
	type run struct {
		ID        string         `json:"id"`
		Suite     string         `json:"suite"`
		Records   int            `json:"records"`
		StartedAt time.Time      `json:"started_at"`
		Models    []modelSummary `json:"models"`
		Partial   bool           `json:"partial,omitempty"`
	}
	var runs []run
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(benchDir(), e.Name(), "report.json"))
		if err != nil {
			continue
		}
		var rep struct {
			Suite     string    `json:"suite"`
			Records   int       `json:"records"`
			Partial   bool      `json:"partial"`
			StartedAt time.Time `json:"started_at"`
			Models    []struct {
				Model string `json:"model"`
				Clean struct {
					N     int     `json:"n"`
					Acc   float64 `json:"acc"`
					Brier float64 `json:"brier"`
				} `json:"clean"`
			} `json:"models"`
		}
		if json.Unmarshal(data, &rep) != nil {
			continue
		}
		rr := run{ID: e.Name(), Suite: rep.Suite, Records: rep.Records, StartedAt: rep.StartedAt, Partial: rep.Partial}
		for _, m := range rep.Models {
			rr.Models = append(rr.Models, modelSummary{Model: m.Model, Acc: m.Clean.Acc, Brier: m.Clean.Brier, N: m.Clean.N})
		}
		runs = append(runs, rr)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "dir": benchDir()})
}

func (s *Server) adminBenchRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run")
	if !validRecipeID(id) { // same character rules keep the path inside benchDir
		fail(w, http.StatusBadRequest, "invalid run id")
		return
	}
	data, err := os.ReadFile(filepath.Join(benchDir(), id, "report.json"))
	if err != nil {
		fail(w, http.StatusNotFound, "no report for run "+id)
		return
	}
	writeRaw(w, http.StatusOK, data)
}

// ---- output capture ----

// lineBuffer keeps the last N lines written to it, collapsing \r progress redraws.
type lineBuffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	partial string
}

func newLineBuffer(max int) *lineBuffer { return &lineBuffer{max: max} }

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	parts := strings.Split(b.partial+string(p), "\n")
	b.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		line = strings.TrimRight(line, "\r")
		if i := strings.LastIndex(line, "\r"); i >= 0 {
			line = line[i+1:]
		}
		b.lines = append(b.lines, line)
	}
	if over := len(b.lines) - b.max; over > 0 {
		b.lines = append([]string(nil), b.lines[over:]...)
	}
	return len(p), nil
}

func (b *lineBuffer) tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := append([]string(nil), b.lines...)
	if p := b.partial; p != "" {
		if i := strings.LastIndex(p, "\r"); i >= 0 {
			p = p[i+1:]
		}
		lines = append(lines, p)
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
