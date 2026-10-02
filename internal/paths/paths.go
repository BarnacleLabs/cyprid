// Package paths locates cyprid's state on disk. Everything lives under
// one directory (default ~/.cyprid, override with CYPRID_HOME).
// Model weights are left in the standard Hugging Face cache so they are shared
// with anything else on the machine.
package paths

import (
	"os"
	"path/filepath"
)

func Home() string {
	if v := os.Getenv("CYPRID_HOME"); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".cyprid"
	}
	return filepath.Join(h, ".cyprid")
}

// Recipes holds user-edited or user-added recipe files, which override built-ins by id.
func Recipes() string { return filepath.Join(Home(), "recipes") }

// Env is the isolated Python environment for one recipe.
func Env(recipeID string) string { return filepath.Join(Home(), "envs", recipeID) }

// Installed holds one marker file per installed model.
func Installed() string { return filepath.Join(Home(), "installed") }

// Logs holds one log file per model's worker process.
func Logs() string { return filepath.Join(Home(), "logs") }

// HubCache is where huggingface_hub keeps downloaded weights, following its own
// rules: HF_HUB_CACHE, else HF_HOME/hub, else ~/.cache/huggingface/hub.
func HubCache() string {
	if v := os.Getenv("HF_HUB_CACHE"); v != "" {
		return v
	}
	if v := os.Getenv("HF_HOME"); v != "" {
		return filepath.Join(v, "hub")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".cache", "huggingface", "hub")
}
