package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequirementParsing(t *testing.T) {
	cases := map[string][2]string{ // requirement -> name, commit
		"von-sdk==1.2.3":      {"von-sdk", ""},
		"laya[serve]==0.3.20": {"laya", ""},
		"kev[serve] @ git+https://github.com/jaredpalmer/kev@5920c5fe4ca8e0970ed4209ac2c9b8e18bea5109": {"kev", "5920c5fe4ca8e0970ed4209ac2c9b8e18bea5109"},
		"huggingface_hub": {"huggingface_hub", ""},
	}
	for req, want := range cases {
		m := reqName.FindStringSubmatch(req)
		if m == nil || m[1] != want[0] {
			t.Errorf("%q: name %v, want %q", req, m, want[0])
		}
		commit := ""
		if g := gitReq.FindStringSubmatch(req); g != nil {
			commit = g[2]
		}
		if commit != want[1] {
			t.Errorf("%q: commit %q, want %q", req, commit, want[1])
		}
	}
	if normaliseName("Huggingface_Hub") != normaliseName("huggingface-hub") {
		t.Error("package names must compare the way pip normalises them")
	}
}

func TestWeightsVersionReadsTheCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	sha := "5df8185a4f2327ad0a7cd117cc4f701ac557b9ae"
	dir := filepath.Join(cache, "models--org--m")
	os.MkdirAll(filepath.Join(dir, "refs"), 0o755)
	os.MkdirAll(filepath.Join(dir, "snapshots", sha), 0o755)
	os.WriteFile(filepath.Join(dir, "refs", "main"), []byte(sha+"\n"), 0o644)

	if w := weightsVersion("org/m"); w.Revision != sha || w.Pinned {
		t.Errorf("main: %+v", w)
	}
	if w := weightsVersion("org/m@" + sha); w.Revision != sha || !w.Pinned {
		t.Errorf("pinned: %+v", w)
	}
	if w := weightsVersion("org/absent"); w.Revision != "" || w.Error == "" {
		t.Errorf("missing repo should report an error: %+v", w)
	}
}
