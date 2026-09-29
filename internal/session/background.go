package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A session's live status (herdr's agent_status, or `claude agents`) tracks only
// the lead agent. It reads idle as soon as the lead ends its turn, even while
// work the lead started in the background is still running. The lead's
// transcript records both halves: each launch, and the <task-notification> with
// a <status> that arrives when the task stops. BackgroundWork reads the
// difference.

// subagentStaleAfter bounds how long a launched-but-unfinished subagent counts
// as working without writing to its own transcript. A launch whose notification
// never landed (the session was killed mid-flight) would otherwise pin the row
// on "working" forever.
var subagentStaleAfter = 15 * time.Minute

var (
	taskIDRE     = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	taskStatusRE = regexp.MustCompile(`<status>[^<]+</status>`)
)

type taskKind int

const (
	kindAgent taskKind = iota // a subagent, or a skill forked into the background
	kindShell                 // a backgrounded Bash command, or a Monitor
)

type openTask struct {
	kind    taskKind
	started time.Time
}

// InFlight is what a session has running behind an idle lead.
type InFlight struct {
	Agents int // subagents
	Tasks  int // shell commands and monitors
}

type transcriptLine struct {
	Type      string          `json:"type"`
	Operation string          `json:"operation"`
	Timestamp time.Time       `json:"timestamp"`
	Content   json.RawMessage `json:"content"` // queue-operation payload
	Origin    *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type launchResult struct {
	AgentID          string `json:"agentId"`
	IsAsync          bool   `json:"isAsync"`    // Agent with run_in_background
	Background       bool   `json:"background"` // a skill forked into the background
	ResumedAgentID   string `json:"resumedAgentId"`
	BackgroundTaskID string `json:"backgroundTaskId"` // Bash, backgrounded
	TaskID           string `json:"taskId"`           // Monitor
	Persistent       *bool  `json:"persistent"`       // present only on a Monitor result
	StoppedTaskID    string `json:"task_id"`          // TaskStop
}

// bgScan is the parse state for one transcript, kept between ticks so each tick
// reads only what was appended since the last one.
type bgScan struct {
	offset int64
	open   map[string]openTask // task id → launched, not yet finished
}

var (
	bgMu    sync.Mutex
	bgScans = map[string]*bgScan{}
)

// BackgroundWork reports what the session with this id has running in the
// background. cwd is the launch root the transcript is filed under. procStart
// is when the session's current process started, or zero when unknown.
//
// Background tasks die with the process that started them, so a task launched
// before procStart is gone even though no notice says so. A shell task only
// counts when procStart is known: unlike a subagent it has no transcript of its
// own to show it is still alive.
func BackgroundWork(id, cwd string, procStart time.Time) InFlight {
	path := findTranscript(id, cwd)
	if path == "" {
		return InFlight{}
	}
	dir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	var bg InFlight
	for tid, t := range scanBackground(path) {
		if !procStart.IsZero() && t.started.Before(procStart) {
			continue
		}
		switch t.kind {
		case kindAgent:
			fi, err := os.Stat(filepath.Join(dir, "agent-"+tid+".jsonl"))
			if err == nil && time.Since(fi.ModTime()) < subagentStaleAfter {
				bg.Agents++
			}
		case kindShell:
			if !procStart.IsZero() {
				bg.Tasks++
			}
		}
	}
	return bg
}

// procStarts is overridable in tests.
var procStarts = sessionProcStarts

// MarkBackground refines an Idle ticket whose lead has work in flight: Working
// while subagents run, else Background while shell commands or monitors do.
func MarkBackground(res map[string]Status, cwd string) {
	var starts map[string]time.Time
	for k, st := range res {
		if st != Idle {
			continue
		}
		if starts == nil {
			starts = procStarts()
		}
		id := DeterministicID(k)
		switch bg := BackgroundWork(id, cwd, starts[id]); {
		case bg.Agents > 0:
			res[k] = Working
		case bg.Tasks > 0:
			res[k] = Background
		}
	}
}

// sessionProcStarts maps session id → start time of the claude process running
// it, found by the --session-id or --resume argument the deck launches with.
// Linux only; elsewhere it finds nothing, and shell tasks go uncounted.
func sessionProcStarts() map[string]time.Time {
	out := map[string]time.Time{}
	boot := bootTime()
	if boot.IsZero() {
		return out
	}
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range pids {
		b, err := os.ReadFile(filepath.Join(p, "cmdline"))
		if err != nil || !bytes.Contains(b, []byte("claude")) {
			continue
		}
		// Match on the program, not the whole line: herdr's own argv carries the
		// same flags after its "--".
		args := strings.Split(string(b), "\x00")
		if filepath.Base(args[0]) != "claude" {
			continue
		}
		for i := 1; i+1 < len(args); i++ {
			if args[i] != "--session-id" && args[i] != "--resume" {
				continue
			}
			if st := procStartTime(p, boot); !st.IsZero() {
				if prev, ok := out[args[i+1]]; !ok || st.After(prev) {
					out[args[i+1]] = st
				}
			}
			break
		}
	}
	return out
}

// clockTicks is USER_HZ, which Linux fixes at 100 on every mainstream arch.
const clockTicks = 100

func bootTime() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(s, 0)
			}
		}
	}
	return time.Time{}
}

