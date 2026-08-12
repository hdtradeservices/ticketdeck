package herd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// TestLiveListParsesRealHerdr exercises List() against a real running herdr
// server. Skipped unless TICKETDECK_HERDR_LIVE=1 (requires a server with an
// agent named ZEN-3175). Verifies the exec command + parser against the binary.
func TestLiveListParsesRealHerdr(t *testing.T) {
	if os.Getenv("TICKETDECK_HERDR_LIVE") == "" {
		t.Skip("set TICKETDECK_HERDR_LIVE=1 with a running herdr server + ZEN-3175 agent")
	}
	agents, err := List()
	if err != nil {
		t.Fatalf("live List(): %v", err)
	}
	t.Logf("live agents: %+v", agents)
	found := false
	for _, a := range agents {
		if a.Name == "ZEN-3175" {
			found = true
			if statusOf(a) == session.None {
				t.Errorf("live ZEN-3175 mapped to None (agent_status=%q)", a.AgentStatus)
			}
		}
	}
	if !found {
		t.Fatalf("expected a live ZEN-3175 agent, got %+v", agents)
	}
}

// sampleAgentsJSON is the real envelope shape captured from herdr 0.7.4's
// `herdr agent list`.
const sampleAgentsJSON = `{"id":"cli:agent:list","result":{"agents":[
  {"agent_status":"working","cwd":"/home/matthew/Repos/walmart","name":"ZEN-3175","pane_id":"w1:p1"},
  {"agent_status":"blocked","cwd":"/home/matthew/Repos/etp","name":"ZEN-3210","pane_id":"w1:p2"},
  {"agent_status":"idle","cwd":"/home/matthew/Repos/etp","name":"ZEN-3181","pane_id":"w1:p3"}
]},"type":"agent_list"}`

func TestStatusMapping(t *testing.T) {
	cases := map[string]session.Status{
		"working": session.Working,
		"blocked": session.NeedsInput,
		"idle":    session.Idle,
		"unknown": session.Idle, // listed = running, state undetected
	}
	for st, want := range cases {
		if got := statusOf(Agent{AgentStatus: st}); got != want {
			t.Errorf("statusOf(%q) = %v, want %v", st, got, want)
		}
	}
}

func TestStatusesMatchByName(t *testing.T) {
	agents, err := parseAgents([]byte(sampleAgentsJSON))
	if err != nil {
		t.Fatal(err)
	}
	got := Statuses([]string{"ZEN-3175", "ZEN-3210", "ZEN-9999"}, agents)
	if got["ZEN-3175"] != session.Working {
		t.Errorf("ZEN-3175 want Working, got %v", got["ZEN-3175"])
	}
	if got["ZEN-3210"] != session.NeedsInput {
		t.Errorf("ZEN-3210 want NeedsInput, got %v", got["ZEN-3210"])
	}
	if got["ZEN-9999"] != session.None {
		t.Errorf("ZEN-9999 want None, got %v", got["ZEN-9999"])
	}
}

func TestPlanStartsNewAgent(t *testing.T) {
	spec, err := Plan(session.Ticket{Key: "ZEN-4242", Title: "t", URL: "http://x"}, nil, "/home/matthew/Repos")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "start" {
		t.Fatalf("want start, got %s", spec.Action)
	}
	cmd := Bin() + " " + strings.Join(spec.Args, " ")
	t.Logf("herdr start command: %s (cwd=%s)", cmd, spec.Cwd)

	for _, want := range []string{
		"agent start ZEN-4242",
		"--cwd /home/matthew/Repos",
		"-- claude",
		"--session-id " + session.DeterministicID("ZEN-4242"),
		"--name ZEN-4242",
		"--append-system-prompt", // ticket identity seeded here
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("start command missing %q\n%s", want, cmd)
		}
	}
	// BR-1/BR-3: no prompt, no model turn on launch.
	if strings.Contains(cmd, "-p ") || strings.Contains(cmd, "--print") {
		t.Errorf("must not run the model: %s", cmd)
	}
}

func TestPlanFocusesExistingAgent(t *testing.T) {
	agents, _ := parseAgents([]byte(sampleAgentsJSON))
	spec, err := Plan(session.Ticket{Key: "ZEN-3175"}, agents, "/def")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "focus" {
		t.Fatalf("want focus, got %s", spec.Action)
	}
	if spec.Foreground {
		t.Error("herdr focus should be fire-and-return, not foreground")
	}
	cmd := Bin() + " " + strings.Join(spec.Args, " ")
	t.Logf("herdr focus command: %s", cmd)
	if cmd != "herdr agent focus ZEN-3175" {
		t.Errorf("unexpected focus command: %s", cmd)
	}
	if spec.Cwd != "/home/matthew/Repos/walmart" {
		t.Errorf("focus should reuse agent cwd, got %s", spec.Cwd)
	}
}

