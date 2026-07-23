package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realAgentsJSON mirrors the actual `claude agents --json --all` output shape.
const realAgentsJSON = `[
  {"cwd":"/home/matthew/Repos/etp","kind":"background","startedAt":1783023170498,"sessionId":"37f1ce39-2f45-4417-bfe9-6df85c35d7ca","name":"headphone jack","state":"done"},
  {"pid":1164337,"cwd":"/home/matthew/Repos/etp","kind":"interactive","startedAt":1784133914047,"sessionId":"da5a49bf-f07f-434f-9edc-bf8b80351158","name":"etp-18","status":"idle"},
  {"pid":1679520,"id":"b2fa649b","cwd":"/home/matthew/Repos/walmart","kind":"background","startedAt":1784205580420,"sessionId":"b2fa649b-5830-42ba-ade7-8dab6ec0f72d","name":"ZEN-3175","status":"busy","state":"working"}
]`

func TestTabLabel(t *testing.T) {
	cases := []struct {
		key, title, want string
	}{
		{"ZEN-42", "fix the thing", "ZEN-42  fix the thing"},
		{"ZEN-42", "", "ZEN-42"},
		{"DOPS-1", "   ", "DOPS-1"},
		// 32-rune cap with ellipsis (agent name stays the bare key elsewhere).
		{"ZEN-3309", "a really long ticket title that overflows the tab", "ZEN-3309  a really long ticket…"},
	}
	for _, c := range cases {
		got := TabLabel(Ticket{Key: c.key, Title: c.title})
		if got != c.want {
			t.Errorf("TabLabel(%q,%q) = %q, want %q", c.key, c.title, got, c.want)
		}
		if r := []rune(got); len(r) > 32 {
			t.Errorf("TabLabel(%q,%q) = %q is %d runes, want ≤32", c.key, c.title, got, len(r))
		}
	}
}

func TestDeterministicIDStableAndValid(t *testing.T) {
	a := DeterministicID("ZEN-3175")
	b := DeterministicID("ZEN-3175")
	if a != b {
		t.Fatalf("id not deterministic: %s != %s", a, b)
	}
	if DeterministicID("zen-3175") != a {
		t.Error("id should be case-insensitive on the key")
	}
	if DeterministicID("ZEN-9999") == a {
		t.Error("different keys must produce different ids")
	}
	// UUID v5 shape: 8-4-4-4-12, version nibble '5', variant '8|9|a|b'.
	parts := strings.Split(a, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[2]) != 4 {
		t.Fatalf("not a uuid: %s", a)
	}
	if parts[2][0] != '5' {
		t.Errorf("expected version 5, got %c in %s", parts[2][0], a)
	}
	if !strings.ContainsRune("89ab", rune(parts[3][0])) {
		t.Errorf("bad variant nibble %c in %s", parts[3][0], a)
	}
}

func TestStatusesMatchByNameAndID(t *testing.T) {
	infos, err := parseInfos([]byte(realAgentsJSON))
	if err != nil {
		t.Fatal(err)
	}
	// ZEN-3175 matches the busy background session by name → Working.
	// ZEN-0000 has no session → None.
	got := Statuses([]string{"ZEN-3175", "ZEN-0000"}, infos)
	if got["ZEN-3175"] != Working {
		t.Errorf("ZEN-3175 should be Working, got %v", got["ZEN-3175"])
	}
	if got["ZEN-0000"] != None {
		t.Errorf("ZEN-0000 should be None, got %v", got["ZEN-0000"])
	}
}

// TestStatusOfRealCombos covers every (status,state) combo observed from the
// real `claude agents --json` output.
func TestStatusOfRealCombos(t *testing.T) {
	cases := []struct {
		status, state string
		pid           int
		want          Status
	}{
		{"busy", "working", 100, Working},
		{"idle", "working", 100, Working},
		{"idle", "blocked", 100, NeedsInput},
		{"waiting", "blocked", 100, NeedsInput},
		{"", "blocked", 0, NeedsInput},
		{"idle", "done", 0, Completed},
		{"", "done", 0, Completed},
		{"idle", "", 100, Idle}, // interactive attached
		{"", "", 0, Stopped},    // present but not running/done
	}
	for _, c := range cases {
		got := statusOf(Info{Status: c.status, State: c.state, PID: c.pid})
		if got != c.want {
			t.Errorf("statusOf(status=%q state=%q pid=%d) = %v, want %v", c.status, c.state, c.pid, got, c.want)
		}
	}
}

