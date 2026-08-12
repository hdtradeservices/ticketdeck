package herd

import (
	"os"
	"os/exec"
	"testing"
)

// TestLiveSendAndEnterSubmits reproduces the `t`-hotkey bug against a real herdr
// server: a Claude session started moments ago swallows the Enter that follows
// typed text, leaving the command unsent in the prompt. It starts a throwaway
// session, fires a harmless /cost at it the way Triage does, and asserts the
// prompt cleared. Skipped unless TICKETDECK_HERDR_LIVE=1.
func TestLiveSendAndEnterSubmits(t *testing.T) {
	if os.Getenv("TICKETDECK_HERDR_LIVE") == "" {
		t.Skip("set TICKETDECK_HERDR_LIVE=1 with a running herdr server")
	}
	const name = "tdlive"
	out, err := exec.Command(herdrBin, "agent", "start", name, "--cwd", os.Getenv("HOME")+"/Repos", "--no-focus", "--", "claude").CombinedOutput()
	if err != nil {
		t.Fatalf("agent start: %v: %s", err, out)
	}
	paneID := parsePaneID(out)
	if paneID == "" {
		t.Fatalf("no pane id in: %s", out)
	}
	defer exec.Command(herdrBin, "pane", "close", paneID).Run()

	_ = exec.Command(herdrBin, "agent", "wait", name, "--status", "idle", "--timeout", "60000").Run()
	waitForPrompt(name)
	if res, err := sendAndEnter(name, paneID, "/cost"); err != nil {
		t.Fatalf("sendAndEnter: %v (%s)", err, res)
	}
	if pending(paneText(name), "/cost") {
		t.Error("/cost still sitting in the prompt after sendAndEnter reported success")
	}
}
