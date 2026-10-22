package tui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// TestLiveMove drives a real session between two real accounts through a real
// herdr server: start it as one account, find it in the poll, move it to the
// other, and find it there. No prompt is sent, so no model turn runs.
//
// Skipped unless TICKETDECK_HERDR_LIVE=1, with HERDR_SOCKET_PATH pointing at a
// throwaway herdr server and TICKETDECK_LIVE_FROM / TICKETDECK_LIVE_TO naming
// two config dirs that are logged in and trust TICKETDECK_LIVE_CWD.
func TestLiveMove(t *testing.T) {
	if os.Getenv("TICKETDECK_HERDR_LIVE") == "" {
		t.Skip("set TICKETDECK_HERDR_LIVE=1 with a throwaway herdr server")
	}
	fromDir, toDir, cwd := os.Getenv("TICKETDECK_LIVE_FROM"), os.Getenv("TICKETDECK_LIVE_TO"), os.Getenv("TICKETDECK_LIVE_CWD")
	if fromDir == "" || toDir == "" || cwd == "" {
		t.Fatal("set TICKETDECK_LIVE_FROM, TICKETDECK_LIVE_TO and TICKETDECK_LIVE_CWD")
	}
	for k, v := range realEnv {
		t.Setenv(k, v)
	}
	from := account.Account{Name: "from", ConfigDir: fromDir}
	to := account.Account{Name: "to", ConfigDir: toDir}
	accts := []account.Account{from, to}
	id := session.NewID()
	const name = "tdlive-move"
	t.Cleanup(func() {
		if agents, err := herd.List(); err == nil {
			_, _ = herd.CloseByName(agents, name)
		}
		time.Sleep(time.Second)
		for _, d := range []string{fromDir, toDir} {
			if p := session.FindTranscript(d, id); p != "" {
				_ = os.Remove(p)
				_ = os.RemoveAll(strings.TrimSuffix(p, ".jsonl"))
			}
		}
	})

	_, out, err := herd.Start(herd.Launch{
		Name: name, Label: "tdlive move", Cwd: cwd, Env: session.AccountEnv(fromDir),
		Inner: []string{"claude", "--session-id", id, "--name", name},
	})
	if err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	find := func(want string) fleet.Session {
		t.Helper()
		led := map[string]ledger.Entry{id: {SessionID: id, Key: name, Kind: ledger.Scratch}}
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			snap := fleet.Poll(fleet.Input{Accounts: accts, Home: from, Ledger: led, Herdr: true, Cwd: cwd})
			if s, ok := snap.Best(name); ok && s.Live && s.Home && s.Account == want && s.ID == id && s.PID > 0 {
				return s
			}
		}
		snap := fleet.Poll(fleet.Input{Accounts: accts, Home: from, Ledger: led, Herdr: true, Cwd: cwd})
		t.Fatalf("never saw %s live on %s: %+v", name, want, snap.Sessions)
		return fleet.Session{}
	}
	s := find("from")
	t.Logf("running on from: pane %s pid %d status %v", s.PaneID, s.PID, s.Status)
	if env := procEnvOf(s.PID); env != fromDir {
		t.Errorf("the process runs as %q, want %q", env, fromDir)
	}
	if os.Getenv("TICKETDECK_LIVE_EMPTY") == "" {
		// /rename is local to Claude — no model turn — and it gives the session
		// a transcript to carry over, as any session someone has used has.
		if _, err := (HerdBackend{}).Send(name, "/rename tdlive renamed"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		for deadline := time.Now().Add(10 * time.Second); session.FindTranscript(fromDir, id) == ""; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the rename never reached the transcript")
			}
		}
	}

	res, err := HerdBackend{}.Move(Move{
		Name: name, PaneID: s.PaneID, PID: s.PID, From: from, To: to,
		Ticket: session.Ticket{Key: name, Title: "tdlive move", Scratch: true, SessionID: id}, Cwd: cwd,
	})
	if err != nil {
		t.Fatalf("move: %v (%s)", err, res)
	}
	if !session.ProcExited(s.PID) {
		t.Error("the old process should be gone once the move returns")
	}
	moved := find("to")
	t.Logf("running on to: pane %s pid %d status %v", moved.PaneID, moved.PID, moved.Status)
	if env := procEnvOf(moved.PID); env != toDir {
		t.Errorf("the moved process runs as %q, want %q", env, toDir)
	}
	if os.Getenv("TICKETDECK_LIVE_EMPTY") == "" {
		if session.FindTranscript(toDir, id) == "" {
			t.Error("the conversation should have been copied to the new account")
		}
		if title := session.TailOf(toDir, id).Title; title != "tdlive renamed" {
			t.Errorf("the moved session's title = %q, want the one it had", title)
		}
	}
}

func procEnvOf(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(b), "\x00") {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			return v
		}
	}
	return ""
}