// stuckScreen is a real capture of the bug: `t` typed /triage into the session,
// the slash-command menu opened, and the Enter never submitted it. Note the
// earlier "❯ hello there" — the transcript echo of a message that DID submit.
const stuckScreen = `❯ hello there

● Hey Matt — what are we working on?

  /triage        Routes a Linear issue to /investigate, /plan, /zeus…
  /babysit       Drives a PR to merge-ready: resolves conflicts, triages…
────────────────────────────────────────────
❯ /triage
────────────────────────────────────────────
  Opus 5 1M | Repos | 0/1m (0%) | effort: high
  -- INSERT -- ⏵⏵ bypass permissions on`

// submittedScreen is the same pane one Enter later: the box is empty again and
// /triage has moved into the transcript.
const submittedScreen = `❯ hello there

● Hey Matt — what are we working on?

❯ /triage

● I'll triage ZEN-3751.
────────────────────────────────────────────
❯
────────────────────────────────────────────
  Opus 5 1M | Repos | 0/1m (0%) | effort: high`

func TestPendingDetectsUnsentPrompt(t *testing.T) {
	if !pending(stuckScreen, "/triage") {
		t.Error("stuck screen: /triage is still in the input box, want pending")
	}
	if pending(submittedScreen, "/triage") {
		t.Error("submitted screen: input box is empty, want not pending")
	}
	// Half-painted box: only the first characters have landed.
	if !pending("────\n❯ /tri\n────\n  Opus 5", "/triage") {
		t.Error("half-painted box should count as pending")
	}
	// A modal covering the input box leaves nothing to press Enter at.
	if pending("   Help  General  Commands\n\n   Esc to cancel", "/triage") {
		t.Error("no input box on screen, want not pending")
	}
}

func TestPromptInputTakesTheLastPromptLine(t *testing.T) {
	in, ok := promptInput(stuckScreen)
	if !ok || in != "/triage" {
		t.Errorf("promptInput = (%q, %v), want (\"/triage\", true)", in, ok)
	}
	if in, ok := promptInput(submittedScreen); !ok || in != "" {
		t.Errorf("promptInput = (%q, %v), want (\"\", true)", in, ok)
	}
	if _, ok := promptInput("no prompt here"); ok {
		t.Error("promptInput found a box where there is none")
	}
}

// fakeHerdr installs a stub `herdr` on herdrBin that answers the three calls
// sendAndEnter makes. `agent read` reports the typed text still sitting in the
// prompt until clearAfter Enters have arrived — clearAfter=1 is a TUI that
// submits on the first Enter, 2 is the bug. Returns the path counting Enters.
func fakeHerdr(t *testing.T, text string, clearAfter int) (enters func() int) {
	t.Helper()
	return fakeHerdrScript(t, fmt.Sprintf(`
n=$(cat %%[1]s 2>/dev/null || echo 0)
case "$1 $2" in
  "agent send") exit 0 ;;
  "pane send-keys") echo $((n+1)) > %%[1]s; exit 0 ;;
  "agent read")
    if [ "$n" -ge %d ]; then box=""; else box=" %s"; fi
    printf '{"result":{"read":{"text":"----\\n❯%%%%s\\n----\\n  Opus 5 1M"}}}' "$box" ;;
esac
`, clearAfter, text))
}

// fakeHerdrScript installs an arbitrary stub `herdr` on herdrBin. The script is
// a fmt template whose %[1]s is a scratch file the stub counts Enters in.
func fakeHerdrScript(t *testing.T, script string) (enters func() int) {
	t.Helper()
	dir := t.TempDir()
	counter := dir + "/enters"
	script = "#!/bin/sh" + fmt.Sprintf(script, counter)

	path := dir + "/herdr"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := herdrBin
	herdrBin = path
	t.Cleanup(func() { herdrBin = old })
	return func() int {
		b, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
}

// fastPolls shrinks the pane-watching timeouts so the retry tests don't sleep.
func fastPolls(t *testing.T) {
	t.Helper()
	old := []time.Duration{typedTimeout, submitTimeout, promptTimeout, pollEvery}
	typedTimeout, submitTimeout, promptTimeout, pollEvery = 300*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		typedTimeout, submitTimeout, promptTimeout, pollEvery = old[0], old[1], old[2], old[3]
	})
}

func TestSendAndEnterRetriesSwallowedEnter(t *testing.T) {
	fastPolls(t)
	enters := fakeHerdr(t, "/triage", 2) // first Enter is swallowed
	res, err := sendAndEnter("ZEN-3751", "w1:pE", "/triage")
	if err != nil {
		t.Fatalf("sendAndEnter: %v", err)
	}
	if res != "sent + Enter" {
		t.Errorf("result = %q, want %q", res, "sent + Enter")
	}
	if got := enters(); got != 2 {
		t.Errorf("pressed Enter %d times, want 2 (one swallowed, one that landed)", got)
	}
}

func TestSendAndEnterStopsAtOneEnterWhenItLands(t *testing.T) {
	fastPolls(t)
	enters := fakeHerdr(t, "/triage", 1)
	if _, err := sendAndEnter("ZEN-3751", "w1:pE", "/triage"); err != nil {
		t.Fatalf("sendAndEnter: %v", err)
	}
	if got := enters(); got != 1 {
		t.Errorf("pressed Enter %d times, want 1", got)
	}
}

