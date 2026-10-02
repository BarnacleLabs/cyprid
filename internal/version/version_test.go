package version

import (
	"strings"
	"testing"
	"time"
)

func TestLabels(t *testing.T) {
	cases := []struct {
		info        Info
		short, long string
	}{
		{Info{Version: "dev"}, "dev", "not built from a git commit"},
		{Info{Version: "dev", Commit: "abcdef1234567"}, "dev abcdef1", "commit abcdef1"},
		{Info{Version: "v0.1.0", Commit: "abcdef1234567", Time: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)}, "v0.1.0", "commit abcdef1 from 2026-09-27"},
		{Info{Version: "v0.1.0", Commit: "abcdef1234567", Modified: true}, "v0.1.0 (modified)", "with uncommitted changes"},
	}
	for _, c := range cases {
		if got := c.info.Short(); got != c.short {
			t.Errorf("Short() = %q, want %q", got, c.short)
		}
		if got := c.info.String(); !strings.Contains(got, c.long) {
			t.Errorf("String() = %q, want it to contain %q", got, c.long)
		}
	}
	if !pseudo.MatchString("v0.0.0-20260927200000-abcdef123456") || pseudo.MatchString("v0.1.0") {
		t.Error("pseudo-version detection")
	}
}
