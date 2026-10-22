package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// settings are the deck choices worth keeping across restarts. Every deck on
// the machine shares them.
type settings struct {
	// Pin names the account new sessions open under. Empty routes each one to
	// whichever account has the most room.
	Pin string `json:"pin,omitempty"`
	// AutoMove moves a session cut off by its account's usage limit to an
	// account with room and tells it to carry on. nil means the default, on.
	AutoMove *bool `json:"auto_move,omitempty"`
}

// autoMove reports whether automatic moves are on. TICKETDECK_AUTO_MOVE=off
// overrides the saved choice, for a deck that must never act on its own.
func (s settings) autoMove() bool {
	switch strings.ToLower(os.Getenv("TICKETDECK_AUTO_MOVE")) {
	case "off", "0", "false", "no":
		return false
	}
	return s.AutoMove == nil || *s.AutoMove
}

// Pin is the account new sessions are pinned to, "" for automatic.
func Pin() string { return loadSettings().Pin }

func settingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ticketdeck", "settings.json")
}

func loadSettings() settings {
	var s settings
	if p := settingsPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &s)
		}
	}
	return s
}

func saveSettings(s settings) {
	p := settingsPath()
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}
