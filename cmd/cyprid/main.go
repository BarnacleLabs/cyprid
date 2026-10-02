// cyprid runs Jev-like decision models behind one Jev-compatible API.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"cyprid/internal/auth"
	"cyprid/internal/bench"
	"cyprid/internal/config"
	"cyprid/internal/install"
	"cyprid/internal/paths"
	"cyprid/internal/recipe"
	"cyprid/internal/reveal"
	"cyprid/internal/server"
	"cyprid/internal/supervisor"
	"cyprid/internal/version"
)

const defaultAddr = "127.0.0.1:8765"

const usage = `cyprid: one Jev-compatible API for many decision models

Usage:
  cyprid serve [flags]            run the server (default ` + defaultAddr + `)
  cyprid ui                       open the web page in your browser, signed in
  cyprid version                  which build of cyprid this is
  cyprid list [--links]           every model: installed, running; --links adds sources and licence
  cyprid info <model>             installed version, sources, context, state
  cyprid install <model>...       create the model's environment and download its weights
  cyprid uninstall [--delete-weights] <model>...
                                  remove a model's environment; --delete-weights also removes its
                                  downloaded weights (except any another installed model uses)
  cyprid ps                       models running in the server
  cyprid start <model>            load a model now instead of on its first request
  cyprid stop <model>             stop a running model
  cyprid logs [-n N] [-f] <model> a model's worker log
  cyprid recipe list | show | edit | reset | path <id>
                                  view, edit or reset the recipes that install and launch models
  cyprid calibrate <model> [--temperature T | --off | --reset]
                                  opt-in calibration, with temperatures fitted from your bench runs
  cyprid open [bench|logs|recipes|weights|config|run <id>|log <model>]
                                  show one of cyprid's folders or files in Finder (default: all of ~/.cyprid)
  cyprid network [on|off|rotate-key]
                                  whether other machines can reach the API
  cyprid bench [--suite S] [--models a,b] [--limit N] [--resume]
                                  score models on a labelled suite (kev-transfer-v4, jevbench-public)

The server's address for client commands comes from CYPRID_URL
(default http://` + defaultAddr + `). State lives in ~/.cyprid (CYPRID_HOME).
`

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "list", "ls":
		err = list(len(args) > 0 && (args[0] == "--links" || args[0] == "-l"))
	case "install":
		err = forEachModel(args, func(c *recipe.Catalog, ref recipe.Ref) error { return install.Install(ref, os.Stdout) })
	case "uninstall":
		deleteWeights := false
		var names []string
		for _, a := range args {
			if a == "--delete-weights" {
				deleteWeights = true
			} else {
				names = append(names, a)
			}
		}
		err = forEachModel(names, func(c *recipe.Catalog, ref recipe.Ref) error {
			var siblings []recipe.Ref
			for _, r := range c.Models() {
				if r.Recipe.ID == ref.Recipe.ID {
					siblings = append(siblings, r)
				}
			}
			if err := install.Uninstall(ref, siblings, os.Stdout); err != nil {
				return err
			}
			if deleteWeights {
				_, err := install.DeleteWeights(ref, c.Models(), os.Stdout)
				return err
			}
			if n := install.CachedBytes(ref, c.Models()); n > 0 {
				fmt.Printf("its weights (%s) are still in the Hugging Face cache; add --delete-weights to remove them\n", install.HumanBytes(n))
			}
			return nil
		})
	case "ps":
		err = ps()
	case "info":
		err = infoCmd(args)
	case "open":
		err = openCmd(args)
	case "ui":
		err = uiCmd()
	case "version", "--version", "-v":
		fmt.Println(version.Get())
	case "start":
		err = adminPost(args, "start?wait=1")
	case "stop":
		err = adminPost(args, "stop")
	case "logs":
		err = logs(args)
	case "recipe", "recipes":
		err = recipeCmd(args)
	case "bench":
		err = benchCmd(args)
	case "network":
		err = networkCmd(args)
	case "calibrate", "calibration":
		err = calibrateCmd(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("CYPRID_ADDR", defaultAddr), "listen address")
	defModel := fs.String("default-model", os.Getenv("CYPRID_DEFAULT_MODEL"), "model used when a request has no \"model\" field")
	maxMem := fs.Float64("max-mem-gb", defaultMemBudget(), "memory budget for running models (0 = unlimited)")
	startWait := fs.Duration("start-wait", 2*time.Minute, "how long a request waits for a cold model before getting 503 + Retry-After")
	netFlag := fs.String("network", "", "on or off: also listen on all network interfaces (saved; toggle at runtime in the UI or with: cyprid network)")
	noBrowser := fs.Bool("no-browser", os.Getenv("CYPRID_NO_BROWSER") != "", "don't open the web page in a browser on start (also CYPRID_NO_BROWSER=1)")
	strict := fs.Bool("strict", false, "reject emulated question types (e.g. noul on GLiNER) unless a request sets \"x_strict\": false")
	fs.Parse(args)

	catalog, err := recipe.Load()
	if err != nil {
		return err
	}
	token, err := auth.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("creating the admin access token: %w", err)
	}
	sup := supervisor.New(supervisor.Config{MaxMemGB: *maxMem})
	srv := server.New(server.Config{AdminToken: token, Addr: *addr, MaxMemGB: *maxMem, DefaultModel: *defModel, StartWait: *startWait, Strict: *strict}, catalog, sup)

	if *netFlag != "" {
		if *netFlag != "on" && *netFlag != "off" {
			return errors.New("--network must be on or off")
		}
		settings, err := config.Load()
		if err != nil {
			return err
		}
		settings.Network = *netFlag == "on"
		if err := config.Save(settings); err != nil {
			return err
		}
	}
	if err := srv.Start(*addr); err != nil {
		sup.StopAll()
		return err
	}

	log.Printf("cyprid %s listening on http://%s (memory budget %.0fGB)", version.Get().Short(), *addr, *maxMem)
	log.Printf("web page: %s", srv.LoginLink(15*time.Minute))
	log.Printf("  (that sign-in link works once, for 15 minutes; afterwards open the page with: cyprid ui)")
	if !*noBrowser {
		// Its own link, so the one printed above still works if this one is used.
		if err := exec.Command("open", srv.LoginLink(2*time.Minute)).Run(); err == nil {
			log.Printf("opened the web page in your browser (use --no-browser to stop that)")
		}
	}
	for _, ref := range catalog.Models() {
		log.Printf("  %-18s %s", ref.Name, install.StatusOf(ref))
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Printf("shutting down: stopping models")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	sup.StopAll()
	return nil
}

