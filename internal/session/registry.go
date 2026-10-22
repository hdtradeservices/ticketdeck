package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Proc is one live Claude Code process, read from the registry its account
// keeps at <config dir>/sessions/<pid>.json. Claude Code writes that file when a
// session starts, keeps its status current, and removes it on exit; `claude
// agents` reads the same files.
//
// The registry is the only place that maps a running process to its session id
// and its account without asking anything that might be slow or absent: a
// scratch session is a bare `claude` with no id on its command line, and herdr
// only learns a session's id when that account has herdr's hook installed.
type Proc struct {
	PID        int
	SessionID  string
	Cwd        string
	Name       string
	Kind       string // "interactive" | "background"
	Status     string // "busy" | "idle" | "waiting"
	WaitingFor string // what a "waiting" session is waiting on, when Claude says
	StartedAt  time.Time
	ConfigDir  string
	// ProcStart is when the process started, zero when it can't be read. It
	// bounds which background tasks can still be alive (see BackgroundWork).
	ProcStart time.Time
	// HerdrSocket and HerdrPane locate the herdr pane the process runs in, read
	// from its environment. Linux only; empty elsewhere and outside herdr.
	HerdrSocket string
	HerdrPane   string
}

type regFile struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	StartedAt  int64  `json:"startedAt"` // epoch milliseconds
	ProcStart  string `json:"procStart"` // clock ticks since boot, as /proc/<pid>/stat has it
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
}

// procRoot is /proc; tests point it at a fixture tree.
var procRoot = "/proc"

// Registry lists the live Claude Code processes running under one account. A
// file whose process has exited — Claude killed hard, so it never cleaned up —
// is skipped, as is one whose pid has since been reused by another process.
func Registry(configDir string) []Proc {
	ents, err := os.ReadDir(filepath.Join(configDir, "sessions"))
	if err != nil {
		return nil
	}
	boot := bootTime()
	var out []Proc
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(configDir, "sessions", e.Name()))
		if err != nil {
			continue
		}
		var f regFile
		if json.Unmarshal(b, &f) != nil || f.PID <= 0 || f.SessionID == "" {
			continue
		}
		if !procAlive(f.PID, f.ProcStart) {
			continue
		}
		p := Proc{
			PID:        f.PID,
			SessionID:  f.SessionID,
			Cwd:        f.Cwd,
			Name:       f.Name,
			Kind:       f.Kind,
			Status:     f.Status,
			WaitingFor: f.WaitingFor,
			ConfigDir:  configDir,
		}
		if f.StartedAt > 0 {
			p.StartedAt = time.UnixMilli(f.StartedAt)
		}
		if !boot.IsZero() {
			p.ProcStart = procStartTime(filepath.Join(procRoot, strconv.Itoa(f.PID)), boot)
		}
		if p.ProcStart.IsZero() {
			p.ProcStart = p.StartedAt
		}
		env := procEnv(f.PID, "HERDR_SOCKET_PATH", "HERDR_PANE_ID")
		p.HerdrSocket, p.HerdrPane = env["HERDR_SOCKET_PATH"], env["HERDR_PANE_ID"]
		out = append(out, p)
	}
	return out
}

// procAlive reports whether pid is still the process the registry recorded.
// Where /proc exists the recorded start time must match, because a pid is
// reused once its process exits and a stale file would otherwise attribute
// someone else's process to the session. Without /proc, signal 0 is the best
// available check.
func procAlive(pid int, procStart string) bool {
	if b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat")); err == nil {
		if procStart == "" {
			return true
		}
		return statField(b, 22) == procStart
	}
	if _, err := os.Stat(procRoot); err == nil {
		return false // /proc exists and the pid isn't in it
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// statField returns the n-th field (1-based) of a /proc/<pid>/stat line.
// Fields are counted from after the ")" that closes the command name, which
// may itself hold spaces.
func statField(b []byte, n int) string {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || n < 3 {
		return ""
	}
	f := strings.Fields(string(b[i+1:]))
	if n-3 >= len(f) {
		return ""
	}
	return f[n-3]
}

// procEnv reads the named variables from a process's initial environment.
// Linux only; returns an empty map elsewhere or when the process is someone
// else's.
func procEnv(pid int, keys ...string) map[string]string {
	out := make(map[string]string, len(keys))
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return out
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		for _, k := range keys {
			if v, ok := bytes.CutPrefix(kv, []byte(k+"=")); ok {
				out[k] = string(v)
			}
		}
	}
	return out
}

// ProcExited reports whether pid has exited, for callers that stop a session
// and must wait for it to finish writing its transcript before touching it.
func ProcExited(pid int) bool {
	if pid <= 0 {
		return true
	}
	if _, err := os.Stat(procRoot); err == nil {
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if err != nil {
			return true
		}
		// A zombie has exited; it only waits on its parent to reap it.
		return statField(b, 3) == "Z"
	}
	err := syscall.Kill(pid, 0)
	return err != nil && err != syscall.EPERM
}
