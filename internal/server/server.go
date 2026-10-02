// Package server exposes one Jev-compatible API in front of every model:
//
//	POST /v1/systemone     route by the "model" field; start the worker if needed
//	GET  /v1/models        every known model with install and run state
//	GET  /healthz
//	/admin/...             management API used by the CLI and the web UI (admin.go)
//	/                      the web UI (internal/ui)
//
// Extensions to the Jev request use an x_ prefix and are stripped before the
// request reaches a backend, so the TypeSafe SDKs work unchanged.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cyprid/internal/auth"
	"cyprid/internal/install"
	"cyprid/internal/recipe"
	"cyprid/internal/supervisor"
	"cyprid/internal/ui"
)

type Config struct {
	// AdminToken is required on every admin API request (see package auth).
	AdminToken   string
	Addr         string
	MaxMemGB     float64
	DefaultModel string
	StartWait    time.Duration // how long a request waits on a cold start before getting 503
	Strict       bool          // reject emulated question types unless a request sets x_strict: false
}

type Server struct {
	cfg    Config
	sup    *supervisor.Supervisor
	client *http.Client

	mu       sync.Mutex
	catalog  *recipe.Catalog // last catalog that loaded cleanly
	badError string          // last recipe load error, logged once

	jobsMu sync.Mutex
	jobs   map[string]*installJob // by model name; the latest install or uninstall

	net network // listeners and network mode (network.go)

	links auth.Links // one-time sign-in codes for the web page

	reportedMu sync.Mutex
	reported   map[string]reportedModel // by model: the name the backend last reported for itself
}

type reportedModel struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func New(cfg Config, catalog *recipe.Catalog, sup *supervisor.Supervisor) *Server {
	return &Server{cfg: cfg, catalog: catalog, sup: sup, client: &http.Client{Timeout: 5 * time.Minute},
		jobs: map[string]*installJob{}, reported: map[string]reportedModel{}}
}

