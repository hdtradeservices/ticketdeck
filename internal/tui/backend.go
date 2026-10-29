package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Backend is the pluggable launch layer. TicketDeck stays the Linear-aware
// front-end; a Backend owns how a selected ticket is opened, stopped, prompted
// and moved. Two impls: the built-in Claude CLI, and herdr.
//
// Which sessions exist, and in what state, is not the backend's question any
// more: one deck runs every account, so that picture comes from every
// account's registry and every workspace at once (internal/fleet).
type Backend interface {
	// Bin is the executable a LaunchSpec's Args are passed to.
	Bin() string
	// Herdr reports whether sessions run in herdr panes the deck can stop,
	// prompt and move. The built-in backend hands the terminal over instead.
	Herdr() bool
	// Plan resolves the launch/attach command for a selected ticket.
	Plan(t session.Ticket, defaultCwd string) (session.LaunchSpec, error)
	// RunDetached executes a fire-and-return (non-foreground) launch, returning
	// combined output. Backends may run multi-step sequences (herdr new-tab).
	RunDetached(spec session.LaunchSpec) (string, error)
	// ScratchSpec builds a launch for an ad-hoc Claude session unrelated to any
	// ticket. t carries its own id, its account, and the name it was given.
	ScratchSpec(t session.Ticket, cwd string) session.LaunchSpec
	// FocusSpec builds a launch that switches to an existing session.
	FocusSpec(ref session.SessionRef) session.LaunchSpec
	// CloseSession stops/closes a session (fire-and-return). The Claude
	// transcript persists, so it stays resumable.
	CloseSession(ref session.SessionRef) (string, error)
	// Send types text into a running session named `name` and submits it, without
	// switching to it (fire-and-return). Used to fire a message at a session.
	Send(name, text string) (string, error)
	// Prompt is Send for a prompt that starts a turn: it returns once the turn
	// is seen to start, and fails when none does (see herd.Prompt).
	Prompt(name, text string) (string, error)
	// Triage runs "/triage" against a ticket's session in the background —
	// starting the session (unfocused) if it isn't running — without leaving the
	// deck.
	Triage(t session.Ticket, cwd string) (string, error)
	// CloseByName closes the session named `name` if it is running; a missing
	// session is not an error. Used to tear down a ticket's session when it moves
	// to a terminal state.
	CloseByName(name string) (string, error)
	// Move stops a session where it runs, copies its conversation to another
	// account when it changes accounts, and starts it again in this deck's
	// workspace (see Move).
	Move(mv Move) (string, error)
	// Rename gives a live session in this deck's workspace a new agent name and
	// tab label.
	Rename(ref session.SessionRef, name, label string) error
	// Keys presses keys in a running session, e.g. Esc to dismiss a dialog.
	Keys(name string, keys ...string) error
	// LimitMenu reports whether a running session shows Claude's own menu at
	// a usage limit, the one dialog the deck may dismiss for it.
	LimitMenu(name string) bool
	// WaitAtLimit answers that menu with its "Wait here, then continue
	// automatically" option, so Claude carries on by itself when the limit
	// resets. false, with nothing pressed, when the screen offers no such option.
	WaitAtLimit(name string) (bool, error)
}

// Move is one session changing account, workspace, or both.
type Move struct {
	// Name is the agent name the session is running under now, and Socket,
	// PaneID and PID where (Socket "" = this deck's workspace). A session that
	// isn't running has no pane: the move only copies and restarts it.
	Name   string
	Socket string
	PaneID string
	PID    int
	// From and To are the accounts. The same account on both sides is an
	// adoption: the session only changes workspace, and nothing is copied.
	From, To account.Account
	// Ticket is the session's identity: its key (or, for an ad-hoc session, the
	// agent name it should run under here), its id, and its system prompt.
	Ticket session.Ticket
	Cwd    string
	Focus  bool
	// Submit is sent once the session is ready again — how a session cut off
	// by a usage limit keeps going on its new account.
	Submit string
	// NewName is the agent name to run under here, when Name is taken.
	NewName string
}

// errRunsInATerminal refuses to move a session running outside herdr.
var errRunsInATerminal = errors.New("it runs in a terminal outside herdr — stop it there, then move it")

// ClaudeBackend drives the built-in `claude` CLI directly (agents + resume).
type ClaudeBackend struct{}

func (ClaudeBackend) Bin() string { return session.Bin() }
func (ClaudeBackend) Herdr() bool { return false }

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
	c.Env = append(os.Environ(), session.AccountEnv(spec.ConfigDir)...)
	out, err := c.CombinedOutput()
	return string(out), err
}

// ScratchSpec: a foreground `claude` with no ticket identity, under its own id.
func (ClaudeBackend) ScratchSpec(t session.Ticket, cwd string) session.LaunchSpec {
	var args []string
	if t.SessionID != "" {
		args = []string{"--session-id", t.SessionID}
	}
	if name := strings.TrimSpace(t.Key); name != "" {
		args = append(args, "--name", name)
	}
	return session.LaunchSpec{
		Args: args, Cwd: cwd, Name: "scratch", Label: "scratch", Action: "scratch", Foreground: true,
		Account: t.Account, ConfigDir: t.ConfigDir, SessionID: t.SessionID,
	}
}

// FocusSpec: resume the session in the foreground terminal.
func (ClaudeBackend) FocusSpec(ref session.SessionRef) session.LaunchSpec {
	id := firstNonEmpty(ref.SessionID, ref.Ref)
	return session.LaunchSpec{Args: []string{"--resume", id}, Action: "focus", Foreground: true, SessionID: id}
}

