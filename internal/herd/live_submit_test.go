package herd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
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

// TestLivePrompt drives Prompt against a real herdr server and a real Claude.
// It stages the 2026-10-22 failure — the text typed into the input box, its
// Enter lost — and checks Prompt submits it and sees the turn start; then it
// sends a plain prompt. Two short turns run, on TICKETDECK_LIVE_MODEL (default
// haiku). Skipped unless TICKETDECK_HERDR_LIVE=1.
func TestLivePrompt(t *testing.T) {
	if os.Getenv("TICKETDECK_HERDR_LIVE") == "" {
		t.Skip("set TICKETDECK_HERDR_LIVE=1 with a running herdr server")
	}
	const name = "tdlive-prompt"
	cwd := firstNonEmpty(os.Getenv("TICKETDECK_LIVE_CWD"), os.Getenv("HOME")+"/Repos")
	model := firstNonEmpty(os.Getenv("TICKETDECK_LIVE_MODEL"), "haiku")
	id := session.NewID()
	paneID, out, err := startAgentPane(name, cwd, name, []string{"claude", "--session-id", id, "--model", model}, false)
	if err != nil {
		t.Fatalf("start %s: %v: %s", name, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(herdrBin, "pane", "close", paneID).Run()
		time.Sleep(time.Second)
		if p := session.FindTranscript(session.ConfigDir(), id); p != "" {
			_ = os.Remove(p)
			_ = os.RemoveAll(strings.TrimSuffix(p, ".jsonl"))
		}
	})
	if !waitInteractive(name) {
		t.Fatalf("%s never became interactive:\n%s", name, liveScreen(t, name))
	}

	const text = "Reply with the single word: ok"
	if out, err := exec.Command(herdrBin, "pane", "send-text", paneID, text).CombinedOutput(); err != nil {
		t.Fatalf("send-text: %v: %s", err, out)
	}
	for deadline := time.Now().Add(5 * time.Second); readBox(liveScreen(t, name), text) != boxPending; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the typed text never showed in the input box:\n%s", liveScreen(t, name))
		}
	}
	res, err := Prompt("", name, text)
	if err != nil || res != "sent + Enter" {
		t.Fatalf("Prompt on text left in the box = %q, %v\n%s", res, err, liveScreen(t, name))
	}
	if out, err := exec.Command(herdrBin, "agent", "wait", agentRef(name), "--until", "idle", "--until", "done", "--timeout", "120000").CombinedOutput(); err != nil {
		t.Fatalf("the turn never ended: %v: %s", err, out)
	}

	res, err = Prompt("", name, text)
	if err != nil || res != "sent" {
		t.Errorf("Prompt on an empty box = %q, %v\n%s", res, err, liveScreen(t, name))
	}
}

// liveScreen reads a pane's visible text.
func liveScreen(t *testing.T, name string) string {
	t.Helper()
	return paneText("", name)
}
