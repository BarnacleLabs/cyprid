// Package install builds each recipe's isolated Python environment with uv and
// runs its prefetch steps, so that starting a model later never downloads anything.
//
// State is two kinds of marker file:
//   - envs/<recipe>/.cyprid-env  holds a hash of the recipe's install section
//   - installed/<model>.json             records that a model's prefetch completed
//
// Editing a recipe's install section changes the hash, which marks its models outdated.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"cyprid/internal/paths"
	"cyprid/internal/recipe"
	"cyprid/shims"
)

type Status string

const (
	Installed    Status = "installed"
	NotInstalled Status = "not installed"
	Outdated     Status = "outdated"
	Remote       Status = "remote"
)

type marker struct {
	EnvHash string   `json:"env_hash"`
	Weights string   `json:"weights"`
	Version *Version `json:"version,omitempty"`
}

func envHash(r *recipe.Recipe) string {
	data, _ := yaml.Marshal(r.Install)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

func envMarkerPath(ref recipe.Ref) string {
	return filepath.Join(ref.EnvDir(), ".cyprid-env")
}

func modelMarkerPath(name string) string {
	return filepath.Join(paths.Installed(), name+".json")
}

// EnvReady reports whether the recipe's environment is built and matches its install section.
func EnvReady(ref recipe.Ref) bool { return envReady(ref) }

func envReady(ref recipe.Ref) bool {
	data, err := os.ReadFile(envMarkerPath(ref))
	return err == nil && string(data) == envHash(ref.Recipe)
}

func StatusOf(ref recipe.Ref) Status {
	if ref.IsRemote() {
		return Remote
	}
	data, err := os.ReadFile(modelMarkerPath(ref.Name))
	if err != nil {
		return NotInstalled
	}
	var m marker
	if json.Unmarshal(data, &m) != nil || m.EnvHash != envHash(ref.Recipe) || m.Weights != ref.Model.Weights || !envReady(ref) {
		return Outdated
	}
	return Installed
}

// Install creates (or rebuilds) the recipe's env and runs the model's prefetch steps.
func Install(ref recipe.Ref, out io.Writer) error {
	if ref.IsRemote() {
		if env := ref.Recipe.Remote.APIKeyEnv; env != "" && os.Getenv(env) == "" {
			fmt.Fprintf(out, "%s is remote; nothing to install. Set %s before running the server (or send an Authorization header).\n", ref.Name, env)
		} else {
			fmt.Fprintf(out, "%s is remote; nothing to install.\n", ref.Name)
		}
		return nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return errors.New("uv not found on PATH; install it with: brew install uv")
	}

	if !envReady(ref) {
		fmt.Fprintf(out, "==> creating Python %s environment for %s\n", ref.Recipe.Install.Python, ref.Recipe.ID)
		os.Remove(envMarkerPath(ref))
		if err := run(out, nil, uv, "venv", "--clear", "--python", ref.Recipe.Install.Python, ref.EnvDir()); err != nil {
			return err
		}
		if pkgs := ref.Recipe.Install.Packages; len(pkgs) > 0 {
			fmt.Fprintf(out, "==> installing %v\n", pkgs)
			args := append([]string{"pip", "install", "--python", ref.Python()}, pkgs...)
			if err := run(out, nil, uv, args...); err != nil {
				return err
			}
		}
		if err := WriteShim(ref); err != nil {
			return err
		}
		if err := os.WriteFile(envMarkerPath(ref), []byte(envHash(ref.Recipe)), 0o644); err != nil {
			return err
		}
	} else if err := WriteShim(ref); err != nil {
		return err
	}

	for _, step := range ref.Recipe.Install.Prefetch {
		args := ref.ExpandAll(step, 0)
		fmt.Fprintf(out, "==> prefetching %s\n", ref.Name)
		if err := run(out, []string{"PYTHONUNBUFFERED=1"}, args[0], args[1:]...); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(paths.Installed(), 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(marker{EnvHash: envHash(ref.Recipe), Weights: ref.Model.Weights})
	if err := os.WriteFile(modelMarkerPath(ref.Name), data, 0o644); err != nil {
		return err
	}
	v, err := RecordVersion(ref, false)
	if err != nil {
		fmt.Fprintf(out, "warning: couldn't record the installed version: %v\n", err)
	}
	fmt.Fprintf(out, "==> %s installed", ref.Name)
	if s := v.Short(); s != "" {
		fmt.Fprintf(out, ": %s", s)
	}
	fmt.Fprintln(out)
	return nil
}

// WriteShim copies the recipe's embedded shim into its env. It runs on every
// install and launch so an upgraded binary always runs its matching shim.
func WriteShim(ref recipe.Ref) error {
	if ref.Recipe.Install.Shim == "" {
		return nil
	}
	src, err := shims.Source(ref.Recipe.Install.Shim)
	if err != nil {
		return fmt.Errorf("recipe %s names unknown shim %q", ref.Recipe.ID, ref.Recipe.Install.Shim)
	}
	if err := os.MkdirAll(filepath.Dir(ref.ShimPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(ref.ShimPath(), src, 0o644)
}

// Uninstall removes a model's marker, and the recipe's env once no model uses it.
// Weights stay in the Hugging Face cache.
func Uninstall(ref recipe.Ref, siblings []recipe.Ref, out io.Writer) error {
	if ref.IsRemote() {
		fmt.Fprintf(out, "%s is remote; nothing to uninstall.\n", ref.Name)
		return nil
	}
	if err := os.Remove(modelMarkerPath(ref.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, s := range siblings {
		if s.Name != ref.Name && StatusOf(s) != NotInstalled {
			fmt.Fprintf(out, "%s uninstalled (environment kept: %s still uses it)\n", ref.Name, s.Name)
			return nil
		}
	}
	if err := os.RemoveAll(ref.EnvDir()); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s uninstalled; removed %s (weights remain in the Hugging Face cache)\n", ref.Name, ref.EnvDir())
	return nil
}

func run(out io.Writer, extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), extraEnv...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}
