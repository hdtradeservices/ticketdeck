package account

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// noAgents is the default stub: no workspace answers, so ownership comes from
// disk alone. Every test sets one — otherwise Owners would shell out to the real
// herdr on the machine running the tests.
func noAgents(t *testing.T) {
	t.Helper()
	stubAgents(t, func(string) ([]herd.Agent, error) { return nil, os.ErrNotExist })
}

// stubAgents drives the herdr lookup, and asserts herdr is installed so the
// tests behave the same on a box without it (CI).
func stubAgents(t *testing.T, fn func(socket string) ([]herd.Agent, error)) {
	t.Helper()
	prevList, prevAvail := listAgents, herdrAvailable
	listAgents, herdrAvailable = fn, func() bool { return true }
	t.Cleanup(func() { listAgents, herdrAvailable = prevList, prevAvail })
}

// stampTranscript writes a ticket's transcript under an account with an explicit
// mtime, which is how Owners decides who wrote last.
func stampTranscript(t *testing.T, configDir, ticketKey string, mod time.Time) {
	t.Helper()
	p := writeTranscript(t, Account{ConfigDir: configDir}, "-home-matthew-Repos", ticketKey, "{}\n")
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// mkSocket creates the herdr socket path a peer account's deck would listen on,
// so herdSocket finds it.
func mkSocket(t *testing.T, home, account string) {
	t.Helper()
	dir := filepath.Join(home, ".config-"+account, "herdr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "herdr.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A session under another subscription is invisible to this deck's backend — the
// whole reason the column exists — so a transcript on disk has to be enough to
// attribute it.
func TestOwnersAttributesAPeersSessionFromDisk(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	noAgents(t)
	stampTranscript(t, filepath.Join(home, ".claude-support"), "ZEN-1", time.Now())

	got := Owners([]string{"ZEN-1", "ZEN-2"}, All())
	if got["ZEN-1"].Name != "support" {
		t.Errorf("ZEN-1 owner = %+v, want support", got["ZEN-1"])
	}
	if _, ok := got["ZEN-2"]; ok {
		t.Errorf("ZEN-2 has no session anywhere but got an owner: %+v", got["ZEN-2"])
	}
}

// A hand-off copies the transcript and leaves the original in place, so both
// accounts hold one. The account that wrote most recently is the one working on
// it now.
func TestOwnersPrefersTheAccountThatWroteLast(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	noAgents(t)
	now := time.Now()
	stampTranscript(t, filepath.Join(home, ".claude"), "ZEN-1", now.Add(-2*time.Hour))
	stampTranscript(t, filepath.Join(home, ".claude-support"), "ZEN-1", now)

	if got := Owners([]string{"ZEN-1"}, All())["ZEN-1"]; got.Name != "support" {
		t.Errorf("owner = %+v, want support (it wrote last)", got)
	}
}

// A live session outranks a newer transcript: the transcript says who touched it
// last, the workspace says who is running it right now.
func TestOwnersPrefersALiveSessionOverANewerTranscript(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	mkSocket(t, home, "support")
	stubAgents(t, func(socket string) ([]herd.Agent, error) {
		if socket == "" { // this deck's own workspace: nothing running
			return nil, nil
		}
		return []herd.Agent{{Name: "ZEN-1", AgentStatus: "working"}}, nil
	})
	now := time.Now()
	stampTranscript(t, filepath.Join(home, ".claude"), "ZEN-1", now)
	stampTranscript(t, filepath.Join(home, ".claude-support"), "ZEN-1", now.Add(-time.Hour))

	got := Owners([]string{"ZEN-1"}, All())["ZEN-1"]
	if got.Name != "support" || !got.Live || got.Status != session.Working {
		t.Errorf("owner = %+v, want a live working support session", got)
	}
}

// An unreachable peer (no deck running there) must degrade to the disk answer
// rather than blanking the column.
func TestOwnersFallsBackWhenAPeerWorkspaceIsUnreachable(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	mkSocket(t, home, "support")
	stubAgents(t, func(string) ([]herd.Agent, error) { return nil, os.ErrDeadlineExceeded })
	stampTranscript(t, filepath.Join(home, ".claude-support"), "ZEN-1", time.Now())

	got := Owners([]string{"ZEN-1"}, All())["ZEN-1"]
	if got.Name != "support" {
		t.Fatalf("owner = %+v, want support from disk", got)
	}
	if got.Live || got.Status != session.Stopped {
		t.Errorf("owner = %+v, want a not-live, resumable status", got)
	}
}

// The peer socket is derived from the config dir, not the display name: the name
// is renameable via TICKETDECK_ACCOUNT, and the launcher keyed the workspace on
// the directory.
func TestHerdSocketFollowsTheConfigDirNotTheName(t *testing.T) {
	home := fakeHome(t, Default, "support")
	mkSocket(t, home, "support")

	renamed := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude-support")}
	want := filepath.Join(home, ".config-support", "herdr", "herdr.sock")
	if got := herdSocket(renamed); got != want {
		t.Errorf("herdSocket = %q, want %q", got, want)
	}

	// The default subscription shares ~/.config, and an account that has never
	// run a deck has no socket to ask.
	if got := herdSocket(Account{Name: Default, ConfigDir: filepath.Join(home, ".claude")}); got != "" {
		t.Errorf("herdSocket(default) = %q, want empty (no socket on disk)", got)
	}
}
