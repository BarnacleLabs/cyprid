package reveal

import (
	"path/filepath"
	"strings"
	"testing"

	"cyprid/internal/recipe"
)

func TestLocate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CYPRID_HOME", home)
	c, err := recipe.Load()
	if err != nil {
		t.Fatal(err)
	}
	for what, want := range map[string]string{"": home, "bench": filepath.Join(home, "bench", "runs"), "logs": filepath.Join(home, "logs")} {
		if got, _, err := Locate(what, "", c); err != nil || got != want {
			t.Errorf("%q: %s %v, want %s", what, got, err, want)
		}
	}
	if got, isFile, err := Locate("log", "von", c); err != nil || !isFile || !strings.HasSuffix(got, "/logs/von.log") {
		t.Errorf("log: %s %v %v", got, isFile, err)
	}
	if got, _, _ := Locate("run", "jevbench-public", c); got != filepath.Join(home, "bench", "runs", "jevbench-public") {
		t.Errorf("run: %s", got)
	}
	for _, bad := range []struct{ what, name string }{
		{"run", "../../etc"}, {"run", "a/b"}, {"run", ""}, {"recipe", "../x"}, {"log", "no-such-model"}, {"passwd", ""},
	} {
		if p, _, err := Locate(bad.what, bad.name, c); err == nil {
			t.Errorf("%s %q should be refused, got %s", bad.what, bad.name, p)
		}
	}
}
