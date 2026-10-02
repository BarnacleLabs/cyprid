// Package reveal opens cyprid's folders and files in Finder. Callers name a
// place ("bench", "logs", a benchmark run, a model's log), never a raw path, so
// the web page can offer "Show in Finder" without being able to open anything else.
package reveal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"cyprid/internal/config"
	"cyprid/internal/install"
	"cyprid/internal/paths"
	"cyprid/internal/recipe"
)

// Places lists the names Locate accepts, for help text and errors.
var Places = []string{"home", "bench", "run <id>", "logs", "log <model>", "recipes", "recipe <id>", "weights", "config"}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Locate resolves a named place to a path, and whether it's a file (revealed
// in its folder) rather than a folder (opened).
func Locate(what, name string, c *recipe.Catalog) (string, bool, error) {
	needName := func() error {
		if !safeName.MatchString(name) {
			return fmt.Errorf("%q needs a name (letters, digits, - _ .)", what)
		}
		return nil
	}
	switch what {
	case "", "home":
		return paths.Home(), false, nil
	case "bench":
		return filepath.Join(paths.Home(), "bench", "runs"), false, nil
	case "run":
		if err := needName(); err != nil {
			return "", false, err
		}
		return filepath.Join(paths.Home(), "bench", "runs", name), false, nil
	case "logs":
		return paths.Logs(), false, nil
	case "log":
		ref, ok := c.Resolve(name)
		if !ok {
			return "", false, fmt.Errorf("unknown model %q", name)
		}
		return filepath.Join(paths.Logs(), ref.Name+".log"), true, nil
	case "recipes":
		return paths.Recipes(), false, nil
	case "recipe":
		if err := needName(); err != nil {
			return "", false, err
		}
		if _, err := os.Stat(recipe.UserPath(name)); err == nil {
			return recipe.UserPath(name), true, nil
		}
		return paths.Recipes(), false, nil // built-in: nothing on disk yet
	case "weights":
		return install.HubCache(), false, nil
	case "config":
		return config.Path(), true, nil
	}
	return "", false, fmt.Errorf("unknown place %q; one of: %v", what, Places)
}

// Open shows a place in Finder: folders are opened (created first for cyprid's
// own folders, so a fresh install still has somewhere to show), files are revealed.
func Open(path string, isFile bool) error {
	if isFile {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s doesn't exist yet", path)
			}
			return err
		}
		return exec.Command("open", "-R", path).Run()
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	return exec.Command("open", path).Run()
}
