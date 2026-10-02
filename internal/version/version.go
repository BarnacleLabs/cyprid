// Package version reports which build of cyprid is running.
//
// Nothing needs setting at build time: when cyprid is built inside its git
// repository, Go records the commit (and the release tag, if the commit has
// one) in the program. Release builds can also set Override with
// -ldflags "-X cyprid/internal/version.Override=v1.2.3".
package version

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Override, when set at link time, is used as the version.
var Override string

type Info struct {
	Version  string    `json:"version"`            // e.g. "v0.1.0", or "dev" for an untagged build
	Commit   string    `json:"commit,omitempty"`   // full git commit
	Time     time.Time `json:"time,omitzero"`      // when that commit was made
	Modified bool      `json:"modified,omitempty"` // built with uncommitted changes
	Go       string    `json:"go"`
	Platform string    `json:"platform"`
}

// Go's version for a build from an untagged commit, e.g. v0.0.0-20260927200000-abcdef123456.
var pseudo = regexp.MustCompile(`^v\d+\.\d+\.\d+-(\d+\.)?(0\.)?\d{14}-[0-9a-f]{12}`)

func Get() Info {
	info := Info{Version: "dev", Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	bi, ok := debug.ReadBuildInfo()
	if ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.time":
				info.Time, _ = time.Parse(time.RFC3339, s.Value)
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
		if v := strings.TrimSuffix(bi.Main.Version, "+dirty"); v != "" && v != "(devel)" && !pseudo.MatchString(v) {
			info.Version = v
		}
	}
	if info.Commit == "" {
		info.Modified = false // with no commit there's nothing to be modified from
	}
	if Override != "" {
		info.Version = Override
	}
	return info
}

func (i Info) ShortCommit() string {
	if len(i.Commit) > 7 {
		return i.Commit[:7]
	}
	return i.Commit
}

// Short is a compact label for logs and the web page: "v0.1.0", "dev abc1234", or "dev".
func (i Info) Short() string {
	s := i.Version
	if i.Version == "dev" && i.Commit != "" {
		s += " " + i.ShortCommit()
	}
	if i.Modified {
		s += " (modified)"
	}
	return s
}

// String is the full description `cyprid version` prints.
func (i Info) String() string {
	var parts []string
	if i.Commit != "" {
		c := "commit " + i.ShortCommit()
		if !i.Time.IsZero() {
			c += " from " + i.Time.UTC().Format("2006-01-02")
		}
		parts = append(parts, c)
	} else {
		parts = append(parts, "not built from a git commit")
	}
	if i.Modified {
		parts = append(parts, "with uncommitted changes")
	}
	parts = append(parts, i.Go, i.Platform)
	return fmt.Sprintf("cyprid %s (%s)", i.Version, strings.Join(parts, ", "))
}
