package herd

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLiveSubmit exercises the `t` hotkey's submit path against a real herdr
// server: start a throwaway session, wait for it the way Triage does, fire a
// harmless /cost at it, and assert the command left the prompt. This is the
// regression guard for the class of bug that has broken `t` twice — a herdr
// release removing the verb the deck submits with (0.9 dropped `agent send`),
// and Claude's TUI swallowing the submit. Skipped unless TICKETDECK_HERDR_LIVE=1.
func TestLiveSubmit(t *testing.T) {
	if os.Getenv("TICKETDECK_HERDR_LIVE") == "" {
		t.Skip("set TICKETDECK_HERDR_LIVE=1 with a running herdr server")
	}
	const name = "tdlive"
	paneID, out, err := startAgentPane(name, os.Getenv("HOME")+"/Repos", name, []string{"claude"}, false)
	if err != nil {
		t.Fatalf("start %s: %v: %s", name, err, out)
	}
	defer func() { _ = exec.Command(herdrBin, "pane", "close", paneID).Run() }()

	if !waitInteractive(name) {
		t.Fatalf("%s never became interactive", name)
	}
	if res, err := submit(name, "/cost"); err != nil {
		t.Fatalf("submit: %v (%s)", err, res)
	}
	if strings.Contains(liveScreen(t, name), "❯ /cost") {
		t.Error("/cost still sitting in the prompt after submit reported success")
	}
}

// liveScreen reads a pane's visible text. Only the live test needs this — the
// deck itself no longer reads panes to work out whether a submit landed.
func liveScreen(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command(herdrBin, "agent", "read", agentRef(name), "--source", "visible", "--format", "text").Output()
	if err != nil {
		t.Fatalf("agent read %s: %v", name, err)
	}
	var r struct {
		Result struct {
			Read struct {
				Text string `json:"text"`
			} `json:"read"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("decode agent read: %v", err)
	}
	return r.Result.Read.Text
}
