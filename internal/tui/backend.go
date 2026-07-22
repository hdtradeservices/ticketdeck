package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Backend is the pluggable launch/status layer. TicketDeck stays the
// Linear-aware front-end; a Backend owns how sessions are discovered and how a
// selected ticket is opened. Two impls: the built-in Claude daemon, and herdr.
type Backend interface {
	// Bin is the executable a LaunchSpec's Args are passed to.
	Bin() string
	// Statuses returns each ticket key's current session status (for badges).
	// cwd is the launch root, used to detect resumable on-disk sessions.
	Statuses(keys []string, cwd string) (map[string]session.Status, error)
	// Plan resolves the launch/attach command for a selected ticket.
	Plan(t session.Ticket, defaultCwd string) (session.LaunchSpec, error)
	// RunDetached executes a fire-and-return (non-foreground) launch, returning
	// combined output. Backends may run multi-step sequences (herdr new-tab).
	RunDetached(spec session.LaunchSpec) (string, error)
	// Sessions lists live sessions not owned by the deck (ticket sessions,
	// off-list tickets, and ad-hoc scratch sessions), for the "other sessions"
	// section.
	Sessions() ([]session.SessionRef, error)
	// ScratchSpec builds a launch for an ad-hoc Claude session unrelated to any
	// ticket (bare `claude`, its own tab).
	ScratchSpec(cwd string) session.LaunchSpec
	// FocusSpec builds a launch that switches to an existing session.
	FocusSpec(ref session.SessionRef) session.LaunchSpec
	// CloseSession stops/closes a session (fire-and-return). The Claude
	// transcript persists, so it stays resumable.
	CloseSession(ref session.SessionRef) (string, error)
	// Send types text into a running session named `name` and submits it, without
	// switching to it (fire-and-return). Used to fire a message at a session.
	Send(name, text string) (string, error)
	// Triage runs "/triage" against a ticket's session in the background —
	// starting the session (unfocused) if it isn't running — without leaving the
	// deck.
	Triage(t session.Ticket, cwd string) (string, error)
	// CloseByName closes the session named `name` if it is running; a missing
	// session is not an error. Used to tear down a ticket's session when it moves
	// to a terminal state.
	CloseByName(name string) (string, error)
}

// ClaudeBackend drives the built-in `claude` CLI directly (agents + resume).
type ClaudeBackend struct{}

func (ClaudeBackend) Bin() string { return session.Bin() }

func (ClaudeBackend) Statuses(keys []string, cwd string) (map[string]session.Status, error) {
	infos, err := session.List()
	if err != nil {
		return nil, err
	}
	res := session.Statuses(keys, infos)
	session.MarkResumable(res, cwd)
	return res, nil
}

func (ClaudeBackend) Plan(t session.Ticket, defaultCwd string) (session.LaunchSpec, error) {
	infos, err := session.List()
	if err != nil {
		return session.LaunchSpec{}, err
	}
	return session.Plan(t, infos, defaultCwd)
}

// RunDetached: claude launches are always foreground, so this is a generic
// fallback and not used in practice.
func (ClaudeBackend) RunDetached(spec session.LaunchSpec) (string, error) {
	c := exec.Command(session.Bin(), spec.Args...)
	c.Dir = spec.Cwd
	out, err := c.CombinedOutput()
	return string(out), err
}

func (ClaudeBackend) Sessions() ([]session.SessionRef, error) {
	infos, err := session.List()
	if err != nil {
		return nil, err
	}
	return session.Refs(infos), nil
}

// ScratchSpec: a bare foreground `claude` (no ticket identity).
func (ClaudeBackend) ScratchSpec(cwd string) session.LaunchSpec {
	return session.LaunchSpec{Cwd: cwd, Name: "scratch", Label: "scratch", Action: "scratch", Foreground: true}
}

