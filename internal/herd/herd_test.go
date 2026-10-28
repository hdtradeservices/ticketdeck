package herd

import (
	"errors"
	"fmt"
	"os"
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

// sampleAgents09JSON is the envelope herdr 0.9.0 returns. Two differences from
// 0.7.4 above, both load-bearing: there is no `name` at all, and the label a
// pane reported for itself arrives under `agent`.
const sampleAgents09JSON = `{"id":"cli:agent:list","result":{"agents":[
  {"agent_status":"working","cwd":"/home/matthew/Repos/walmart","agent":"ZEN-3175","pane_id":"w1:p1"},
  {"agent_status":"unknown","cwd":"/home/matthew/Repos","agent":"deck","pane_id":"w1:p2"}
]},"type":"agent_list"}`

// Reading only `name` made every 0.9.0 agent anonymous: Sessions() skipped the
// whole list, CloseByName matched nothing, and focusDeck found no deck.
func TestParseAgentsReadsHerdr09AgentField(t *testing.T) {
	agents, err := parseAgents([]byte(sampleAgents09JSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(agents))
	}
	if agents[0].Name != "ZEN-3175" {
		t.Errorf("0.9.0 agent name = %q, want ZEN-3175", agents[0].Name)
	}
	if agents[1].Name != "deck" {
		t.Errorf("0.9.0 deck name = %q, want deck", agents[1].Name)
	}
	// Sessions() excludes the deck by name, which only works once the name resolves.
	if got := Sessions(agents); len(got) != 1 || got[0].Name != "ZEN-3175" {
		t.Errorf("Sessions() = %+v, want just ZEN-3175", got)
	}
}

// 0.7.4 still sends `name`, and it must keep winning: an entry carrying both
// must not be renamed by the fallback.
func TestParseAgentsPrefersNameOverAgent(t *testing.T) {
	agents, err := parseAgents([]byte(`{"result":{"agents":[
	  {"name":"ZEN-1","agent":"claude","pane_id":"w1:p1"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Name != "ZEN-1" {
		t.Errorf("name = %q, want ZEN-1 (agent must not override it)", agents[0].Name)
	}
}

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
	// herdr 0.9 only accepts lowercase agent names; the deck still shows ZEN-3175.
	if cmd != "herdr agent focus zen-3175" {
		t.Errorf("unexpected focus command: %s", cmd)
	}
	if spec.Cwd != "/home/matthew/Repos/walmart" {
		t.Errorf("focus should reuse agent cwd, got %s", spec.Cwd)
	}
}

// fakeHerdrScript installs a stub `herdr` on herdrBin. The script is a fmt
// template whose %[1]s is a scratch file; the returned closure reads it back, so
// a stub can log the argv it was called with or count its own invocations.
func fakeHerdrScript(t *testing.T, script string) (scratch func() string) {
	t.Helper()
	dir := t.TempDir()
	file := dir + "/scratch"
	script = "#!/bin/sh" + fmt.Sprintf(script, file)

	path := dir + "/herdr"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := herdrBin
	herdrBin = path
	t.Cleanup(func() { herdrBin = old })
	return func() string {
		b, err := os.ReadFile(file)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
}

// fastPolls shrinks the readiness budget so the waiting tests don't sleep.
func fastPolls(t *testing.T) {
	t.Helper()
	oldReady, oldPoll := readyTimeout, pollEvery
	readyTimeout, pollEvery = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { readyTimeout, pollEvery = oldReady, oldPoll })
}

// herdr 0.9 only accepts lowercase agent names, and `agent prompt` is one call:
// the deck no longer types the text and presses Enter itself.
func TestSubmitPromptsByLowercaseName(t *testing.T) {
	log := fakeHerdrScript(t, `
echo "$@" >> %[1]s
`)
	if _, err := submit("ZEN-3751", "/triage"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := log(); got != "agent prompt zen-3751 /triage" {
		t.Errorf("submit ran %q, want \"agent prompt zen-3751 /triage\"", got)
	}
}

// herdr refuses to prompt a blocked agent, which is what stops a /triage
// answering a trust dialog. That refusal has to reach the deck's error line
// rather than being reported as a send.
func TestSubmitSurfacesHerdrRefusal(t *testing.T) {
	fakeHerdrScript(t, `
: %[1]s
printf '{"error":{"code":"agent_blocked","message":"agent is blocked"}}'
exit 1
`)
	_, err := submit("ZEN-3751", "/triage")
	if err == nil {
		t.Fatal("submit reported success for a refused prompt")
	}
	if !strings.Contains(err.Error(), "agent_blocked") {
		t.Errorf("error %q drops herdr's reason", err)
	}
}

// promptStub is a herdr whose agent shows the input box in the state file
// next to the scratch log: "empty", or "typed" when the deck's text sits in
// it unsent. stall makes `agent prompt --wait` report no turn, leaving the
// text typed; Enter on a typed box starts the turn.
func promptStub(t *testing.T, box string, stall bool) (log func() string) {
	t.Helper()
	stalled := "0"
	if stall {
		stalled = "1"
	}
	return fakeHerdrScript(t, `
log=%[1]s
state=$log.box
[ -f $state ] || echo `+box+` > $state
echo "$@" >> $log
case "$1 $2" in
"agent read")
  if [ "$(cat $state)" = typed ]; then
    printf '● Done.\n\n────\n❯ Your last turn was cut off because the account running\n  this session hit its usage limit.\n────\n  ⏵⏵ bypass permissions on\n'
  else
    printf '● Done.\n\n❯ an earlier message\n────\n❯ \n────\n'
  fi ;;
"agent prompt")
  if [ `+stalled+` = 1 ]; then
    echo typed > $state
    printf '{"error":{"code":"agent_prompt_stalled","message":"no working state within 5000ms"}}'
    exit 1
  fi ;;
"agent send-keys")
  [ "$(cat $state)" = typed ] && echo started > $state ;;
"agent wait")
  [ "$(cat $state)" = started ] || exit 1 ;;
