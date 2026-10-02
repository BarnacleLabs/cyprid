package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cyprid/internal/recipe"
)

// RepoUsage describes one Hugging Face repo a model downloaded, for deciding
// what deleting its weights would free.
type RepoUsage struct {
	Repo       string   `json:"repo"`
	Bytes      int64    `json:"bytes"`
	Present    bool     `json:"present"`
	SharedWith []string `json:"shared_with,omitempty"` // other installed models that use it; these are kept
}

// repoDir is where huggingface_hub caches a repo.
func repoDir(repo string) string {
	return filepath.Join(hubCache(), "models--"+strings.ReplaceAll(recipe.SplitRevision(repo), "/", "--"))
}

// A cached repo's blobs are either files, or (with newer huggingface_hub) links
// into a shared content-addressed store under the cache, where one file can back
// several repos. So sizes follow links, and a shared file is only deleted once
// nothing else in the cache links to it.
type blob struct {
	path   string // the entry in the repo's blobs directory
	target string // the file holding the data (path itself when it isn't a link)
	size   int64
}

func repoBlobs(dir string) ([]blob, bool) {
	entries, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil {
		return nil, false
	}
	var out []blob
	for _, e := range entries {
		p := filepath.Join(dir, "blobs", e.Name())
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue // a dangling link holds no data
		}
		info, err := os.Stat(target)
		if err != nil {
			continue
		}
		out = append(out, blob{path: p, target: target, size: info.Size()})
	}
	return out, true
}

// otherRefs counts, for every data file, the blob entries in repos other than skip that use it.
func otherRefs(skip map[string]bool) map[string]int {
	refs := map[string]int{}
	dirs, _ := filepath.Glob(filepath.Join(hubCache(), "models--*"))
	for _, d := range dirs {
		if skip[d] {
			continue
		}
		blobs, _ := repoBlobs(d)
		for _, b := range blobs {
			refs[b.target]++
		}
	}
	return refs
}

// freeable is the data that deleting these repos would release: files no other repo uses, counted once.
func freeable(dirs []string) (int64, map[string]int64) {
	skip := map[string]bool{}
	for _, d := range dirs {
		skip[d] = true
	}
	refs := otherRefs(skip)
	files := map[string]int64{}
	for _, d := range dirs {
		blobs, _ := repoBlobs(d)
		for _, b := range blobs {
			if refs[b.target] == 0 {
				files[b.target] = b.size
			}
		}
	}
	var total int64
	for _, n := range files {
		total += n
	}
	return total, files
}

func repoBytes(dir string) (int64, bool) {
	blobs, ok := repoBlobs(dir)
	seen := map[string]bool{}
	var total int64
	for _, b := range blobs {
		if !seen[b.target] {
			seen[b.target] = true
			total += b.size
		}
	}
	return total, ok
}

// WeightsPlan lists the repos ref downloaded and which of them other installed models still use.
// Only repos a recipe names are ever considered, so nothing else in the cache is touched.
func WeightsPlan(ref recipe.Ref, all []recipe.Ref) []RepoUsage {
	var plan []RepoUsage
	for _, repo := range ref.Repos() {
		id := recipe.SplitRevision(repo)
		u := RepoUsage{Repo: id}
		u.Bytes, u.Present = repoBytes(repoDir(id))
		for _, other := range all {
			if other.Name == ref.Name || other.IsRemote() || StatusOf(other) == NotInstalled {
				continue
			}
			for _, r := range other.Repos() {
				if recipe.SplitRevision(r) == id {
					u.SharedWith = append(u.SharedWith, other.Name)
				}
			}
		}
		plan = append(plan, u)
	}
	return plan
}

// deletable is the repos of ref that deleting its weights would remove.
func deletable(ref recipe.Ref, all []recipe.Ref) []string {
	var dirs []string
	for _, u := range WeightsPlan(ref, all) {
		if u.Present && len(u.SharedWith) == 0 {
			dirs = append(dirs, repoDir(u.Repo))
		}
	}
	return dirs
}

// CachedBytes is how much disk deleting ref's weights would free.
func CachedBytes(ref recipe.Ref, all []recipe.Ref) int64 {
	n, _ := freeable(deletable(ref, all))
	return n
}

// DeleteWeights removes ref's cached repos that no other installed model uses, and
// the data files behind them that nothing else in the cache links to.
// The model itself must not be installed, or it would be left without weights.
func DeleteWeights(ref recipe.Ref, all []recipe.Ref, out io.Writer) (int64, error) {
	if st := StatusOf(ref); st != NotInstalled {
		return 0, fmt.Errorf("%s is %s; uninstall it before deleting its weights", ref.Name, st)
	}
	for _, u := range WeightsPlan(ref, all) {
		switch {
		case !u.Present:
			fmt.Fprintf(out, "%s: not in the cache\n", u.Repo)
		case len(u.SharedWith) > 0:
			fmt.Fprintf(out, "%s: kept, used by %s\n", u.Repo, strings.Join(u.SharedWith, ", "))
		}
	}
	dirs := deletable(ref, all)
	total, files := freeable(dirs)
	// Resolve the cache path the way data-file paths were resolved (on macOS /var is a
	// link to /private/var), so the inside-the-cache check compares like with like.
	cache, err := filepath.EvalSymlinks(hubCache())
	if err != nil {
		return 0, err
	}
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			return 0, err
		}
		fmt.Fprintf(out, "%s: deleted\n", strings.ReplaceAll(strings.TrimPrefix(filepath.Base(d), "models--"), "--", "/"))
	}
	for f := range files {
		// Only ever delete inside the cache; anything a link points outside of it is left alone.
		if strings.HasPrefix(f, cache+string(filepath.Separator)) {
			os.Remove(f)
		}
	}
	fmt.Fprintf(out, "freed %s\n", HumanBytes(total))
	return total, nil
}

func HumanBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d KB", n/1000)
}