// recipes reloads the recipe files so edits apply without restarting the server
// (a running model picks them up the next time it starts). If a user recipe is
// broken, the last good catalog stays in use.
func (s *Server) recipes() *recipe.Catalog {
	c, err := recipe.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if err.Error() != s.badError {
			log.Printf("recipe error, still using previous recipes: %v", err)
			s.badError = err.Error()
		}
		return s.catalog
	}
	s.catalog, s.badError = c, ""
	return c
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/systemone", s.apiAuth(http.HandlerFunc(s.systemOne)))
	mux.Handle("GET /v1/models", s.apiAuth(http.HandlerFunc(s.models)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("/admin/", localOnly(s.requireAdmin(s.adminRoutes())))
	mux.Handle("/", localOnly(s.signIn(ui.Handler())))
	return logRequests(mux)
}

var questionTypes = map[string]bool{"noul": true, "choice": true, "score": true}

func (s *Server) systemOne(w http.ResponseWriter, r *http.Request) {
	// Top-level fields stay raw JSON so state and questions reach the backend
	// byte for byte: re-encoding them through Go maps would sort object keys,
	// silently reordering choice options, and option order can change answers.
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		fail(w, http.StatusUnprocessableEntity, "request body must be a JSON object: "+err.Error())
		return
	}

	catalog := s.recipes()
	var name string
	json.Unmarshal(body["model"], &name)
	if name == "" {
		name = s.cfg.DefaultModel
	}
	if name == "" {
		fail(w, http.StatusUnprocessableEntity, "no model given and no default configured; known models: "+strings.Join(catalog.Names(), ", "))
		return
	}
	ref, ok := catalog.Resolve(name)
	if !ok {
		fail(w, http.StatusNotFound, fmt.Sprintf("unknown model %q; known models: %s", name, strings.Join(catalog.Names(), ", ")))
		return
	}

	strict := s.cfg.Strict
	if raw, ok := body["x_strict"]; ok {
		json.Unmarshal(raw, &strict)
	}
	wantCalibration := true
	if raw, ok := body["x_calibrate"]; ok {
		json.Unmarshal(raw, &wantCalibration)
	}
	for k := range body {
		if strings.HasPrefix(k, "x_") {
			delete(body, k)
		}
	}
	if msg := validate(body, ref, strict); msg != "" {
		fail(w, http.StatusUnprocessableEntity, msg)
		return
	}
	body["model"], _ = json.Marshal(ref.UpstreamModel())
	payload, _ := json.Marshal(body)

	start := time.Now()
	var (
		baseURL string
		header  = http.Header{"Content-Type": {"application/json"}}
	)
	if ref.IsRemote() {
		baseURL = strings.TrimRight(ref.Recipe.Remote.URL, "/")
		if auth := r.Header.Get("Authorization"); auth != "" {
			header.Set("Authorization", auth)
		} else if env := ref.Recipe.Remote.APIKeyEnv; env != "" && os.Getenv(env) != "" {
			header.Set("Authorization", "Bearer "+os.Getenv(env))
		} else if env != "" {
			fail(w, http.StatusUnauthorized, fmt.Sprintf("%s needs an API key: set %s for the server, or send an Authorization header", ref.Name, env))
			return
		}
	} else {
		url, release, err := s.sup.Acquire(r.Context(), ref, s.cfg.StartWait)
		if err != nil {
			s.acquireFailed(w, ref, err)
			return
		}
		defer release()
		baseURL = url
	}

	status, respBody, err := s.forward(r.Context(), baseURL+"/v1/systemone", header, payload)
	if err != nil {
		fail(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", ref.Name, err))
		return
	}
	backendModel := s.noteReportedModel(ref, status, respBody)
	out, changed := normalise(ref, status, respBody, body["questions"])
	if backendModel != "" {
		w.Header().Set("X-Cyprid-Backend-Model", backendModel)
	}
	w.Header().Set("X-Cyprid-Model", ref.Name)
	w.Header().Set("X-Cyprid-Latency-Ms", strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	if len(changed) > 0 {
		w.Header().Set("X-Cyprid-Normalised", strings.Join(changed, ","))
	}
	if t, _ := s.calibrationFor(ref); t > 0 && wantCalibration && status < 300 {
		if calibrated, ok := calibrate(out, body["questions"], t); ok {
			out = calibrated
			w.Header().Set("X-Cyprid-Calibrated", strconv.FormatFloat(t, 'f', -1, 64))
		}
	}
	writeRaw(w, status, out)
}

// validate does the checks every backend needs, plus capability checks that
// only the gateway knows about. Backends still do their own detailed validation.
func validate(body map[string]json.RawMessage, ref recipe.Ref, strict bool) string {
	if _, ok := body["state"]; !ok {
		return "state is required"
	}
	var qs map[string]json.RawMessage
	if json.Unmarshal(body["questions"], &qs) != nil || len(qs) == 0 {
		return "questions must be a non-empty object mapping ids to questions"
	}
	for id, raw := range qs {
		var q struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &q) != nil {
			return fmt.Sprintf("questions.%s must be an object", id)
		}
		if !questionTypes[q.Type] {
			return fmt.Sprintf("questions.%s.type must be noul, choice or score", id)
		}
		switch ref.Capability(q.Type) {
		case "unsupported":
			return fmt.Sprintf("questions.%s: model %s does not support %s questions", id, ref.Name, q.Type)
		case "emulated":
			if strict {
				return fmt.Sprintf("questions.%s: model %s only emulates %s questions; send \"x_strict\": false to allow it", id, ref.Name, q.Type)
			}
		}
	}
	return ""
}

func (s *Server) forward(ctx context.Context, url string, header http.Header, payload []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header = header
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// noteReportedModel keeps the model name a backend reports for itself (such as
// "decider-2b-v11" or "jev-1.13.0"), which the response then replaces with the
// name the client used. An echo of the name we sent says nothing, so it is ignored.
func (s *Server) noteReportedModel(ref recipe.Ref, status int, body []byte) string {
	if status >= 300 {
		return ""
	}
	var resp struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Model == "" || resp.Model == ref.UpstreamModel() || resp.Model == ref.Name {
		return ""
	}
	s.reportedMu.Lock()
	s.reported[ref.Name] = reportedModel{Name: resp.Model, At: time.Now()}
	s.reportedMu.Unlock()
	return resp.Model
}

func (s *Server) acquireFailed(w http.ResponseWriter, ref recipe.Ref, err error) {
	var startErr *supervisor.StartError
	var memErr *supervisor.MemoryError
	switch {
	case errors.Is(err, supervisor.ErrNotInstalled):
		reason := "not installed"
		if install.StatusOf(ref) == install.Outdated {
			reason = "outdated (its recipe's install section changed)"
		}
		fail(w, http.StatusConflict, fmt.Sprintf("model %s is %s; run: cyprid install %s", ref.Name, reason, ref.Name))
	case errors.Is(err, supervisor.ErrStillLoading):
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, fmt.Sprintf("model %s is loading; retry shortly", ref.Name))
	case errors.As(err, &memErr):
		fail(w, http.StatusServiceUnavailable, memErr.Error())
	case errors.As(err, &startErr):
		if startErr.Retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(startErr.Retry.Seconds())+1))
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"detail": startErr.Error() + "; see: cyprid logs " + ref.Name,
			"logs":   startErr.Logs,
		})
	case errors.Is(err, context.Canceled):
		// client went away
	default:
		fail(w, http.StatusInternalServerError, err.Error())
	}
}

