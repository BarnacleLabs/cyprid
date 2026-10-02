// Package recipe describes how to install, launch and talk to each backend.
//
// A recipe is a YAML file. Built-in recipes are embedded in the binary; a file
// in ~/.cyprid/recipes with the same id replaces the built-in one, and
// files with new ids add backends. One recipe can expose several models (e.g.
// the Kev sizes), which share an environment but each run as their own process.
package recipe

import (
	"embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"cyprid/internal/paths"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

type Recipe struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description,omitempty"`
	// Homepage is the project's source (code repository or project page); License is its licence.
	Homepage string `yaml:"homepage,omitempty"`
	License  string `yaml:"license,omitempty"`
	// Base is the foundation model this is built on and who made it, e.g.
	// "Qwen3.5 (Alibaba, China)". A model can override it.
	Base string `yaml:"base,omitempty"`
	// Context is the longest input the model accepts as served here. A model can override it.
	Context *Context `yaml:"context,omitempty"`
	// Calibration is an opt-in temperature the gateway applies to answers. Off unless set.
	Calibration *Calibration `yaml:"calibration,omitempty"`

	// Remote backends are already-running Jev-compatible servers; nothing is installed or launched.
	Remote *Remote `yaml:"remote,omitempty"`

	Install Install `yaml:"install,omitempty"`
	Launch  Launch  `yaml:"launch,omitempty"`

	// Capabilities maps a question type (noul, choice, score) to native, emulated or unsupported.
	// Types not listed are native.
	Capabilities map[string]string `yaml:"capabilities,omitempty"`

	Models map[string]Model `yaml:"models"`

	Source string `yaml:"-"` // "builtin" or the path of the user file
}

type Remote struct {
	URL       string `yaml:"url"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
}

type Install struct {
	Python   string   `yaml:"python,omitempty"`
	Packages []string `yaml:"packages,omitempty"`
	// Shim names an embedded worker script (shims/<name>/server.py) copied into the env as {shim}.
	Shim string `yaml:"shim,omitempty"`
	// Prefetch commands run once per model at install time, typically to download weights,
	// so that nothing is downloaded when a request arrives.
	Prefetch [][]string `yaml:"prefetch,omitempty"`
}

type Launch struct {
	Cmd         []string          `yaml:"cmd,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	Ready       Ready             `yaml:"ready,omitempty"`
	IdleTimeout time.Duration     `yaml:"idle_timeout,omitempty"` // 0 = never stop an idle model
	MemGB       float64           `yaml:"mem_gb,omitempty"`
}

