package herd

import (
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
			got := ScratchSpec(c.live, "/repos")
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
