// Package session bridges TicketDeck to the local `claude` CLI: it discovers
// existing Claude Code sessions, binds each ticket to a deterministic session
// id, and builds the launch/resume commands. It performs no model calls itself
// (BR-1) — discovery is a read-only `claude agents --json` and launching seeds
// context via a file, never an auto-submitted prompt (BR-3).
package session

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Status is a ticket's Claude Code session status, granular enough to badge.
type Status int

const (
	None       Status = iota // no session bound to this ticket
	Working                  // running and actively working
	NeedsInput               // running but waiting on the user (waiting/blocked)
	Idle                     // running, attached-idle (nothing in flight)
	Completed                // finished (state=done); resumable
	Stopped                  // exists but not running and not done; resumable
)

// running reports whether a session process is live (attach, don't fork).
func (s Status) running() bool {
	return s == Working || s == NeedsInput || s == Idle
}

// resumable reports whether a stopped session can be reopened from disk.
func (s Status) resumable() bool {
	return s == Completed || s == Stopped
}

// ticketdeckNamespace is a fixed UUID namespace so DeterministicID is stable
// across runs and machines for a given ticket key.
var ticketdeckNamespace = [16]byte{
	0x9f, 0x1c, 0x3a, 0x77, 0x2b, 0x4e, 0x5d, 0x61,
	0x8a, 0x0c, 0xd3, 0xe2, 0x11, 0x77, 0x42, 0xbb,
}

// DeterministicID returns the RFC-4122 v5 UUID TicketDeck uses as the Claude
// session id for a ticket. Same ticket key → same id, always.
func DeterministicID(ticketKey string) string {
	h := sha1.New()
	h.Write(ticketdeckNamespace[:])
	h.Write([]byte(strings.ToLower(ticketKey)))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC-4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// Info is one entry from `claude agents --json`.
type Info struct {
	PID       int    `json:"pid"`
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"` // "background" | "interactive"
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"` // "busy" | "idle"
	State     string `json:"state"`  // "working" | "done" | ...
}

// claudeBin is the CLI to invoke; overridable in tests.
var claudeBin = "claude"

// List returns all known Claude sessions (including completed) via
// `claude agents --json --all`. Read-only; no TTY required.
func List() ([]Info, error) {
	out, err := exec.Command(claudeBin, "agents", "--json", "--all").Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents: %w", err)
	}
	return parseInfos(out)
}

func parseInfos(b []byte) ([]Info, error) {
	var infos []Info
	if err := json.Unmarshal(b, &infos); err != nil {
		return nil, fmt.Errorf("decode agents json: %w", err)
	}
	return infos, nil
}

// statusOf classifies a single Info from the raw `claude agents` fields.
// Observed values: status ∈ {busy, idle, waiting}, state ∈ {working, blocked, done}.
func statusOf(i Info) Status {
	switch {
	case i.State == "done":
		return Completed
	case i.Status == "waiting" || i.State == "blocked":
		return NeedsInput
	case i.State == "working" || i.Status == "busy":
		return Working
	case i.PID > 0 && i.Status == "idle":
		return Idle
	case i.PID > 0:
		return Working
	default:
		return Stopped
	}
}

// Statuses maps each ticket key to its session Status, matching by the ticket's
// deterministic session id or by name (case-insensitive).
func Statuses(ticketKeys []string, infos []Info) map[string]Status {
	byID := map[string]Info{}
	byName := map[string]Info{}
	for _, in := range infos {
		if in.SessionID != "" {
			byID[in.SessionID] = in
		}
		if in.Name != "" {
			byName[strings.ToLower(in.Name)] = in
		}
	}
	res := make(map[string]Status, len(ticketKeys))
	for _, k := range ticketKeys {
		if in, ok := byID[DeterministicID(k)]; ok {
			res[k] = statusOf(in)
			continue
		}
		if in, ok := byName[strings.ToLower(k)]; ok {
			res[k] = statusOf(in)
			continue
		}
		res[k] = None
	}
	return res
}

// Refs maps live session Infos to SessionRefs (name, status, session id) for the
// deck's "other sessions" list.
func Refs(infos []Info) []SessionRef {
	out := make([]SessionRef, 0, len(infos))
	for _, in := range infos {
		name := in.Name
		if name == "" {
			name = in.SessionID
		}
		out = append(out, SessionRef{Name: name, Status: statusOf(in), Ref: in.SessionID})
	}
	return out
}

// findByBinding returns the Info bound to a ticket, if any.
func findByBinding(ticketKey string, infos []Info) (Info, bool) {
	id := DeterministicID(ticketKey)
	for _, in := range infos {
		if in.SessionID == id || strings.EqualFold(in.Name, ticketKey) {
			return in, true
		}
	}
	return Info{}, false
}

// SessionRef identifies a live backend session for the deck's "other sessions"
// list (agents/sessions not tied to a visible ticket). Ref is an opaque backend
// handle used to focus or close it (a herd pane id, or a claude session id).
type SessionRef struct {
	Name   string
	Status Status
	Ref    string
}

