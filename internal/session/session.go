// Package session bridges TicketDeck to the local `claude` CLI: it discovers
// existing Claude Code sessions, binds each ticket to a deterministic session
// id, and builds the launch/resume commands. It performs no model calls itself
// (BR-1) — discovery is a read-only `claude agents --json` and launching seeds
// context via a file, never an auto-submitted prompt (BR-3).
package session

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	Background               // running, lead idle, a shell command or monitor still going
	Unread                   // running, finished a turn nobody has looked at yet
	Limited                  // running, stopped because its account hit a usage limit
	Errored                  // running, stopped on an API error (login lapsed, API unreachable)
)

// Running reports whether a session process is live (attach, don't fork).
func (s Status) Running() bool {
	switch s {
	case Working, NeedsInput, Idle, Background, Unread, Limited, Errored:
		return true
	}
	return false
}

// Waiting reports whether a live session is waiting on nobody but the user:
// its turn is over and nothing it started is still running.
func (s Status) Waiting() bool {
	switch s {
	case Idle, Unread, Limited, Errored:
		return true
	}
	return false
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

// NewID returns a random (v4) session id, for an ad-hoc session: one the deck
// names itself so it can find the session's transcript and account again.
func NewID() string {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		// crypto/rand does not fail on any supported platform; fall back to the
		// clock rather than handing claude an invalid id.
		binary.BigEndian.PutUint64(u[:8], uint64(time.Now().UnixNano()))
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC-4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// Info is one entry from `claude agents --json`.
type Info struct {
	PID        int    `json:"pid"`
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"` // "background" | "interactive"
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	Status     string `json:"status"` // "busy" | "idle" | "waiting"
	State      string `json:"state"`  // "working" | "done" | ...
	WaitingFor string `json:"waitingFor"`
	StartedAt  int64  `json:"startedAt"` // epoch milliseconds
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
	// Socket is the herdr server the session runs in: "" for this deck's own
	// workspace, else another server's socket (another deck's workspace).
	Socket string
	// Account is the subscription running it, when known.
	Account string
	// Title is what to call it on screen when it isn't Name: the name the user
	// gave it, or the one Claude generated from the conversation.
	Title string
	// SessionID is the Claude session it runs, when known.
	SessionID string
	// Cwd is where it was started.
	Cwd string
}

// Live reports whether the session is running, as opposed to one the deck
// remembers that is only resumable.
func (r SessionRef) Live() bool { return r.Status.Running() }

// Ticket carries the fields seeded into a launched session's identity. No
// description is fetched here (zero extra Linear calls); the URL lets Claude
// pull the full record on the user's first turn.
//
// It covers both units of work the deck opens a session for: a Linear issue and
// a Linear project. Project is what tells them apart — everything downstream
// (session id, herdr agent name, resume) is keyed on Key alone and doesn't care
// which it is.
type Ticket struct {
	Key       string
	Title     string
	URL       string
	Branch    string
	Status    string
	PrioLabel string
	Team      string
	// Project marks this unit as a Linear project rather than an issue, which
	// changes only how the identity prompt reads.
	Project bool
	// Context is extra identity a project needs and an issue doesn't: its
	// summary, and the keys of my open tickets in it, so the session starts
	// knowing its own scope instead of having to go find it.
	Context string
	// Scratch marks an ad-hoc session: no ticket identity is seeded, and Key is
	// only the name it is filed under.
	Scratch bool
	// SessionID overrides the id derived from Key. An ad-hoc session carries a
	// random one, and a session adopted from another deck keeps whatever id it
	// was started with.
	SessionID string
	// Account and ConfigDir name the Claude subscription to run it under. Empty
	// means the one this process runs as.
	Account   string
	ConfigDir string
	// Resume opens the session's existing conversation even though its account
	// doesn't hold the transcript yet: the launch copies it there first.
	Resume bool
	// Model pins the model the session runs, "" for its account's default. A
	// move sets it, so a session keeps its model on an account whose default
	// differs.
	Model string
}

// ID is the Claude session id the unit runs as.
func (t Ticket) ID() string {
	if t.SessionID != "" {
		return t.SessionID
	}
	return DeterministicID(t.Key)
}

// Dir is the config dir of the account the unit runs under.
func (t Ticket) Dir() string { return firstNonEmpty(t.ConfigDir, ConfigDir()) }

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
	// Account and ConfigDir name the subscription the session runs under; the
	// backend gives the process that account's environment (see AccountEnv).
	// Empty means this process's own.
	Account   string
	ConfigDir string
	// SessionID is the Claude session the launch opens.
	SessionID string
	// NoFocus starts the session without switching to it.
	NoFocus bool
	// Submit is a prompt to send once the session is ready for input.
	Submit string
	// CopyFrom is the config dir holding the session's conversation, when it
	// isn't ConfigDir's: the launch copies it over before resuming.
	CopyFrom string
}

// AccountEnv is the environment that runs claude as the account at configDir.
//
// For this process's own account it is nothing at all: the session inherits
// exactly what this process has. That is not the same as naming the directory
// — Claude Code keeps its settings, trusted folders included, in
// ~/.claude.json while CLAUDE_CONFIG_DIR is unset but in
// $CLAUDE_CONFIG_DIR/.claude.json once it is set, even to ~/.claude, so
// setting it would land the primary account's sessions on a second, emptier
// config that asks to trust every folder again.
//
// CLAUDE_CODE_OAUTH_TOKEN is cleared for any other account: set, it belongs to
// whichever account exported it and silently overrides the config dir this
// sets, so the session would bill the wrong subscription.
func AccountEnv(configDir string) []string {
	if configDir == "" || filepath.Clean(configDir) == filepath.Clean(ConfigDir()) {
		return nil
	}
	env := []string{"CLAUDE_CONFIG_DIR=" + configDir}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN=")
	}
	return env
}

