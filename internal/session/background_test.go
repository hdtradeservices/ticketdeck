package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Transcript lines trimmed from real Claude Code sessions (2026-09-28). Every
// launch is stamped at t0; procStart in the tests sits before or after it.
const (
	launchLine  = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Async agent launched"}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"a111","description":"Triage ZEN-1"}}`
	forkLine    = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[]},"toolUseResult":{"success":true,"commandName":"code-review","status":"forked","background":true,"agentId":"a222"}}`
	syncFork    = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[]},"toolUseResult":{"success":true,"commandName":"code-review","status":"forked","agentId":"a333"}}`
	resumeLine  = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[]},"toolUseResult":{"success":true,"message":"Resuming agent a111","resumedAgentId":"a111"}}`
	bashBgLine  = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[]},"toolUseResult":{"backgroundTaskId":"bzyv","interrupted":false}}`
	monitorLine = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[]},"toolUseResult":{"taskId":"bmon","timeoutMs":1800000,"persistent":false}}`
	taskStop    = `{"type":"user","timestamp":"2026-09-28T12:00:00Z","message":{"role":"user","content":[]},"toolUseResult":{"message":"Successfully stopped task: bzyv","task_id":"bzyv","task_type":"local_bash"}}`
	plainLine   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"<task-id>a111</task-id><status>completed</status>"}]}}`
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func queued(id string) string {
	return `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>` + id + `</task-id>\n<status>completed</status>\n</task-notification>"}`
}

func notice(id string) string {
	return `{"type":"user","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>\n<task-id>` + id + `</task-id>\n<status>completed</status>\n</task-notification>"}}`
}

// monitorEvent is the notice a Monitor sends per event: no <status>.
func monitorEvent(id string) string {
	return `{"type":"user","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>\n<task-id>` + id + `</task-id>\n<summary>Monitor event</summary>\n<event>build: pass</event>\n</task-notification>"}}`
}