func TestPlanNewLaunchSeedsContextNoPrompt(t *testing.T) {
	spec, err := Plan(Ticket{Key: "ZEN-4242", Title: "t", URL: "http://x"}, nil, "/home/matthew/Repos")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "new" {
		t.Fatalf("expected new, got %s", spec.Action)
	}
	if spec.Cwd != "/home/matthew/Repos" {
		t.Errorf("new launch should use default cwd, got %s", spec.Cwd)
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "--session-id "+DeterministicID("ZEN-4242")) {
		t.Errorf("missing deterministic --session-id: %v", spec.Args)
	}
	if !strings.Contains(joined, "--name ZEN-4242") {
		t.Errorf("missing --name: %v", spec.Args)
	}
	// BR-1/BR-3: launching must never pass a prompt / -p to the model.
	for _, a := range spec.Args {
		if a == "-p" || a == "--print" {
			t.Fatalf("launch args must not run the model: %v", spec.Args)
		}
	}
	// Ticket identity is seeded via an appended system prompt naming the ticket.
	if !strings.Contains(joined, "--append-system-prompt") {
		t.Errorf("missing --append-system-prompt: %v", spec.Args)
	}
	if !strings.Contains(joined, "ZEN-4242") {
		t.Errorf("system prompt should name the ticket: %v", spec.Args)
	}
}

func TestPlanResumesWhenTranscriptOnDisk(t *testing.T) {
	// A session the daemon has dropped but whose transcript is still on disk
	// must resume, not collide on --session-id.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cwd := "/home/matthew/Repos"
	id := DeterministicID("ZEN-DISK")
	if err := os.MkdirAll(filepath.Dir(TranscriptPath(id, cwd)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TranscriptPath(id, cwd), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := Plan(Ticket{Key: "ZEN-DISK"}, nil, cwd) // nil infos = daemon forgot it
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "resume" {
		t.Fatalf("want resume (transcript on disk), got %s: %v", spec.Action, spec.Args)
	}
	if spec.Args[0] != "--resume" || spec.Args[1] != id {
		t.Errorf("bad resume args: %v", spec.Args)
	}
}

func TestPlanNewWhenNoTranscript(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	spec, err := Plan(Ticket{Key: "ZEN-FRESH"}, nil, "/home/matthew/Repos")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "new" {
		t.Fatalf("want new (no transcript), got %s", spec.Action)
	}
}

func TestPlanLiveGoesToAgentsView(t *testing.T) {
	infos, _ := parseInfos([]byte(realAgentsJSON))
	spec, err := Plan(Ticket{Key: "ZEN-3175"}, infos, "/def")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "agents-view" {
		t.Fatalf("live session should route to agents view, got %s", spec.Action)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "agents" {
		t.Errorf("expected [agents], got %v", spec.Args)
	}
	// Reuse the running session's cwd, not the default.
	if spec.Cwd != "/home/matthew/Repos/walmart" {
		t.Errorf("should reuse session cwd, got %s", spec.Cwd)
	}
}

func TestPlanDeadResumes(t *testing.T) {
	dead := []Info{{SessionID: DeterministicID("ZEN-7"), Name: "ZEN-7", Cwd: "/home/matthew/Repos/amazon", State: "done"}}
	spec, err := Plan(Ticket{Key: "ZEN-7", Title: "x"}, dead, "/def")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Action != "resume" {
		t.Fatalf("expected resume, got %s", spec.Action)
	}
	if spec.Args[0] != "--resume" || spec.Args[1] != DeterministicID("ZEN-7") {
		t.Errorf("bad resume args: %v", spec.Args)
	}
	if spec.Cwd != "/home/matthew/Repos/amazon" {
		t.Errorf("resume should reuse original cwd, got %s", spec.Cwd)
	}
}
