// Package config holds server settings that can change at runtime and must
// survive a restart, in ~/.cyprid/config.json.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"cyprid/internal/paths"
)

type Settings struct {
	// Network makes the API listen on all interfaces, not just localhost.
	Network bool `json:"network"`
	// APIKey is required from clients on other machines. Local clients never need it.
	APIKey string `json:"api_key,omitempty"`
	// Calibration holds per-model temperatures the user chose, overriding any recipe
	// value. 0 means explicitly off.
	Calibration map[string]float64 `json:"calibration,omitempty"`
}

var mu sync.Mutex

func path() string { return filepath.Join(paths.Home(), "config.json") }

// Path is the settings file's location.
func Path() string { return path() }

func Load() (Settings, error) {
	mu.Lock()
	defer mu.Unlock()
	var s Settings
	data, err := os.ReadFile(path())
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

func Save(s Settings) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path()), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // holds the API key
		return err
	}
	return os.Rename(tmp, path())
}

func NewAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "cy-" + hex.EncodeToString(b)
}