// Bin returns the claude binary name (for building the exec.Cmd).
func Bin() string { return claudeBin }

// SystemPrompt is the ticket-identity system prompt appended to a launched
// session so the model unambiguously knows which ticket it is — without an
// auto-submitted prompt or a shared context dir (BR-3: session config, not a
// turn). First token spend is still the user's first message.
func SystemPrompt(t Ticket) string {
	if t.Project {
		return projectSystemPrompt(t)
	}
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

// projectSystemPrompt is the identity for a project session. It names the
// project by title, never by Key: the key is a synthetic "proj-<slug>" the user
// never sees, so telling the model that is its subject would be actively
// misleading.
func projectSystemPrompt(t Ticket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on the Linear project %q.", t.Title)
	if t.Status != "" {
		fmt.Fprintf(&b, " Status: %s.", t.Status)
	}
	if t.PrioLabel != "" {
		fmt.Fprintf(&b, " Priority: %s.", t.PrioLabel)
	}
	if t.Team != "" {
		fmt.Fprintf(&b, " Team: %s.", t.Team)
	}
	if t.Context != "" {
		fmt.Fprintf(&b, " %s", t.Context)
	}
	if t.URL != "" {
		fmt.Fprintf(&b, " Project page (description, milestones, issues): %s — fetch it via the Linear MCP when you need detail.", t.URL)
	}
	fmt.Fprintf(&b, " When the user says \"this project\" they mean %q. A ticket named without a project means one of this project's tickets.", t.Title)
	return b.String()
}

// LaunchArgs are the claude args that seed a session's ticket identity. An
// ad-hoc session has none to seed.
func LaunchArgs(t Ticket) []string {
	if t.Scratch {
		return nil
	}
	return []string{"--append-system-prompt", SystemPrompt(t)}
}

// ClaudeArgs is the claude argv (after the binary) that opens t: resume when its
// account already holds a transcript for it, else start fresh bound to its id.
func ClaudeArgs(t Ticket) []string {
	id := t.ID()
	var args []string
	if t.Resume || FindTranscript(t.Dir(), id) != "" {
		args = append([]string{"--resume", id}, LaunchArgs(t)...)
	} else {
		name := t.Key
		if t.Scratch && t.Title != "" {
			name = t.Title // an ad-hoc session's key is its agent name; its title is what the user called it
		}
		args = append([]string{"--session-id", id, "--name", name}, LaunchArgs(t)...)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	return args
}

// TabLabel is the herdr tab title for a ticket: the key plus a short slice of
// the title, so a tab reads "ZEN-3309  fix shipworks sweep" instead of a bare
// id. The agent name stays the bare key (that's the identity everything matches
// on) — only the visible tab label carries the title.
func TabLabel(t Ticket) string {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		return t.Key
	}
	// A project's key is a synthetic "proj-<slug>" that means nothing to a
	// reader, so its tab is titled by name alone, as is an ad-hoc session's.
	if t.Project || t.Scratch {
		return truncRunes(title, 32)
	}
	return truncRunes(t.Key+"  "+title, 32)
}

// truncRunes shortens s to at most max runes, appending an ellipsis when cut.
func truncRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

// Plan resolves what should happen when a ticket is selected, given the current
// session list and a default working directory (the repos-root fallback).
func Plan(t Ticket, infos []Info, defaultCwd string) (LaunchSpec, error) {
	id := t.ID()
	acct := func(s LaunchSpec) LaunchSpec {
		s.Account, s.ConfigDir, s.SessionID = t.Account, t.ConfigDir, id
		return s
	}

	if in, ok := findByBinding(t.Key, infos); ok {
		st := statusOf(in)
		switch {
		case st.Running():
			// One terminal per session (BR-4): don't fork a running session —
			// hand off to the interactive agent view to attach it there.
			return acct(LaunchSpec{Args: []string{"agents"}, Cwd: firstNonEmpty(in.Cwd, defaultCwd), Action: "agents-view", Foreground: true}), nil
		case st.resumable():
			args := append([]string{"--resume", id}, LaunchArgs(t)...)
			return acct(LaunchSpec{Args: args, Cwd: firstNonEmpty(in.Cwd, defaultCwd), Action: "resume", Foreground: true}), nil
		}
	}

	// Not in the daemon's list, but a transcript may still be on disk (the
	// daemon drops stopped sessions) — resume it rather than colliding on
	// --session-id.
	if t.Resume || FindTranscript(t.Dir(), id) != "" {
		args := append([]string{"--resume", id}, LaunchArgs(t)...)
		return acct(LaunchSpec{Args: args, Cwd: defaultCwd, Action: "resume", Foreground: true}), nil
	}

	// No session at all → launch a fresh one bound to its id.
	args := append([]string{"--session-id", id, "--name", t.Key}, LaunchArgs(t)...)
	return acct(LaunchSpec{Args: args, Cwd: defaultCwd, Action: "new", Foreground: true}), nil
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

// ConfigDir returns Claude Code's config directory — the first entry of
// $CLAUDE_CONFIG_DIR if set (this is how a per-subscription "account" is
// selected: matt = ~/.claude, support = ~/.claude-support, …), else ~/.claude.
// Transcripts and credentials live under it, so TicketDeck must resolve it the
// same way Claude Code does to stay correct across accounts.
func ConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		// CLAUDE_CONFIG_DIR may be a path-list; the first is where state lives.
		if i := strings.IndexByte(d, byte(os.PathListSeparator)); i >= 0 {
			d = d[:i]
		}
		if d != "" {
			return d
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// TranscriptPath is where claude stores a session's transcript for a given cwd,
// under this process's own account.
func TranscriptPath(id, cwd string) string { return TranscriptPathIn(ConfigDir(), id, cwd) }

// TranscriptPathIn is TranscriptPath under the account at configDir. The project
// slug is the cwd with every non-alphanumeric rune replaced by '-'.
func TranscriptPathIn(configDir, id, cwd string) string {
	return filepath.Join(configDir, "projects", ProjectSlug(cwd), id+".jsonl")
}

// ProjectSlug is the directory name Claude Code files a cwd's sessions under.
func ProjectSlug(p string) string {
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
