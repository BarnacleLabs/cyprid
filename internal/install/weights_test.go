package install

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"cyprid/internal/recipe"
)

// fakeCache builds a Hugging Face cache where repos a and b both link to one
// shared data file, a also has its own file, and b links to a file outside the cache.
func fakeCache(t *testing.T) (cache, shared, ownA, outside string) {
	t.Helper()
	cache = t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	t.Setenv("CYPRID_HOME", t.TempDir()) // nothing installed
	store := filepath.Join(cache, "blobs", "ab")
	os.MkdirAll(store, 0o755)
	shared = filepath.Join(store, "shared-data")
	os.WriteFile(shared, make([]byte, 1000), 0o644)
	outside = filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(outside, make([]byte, 50), 0o644)

	for _, repo := range []string{"a", "b"} {
		blobs := filepath.Join(cache, "models--org--"+repo, "blobs")
		os.MkdirAll(blobs, 0o755)
		os.Symlink(shared, filepath.Join(blobs, "s1"))
	}
	ownA = filepath.Join(cache, "models--org--a", "blobs", "own")
	os.WriteFile(ownA, make([]byte, 200), 0o644)
	os.Symlink(outside, filepath.Join(cache, "models--org--b", "blobs", "out"))
	return
}

func ref(name string, repos ...string) recipe.Ref {
	return recipe.Ref{Name: name, Recipe: &recipe.Recipe{ID: name}, Model: recipe.Model{Repos: repos}}
}

func TestDeleteWeightsKeepsSharedData(t *testing.T) {
	cache, shared, ownA, outside := fakeCache(t)
	a, b := ref("ma", "org/a"), ref("mb", "org/b")
	all := []recipe.Ref{a, b}

	if got := CachedBytes(a, all); got != 200 {
		t.Errorf("deleting a frees only its own file while b links to the shared one: got %d, want 200", got)
	}
	if n, _ := repoBytes(filepath.Join(cache, "models--org--a")); n != 1200 {
		t.Errorf("a's size follows links: got %d, want 1200", n)
	}

	freed, err := DeleteWeights(a, all, io.Discard)
	if err != nil || freed != 200 {
		t.Fatalf("freed %d, err %v", freed, err)
	}
	if _, err := os.Stat(ownA); !os.IsNotExist(err) {
		t.Error("a's own file should be gone")
	}
	if _, err := os.Stat(shared); err != nil {
		t.Error("the shared file must survive: b still links to it")
	}

	freed, err = DeleteWeights(b, all, io.Discard)
	if err != nil || freed != 1050 {
		t.Fatalf("second delete freed %d (want 1000 shared + 50 counted), err %v", freed, err)
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Error("the shared file should go once nothing links to it")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a file outside the cache must never be deleted")
	}
}

func TestDeleteWeightsRefusesInstalledModel(t *testing.T) {
	fakeCache(t)
	a := ref("ma", "org/a")
	os.MkdirAll(filepathInstalled(), 0o755)
	os.WriteFile(filepath.Join(filepathInstalled(), "ma.json"), []byte(`{}`), 0o644)
	if _, err := DeleteWeights(a, []recipe.Ref{a}, io.Discard); err == nil {
		t.Error("an installed model's weights must not be deleted")
	}
}

func filepathInstalled() string { return filepath.Dir(modelMarkerPath("x")) }