type Ready struct {
	Path    string        `yaml:"path,omitempty"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// Body, when set, makes the readiness check a JSON POST of this body instead
	// of a GET. Use a real request for servers that answer health checks before
	// their model has loaded.
	Body string `yaml:"body,omitempty"`
}

type Model struct {
	Description string `yaml:"description,omitempty"`
	// Homepage is a model card, when it isn't implied by Weights.
	Homepage    string       `yaml:"homepage,omitempty"`
	Base        string       `yaml:"base,omitempty"`        // overrides the recipe's base
	Context     *Context     `yaml:"context,omitempty"`     // overrides the recipe's context
	Calibration *Calibration `yaml:"calibration,omitempty"` // overrides the recipe's calibration
	Weights     string       `yaml:"weights,omitempty"`
	Aliases     []string     `yaml:"aliases,omitempty"`
	// UpstreamModel is the "model" value sent to the backend; empty means the model's own name.
	UpstreamModel string  `yaml:"upstream_model,omitempty"`
	MemGB         float64 `yaml:"mem_gb,omitempty"`
	// ExtraArgs are appended to launch.cmd for this model only.
	ExtraArgs []string `yaml:"extra_args,omitempty"`
	// Repos are the Hugging Face repos whose downloaded revisions identify this
	// model's version. Defaults to Weights when that is a Hub repo id; set it
	// for backends that fetch their own weights (Von, Laya).
	Repos []string `yaml:"repos,omitempty"`
}

// Ref is one servable model: a name plus the recipe that knows how to run it.
type Ref struct {
	Name   string
	Recipe *Recipe
	Model  Model
}

func (r Ref) IsRemote() bool { return r.Recipe.Remote != nil }

// Calibration is temperature scaling: probabilities are raised to 1/Temperature and
// renormalised. It never changes which answer is chosen, only how confident it looks.
type Calibration struct {
	Temperature float64 `yaml:"temperature" json:"temperature"`
}

func (r Ref) Calibration() *Calibration {
	if r.Model.Calibration != nil {
		return r.Model.Calibration
	}
	return r.Recipe.Calibration
}

// Context describes the input length a model accepts.
type Context struct {
	Tokens int    `yaml:"tokens" json:"tokens"`
	Note   string `yaml:"note,omitempty" json:"note,omitempty"` // what the limit covers, and where it comes from
}

func (r Ref) Context() *Context {
	if r.Model.Context != nil {
		return r.Model.Context
	}
	return r.Recipe.Context
}

// Base is the foundation model and its maker.
func (r Ref) Base() string {
	if r.Model.Base != "" {
		return r.Model.Base
	}
	return r.Recipe.Base
}

type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Links points at a model's original sources: the project, its model card, and
// its weights on Hugging Face (derived from Weights when that is a Hub repo id).
func (r Ref) Links() []Link {
	var links []Link
	seen := map[string]bool{}
	add := func(label, u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			links = append(links, Link{label, u})
		}
	}
	add(hostLabel(r.Recipe.Homepage), r.Recipe.Homepage)
	add("Model card", r.Model.Homepage)
	repos := r.Repos()
	for _, repo := range repos {
		label := "Weights"
		if len(repos) > 1 {
			label = "Weights: " + repo[strings.Index(repo, "/")+1:]
		}
		add(label, "https://huggingface.co/"+SplitRevision(repo))
	}
	return links
}

// Repos is the model's Hugging Face repos (each optionally "@revision").
func (r Ref) Repos() []string {
	if len(r.Model.Repos) > 0 {
		return r.Model.Repos
	}
	if hubID.MatchString(SplitRevision(r.Model.Weights)) {
		return []string{r.Model.Weights}
	}
	return nil
}

// SplitRevision returns the repo id without any "@revision".
func SplitRevision(repo string) string { return strings.SplitN(repo, "@", 2)[0] }

var hubID = regexp.MustCompile(`^[A-Za-z0-9][\w.-]*/[\w.-]+$`)

func hostLabel(u string) string {
	switch {
	case strings.Contains(u, "://github.com/"):
		return "GitHub"
	case strings.Contains(u, "://huggingface.co/"):
		return "Hugging Face"
	}
	return "Website"
}

// checkURL allows only absolute http(s) URLs: the UI renders these as links,
// so a recipe must not be able to smuggle in javascript: or data: URLs.
func checkURL(field, u string) error {
	if u == "" {
		return nil
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("%s must be an http(s) URL, got %q", field, u)
	}
	return nil
}

func (r Ref) MemGB() float64 {
	if r.Model.MemGB > 0 {
		return r.Model.MemGB
	}
	return r.Recipe.Launch.MemGB
}

func (r Ref) UpstreamModel() string {
	if r.Model.UpstreamModel != "" {
		return r.Model.UpstreamModel
	}
	return r.Name
}

// Capability returns native, emulated or unsupported for a question type.
func (r Ref) Capability(qtype string) string {
	if c, ok := r.Recipe.Capabilities[qtype]; ok {
		return c
	}
	return "native"
}

func (r Ref) EnvDir() string { return paths.Env(r.Recipe.ID) }
func (r Ref) Python() string { return filepath.Join(r.EnvDir(), "bin", "python") }
func (r Ref) ShimPath() string {
	if r.Recipe.Install.Shim == "" {
		return ""
	}
	return filepath.Join(r.EnvDir(), "shim", r.Recipe.Install.Shim+".py")
}

// WeightsPath is the local folder holding the model's weights at the pinned
// revision (weights "org/name@<commit>"), or the latest downloaded one when
// unpinned. Backends that accept a folder load exactly this version, offline.
func (r Ref) WeightsPath() string {
	repo := SplitRevision(r.Model.Weights)
	if !hubID.MatchString(repo) {
		return r.Model.Weights // already a local path
	}
	dir := filepath.Join(paths.HubCache(), "models--"+strings.ReplaceAll(repo, "/", "--"))
	rev := "main"
	if i := strings.Index(r.Model.Weights, "@"); i >= 0 {
		rev = r.Model.Weights[i+1:]
	}
	if data, err := os.ReadFile(filepath.Join(dir, "refs", rev)); err == nil {
		rev = strings.TrimSpace(string(data)) // a branch or tag name: follow it to its commit
	}
	return filepath.Join(dir, "snapshots", rev)
}

// Expand substitutes {python}, {env}, {shim}, {weights}, {weights_path}, {model}, {home} and {port}.
// Other brace text is left alone, so inline Python like f'{x}' survives.
func (r Ref) Expand(s string, port int) string {
	return strings.NewReplacer(
		"{python}", r.Python(),
		"{env}", r.EnvDir(),
		"{shim}", r.ShimPath(),
		"{weights_path}", r.WeightsPath(),
		"{weights}", r.Model.Weights,
		"{model}", r.Name,
		"{home}", paths.Home(),
		"{port}", strconv.Itoa(port),
	).Replace(s)
}

// LaunchArgs is the model's full command line: the recipe's launch.cmd plus its extra_args.
func (r Ref) LaunchArgs(port int) []string {
	return r.ExpandAll(append(append([]string(nil), r.Recipe.Launch.Cmd...), r.Model.ExtraArgs...), port)
}

func (r Ref) ExpandAll(args []string, port int) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = r.Expand(a, port)
	}
	return out
}

type Catalog struct {
	Recipes []*Recipe
	models  map[string]Ref
	aliases map[string]string
}

// Load reads the built-in recipes, then applies user overrides and additions.
func Load() (*Catalog, error) {
	byID := map[string]*Recipe{}

	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, err
		}
		r, err := Parse(data, "builtin")
		if err != nil {
			return nil, fmt.Errorf("built-in recipe %s: %w", e.Name(), err)
		}
		byID[r.ID] = r
	}

	files, _ := filepath.Glob(filepath.Join(paths.Recipes(), "*.yaml"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		r, err := Parse(data, f)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", f, err)
		}
		byID[r.ID] = r
	}

	c := &Catalog{models: map[string]Ref{}, aliases: map[string]string{}}
	for _, r := range byID {
		c.Recipes = append(c.Recipes, r)
	}
	sort.Slice(c.Recipes, func(i, j int) bool { return c.Recipes[i].ID < c.Recipes[j].ID })

	for _, r := range c.Recipes {
		for name, m := range r.Models {
			if prev, dup := c.models[name]; dup {
				return nil, fmt.Errorf("model %q is defined by both recipe %q and %q", name, prev.Recipe.ID, r.ID)
			}
			c.models[name] = Ref{Name: name, Recipe: r, Model: m}
		}
	}
	for _, r := range c.Recipes {
		for name, m := range r.Models {
			for _, a := range m.Aliases {
				if _, clash := c.models[a]; clash {
					return nil, fmt.Errorf("alias %q of model %q clashes with a model name", a, name)
				}
				if prev, dup := c.aliases[a]; dup && prev != name {
					return nil, fmt.Errorf("alias %q is used by both %q and %q", a, prev, name)
				}
				c.aliases[a] = name
			}
		}
	}
	return c, nil
}

func Parse(data []byte, source string) (*Recipe, error) {
	var r Recipe
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	r.Source = source
	switch {
	case r.ID == "":
		return nil, fmt.Errorf("missing id")
	case len(r.Models) == 0:
		return nil, fmt.Errorf("no models defined")
	case r.Remote == nil && len(r.Launch.Cmd) == 0:
		return nil, fmt.Errorf("needs either remote.url or launch.cmd")
	case r.Remote == nil && r.Install.Python == "":
		return nil, fmt.Errorf("install.python is required for local recipes")
	}
	if err := checkURL("homepage", r.Homepage); err != nil {
		return nil, err
	}
	for name, m := range r.Models {
		if err := checkURL("models."+name+".homepage", m.Homepage); err != nil {
			return nil, err
		}
		for _, repo := range m.Repos {
			if !hubID.MatchString(SplitRevision(repo)) {
				return nil, fmt.Errorf("models.%s.repos: %q is not a Hugging Face repo id (org/name)", name, repo)
			}
		}
	}
	checkCal := func(where string, c *Calibration) error {
		if c != nil && (c.Temperature <= 0 || c.Temperature > 20) {
			return fmt.Errorf("%s.calibration.temperature must be between 0 and 20, got %v", where, c.Temperature)
		}
		return nil
	}
	if err := checkCal("recipe", r.Calibration); err != nil {
		return nil, err
	}
	for name, m := range r.Models {
		if err := checkCal("models."+name, m.Calibration); err != nil {
			return nil, err
		}
	}
	for t, c := range r.Capabilities {
		if c != "native" && c != "emulated" && c != "unsupported" {
			return nil, fmt.Errorf("capability %s: %q must be native, emulated or unsupported", t, c)
		}
	}
	if r.Launch.Ready.Timeout == 0 {
		r.Launch.Ready.Timeout = 5 * time.Minute
	}
	return &r, nil
}

func (c *Catalog) Resolve(name string) (Ref, bool) {
	if canonical, ok := c.aliases[name]; ok {
		name = canonical
	}
	ref, ok := c.models[name]
	return ref, ok
}

func (c *Catalog) Models() []Ref {
	out := make([]Ref, 0, len(c.models))
	for _, r := range c.models {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *Catalog) Names() []string {
	var names []string
	for _, r := range c.Models() {
		names = append(names, r.Name)
	}
	return names
}

func (c *Catalog) Recipe(id string) (*Recipe, bool) {
	for _, r := range c.Recipes {
		if r.ID == id {
			return r, true
		}
	}
	return nil, false
}

// Builtin returns the embedded source of a built-in recipe.
func Builtin(id string) ([]byte, bool) {
	data, err := builtinFS.ReadFile("builtin/" + id + ".yaml")
	return data, err == nil
}

// UserPath is where a user override for recipe id lives.
func UserPath(id string) string { return filepath.Join(paths.Recipes(), id+".yaml") }