type modelInfo struct {
	Name         string            `json:"name"`
	Recipe       string            `json:"recipe"`
	Description  string            `json:"description,omitempty"`
	Aliases      []string          `json:"aliases,omitempty"`
	Kind         string            `json:"kind"`
	Installed    install.Status    `json:"installed"`
	Capabilities map[string]string `json:"capabilities"`
	MemGB        float64           `json:"mem_gb,omitempty"`
	License      string            `json:"license,omitempty"`
	Base         string            `json:"base,omitempty"`                    // foundation model and its maker
	Context      *recipe.Context   `json:"context,omitempty"`                 // longest input accepted as served
	Calibration  float64           `json:"calibration_temperature,omitempty"` // opt-in temperature the gateway applies
	Links        []recipe.Link     `json:"links,omitempty"`                   // original sources: project, model card, weights
	Version      *install.Version  `json:"version,omitempty"`                 // what is installed: package versions, weights revisions
	Reported     *reportedModel    `json:"reported_model,omitempty"`          // the name the backend last reported for itself
	CachedBytes  int64             `json:"cached_weights_bytes,omitempty"`    // disk that deleting its weights would free
	Job          *jobStatus        `json:"install_job,omitempty"`             // a running or failed install/uninstall
	supervisor.Status
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	var out []modelInfo
	all := s.recipes().Models()
	for _, ref := range all {
		info := modelInfo{
			Name:         ref.Name,
			Recipe:       ref.Recipe.ID,
			Description:  ref.Model.Description,
			Aliases:      ref.Model.Aliases,
			Kind:         "local",
			Installed:    install.StatusOf(ref),
			Capabilities: map[string]string{},
			MemGB:        ref.MemGB(),
			License:      ref.Recipe.License,
			Base:         ref.Base(),
			Context:      ref.Context(),
			Links:        ref.Links(),
			Version:      install.VersionOf(ref),
		}
		if !ref.IsRemote() {
			info.CachedBytes = install.CachedBytes(ref, all)
		}
		info.Calibration, _ = s.calibrationFor(ref)
		s.reportedMu.Lock()
		if rm, ok := s.reported[ref.Name]; ok {
			info.Reported = &rm
		}
		s.reportedMu.Unlock()
		for t := range questionTypes {
			info.Capabilities[t] = ref.Capability(t)
		}
		if ref.IsRemote() {
			info.Kind = "remote"
			info.Status = supervisor.Status{State: "remote"}
		} else {
			info.Status = s.sup.Status(ref.Name)
		}
		if js := s.jobStatus(ref.Name, 0); js != nil && js.State != "done" {
			info.Job = js
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func fail(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // messages like "Bearer <key>" should read as written
	enc.Encode(v)
	writeRaw(w, status, bytes.TrimRight(buf.Bytes(), "\n"))
}

func writeRaw(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method == http.MethodGet && rec.status < 400 {
			return // the web UI polls; only log requests that change or fail something
		}
		model := w.Header().Get("X-Cyprid-Model")
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), model)
	})
}