// defaultMemBudget is 75% of physical memory; Apple Silicon shares it between CPU and GPU.
func defaultMemBudget() float64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	bytes, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return bytes / (1 << 30) * 0.75
}

func forEachModel(names []string, fn func(*recipe.Catalog, recipe.Ref) error) error {
	if len(names) == 0 {
		return errors.New("name at least one model (see: cyprid list)")
	}
	c, err := recipe.Load()
	if err != nil {
		return err
	}
	for _, n := range names {
		ref, ok := c.Resolve(n)
		if !ok {
			return fmt.Errorf("unknown model %q; known models: %s", n, strings.Join(c.Names(), ", "))
		}
		if err := fn(c, ref); err != nil {
			return fmt.Errorf("%s: %w", ref.Name, err)
		}
	}
	return nil
}

type modelInfo struct {
	Name      string   `json:"name"`
	Recipe    string   `json:"recipe"`
	Aliases   []string `json:"aliases"`
	Installed string   `json:"installed"`
	State     string   `json:"state"`
	Port      int      `json:"port"`
	MemGB     float64  `json:"mem_gb"`
	Inflight  int      `json:"inflight"`
	Error     string   `json:"error"`
	Desc      string   `json:"description"`
	Reported  *struct {
		Name string    `json:"name"`
		At   time.Time `json:"at"`
	} `json:"reported_model"`
}