esac
`)
}

const carryOnText = "Your last turn was cut off because the account running this session hit its usage limit. Please continue."

// A prompt's turn is confirmed, not assumed: herdr is asked to wait for it.
func TestPromptWaitsToSeeTheTurnStart(t *testing.T) {
	log := promptStub(t, "empty", false)
	out, err := Prompt("", "ZEN-1", carryOnText)
	if err != nil || out != "sent" {
		t.Fatalf("Prompt = %q, %v", out, err)
	}
	if !strings.Contains(log(), "agent prompt zen-1 "+carryOnText+" --wait --until working --until blocked") {
		t.Errorf("the prompt wasn't sent with a wait for its turn:\n%s", log())
	}
}

// The 2026-10-22 failure: the Enter was dropped, the text sat in the box, and
// no turn started. Prompt presses Enter on it and sees the turn start.
func TestPromptPressesEnterOnTextLeftInTheBox(t *testing.T) {
	log := promptStub(t, "empty", true)
	out, err := Prompt("", "ZEN-1", carryOnText)
	if err != nil || out != "sent + Enter" {
		t.Fatalf("Prompt = %q, %v\n%s", out, err, log())
	}
	if n := strings.Count(log(), "agent send-keys zen-1 Enter"); n != 1 {
		t.Errorf("Enter pressed %d times, want once:\n%s", n, log())
	}
}

// Text a failed attempt left behind is submitted, not typed again after itself.
func TestPromptSubmitsLeftoverTextInsteadOfRetyping(t *testing.T) {
	log := promptStub(t, "typed", false)
	if _, err := Prompt("", "ZEN-1", carryOnText); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if strings.Contains(log(), "agent prompt") {
		t.Errorf("typed the prompt again over the one still in the box:\n%s", log())
	}
}

// Enter is only ever pressed on the deck's own text. A box holding someone's
// draft is left alone, and the missing turn is reported.
func TestPromptLeavesSomeoneElsesDraftAlone(t *testing.T) {
	log := fakeHerdrScript(t, `
echo "$@" >> %[1]s
case "$1 $2" in
"agent read") printf '────\n❯ half a thought of my own\n────\n' ;;
"agent prompt") printf '{"error":{"code":"agent_prompt_stalled"}}'; exit 1 ;;
"agent wait") exit 1 ;;
esac
`)
	_, err := Prompt("", "ZEN-1", carryOnText)
	if !errors.Is(err, ErrNoTurn) {
		t.Errorf("err = %v, want ErrNoTurn", err)
	}
	if strings.Contains(log(), "send-keys") {
		t.Errorf("pressed a key in a box holding someone's draft:\n%s", log())
	}
}

// herdr's own refusal — a blocked agent — is an error, never an Enter.
func TestPromptSurfacesARefusal(t *testing.T) {
	log := fakeHerdrScript(t, `
echo "$@" >> %[1]s
case "$1 $2" in
"agent read") printf '❯ 1. Yes, I trust this folder\n' ;;
"agent prompt") printf '{"error":{"code":"agent_blocked"}}'; exit 1 ;;
esac
`)
	_, err := Prompt("", "ZEN-1", carryOnText)
	if err == nil || !strings.Contains(err.Error(), "agent_blocked") {
		t.Errorf("err = %v, want herdr's refusal", err)
	}
	if strings.Contains(log(), "send-keys") {
		t.Errorf("pressed a key at a dialog:\n%s", log())
	}
}

// Screens trimmed from Claude Code 2.1.308 on the workbench (2026-10-28).
func TestLimitMenu(t *testing.T) {
	const menu = `● Artifact comment monitor for https://claude.ai/artifact/AL9toA6bLiVKqx7f19mJFh didn't resume: it couldn't restart in this session.
  replies back on.
▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔
   What do you want to do?
   ❯ 1. Stop and wait for limit to reset
     2. Wait here, then continue automatically at Oct 30, 8am
     3. Ask your admin for more usage
   Enter to confirm · Esc to cancel
`
	const trust = ` Do you trust the files in this folder?
 /home/x/Repos
 ❯ 1. Yes, proceed
   2. No, exit
 Enter to confirm · Esc to exit