func TestSendAndEnterGivesUpLoudly(t *testing.T) {
	fastPolls(t)
	enters := fakeHerdr(t, "/triage", 99) // a prompt that never clears
	_, err := sendAndEnter("ZEN-3751", "w1:pE", "/triage")
	if err == nil {
		t.Fatal("want an error when /triage never leaves the prompt")
	}
	if !strings.Contains(err.Error(), "unsent") {
		t.Errorf("error should say the text is unsent, got: %v", err)
	}
	if got := enters(); got != submitAttempts {
		t.Errorf("pressed Enter %d times, want %d", got, submitAttempts)
	}
}

// A read that comes back without an input box says nothing about whether the
// Enter landed, and mid-paint — exactly when an Enter gets swallowed — is when
// that is most likely. Treating it as "the text is gone" would report the stuck
// case as a submit, so it reports unverified and stops pressing keys at a pane it
// can no longer see.
func TestSendAndEnterDoesNotCallAnUnreadablePaneASubmit(t *testing.T) {
	fastPolls(t)
	enters := fakeHerdrScript(t, `
n=$(cat %[1]s 2>/dev/null || echo 0)
case "$1 $2" in
  "agent send") exit 0 ;;
  "pane send-keys") echo $((n+1)) > %[1]s; exit 0 ;;
  "agent read")
    if [ "$n" -ge 1 ]; then exit 1; fi
    printf '{"result":{"read":{"text":"----\\n❯ /triage\\n----\\n  Opus 5 1M"}}}' ;;
esac
`)
	res, err := sendAndEnter("ZEN-3751", "w1:pE", "/triage")
	if err != nil {
		t.Fatalf("sendAndEnter: %v", err)
	}
	if !strings.Contains(res, "unverified") {
		t.Errorf("result = %q, want it flagged unverified", res)
	}
	if got := enters(); got != 1 {
		t.Errorf("pressed Enter %d times, want 1 — no point keying a pane we can't read", got)
	}
}

func TestReadBoxSeparatesUnknownFromSubmitted(t *testing.T) {
	if got := readBox("   Help  General\n\n   Esc to cancel", "/triage"); got != boxUnknown {
		t.Errorf("no box on screen = %v, want boxUnknown", got)
	}
	if got := readBox(submittedScreen, "/triage"); got != boxSubmitted {
		t.Errorf("empty box = %v, want boxSubmitted", got)
	}
	if got := readBox(stuckScreen, "/triage"); got != boxPending {
		t.Errorf("text still in the box = %v, want boxPending", got)
	}
	if got := readBox("----\n❯ something the user typed\n----", "/triage"); got != boxSubmitted {
		t.Errorf("box holding other text = %v, want boxSubmitted", got)
	}
}

// fakeScreen serves one fixed pane body for every `agent read`, so a caller can
// be pointed at a specific TUI state (a trust dialog, an unreadable pane).
func fakeScreen(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  \"agent read\") " + body + " ;;\nesac\n"
	path := dir + "/herdr"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := herdrBin
	herdrBin = path
	t.Cleanup(func() { herdrBin = old })
}

// A cold session sitting on Claude's trust prompt has an input box, and it is
// not ours to type into: "/triage" there answers the dialog. The wait has to
// report that rather than proceeding once its budget runs out.
func TestWaitForPromptRefusesAnOccupiedBox(t *testing.T) {
	fastPolls(t)
	fakeScreen(t, `printf '{"result":{"read":{"text":"Do you trust this folder?\\n❯ 1. Yes, proceed\\n  2. No"}}}'`)
	if waitForPrompt("ZEN-3751") {
		t.Error("a box occupied for the whole budget is not safe to type into")
	}
}

// A pane herdr can't read tells us nothing, and refusing there would mean `t`
// silently stops working. Fall through to the blind path instead.
func TestWaitForPromptAllowsAnUnreadablePane(t *testing.T) {
	fastPolls(t)
	fakeScreen(t, "exit 1")
	if !waitForPrompt("ZEN-3751") {
		t.Error("an unreadable pane should not block the submit")
	}
}

func TestWaitForPromptAcceptsAnEmptyBox(t *testing.T) {
	fastPolls(t)
	fakeScreen(t, `printf '{"result":{"read":{"text":"----\\n❯\\n----"}}}'`)
	if !waitForPrompt("ZEN-3751") {
		t.Error("an empty prompt box is exactly what this waits for")
	}
}

func TestTicketKeyRE(t *testing.T) {
	match := []string{"ZEN-3309", "DOPS-1", "A-9", "ABC123-42"}
	for _, s := range match {
		if !ticketKeyRE.MatchString(s) {
			t.Errorf("ticketKeyRE should match %q", s)
		}
	}
	for _, s := range []string{"deck", "scratch-1", "zen-3309", "ZEN", "ZEN-", "-5", "ZEN 3309"} {
		if ticketKeyRE.MatchString(s) {
			t.Errorf("ticketKeyRE should not match %q", s)
		}
	}
}