// CloseSession is a no-op for the built-in backend (no daemon to stop a session).
func (ClaudeBackend) CloseSession(ref session.SessionRef) (string, error) {
	return "", fmt.Errorf("closing sessions requires the herdr backend")
}

// Send is unsupported for the built-in backend (no multiplexer to inject into).
func (ClaudeBackend) Send(name, text string) (string, error) {
	return "", fmt.Errorf("sending to a session requires the herdr backend")
}

func (ClaudeBackend) Prompt(name, text string) (string, error) {
	return "", fmt.Errorf("sending to a session requires the herdr backend")
}

// CloseByName is a no-op for the built-in backend (no daemon to stop a session).
func (ClaudeBackend) CloseByName(name string) (string, error) { return "", nil }

// Triage (background) needs the herdr multiplexer.
func (ClaudeBackend) Triage(t session.Ticket, cwd string) (string, error) {
	return "", fmt.Errorf("background triage requires the herdr backend")
}

// Move without herdr can't stop or start anything; it copies the conversation
// so the next open resumes on the new account.
func (ClaudeBackend) Move(mv Move) (string, error) {
	if mv.PaneID != "" || mv.PID > 0 {
		return "", errors.New("moving a running session requires the herdr backend")
	}
	if mv.From.ConfigDir == mv.To.ConfigDir {
		return "", nil
	}
	err := account.CopySession(mv.From, mv.To, mv.Ticket.ID(), mv.Ticket.Key)
	if errors.Is(err, account.ErrNoSession) {
		return "nothing to copy yet", nil
	}
	return "copied", err
}

func (ClaudeBackend) Rename(session.SessionRef, string, string) error {
	return errors.New("renaming a running session requires the herdr backend")
}

func (ClaudeBackend) Keys(string, ...string) error {
	return errors.New("pressing keys in a session requires the herdr backend")
}

func (ClaudeBackend) LimitMenu(string) bool { return false }

func (ClaudeBackend) WaitAtLimit(string) (bool, error) { return false, nil }

// HerdBackend drives herdr (agent multiplexer) — start/attach/list.
type HerdBackend struct{}

func (HerdBackend) Bin() string { return herd.Bin() }
func (HerdBackend) Herdr() bool { return true }

func (HerdBackend) Plan(t session.Ticket, defaultCwd string) (session.LaunchSpec, error) {
	agents, err := herd.List()
	if err != nil {
		debugLog.Printf("herd.List (plan %s) error: %v", t.Key, err)
		return session.LaunchSpec{}, err
	}
	return herd.Plan(t, agents, defaultCwd)
}

// RunDetached executes the herd launch — focus switches to a pane; start/scratch
// opens it in its own new tab (herd.Run).
func (HerdBackend) RunDetached(spec session.LaunchSpec) (string, error) {
	return herd.Run(spec)
}

func (HerdBackend) ScratchSpec(t session.Ticket, cwd string) session.LaunchSpec {
	agents, _ := herd.List() // best-effort; naming just needs the live names
	return herd.ScratchSpec(herd.Sessions(agents), t, cwd)
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

func (HerdBackend) Prompt(name, text string) (string, error) {
	agents, err := herd.List()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if strings.EqualFold(a.Name, name) {
			return herd.Prompt(a.Socket, a.Name, text)
		}
	}
	return "", fmt.Errorf("no running session for %s", name)
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

// Move stops the session, copies its conversation when it changes account, and
// starts it again in a new tab of this workspace under the target account with
// --resume. What carries over is the conversation, not the process: anything
// it had running in the background stops with it.
func (HerdBackend) Move(mv Move) (string, error) {
	// A running session with no pane runs in a plain terminal: nothing here can
	// stop it, and starting it again would fork its conversation.
	if mv.PaneID == "" && mv.PID > 0 {
		return "", errRunsInATerminal
	}
	if mv.PaneID != "" {
		if err := herd.Stop(mv.Socket, mv.PaneID, mv.PID); err != nil {
			return "", err
		}
	}
	// A session nobody has typed into may have no transcript yet; it starts
	// fresh under its own id on the new account instead.
	if mv.From.ConfigDir != mv.To.ConfigDir {
		if err := account.CopySession(mv.From, mv.To, mv.Ticket.ID(), mv.Ticket.Key); err != nil && !errors.Is(err, account.ErrNoSession) {
			return "", err
		}
	}
	t := mv.Ticket
	t.Account, t.ConfigDir = mv.To.Name, mv.To.ConfigDir
	name := firstNonEmpty(mv.NewName, mv.Name, t.Key)
	inner := append([]string{"claude"}, session.ClaudeArgs(t)...)
	_, out, err := herd.Start(herd.Launch{
		Name: name, Label: session.TabLabel(t), Cwd: mv.Cwd, Env: session.AccountEnv(mv.To.ConfigDir),
		Inner: inner, Focus: mv.Focus, Submit: mv.Submit,
	})
	return out, err // herd.ErrBlockedAtStart: moved, but stopped at a startup prompt
}

func (HerdBackend) Keys(name string, keys ...string) error {
	return herd.SendKeys("", name, keys...)
}

func (HerdBackend) LimitMenu(name string) bool { return herd.LimitMenu("", name) }

func (HerdBackend) WaitAtLimit(name string) (bool, error) { return herd.WaitAtLimit("", name) }

func (HerdBackend) Rename(ref session.SessionRef, name, label string) error {
	tab := ""
	agents, err := herd.List()
	if err == nil {
		for _, a := range agents {
			if strings.EqualFold(a.Name, ref.Name) {
				tab = a.TabID
			}
		}
	}
	return herd.Rename("", ref.Name, name, tab, label)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