// bgFixture writes a session transcript plus a subagent transcript per agent id,
// and returns the session id and launch cwd.
func bgFixture(t *testing.T, lines []string, agents ...string) (id, cwd string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	cwd = "/home/x/Repos"
	id = DeterministicID("ZEN-1")
	path := TranscriptPath(id, cwd)
	sub := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if err := os.WriteFile(filepath.Join(sub, "agent-"+a+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return id, cwd
}

func TestBackgroundWork(t *testing.T) {
	before := t0.Add(-time.Hour) // the process predates every launch
	cases := []struct {
		name      string
		lines     []string
		procStart time.Time
		want      InFlight
	}{
		{"no launches", []string{plainLine}, before, InFlight{}},
		{"one subagent running", []string{launchLine}, before, InFlight{Agents: 1}},
		{"launched then notified", []string{launchLine, notice("a111")}, before, InFlight{}},
		{"notice queued while the lead was busy", []string{launchLine, queued("a111")}, before, InFlight{}},
		{"queue removal is not a notice", []string{launchLine, strings.Replace(queued("a111"), "enqueue", "remove", 1)}, before, InFlight{Agents: 1}},
		{"resumed after notice", []string{launchLine, notice("a111"), resumeLine}, before, InFlight{Agents: 1}},
		{"background skill fork", []string{forkLine}, before, InFlight{Agents: 1}},
		{"foreground skill fork is not background", []string{syncFork}, before, InFlight{}},
		{"task-id outside a notification is ignored", []string{launchLine, plainLine}, before, InFlight{Agents: 1}},
		{"two subagents, one done", []string{launchLine, forkLine, notice("a222")}, before, InFlight{Agents: 1}},

		{"background shell command", []string{bashBgLine}, before, InFlight{Tasks: 1}},
		{"shell command finished", []string{bashBgLine, notice("bzyv")}, before, InFlight{}},
		{"shell command stopped", []string{bashBgLine, taskStop}, before, InFlight{}},
		{"monitor running", []string{monitorLine}, before, InFlight{Tasks: 1}},
		{"monitor event does not end it", []string{monitorLine, monitorEvent("bmon")}, before, InFlight{Tasks: 1}},
		{"monitor expired", []string{monitorLine, monitorEvent("bmon"), notice("bmon")}, before, InFlight{}},
		{"subagent and shell together", []string{launchLine, bashBgLine}, before, InFlight{Agents: 1, Tasks: 1}},

		// The process restarted after these launched, so they died with the old one.
		{"launched by a previous process", []string{launchLine, bashBgLine}, t0.Add(time.Minute), InFlight{}},
		// With no process to date against, a shell task can't be told from a dead one.
		{"process unknown: shell uncounted", []string{launchLine, bashBgLine}, time.Time{}, InFlight{Agents: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, cwd := bgFixture(t, c.lines, "a111", "a222", "a333")
			if got := BackgroundWork(id, cwd, c.procStart); got != c.want {
				t.Errorf("BackgroundWork = %+v, want %+v", got, c.want)
			}
		})
	}
}

// A launch whose notification never landed stops counting once its subagent
// goes quiet, so a killed session doesn't read "working" forever.
func TestBackgroundWorkStaleSubagent(t *testing.T) {
	id, cwd := bgFixture(t, []string{launchLine}, "a111")
	sub := filepath.Join(strings.TrimSuffix(TranscriptPath(id, cwd), ".jsonl"), "subagents", "agent-a111.jsonl")
	old := time.Now().Add(-subagentStaleAfter - time.Minute)
	if err := os.Chtimes(sub, old, old); err != nil {
		t.Fatal(err)
	}
	if got := BackgroundWork(id, cwd, time.Time{}); got.Agents != 0 {
		t.Errorf("stale subagent counted: %+v", got)
	}
}

// Appends are read incrementally, and a half-written trailing line waits for
// the next tick instead of being dropped.
func TestBackgroundWorkIncremental(t *testing.T) {
	id, cwd := bgFixture(t, []string{launchLine}, "a111")
	path := TranscriptPath(id, cwd)
	if got := BackgroundWork(id, cwd, time.Time{}); got.Agents != 1 {
		t.Fatalf("after launch = %+v, want 1 agent", got)
	}

	n := notice("a111")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(n[:20])
	if got := BackgroundWork(id, cwd, time.Time{}); got.Agents != 1 {
		t.Errorf("partial notice = %+v, want 1 agent", got)
	}
	f.WriteString(n[20:] + "\n")
	if got := BackgroundWork(id, cwd, time.Time{}); got.Agents != 0 {
		t.Errorf("after notice = %+v, want none", got)
	}
}

func TestMarkBackground(t *testing.T) {
	orig := procStarts
	t.Cleanup(func() { procStarts = orig })

	cases := []struct {
		name  string
		lines []string
		want  Status
	}{
		{"subagent running reads working", []string{launchLine, bashBgLine}, Working},
		{"only a shell task reads background", []string{bashBgLine}, Background},
		{"nothing in flight stays idle", []string{bashBgLine, notice("bzyv")}, Idle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, _ := bgFixture(t, c.lines, "a111")
			procStarts = func() map[string]time.Time { return map[string]time.Time{id: t0.Add(-time.Hour)} }
			res := map[string]Status{"ZEN-1": Idle, "ZEN-2": Idle, "ZEN-3": NeedsInput}
			MarkBackground(res, "/home/x/Repos")
			if res["ZEN-1"] != c.want {
				t.Errorf("ZEN-1 = %v, want %v", res["ZEN-1"], c.want)
			}
			if res["ZEN-2"] != Idle {
				t.Errorf("ZEN-2 = %v, want Idle (no transcript)", res["ZEN-2"])
			}
			if res["ZEN-3"] != NeedsInput {
				t.Errorf("ZEN-3 = %v, want NeedsInput (only Idle is refined)", res["ZEN-3"])
			}
		})
	}
}

// The live process scan must at least date this test binary's own process
// sensibly, which exercises the /proc/<pid>/stat parse on Linux.
func TestProcStartTime(t *testing.T) {
	boot := bootTime()
	if boot.IsZero() {
		t.Skip("no /proc/stat")
	}
	st := procStartTime("/proc/self", boot)
	if st.IsZero() || st.After(time.Now()) || time.Since(st) > time.Hour {
		t.Errorf("procStartTime(self) = %v, want within the last hour", st)
	}
}

// sessionProcStarts finds a process whose program is claude by the session id
// in its argv. The stand-in is a shell whose argv[0] reads "claude".
func TestSessionProcStarts(t *testing.T) {
	if bootTime().IsZero() {
		t.Skip("no /proc")
	}
	id := DeterministicID("ZEN-PROC-TEST")
	cmd := &exec.Cmd{Path: "/bin/sh", Args: []string{"claude", "-c", "sleep 30; :", "sh", "--session-id", id}}
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	st, ok := sessionProcStarts()[id]
	if !ok {
		t.Fatalf("session %s not found among live processes", id)
	}
	if time.Since(st) > time.Minute || st.After(time.Now().Add(time.Second)) {
		t.Errorf("start time %v, want just now", st)
	}
}
