package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsLoad(t *testing.T) {
	t.Setenv("CYPRID_HOME", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]string{"kev": "kev-4b", "jev-latest": "jev", "gliner-decide": "gliner2.5-decide", "laya": "laya"} {
		ref, ok := c.Resolve(alias)
		if !ok || ref.Name != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", alias, ref.Name, ok, want)
		}
	}
	gliner, _ := c.Resolve("gliner2.5-decide")
	if gliner.Capability("noul") != "emulated" || gliner.Capability("choice") != "native" {
		t.Error("gliner capabilities wrong")
	}
}

func TestUserOverrideReplacesBuiltin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CYPRID_HOME", home)
	os.MkdirAll(filepath.Join(home, "recipes"), 0o755)
	os.WriteFile(filepath.Join(home, "recipes", "laya.yaml"), []byte(`id: laya
install: {python: "3.12"}
launch: {cmd: ["{env}/bin/laya-serve"], idle_timeout: 5m}
models:
  laya-custom: {}
`), 0o644)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Resolve("laya"); ok {
		t.Error("built-in laya model should be replaced by the user recipe")
	}
	ref, ok := c.Resolve("laya-custom")
	if !ok || !strings.HasSuffix(ref.Recipe.Source, "laya.yaml") {
		t.Errorf("user recipe not loaded: %+v", ref)
	}
}

func TestExpandLeavesOtherBracesAlone(t *testing.T) {
	t.Setenv("CYPRID_HOME", "/h")
	ref := Ref{Name: "m", Recipe: &Recipe{ID: "r"}, Model: Model{Weights: "org/w"}}
	got := ref.Expand("{python} --run {weights} --port {port} f'{x.y}'", 1234)
	want := "/h/envs/r/bin/python --run org/w --port 1234 f'{x.y}'"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestParseRejectsBadRecipes(t *testing.T) {
	for _, bad := range []string{
		`models: {m: {}}`,                   // no id
		`id: x`,                             // no models
		`id: x` + "\n" + `models: {m: {}}`,  // no remote or launch
		`id: x` + "\n" + `unknown_field: 1`, // typo'd field
		"id: x\nremote: {url: u}\ncapabilities: {noul: maybe}\nmodels: {m: {}}", // bad capability
	} {
		if _, err := Parse([]byte(bad), "test"); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestLinks(t *testing.T) {
	ref := Ref{Name: "m", Recipe: &Recipe{Homepage: "https://github.com/o/p"}, Model: Model{Weights: "o/m@v2"}}
	got := ref.Links()
	if len(got) != 2 || got[0] != (Link{"GitHub", "https://github.com/o/p"}) || got[1] != (Link{"Weights", "https://huggingface.co/o/m"}) {
		t.Errorf("links %+v", got)
	}
	local := Ref{Name: "m", Recipe: &Recipe{}, Model: Model{Weights: "/Users/me/checkpoints/run"}}
	if len(local.Links()) != 0 {
		t.Errorf("a local path isn't a Hub id: %+v", local.Links())
	}
}

func TestRecipeURLsMustBeHTTP(t *testing.T) {
	for _, bad := range []string{
		"id: x\nhomepage: javascript:alert(1)\nremote: {url: u}\nmodels: {m: {}}",
		"id: x\nremote: {url: u}\nmodels: {m: {homepage: \"data:text/html,hi\"}}",
		"id: x\nhomepage: github.com/o/p\nremote: {url: u}\nmodels: {m: {}}",
	} {
		if _, err := Parse([]byte(bad), "test"); err == nil {
			t.Errorf("expected rejection: %q", bad)
		}
	}
}

// Built-in weights must name an exact commit, so an install reproduces the
// benchmarked model. Laya and Von download their own weights and can't be pinned.
func TestBuiltinWeightsPinned(t *testing.T) {
	t.Setenv("CYPRID_HOME", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	unpinnable := map[string]bool{"laya": true, "von": true}
	for _, ref := range c.Models() {
		if ref.Recipe.Remote != nil || unpinnable[ref.Recipe.ID] {
			continue
		}
		for _, repo := range append([]string{ref.Model.Weights}, ref.Model.Repos...) {
			if _, rev, _ := strings.Cut(repo, "@"); len(rev) != 40 {
				t.Errorf("%s: %q isn't pinned to a full commit", ref.Name, repo)
			}
		}
	}
}

func TestWeightsPath(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	dir := filepath.Join(hub, "models--org--name")
	os.MkdirAll(filepath.Join(dir, "refs"), 0o755)
	os.WriteFile(filepath.Join(dir, "refs", "main"), []byte("abc123\n"), 0o644)

	for weights, want := range map[string]string{
		"org/name@def456":    filepath.Join(dir, "snapshots", "def456"), // pinned
		"org/name":           filepath.Join(dir, "snapshots", "abc123"), // follows main
		"org/name@main":      filepath.Join(dir, "snapshots", "abc123"),
		"/Users/me/my-model": "/Users/me/my-model", // local folder, unchanged
	} {
		ref := Ref{Model: Model{Weights: weights}}
		if got := ref.WeightsPath(); got != want {
			t.Errorf("WeightsPath(%q) = %q; want %q", weights, got, want)
		}
	}
}