func fetchModels() ([]modelInfo, error) {
	resp, err := http.Get(serverURL() + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("server not reachable at %s (start it with: cyprid serve)", serverURL())
	}
	defer resp.Body.Close()
	var out struct{ Models []modelInfo }
	return out.Models, json.NewDecoder(resp.Body).Decode(&out)
}

func list(showLinks bool) error {
	c, err := recipe.Load()
	if err != nil {
		return err
	}
	running := map[string]modelInfo{}
	if ms, err := fetchModels(); err == nil {
		for _, m := range ms {
			running[m.Name] = m
		}
	}
	if showLinks {
		for _, ref := range c.Models() {
			fmt.Printf("%s", ref.Name)
			if ref.Recipe.License != "" {
				fmt.Printf("  (%s)", ref.Recipe.License)
			}
			fmt.Println()
			for _, l := range ref.Links() {
				fmt.Printf("  %-13s %s\n", l.Label, l.URL)
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tRECIPE\tINSTALLED\tVERSION\tSTATE\tMEM\tDESCRIPTION")
	for _, ref := range c.Models() {
		state := "-"
		if m, ok := running[ref.Name]; ok {
			state = m.State
		}
		mem := "-"
		if ref.MemGB() > 0 {
			mem = fmt.Sprintf("~%.0fGB", ref.MemGB())
		}
		name := ref.Name
		if len(ref.Model.Aliases) > 0 {
			name += " (" + strings.Join(ref.Model.Aliases, ", ") + ")"
		}
		version := "-"
		if v := install.VersionOf(ref); v != nil && len(v.Packages) > 0 {
			version = v.Packages[0].Version
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, ref.Recipe.ID, install.StatusOf(ref), version, state, mem, ref.Model.Description)
	}
	return tw.Flush()
}

func ps() error {
	ms, err := fetchModels()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tSTATE\tPORT\tIN-FLIGHT\tMEM\tERROR")
	n := 0
	for _, m := range ms {
		if m.State == "stopped" || m.State == "remote" {
			continue
		}
		n++
		port := "-"
		if m.Port > 0 {
			port = strconv.Itoa(m.Port)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t~%.0fGB\t%s\n", m.Name, m.State, port, m.Inflight, m.MemGB, m.Error)
	}
	tw.Flush()
	if n == 0 {
		fmt.Println("(no models running; they start on their first request, or with: cyprid start <model>)")
	}
	return nil
}

func adminPost(args []string, action string) error {
	if len(args) != 1 {
		return errors.New("name one model")
	}
	resp, err := adminRequest("POST", serverURL()+"/admin/models/"+args[0]+"/"+action, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Detail string
			Logs   []string
		}
		json.Unmarshal(body, &e)
		for _, l := range e.Logs {
			fmt.Fprintln(os.Stderr, "  |", l)
		}
		return errors.New(e.Detail)
	}
	fmt.Println(strings.TrimSpace(string(body)))
	return nil
}

// logs reads the model's log file directly, so it works whether or not the server is running.
func logs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	n := fs.Int("n", 100, "lines to show")
	follow := fs.Bool("f", false, "keep printing new output")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("name one model")
	}
	c, err := recipe.Load()
	if err != nil {
		return err
	}
	ref, ok := c.Resolve(fs.Arg(0))
	if !ok {
		return fmt.Errorf("unknown model %q", fs.Arg(0))
	}
	path := filepath.Join(paths.Logs(), ref.Name+".log")
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no logs yet for %s (%s)", ref.Name, path)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > *n {
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	if !*follow {
		return nil
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		fmt.Print(line)
		if err == io.EOF {
			time.Sleep(300 * time.Millisecond)
		} else if err != nil {
			return err
		}
	}
}

func recipeCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	c, err := recipe.Load()
	sub := args[0]
	if err != nil && sub != "edit" && sub != "reset" && sub != "path" {
		return err
	}
	needID := func() (string, error) {
		if len(args) != 2 {
			return "", fmt.Errorf("usage: cyprid recipe %s <id>", sub)
		}
		return args[1], nil
	}
	switch sub {
	case "list":
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RECIPE\tSOURCE\tMODELS\tDESCRIPTION")
		for _, r := range c.Recipes {
			var names []string
			for _, ref := range c.Models() {
				if ref.Recipe == r {
					names = append(names, ref.Name)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Source, strings.Join(names, ", "), r.Description)
		}
		return tw.Flush()
	case "show":
		id, err := needID()
		if err != nil {
			return err
		}
		r, ok := c.Recipe(id)
		if !ok {
			return fmt.Errorf("unknown recipe %q", id)
		}
		if r.Source == "builtin" {
			data, _ := recipe.Builtin(id)
			fmt.Printf("# built-in (edit with: cyprid recipe edit %s)\n%s", id, data)
			return nil
		}
		data, err := os.ReadFile(r.Source)
		fmt.Printf("# %s\n%s", r.Source, data)
		return err
	case "path":
		id, err := needID()
		if err != nil {
			return err
		}
		fmt.Println(recipe.UserPath(id))
		return nil
	case "edit":
		id, err := needID()
		if err != nil {
			return err
		}
		path := recipe.UserPath(id)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			data, ok := recipe.Builtin(id)
			if !ok {
				data = []byte(fmt.Sprintf("id: %s\ndescription: \ninstall:\n  python: \"3.12\"\n  packages: []\nlaunch:\n  cmd: []\n  ready:\n    path: /health\nmodels:\n  %s: {}\n", id, id))
			}
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return err
			}
		}
		editor := envOr("VISUAL", envOr("EDITOR", "nano")) // nano shows its keys on screen; vi traps newcomers
		ed := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
		ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := ed.Run(); err != nil {
			return err
		}
		data, _ := os.ReadFile(path)
		if _, err := recipe.Parse(data, path); err != nil {
			return fmt.Errorf("%s is not valid (the server keeps using the previous version until fixed): %w", path, err)
		}
		fmt.Printf("saved %s; it applies the next time the model starts (reinstall if you changed the install section)\n", path)
		return nil
	case "reset":
		id, err := needID()
		if err != nil {
			return err
		}
		if _, ok := recipe.Builtin(id); !ok {
			return fmt.Errorf("%q has no built-in recipe to reset to (delete %s to remove it)", id, recipe.UserPath(id))
		}
		if err := os.Remove(recipe.UserPath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("%s reset to the built-in recipe\n", id)
		return nil
	}
	return fmt.Errorf("unknown recipe command %q", sub)
}

