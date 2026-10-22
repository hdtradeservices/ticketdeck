package herd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLiveWatch runs Watch against a real herdr server: an agent appearing in
// a new tab, then changing status, must each reach the deck. It needs no
// Claude session — the agent's status is reported the way herdr's own hooks
// report it — so it spends nothing. Skipped unless TICKETDECK_HERDR_LIVE=1,
// with HERDR_SOCKET_PATH pointing at a throwaway server that has a workspace.
func TestLiveWatch(t *testing.T) {
	if os.Getenv("TICKETDECK_HERDR_LIVE") == "" {
		t.Skip("set TICKETDECK_HERDR_LIVE=1 with a throwaway herdr server")
	}
	changed := make(chan struct{}, 64)
	up := make(chan bool, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, OwnSocket(), func() { changed <- struct{}{} }, func(b bool) { up <- b })
	select {
	case b := <-up:
		if !b {
			t.Fatal("the first report was down")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never subscribed")
	}
	waitChanged := func(what string) {
		t.Helper()
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatalf("no change reported after %s", what)
		}
	}
	drain := func() {
		for {
			select {
			case <-changed:
			case <-time.After(300 * time.Millisecond):
				return
			}
		}
	}

	out, err := exec.Command(herdrBin, "tab", "create", "--label", "tdlive-events").Output()
	if err != nil {
		t.Fatalf("tab create: %v", err)
	}
	var tab struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &tab); err != nil || tab.Result.RootPane.PaneID == "" {
		t.Fatalf("tab create said %s", out)
	}
	pane := tab.Result.RootPane.PaneID
	defer func() { _ = exec.Command(herdrBin, "pane", "close", pane).Run() }()
	waitChanged("a tab opened")
	drain()

	report := func(state string) {
		t.Helper()
		if out, err := exec.Command(herdrBin, "pane", "report-agent", pane, "--source", "tdlive", "--agent", "claude", "--state", state).CombinedOutput(); err != nil {
			t.Fatalf("report-agent %s: %v: %s", state, err, out)
		}
	}
	report("working")
	waitChanged("an agent appeared")
	drain()
	// The agent is now followed: a status change alone must come through.
	report("idle")
	waitChanged("the agent went idle")
}