// FocusSpec: resume the session in the foreground terminal.
func (ClaudeBackend) FocusSpec(ref session.SessionRef) session.LaunchSpec {
	return session.LaunchSpec{Args: []string{"--resume", ref.Ref}, Action: "focus", Foreground: true}
}

// CloseSession is a no-op for the built-in backend (no daemon to stop a session).
func (ClaudeBackend) CloseSession(ref session.SessionRef) (string, error) {
	return "", fmt.Errorf("closing sessions requires the herdr backend")
}

// Send is unsupported for the built-in backend (no multiplexer to inject into).
func (ClaudeBackend) Send(name, text string) (string, error) {
	return "", fmt.Errorf("sending to a session requires the herdr backend")
}

// CloseByName is a no-op for the built-in backend (no daemon to stop a session).
func (ClaudeBackend) CloseByName(name string) (string, error) { return "", nil }

// Triage (background) needs the herdr multiplexer.
func (ClaudeBackend) Triage(t session.Ticket, cwd string) (string, error) {
	return "", fmt.Errorf("background triage requires the herdr backend")
}

// HerdBackend drives herdr (agent multiplexer) — start/attach/list.
type HerdBackend struct{}

func (HerdBackend) Bin() string { return herd.Bin() }

func (HerdBackend) Statuses(keys []string, cwd string) (map[string]session.Status, error) {
	agents, err := herd.List()
	if err != nil {
		debugLog.Printf("herd.List error: %v", err)
		return nil, err
	}
	debugLog.Printf("herd agents (%d): %s", len(agents), agentSummary(agents))
	res := herd.Statuses(keys, agents)
	// herdr only lists running agents; a fresh server has none. Surface earlier
	// sessions still on disk as resumable so the badges aren't all blank.
	session.MarkResumable(res, cwd)
	return res, nil
}

func (HerdBackend) Plan(t session.Ticket, defaultCwd string) (session.LaunchSpec, error) {
	agents, err := herd.List()
	if err != nil {
		debugLog.Printf("herd.List (plan %s) error: %v", t.Key, err)
		return session.LaunchSpec{}, err
	}
	debugLog.Printf("plan %s vs herd agents (%d): %s", t.Key, len(agents), agentSummary(agents))
	return herd.Plan(t, agents, defaultCwd)
}

// RunDetached executes the herd launch — focus switches to a pane; start/scratch
// opens it in its own new tab (herd.Run).
func (HerdBackend) RunDetached(spec session.LaunchSpec) (string, error) {
	return herd.Run(spec)
}

func (HerdBackend) Sessions() ([]session.SessionRef, error) {
	agents, err := herd.List()
	if err != nil {
		return nil, err
	}
	return herd.Sessions(agents), nil
}

func (HerdBackend) ScratchSpec(cwd string) session.LaunchSpec {
	agents, _ := herd.List() // best-effort; naming just needs the current count
	return herd.ScratchSpec(herd.Sessions(agents), cwd)
}

func (HerdBackend) FocusSpec(ref session.SessionRef) session.LaunchSpec {
	return herd.FocusSpec(ref)
}

func (HerdBackend) CloseSession(ref session.SessionRef) (string, error) {
	return herd.Close(ref)
}

func (HerdBackend) Send(name, text string) (string, error) {
	agents, err := herd.List()
	if err != nil {
		return "", err
	}
	return herd.Send(agents, name, text)
}

func (HerdBackend) CloseByName(name string) (string, error) {
	agents, err := herd.List()
	if err != nil {
		return "", err
	}
	return herd.CloseByName(agents, name)
}

func (HerdBackend) Triage(t session.Ticket, cwd string) (string, error) {
	agents, err := herd.List()
	if err != nil {
		return "", err
	}
	return herd.Triage(agents, t, cwd)
}

func agentSummary(agents []herd.Agent) string {
	parts := make([]string, len(agents))
	for i, a := range agents {
		parts[i] = a.Name + "=" + a.AgentStatus
	}
	return strings.Join(parts, " ")
}