func serverURL() string {
	return strings.TrimRight(envOr("CYPRID_URL", "http://"+defaultAddr), "/")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	suite := fs.String("suite", "kev-transfer-v4", "built-in suite name or a JSONL file in Kev's format")
	models := fs.String("models", "", "comma-separated models (default: every installed local model)")
	limit := fs.Int("limit", 0, "only use the first N records (for a quick check)")
	conc := fs.Int("concurrency", 1, "requests in flight per model (1 gives honest latency numbers)")
	outDir := fs.String("out", "", "where to write predictions and report.json (default ~/.cyprid/bench/runs/<time>)")
	resume := fs.Bool("resume", false, "continue an interrupted run in --out, skipping records already answered")
	fs.Parse(args)

	recs, builtin, err := bench.LoadSuite(*suite)
	if err != nil {
		return err
	}
	if *limit > 0 && *limit < len(recs) {
		recs = recs[:*limit]
	}

	var names []string
	if *models != "" {
		names = strings.Split(*models, ",")
	} else {
		ms, err := fetchModels()
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.Installed == string(install.Installed) {
				names = append(names, m.Name)
			}
		}
		if len(names) == 0 {
			return errors.New("no installed models; install one or pass --models")
		}
	}

	started := time.Now()
	if *outDir == "" {
		*outDir = filepath.Join(paths.Home(), "bench", "runs", started.Format("20060102-150405"))
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	rep := bench.Report{Suite: *suite, Records: len(recs), StartedAt: started, Published: map[string]bench.Published{}}
	token, err := auth.Load()
	if err != nil {
		return err
	}
	runner := &bench.Runner{ServerURL: serverURL(), Concurrency: *conc, AdminToken: token}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		path := filepath.Join(*outDir, "predictions-"+name+".jsonl")
		var preds []bench.Prediction
		if *resume {
			preds = readPredictions(path) // answered rows only; failed ones are retried
		}
		answered := map[string]bool{}
		for _, p := range preds {
			answered[p.ID] = true
		}
		var todo []bench.Record
		for _, r := range recs {
			if !answered[r.Meta.ID] {
				todo = append(todo, r)
			}
		}
		// Keep what's already answered, then append each record the moment it's answered,
		// so an interrupted run loses nothing.
		if err := writeJSONL(path, preds); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		if len(todo) > 0 {
			fmt.Printf("%s: loading...", name)
			if err := runner.Warm(name); err != nil {
				f.Close()
				fmt.Println()
				return err
			}
			if len(preds) > 0 {
				fmt.Printf("\r\033[K%s: resuming, %d of %d records already answered\n", name, len(recs)-len(todo), len(recs))
			}
		}
		t0, already := time.Now(), len(recs)-len(todo)
		runner.Progress = func(done, total int) {
			if done%25 == 0 || done == total {
				fmt.Printf("\r\033[K%s: %d/%d records (%s)", name, already+done, len(recs), time.Since(t0).Round(time.Second))
			}
		}
		runner.OnRecord = func(ps []bench.Prediction) {
			for _, p := range ps {
				enc.Encode(p)
			}
			preds = append(preds, ps...)
		}
		runner.Run(ctx, name, todo)
		f.Close()
		fmt.Println()
		if len(preds) > 0 {
			rep.Models = append(rep.Models, bench.Summarise(name, preds))
		}
		if ctx.Err() != nil {
			rep.Partial = true
		}
	}
	if builtin != nil {
		for name, pub := range builtin.Published {
			ran := false
			for _, n := range names {
				ran = ran || n == name
			}
			// Show published numbers for models we ran (to check the harness) and for ones we can't (Jev without a key).
			if ran || name == "jev" {
				rep.Published[name] = pub
			}
		}
	}
	if len(rep.Models) == 0 {
		return errors.New("interrupted before any record was answered")
	}
	rep.Print(os.Stdout)
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "report.json"), data, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nPredictions and report.json written to %s\n", *outDir)
	if rep.Partial {
		limitArg := ""
		if *limit > 0 {
			limitArg = fmt.Sprintf(" --limit %d", *limit)
		}
		fmt.Printf("Interrupted. Continue with: cyprid bench --suite %s --models %s%s --out %s --resume\n", *suite, strings.Join(names, ","), limitArg, *outDir)
	}
	return nil
}