// procStartTime reads field 22 (starttime) of /proc/<pid>/stat. Fields are
// counted after the ")" that closes the command name, which may hold spaces.
func procStartTime(procDir string, boot time.Time) time.Time {
	b, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return time.Time{}
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return time.Time{}
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return boot.Add(time.Duration(ticks) * time.Second / clockTicks)
}

// findTranscript locates a session's transcript: under cwd's project first, then
// any project, since a session keeps the slug of wherever it was started.
func findTranscript(id, cwd string) string {
	if p := TranscriptPath(id, cwd); fileExists(p) {
		return p
	}
	m, _ := filepath.Glob(filepath.Join(ConfigDir(), "projects", "*", id+".jsonl"))
	if len(m) > 0 {
		return m[0]
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// scanBackground returns the tasks launched in path and not yet finished. The
// returned map is a copy.
func scanBackground(path string) map[string]openTask {
	bgMu.Lock()
	defer bgMu.Unlock()

	s := bgScans[path]
	fi, err := os.Stat(path)
	if err != nil {
		delete(bgScans, path)
		return nil
	}
	if s == nil || fi.Size() < s.offset {
		s = &bgScan{open: map[string]openTask{}}
		bgScans[path] = s
	}
	if fi.Size() > s.offset {
		s.advance(path)
	}

	out := make(map[string]openTask, len(s.open))
	for k, v := range s.open {
		out[k] = v
	}
	return out
}

// advance reads complete lines from s.offset onward. A trailing line still
// being written is left for the next tick.
func (s *bgScan) advance(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // EOF or a partial line: resume here next time
		}
		s.offset += int64(len(line))
		s.apply(line)
	}
}

func (s *bgScan) apply(line []byte) {
	// Cheap prefilter: most lines are neither a launch nor a notification, and
	// some are megabytes of tool output.
	if !bytes.Contains(line, []byte("<task-id>")) && !bytes.Contains(line, []byte(`"toolUseResult"`)) {
		return
	}
	var l transcriptLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	// A notice that arrives while the lead is mid-turn is only queued: the
	// enqueue is written when the task stops, the user message much later.
	var notice json.RawMessage
	switch {
	case l.Type == "queue-operation" && l.Operation == "enqueue":
		notice = l.Content
	case l.Origin != nil && l.Origin.Kind == "task-notification":
		notice = l.Message.Content
	}
	if notice != nil {
		// A Monitor sends a notice per event it sees; only one carrying a
		// <status> means the task stopped.
		if bytes.Contains(notice, []byte("<task-notification>")) && taskStatusRE.Match(notice) {
			if m := taskIDRE.FindSubmatch(notice); m != nil {
				delete(s.open, string(m[1]))
			}
		}
		return
	}
	var r launchResult
	if len(l.ToolUseResult) == 0 || json.Unmarshal(l.ToolUseResult, &r) != nil {
		return
	}
	switch {
	case r.ResumedAgentID != "":
		s.open[r.ResumedAgentID] = openTask{kindAgent, l.Timestamp}
	case r.AgentID != "" && (r.IsAsync || r.Background):
		s.open[r.AgentID] = openTask{kindAgent, l.Timestamp}
	case r.BackgroundTaskID != "":
		s.open[r.BackgroundTaskID] = openTask{kindShell, l.Timestamp}
	case r.TaskID != "" && r.Persistent != nil:
		s.open[r.TaskID] = openTask{kindShell, l.Timestamp}
	case r.StoppedTaskID != "":
		delete(s.open, r.StoppedTaskID)
	}
}
