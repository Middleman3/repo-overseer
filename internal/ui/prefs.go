package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Prefs are persisted under the user config dir (e.g. ~/.config/nested-git-tui/config.json on Linux).
type Prefs struct {
	SkipArchiveConfirm bool `json:"skip_archive_confirm"`
}

func prefsPath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "nested-git-tui", "config.json"), nil
}

func loadPrefs() Prefs {
	path, err := prefsPath()
	if err != nil {
		return Prefs{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Prefs{}
	}
	var p Prefs
	if json.Unmarshal(data, &p) != nil {
		return Prefs{}
	}
	return p
}

func savePrefs(p Prefs) error {
	path, err := prefsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