// readPredictions loads a saved run's answered predictions, skipping failed records so a resume retries them.
func readPredictions(path string) []bench.Prediction {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ids []string
	byRecord := map[string][]bench.Prediction{}
	failed := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var p bench.Prediction
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		if _, seen := byRecord[p.ID]; !seen {
			ids = append(ids, p.ID)
		}
		byRecord[p.ID] = append(byRecord[p.ID], p)
		if p.Error != "" {
			failed[p.ID] = true
		}
	}
	var out []bench.Prediction
	for _, id := range ids {
		if !failed[id] {
			out = append(out, byRecord[id]...)
		}
	}
	return out
}

func writeJSONL(path string, rows []bench.Prediction) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func networkCmd(args []string) error {
	var method, path string
	var body io.Reader
	switch {
	case len(args) == 0:
		method, path = "GET", "/admin/network"
	case args[0] == "on" || args[0] == "off":
		method, path = "PUT", "/admin/network"
		body = strings.NewReader(fmt.Sprintf(`{"enabled": %v}`, args[0] == "on"))
	case args[0] == "rotate-key":
		method, path = "POST", "/admin/network/rotate-key"
	default:
		return errors.New("usage: cyprid network [on|off|rotate-key]")
	}
	resp, err := adminRequest(method, serverURL()+path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var info struct {
		Enabled, Listening bool
		URLs               []string
		APIKey             string `json:"api_key"`
		Error, Detail      string
	}
	json.NewDecoder(resp.Body).Decode(&info)
	if resp.StatusCode >= 300 {
		return errors.New(info.Detail)
	}
	if !info.Enabled {
		fmt.Println("network: off (localhost only)")
		return nil
	}
	state := "on"
	if !info.Listening {
		state = "on, but not listening: " + info.Error
	}
	fmt.Printf("network: %s\n", state)
	for _, u := range info.URLs {
		fmt.Printf("  %s\n", u)
	}
	fmt.Printf("API key (other machines send Authorization: Bearer <key>):\n  %s\n", info.APIKey)
	return nil
}

func infoCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: cyprid info <model>")
	}
	c, err := recipe.Load()
	if err != nil {
		return err
	}
	ref, ok := c.Resolve(args[0])
	if !ok {
		return fmt.Errorf("unknown model %q; known models: %s", args[0], strings.Join(c.Names(), ", "))
	}
	fmt.Printf("%s", ref.Name)
	if len(ref.Model.Aliases) > 0 {
		fmt.Printf(" (%s)", strings.Join(ref.Model.Aliases, ", "))
	}
	fmt.Println()
	if ref.Model.Description != "" {
		fmt.Printf("  %s\n", ref.Model.Description)
	}
	fmt.Printf("\n  recipe      %s (%s)\n", ref.Recipe.ID, ref.Recipe.Source)
	if ref.Recipe.License != "" {
		fmt.Printf("  licence     %s\n", ref.Recipe.License)
	}
	if b := ref.Base(); b != "" {
		fmt.Printf("  built on    %s\n", b)
	}
	if c := ref.Context(); c != nil {
		fmt.Printf("  context     %s tokens", commaInt(c.Tokens))
		if c.Note != "" {
			fmt.Printf(" (%s)", c.Note)
		}
		fmt.Println()
	}
	fmt.Printf("  installed   %s\n", install.StatusOf(ref))

	if v := install.VersionOf(ref); v != nil {
		fmt.Println("\n  Version")
		for _, p := range v.Packages {
			line := fmt.Sprintf("    %-28s %s", p.Name, p.Version)
			if p.Commit != "" {
				line += fmt.Sprintf("  (git %s @ %s)", p.Git, p.Commit)
			}
			fmt.Println(line)
		}
		for _, w := range v.Weights {
			if w.Error != "" {
				fmt.Printf("    %-28s %s\n", w.Repo, w.Error)
				continue
			}
			line := fmt.Sprintf("    %-28s revision %s", w.Repo, w.Revision)
			if !w.Date.IsZero() {
				line += ", committed " + w.Date.Format("2006-01-02")
			}
			if !w.Pinned {
				line += " (latest at install time; not pinned)"
			}
			fmt.Println(line)
		}
		when := "at install"
		if v.Backfilled {
			when = "from disk, after install"
		}
		fmt.Printf("    recorded %s, %s\n", v.RecordedAt.Format("2006-01-02 15:04"), when)
	}

	if ms, err := fetchModels(); err == nil {
		for _, m := range ms {
			if m.Name != ref.Name {
				continue
			}
			fmt.Printf("\n  state       %s", m.State)
			if m.Port > 0 {
				fmt.Printf(" (port %d)", m.Port)
			}
			fmt.Println()
			if m.Reported != nil {
				fmt.Printf("  reports     %s (last response, %s)\n", m.Reported.Name, m.Reported.At.Format("15:04:05"))
			}
		}
	}

	if plan := install.WeightsPlan(ref, c.Models()); len(plan) > 0 {
		fmt.Println("\n  Downloaded weights")
		for _, u := range plan {
			switch {
			case !u.Present:
				fmt.Printf("    %-28s not in the cache\n", u.Repo)
			case len(u.SharedWith) > 0:
				fmt.Printf("    %-28s %s (also used by %s)\n", u.Repo, install.HumanBytes(u.Bytes), strings.Join(u.SharedWith, ", "))
			default:
				fmt.Printf("    %-28s %s\n", u.Repo, install.HumanBytes(u.Bytes))
			}
		}
	}

	if links := ref.Links(); len(links) > 0 {
		fmt.Println("\n  Sources")
		for _, l := range links {
			fmt.Printf("    %-13s %s\n", l.Label, l.URL)
		}
	}
	return nil
}

func commaInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func calibrateCmd(args []string) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	temp := fs.Float64("temperature", 0, "turn calibration on with this temperature (e.g. 2.2)")
	off := fs.Bool("off", false, "turn calibration off for this model, even if its recipe sets one")
	reset := fs.Bool("reset", false, "forget your setting and use the recipe's (normally off)")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("usage: cyprid calibrate <model> [--temperature T | --off | --reset]")
	}
	url := serverURL() + "/admin/models/" + name + "/calibration"
	var body io.Reader
	method := "GET"
	switch {
	case *temp > 0:
		method, body = "PUT", strings.NewReader(fmt.Sprintf(`{"temperature": %g}`, *temp))
	case *off:
		method, body = "PUT", strings.NewReader(`{"temperature": 0}`)
	case *reset:
		method, body = "PUT", strings.NewReader(`{"temperature": null}`)
	}
	resp, err := adminRequest(method, url, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var info struct {
		Model, Source, Detail string
		Temperature           float64
		Suggestions           []struct {
			Run, Suite string
			N          int
			T          float64 `json:"temperature"`
			EB         float64 `json:"ece_before"`
			EA         float64 `json:"ece_after"`
			CB         float64 `json:"confident_errors_before"`
			CA         float64 `json:"confident_errors_after"`
		}
	}
	json.NewDecoder(resp.Body).Decode(&info)
	if resp.StatusCode >= 300 {
		return errors.New(info.Detail)
	}
	if info.Temperature > 0 {
		fmt.Printf("%s: calibrated, temperature %g (%s)\n", info.Model, info.Temperature, info.Source)
	} else {
		fmt.Printf("%s: calibration %s\n", info.Model, info.Source)
	}
	if method != "GET" {
		return nil
	}
	if len(info.Suggestions) == 0 {
		fmt.Println("\nNo benchmark runs of this model to fit a temperature from; run: cyprid bench --models " + name)
		return nil
	}
	fmt.Println("\nTemperatures fitted from your benchmark runs (answers never change; confidence does):")
	for _, sg := range info.Suggestions {
		fmt.Printf("  %-28s %-16s T=%-5.2f n=%-4d ECE %.3f -> %.3f  confident errors %.1f%% -> %.1f%%\n",
			sg.Run, sg.Suite, sg.T, sg.N, sg.EB, sg.EA, sg.CB*100, sg.CA*100)
	}
	fmt.Println("\nThese are fitted and scored on the same run, so they flatter a little; a temperature that")
	fmt.Println("helps on runs from different suites is the one to trust. Turn it on with --temperature T.")
	return nil
}

func openCmd(args []string) error {
	what, name := "", ""
	if len(args) > 0 {
		what = args[0]
	}
	if len(args) > 1 {
		name = args[1]
	}
	c, err := recipe.Load()
	if err != nil {
		return err
	}
	path, isFile, err := reveal.Locate(what, name, c)
	if err != nil {
		return err
	}
	if err := reveal.Open(path, isFile); err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

// adminRequest calls the running server's admin API with this user's access token.
func adminRequest(method, url string, body io.Reader) (*http.Response, error) {
	token, err := auth.Load()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.Header, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("server not reachable at %s (start it with: cyprid serve)", serverURL())
	}
	return resp, nil
}

// uiCmd opens the web page, signed in, in the default browser.
func uiCmd() error {
	resp, err := adminRequest("POST", serverURL()+"/admin/login-link", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct{ URL, Detail string }
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 || out.URL == "" {
		return fmt.Errorf("couldn't get a sign-in link: %s", out.Detail)
	}
	if err := exec.Command("open", out.URL).Run(); err != nil {
		return err
	}
	fmt.Println("opened the cyprid web page in your browser")
	return nil
}
