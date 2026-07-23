package herd

import (
	"os"
	"strings"
	"testing"

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