`
	const permission = ` Bash command
   rm -rf build
 Do you want to proceed?
 ❯ 1. Yes
   2. No, and tell Claude what to do differently (esc)
`
	quoted := "● It asked: What do you want to do? 1. Stop and wait for limit to reset · Esc to cancel\n" +
		strings.Repeat("  more of the reply\n", 12) + "───\n❯ \n───\n  ⏵⏵ bypass permissions on\n"
	for _, c := range []struct {
		name, screen string
		want         bool
	}{
		{"the limit menu", menu, true},
		{"a trust dialog", trust, false},
		{"a permission prompt", permission, false},
		{"the menu quoted further up", quoted, false},
		{"an unreadable screen", "", false},
	} {
		if got := limitMenu(c.screen); got != c.want {
			t.Errorf("%s: limitMenu = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadBox(t *testing.T) {
	const text = "Please continue where you left off."
	for _, c := range []struct {
		screen string
		want   boxState
	}{
		{"● Done.\n❯ " + text + "\n────\n", boxPending},
		{"❯ Please continue\n  where you left off.\n", boxPending}, // wrapped
		{"❯ " + text + "\n● Working…\n────\n❯ \n────\n", boxSubmitted},
		{"❯ something else entirely\n", boxSubmitted},
		{"● Thinking…\n", boxUnknown},
		{"", boxUnknown},
	} {
		if got := readBox(c.screen, text); got != c.want {
			t.Errorf("readBox(%q) = %v, want %v", c.screen, got, c.want)
		}
	}
}

// herdr calls a just-spawned agent "idle" long before Claude paints its input
// box, so the wait watches interactive_ready instead.
func TestWaitInteractiveWaitsForTheInputBox(t *testing.T) {
	fastPolls(t)
	calls := fakeHerdrScript(t, `
n=$(cat %[1]s 2>/dev/null || echo 0)
n=$((n + 1))
echo $n > %[1]s
if [ $n -lt 3 ]; then
  printf '{"result":{"agent":{"interactive_ready":false}}}'
else
  printf '{"result":{"agent":{"interactive_ready":true}}}'
fi
`)
	if !waitInteractive("ZEN-3751") {
		t.Fatal("waitInteractive gave up on an agent that became ready")
	}
	if calls() != "3" {
		t.Errorf("polled %s times, want 3", calls())
	}
}

// A session stuck on a trust dialog never reports ready. The wait has to run out
// and say so, rather than typing /triage at the dialog.
func TestWaitInteractiveGivesUpOnASessionThatNeverPaints(t *testing.T) {
	fastPolls(t)
	fakeHerdrScript(t, `
: %[1]s
printf '{"result":{"agent":{"interactive_ready":false}}}'
`)
	if waitInteractive("ZEN-3751") {
		t.Error("waitInteractive called a session that never painted ready")
	}
}

// An agent herdr can't read is not ready either — a stub that errors must not
// read as a green light.
func TestWaitInteractiveTreatsAnUnreadableAgentAsNotReady(t *testing.T) {
	fastPolls(t)
	fakeHerdrScript(t, `
: %[1]s
exit 1
`)
	if waitInteractive("ZEN-3751") {
		t.Error("waitInteractive treated an unreadable agent as ready")
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

// A closed scratch leaves a gap, and reissuing a live name costs a failed launch.
func TestScratchSpecPicksLowestFreeName(t *testing.T) {
	refs := func(names ...string) []session.SessionRef {
		out := make([]session.SessionRef, 0, len(names))
		for _, n := range names {
			out = append(out, session.SessionRef{Name: n})
		}
		return out
	}
	cases := []struct {
		name string
		live []session.SessionRef
		want string
	}{
		{"none live", nil, "scratch-1"},
		{"one live", refs("scratch-1"), "scratch-2"},
		{"gap at 1", refs("ZEN-4112", "scratch-2"), "scratch-1"},
		{"contiguous", refs("scratch-1", "scratch-2", "scratch-3"), "scratch-4"},
		{"out of order", refs("scratch-3", "scratch-1"), "scratch-2"},
		{"non-numeric suffix does not reserve", refs("scratch-notes"), "scratch-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScratchSpec(c.live, session.Ticket{}, "/repos")
			if got.Name != c.want || got.Label != c.want {
				t.Errorf("name/label = %q/%q, want %q", got.Name, got.Label, c.want)
			}
			want := strings.Join([]string{"agent", "start", c.want, "--cwd", "/repos", "--", "claude"}, " ")
			if strings.Join(got.Args, " ") != want {
				t.Errorf("args = %q, want %q", strings.Join(got.Args, " "), want)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"0.9.1", false}, {"0.9.2", true}, {"0.9.5", true}, {"0.10.0", true}, {"1.0.0", true},
		{"0.8.9", false}, {"v0.9.3", true}, {"0.9.2-preview", true}, {"garbage", false}, {"", false},
	} {
		if got := AtLeast(c.v, 0, 9, 2); got != c.want {
			t.Errorf("AtLeast(%q, 0.9.2) = %v, want %v", c.v, got, c.want)
		}
	}
}
