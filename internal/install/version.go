package install

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cyprid/internal/paths"
	"cyprid/internal/recipe"
)

// Version records exactly what an install put on disk: the recipe's packages
// as installed, and the Hugging Face revisions of the model's weights.
type Version struct {
	Packages   []PackageVersion `json:"packages,omitempty"`
	Weights    []WeightsVersion `json:"weights,omitempty"`
	RecordedAt time.Time        `json:"recorded_at"`
	Backfilled bool             `json:"backfilled,omitempty"` // recorded after the install, from what was on disk
}

type PackageVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Git     string `json:"git,omitempty"` // repository URL, for packages installed from git
	Commit  string `json:"commit,omitempty"`
}

type WeightsVersion struct {
	Repo     string    `json:"repo"`
	Revision string    `json:"revision,omitempty"` // commit sha of the downloaded snapshot
	Date     time.Time `json:"date,omitempty"`     // when that revision was committed on Hugging Face
	Pinned   bool      `json:"pinned"`             // the recipe names this revision, rather than taking the latest
	Error    string    `json:"error,omitempty"`
}

// Short is a one-line summary, e.g. "von-sdk 1.2.3 · wfzyx/von@5df8185".
func (v *Version) Short() string {
	if v == nil {
		return ""
	}
	var parts []string
	for _, p := range v.Packages {
		s := p.Name + " " + p.Version
		if p.Commit != "" {
			s += " (" + shortSHA(p.Commit) + ")"
		}
		parts = append(parts, s)
	}
	for _, w := range v.Weights {
		if w.Revision != "" {
			parts = append(parts, w.Repo+"@"+shortSHA(w.Revision))
		}
	}
	return strings.Join(parts, " · ")
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

var reqName = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)
var gitReq = regexp.MustCompile(`git\+(\S+?)@([0-9a-fA-F]{7,40})\s*$`)

func normaliseName(n string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(n, "-"))
}

// computeVersion inspects the environment and the Hugging Face cache.
func computeVersion(ref recipe.Ref) (*Version, error) {
	v := &Version{RecordedAt: time.Now()}
	if len(ref.Recipe.Install.Packages) > 0 {
		installed, err := pipList(ref)
		if err != nil {
			return nil, err
		}
		for _, req := range ref.Recipe.Install.Packages {
			m := reqName.FindStringSubmatch(req)
			if m == nil {
				continue
			}
			pv := PackageVersion{Name: m[1], Version: installed[normaliseName(m[1])]}
			if g := gitReq.FindStringSubmatch(req); g != nil {
				pv.Git, pv.Commit = g[1], g[2]
			}
			if pv.Version == "" {
				pv.Version = "not found in the environment"
			}
			v.Packages = append(v.Packages, pv)
		}
	}
	for _, repo := range ref.Repos() {
		v.Weights = append(v.Weights, weightsVersion(repo))
	}
	return v, nil
}

func pipList(ref recipe.Ref) (map[string]string, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(uv, "pip", "list", "--python", ref.Python(), "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("listing packages: %w", err)
	}
	var rows []struct{ Name, Version string }
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range rows {
		m[normaliseName(r.Name)] = r.Version
	}
	return m, nil
}

// hubCache follows huggingface_hub's own resolution of the cache directory.
// HubCache is where huggingface_hub keeps downloaded weights.
func HubCache() string { return paths.HubCache() }

func hubCache() string { return paths.HubCache() }

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func weightsVersion(repoSpec string) WeightsVersion {
	repo := recipe.SplitRevision(repoSpec)
	rev := "main"
	if i := strings.Index(repoSpec, "@"); i >= 0 {
		rev = repoSpec[i+1:]
	}
	w := WeightsVersion{Repo: repo, Pinned: rev != "main"}
	dir := repoDir(repo)
	if fullSHA.MatchString(rev) {
		if _, err := os.Stat(filepath.Join(dir, "snapshots", rev)); err == nil {
			w.Revision = rev
		}
	} else if data, err := os.ReadFile(filepath.Join(dir, "refs", rev)); err == nil {
		w.Revision = strings.TrimSpace(string(data))
	} else if snaps, _ := os.ReadDir(filepath.Join(dir, "snapshots")); len(snaps) == 1 && fullSHA.MatchString(snaps[0].Name()) {
		// Downloaded by exact commit (as Kev does for its base model), so there's no
		// branch pointer, only the one snapshot: that is the revision, and it's pinned.
		w.Revision, w.Pinned = snaps[0].Name(), true
	}
	if w.Revision == "" {
		w.Error = "not in the Hugging Face cache"
		return w
	}
	w.Date = commitDate(repo, w.Revision)
	return w
}

// commitDate asks Hugging Face when a revision was committed (best effort; zero if offline).
func commitDate(repo, sha string) time.Time {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://huggingface.co/api/models/" + repo + "/revision/" + sha)
	if err != nil {
		return time.Time{}
	}
	defer resp.Body.Close()
	var info struct {
		LastModified time.Time `json:"lastModified"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&info) != nil {
		return time.Time{}
	}
	return info.LastModified
}

// RecordVersion computes the installed version and saves it with the install marker.
func RecordVersion(ref recipe.Ref, backfill bool) (*Version, error) {
	v, err := computeVersion(ref)
	if err != nil {
		return nil, err
	}
	v.Backfilled = backfill
	data, err := os.ReadFile(modelMarkerPath(ref.Name))
	if err != nil {
		return nil, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m.Version = v
	data, _ = json.Marshal(m)
	return v, os.WriteFile(modelMarkerPath(ref.Name), data, 0o644)
}

// VersionOf returns the recorded version of an installed model, recording it
// first (from what is on disk) for models installed before versions were kept.
func VersionOf(ref recipe.Ref) *Version {
	if ref.IsRemote() {
		return nil
	}
	data, err := os.ReadFile(modelMarkerPath(ref.Name))
	if err != nil {
		return nil
	}
	var m marker
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	if m.Version != nil && sameRepos(m.Version, ref) {
		return m.Version
	}
	if StatusOf(ref) != Installed {
		return nil
	}
	v, err := RecordVersion(ref, true)
	if err != nil {
		return nil
	}
	return v
}

// sameRepos reports whether a recorded version covers the repos the recipe now names
// (a recipe that starts listing a base model gets its version re-recorded).
func sameRepos(v *Version, ref recipe.Ref) bool {
	repos := ref.Repos()
	if len(v.Weights) != len(repos) {
		return false
	}
	for i, r := range repos {
		if v.Weights[i].Repo != recipe.SplitRevision(r) {
			return false
		}
	}
	return true
}
