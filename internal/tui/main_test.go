package tui

import (
	"os"
	"testing"

	"github.com/hdtradeservices/ticketdeck/internal/fleet"
)

// realEnv is the environment TestMain hid, for the one live test that needs
// the real machine back.
var realEnv = map[string]string{}

// TestMain keeps every test off the machine it runs on. The deck reads and
// writes ~/.ticketdeck (its ledger, settings and caches) and polls herdr and
// every account's live sessions; a test that reached any of those would read
// the developer's real sessions, or write into their real ledger.
func TestMain(m *testing.M) {
	for _, k := range []string{"HOME", "HERDR_SOCKET_PATH", "HERDR_CLIENT_SOCKET_PATH"} {
		realEnv[k] = os.Getenv(k)
	}
	home, err := os.MkdirTemp("", "ticketdeck-tui-test")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "TICKETDECK_ACCOUNT", "HERDR_PANE_ID", "HERDR_SOCKET_PATH", "XDG_CONFIG_HOME", "TICKETDECK_AUTO_MOVE"} {
		_ = os.Unsetenv(k)
	}
	pollFleet = func(fleet.Input) fleet.Snapshot { return fleet.Snapshot{} }
	notify = func(string, string) {}
	dismissBeat = 0
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