// Ticket carries the fields seeded into the context file. No description is
// fetched here (zero extra Linear calls); the URL lets Claude pull the full
// ticket on the user's first turn.
type Ticket struct {
	Key       string
	Title     string
	URL       string
	Branch    string
	Status    string
	PrioLabel string
	Team      string
}

// LaunchSpec is a fully-resolved command for TicketDeck to run.
type LaunchSpec struct {
	Args   []string // args to the backend binary
	Cwd    string   // working directory to run in
	Name   string   // agent name (herd): ticket key, or "scratch-N" for ad-hoc sessions
	Label  string   // tab label (herd)
	Action string   // "new" | "resume" | "agents-view" | "start" | "focus" | "scratch"
	// Foreground: hand the terminal to the command (interactive claude). When
	// false the command is fire-and-return (herdr dispatches a pane and exits),
	// run detached with its output captured.
	Foreground bool
}

// Bin returns the claude binary name (for building the exec.Cmd).
func Bin() string { return claudeBin }

// SystemPrompt is the ticket-identity system prompt appended to a launched
// session so the model unambiguously knows which ticket it is — without an
// auto-submitted prompt or a shared context dir (BR-3: session config, not a
// turn). First token spend is still the user's first message.
func SystemPrompt(t Ticket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on Linear ticket %s: %s.", t.Key, t.Title)
	if t.Status != "" {
		fmt.Fprintf(&b, " Status: %s.", t.Status)
	}
	if t.Team != "" {
		fmt.Fprintf(&b, " Team: %s.", t.Team)
	}
	if t.Branch != "" {
		fmt.Fprintf(&b, " Suggested branch: %s.", t.Branch)
	}
	if t.URL != "" {
		fmt.Fprintf(&b, " Full ticket (description, comments): %s — fetch it via the Linear MCP when you need detail.", t.URL)
	}
	fmt.Fprintf(&b, " When the user says \"this ticket\" they mean %s.", t.Key)
	return b.String()
}

// LaunchArgs are the claude args that seed a session's ticket identity.
func LaunchArgs(t Ticket) []string {
	return []string{"--append-system-prompt", SystemPrompt(t)}
}

// Plan resolves what should happen when a ticket is selected, given the current
// session list and a default working directory (the repos-root fallback).
func Plan(t Ticket, infos []Info, defaultCwd string) (LaunchSpec, error) {
	id := DeterministicID(t.Key)

	if in, ok := findByBinding(t.Key, infos); ok {
		st := statusOf(in)
		switch {
		case st.running():
			// One terminal per session (BR-4): don't fork a running session —
			// hand off to the interactive agent view to attach it there.
			return LaunchSpec{Args: []string{"agents"}, Cwd: firstNonEmpty(in.Cwd, defaultCwd), Action: "agents-view", Foreground: true}, nil
		case st.resumable():
			args := append([]string{"--resume", id}, LaunchArgs(t)...)
			return LaunchSpec{Args: args, Cwd: firstNonEmpty(in.Cwd, defaultCwd), Action: "resume", Foreground: true}, nil
		}
	}

	// Not in the daemon's list, but a transcript may still be on disk (the
	// daemon drops stopped sessions) — resume it rather than colliding on
	// --session-id.
	if SessionExists(id, defaultCwd) {
		args := append([]string{"--resume", id}, LaunchArgs(t)...)
		return LaunchSpec{Args: args, Cwd: defaultCwd, Action: "resume", Foreground: true}, nil
	}

	// No session at all → launch a fresh one bound to the deterministic id.
	args := append([]string{"--session-id", id, "--name", t.Key}, LaunchArgs(t)...)
	return LaunchSpec{Args: args, Cwd: defaultCwd, Action: "new", Foreground: true}, nil
}

// SessionExists reports whether a claude transcript for the given session id
// already exists on disk under the project dir for cwd. This is the reliable
// "resumable" signal — `claude agents` drops stopped sessions but the transcript
// persists at ~/.claude/projects/<slug>/<id>.jsonl.
func SessionExists(id, cwd string) bool {
	_, err := os.Stat(TranscriptPath(id, cwd))
	return err == nil
}

// MarkResumable promotes any ticket currently showing None to Stopped when a
// Claude transcript for it exists on disk. This surfaces resumable sessions the
// live listing can't see — e.g. a freshly started herdr server that has no
// running agents yet but whose earlier sessions are still on disk.
func MarkResumable(res map[string]Status, cwd string) {
	for k, st := range res {
		if st == None && SessionExists(DeterministicID(k), cwd) {
			res[k] = Stopped
		}
	}
}

// TranscriptPath is where claude stores a session's transcript for a given cwd.
// The project slug is the cwd with every non-alphanumeric rune replaced by '-'.
func TranscriptPath(id, cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".claude", "projects", projectSlug(cwd), id+".jsonl")
}

func projectSlug(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
