// Package herd is an alternative launch backend that drives herdr
// (https://herdr.dev) — an agent-aware terminal multiplexer — instead of
// managing Claude sessions directly. TicketDeck stays the Linear-aware layer;
// herdr owns multiplexing, detach/re-attach persistence, and agent-state.
//
// Commands used (verified against herdr 0.7.4):
//
//	herdr agent list                              — enumerate agents (JSON over socket)
//	herdr agent start <name> --cwd <dir> -- <cmd> — launch a named agent
//	herdr agent attach <name>                     — re-attach a running agent
//
// `herdr agent list` prints {"result":{"agents":[…]}} when a server is running;
// with no server it prints usage text, so List fails soft (badges stay empty).
package herd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

var herdrBin = "herdr" // overridable in tests

// Bin returns the herdr binary name.
func Bin() string { return herdrBin }

// Available reports whether herdr is installed and on PATH.
func Available() bool {
	_, err := exec.LookPath(herdrBin)
	return err == nil
}

// Agent is one entry from `herdr agent list`'s result.agents[]. Unknown fields
// are ignored.
type Agent struct {
	Name        string `json:"name"`
	Cwd         string `json:"cwd"`
	AgentStatus string `json:"agent_status"` // idle | working | blocked | unknown
	PaneID      string `json:"pane_id"`
}

// agentListResp is the envelope herdr wraps agent list results in.
type agentListResp struct {
	Result struct {
		Agents []Agent `json:"agents"`
	} `json:"result"`
}

// List enumerates herdr agents (requires a running herdr server).
func List() ([]Agent, error) {
	out, err := exec.Command(herdrBin, "agent", "list").Output()
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	return parseAgents(out)
}

func parseAgents(b []byte) ([]Agent, error) {
	var resp agentListResp
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("decode herdr agents json: %w", err)
	}
	return resp.Result.Agents, nil
}

// statusOf maps a herdr agent_status to TicketDeck's session.Status. herdr only
// lists live panes, so there is no completed/stopped state — a listed agent is
// running; "unknown" means running but state not yet detected.
func statusOf(a Agent) session.Status {
	switch strings.ToLower(a.AgentStatus) {
	case "working":
		return session.Working
	case "blocked":
		return session.NeedsInput
	default: // "idle", "unknown", or anything else: listed = running
		return session.Idle
	}
}

// Statuses maps each ticket key to its herdr agent status. TicketDeck names
// each agent with the ticket key, so matching is by name.
func Statuses(ticketKeys []string, agents []Agent) map[string]session.Status {
	byName := map[string]Agent{}
	for _, a := range agents {
		if a.Name != "" {
			byName[strings.ToLower(a.Name)] = a
		}
	}
	res := make(map[string]session.Status, len(ticketKeys))
	for _, k := range ticketKeys {
		if a, ok := byName[strings.ToLower(k)]; ok {
			res[k] = statusOf(a)
		} else {
			res[k] = session.None
		}
	}
	return res
}

// Plan resolves the herdr command for a selected ticket: attach if an agent for
// it already exists, else start a new one running Claude bound to the ticket's
// deterministic session id, with the ticket identity seeded via an appended
// system prompt (BR-3: no auto-submitted prompt).
func Plan(t session.Ticket, agents []Agent, defaultCwd string) (session.LaunchSpec, error) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, t.Key) {
			// Existing pane → just switch the workspace focus to it. Fire-and-
			// return; herdr owns the pane.
			return session.LaunchSpec{
				Args:   []string{"agent", "focus", t.Key},
				Cwd:    firstNonEmpty(a.Cwd, defaultCwd),
				Action: "focus",
			}, nil
		}
	}
	// No herdr pane → start one. Args here are the representative command shown in
	// --dry-launch; Run() performs the real (multi-step) new-tab launch.
	args := append([]string{"agent", "start", t.Key, "--cwd", defaultCwd, "--"}, claudeInner(t, defaultCwd)...)
	return session.LaunchSpec{Args: args, Cwd: defaultCwd, Name: t.Key, Label: t.Key, Action: "start"}, nil
}

// ScratchSpec builds a launch for an ad-hoc Claude session not tied to any
// ticket: a bare `claude` (no session id, no ticket system prompt) opened in its
// own new tab. The name is unique among existing scratch-N agents.
func ScratchSpec(agents []session.SessionRef, cwd string) session.LaunchSpec {
	n := 1
	for _, a := range agents {
		if strings.HasPrefix(a.Name, "scratch-") {
			n++
		}
	}
	name := fmt.Sprintf("scratch-%d", n)
	args := []string{"agent", "start", name, "--cwd", cwd, "--", "claude"}
	return session.LaunchSpec{Args: args, Cwd: cwd, Name: name, Label: name, Action: "scratch"}
}

// FocusSpec switches the workspace to an existing session's pane.
func FocusSpec(ref session.SessionRef) session.LaunchSpec {
	return session.LaunchSpec{Args: []string{"agent", "focus", ref.Name}, Name: ref.Name, Action: "focus"}
}

// Sessions lists every live herd agent except the deck, as SessionRefs, so the
// deck can show non-ticket / off-list sessions and manage them.
func Sessions(agents []Agent) []session.SessionRef {
	out := make([]session.SessionRef, 0, len(agents))
	for _, a := range agents {
		if a.Name == "" || a.Name == "deck" {
			continue
		}
		out = append(out, session.SessionRef{Name: a.Name, Status: statusOf(a), Ref: a.PaneID})
	}
	return out
}

// Close stops a session by closing its herd pane (Ref = pane id). The Claude
// transcript persists on disk, so the session remains resumable.
func Close(ref session.SessionRef) (string, error) {
	if ref.Ref == "" {
		return "", fmt.Errorf("no pane id for %s", ref.Name)
	}
	out, err := exec.Command(herdrBin, "pane", "close", ref.Ref).CombinedOutput()
	return string(out), err
}

// CloseByName closes the pane of the agent named `name`, if one is running.
// Returns ("", nil) when there is no such agent — nothing to close is not an
// error (e.g. moving a ticket to Done when its session was never opened).
func CloseByName(agents []Agent, name string) (string, error) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, name) {
			if a.PaneID == "" {
				return "", nil
			}
			out, err := exec.Command(herdrBin, "pane", "close", a.PaneID).CombinedOutput()
			return string(out), err
		}
	}
	return "", nil
}

// Send types text into the named session and submits it, without switching to
// the pane. It writes the literal text with `agent send` (robust, addressed by
// name), then presses Enter with `pane send-keys` — this pair works on Claude's
// TUI panes, where `pane run` can fail opaquely. Used to fire a message (e.g.
// "/triage") at a ticket's running Claude session from the deck.
func Send(agents []Agent, name, text string) (string, error) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, name) {
			return sendAndEnter(a.Name, a.PaneID, text)
		}
	}
	return "", fmt.Errorf("no running session for %s", name)
}

// sendAndEnter writes literal text to a named agent then submits it with Enter.
func sendAndEnter(name, paneID, text string) (string, error) {
	if out, err := exec.Command(herdrBin, "agent", "send", name, text).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("agent send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if paneID == "" {
		return "", fmt.Errorf("no pane id to submit %s", name)
	}
	if out, err := exec.Command(herdrBin, "pane", "send-keys", paneID, "Enter").CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("submit Enter: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return "sent + Enter", nil
}

// Triage runs "/triage" against a ticket's session in the background, without
// leaving the deck. If the session is already running it just submits /triage;
// otherwise it starts the session in its own new (unfocused) tab, waits for
// Claude to be ready, submits /triage, and refocuses the deck — so it works
// while you keep triaging other tickets.
func Triage(agents []Agent, t session.Ticket, cwd string) (string, error) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, t.Key) {
			return sendAndEnter(a.Name, a.PaneID, "/triage")
		}
	}
	// Not running → start it in the background.
	inner := claudeInner(t, cwd)
	startArgs := append([]string{"agent", "start", t.Key, "--cwd", cwd, "--no-focus", "--"}, inner...)
	startOut, err := exec.Command(herdrBin, startArgs...).CombinedOutput()
	if err != nil {
		return string(startOut), fmt.Errorf("agent start: %w: %s", err, strings.TrimSpace(string(startOut)))
	}
	paneID := parsePaneID(startOut)
	if paneID == "" {
		return string(startOut), fmt.Errorf("could not parse pane id from agent start")
	}
	// Own tab, but do not steal focus from the deck.
	if out, err := exec.Command(herdrBin, "pane", "move", paneID, "--new-tab", "--label", t.Key).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("pane move: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Wait until Claude is up and idle (ready for input), then submit /triage.
	_ = exec.Command(herdrBin, "agent", "wait", t.Key, "--status", "idle", "--timeout", "60000").Run()
	out, err := sendAndEnter(t.Key, paneID, "/triage")
	// Make sure focus is back on the deck regardless of what the new tab did.
	_ = exec.Command(herdrBin, "agent", "focus", "deck").Run()
	if err != nil {
		return out, err
	}
	return "started + /triage (background)", nil
}

// claudeInner is the claude argv for a ticket: resume if a transcript already
// exists on disk (the daemon drops stopped sessions, so --session-id would
// collide), else create bound to the deterministic id. Identity is seeded via an
// appended system prompt (BR-3: no auto-submitted prompt).
func claudeInner(t session.Ticket, cwd string) []string {
	id := session.DeterministicID(t.Key)
	if session.SessionExists(id, cwd) {
		return append([]string{"claude", "--resume", id}, session.LaunchArgs(t)...)
	}
	return append([]string{"claude", "--session-id", id, "--name", t.Key}, session.LaunchArgs(t)...)
}

// Run executes a herd launch spec, returning combined output for the log.
//   - focus: switch to an existing pane.
//   - start/scratch: open in its OWN new tab (so panes don't accumulate as
//     splits) — start the agent unfocused, then move its pane to a new focused
//     tab, leaving the deck's tab clean. The inner command (claude …) is taken
//     verbatim from spec.Args after the "--", so Run is ticket-agnostic and
//     serves both ticket sessions and ad-hoc scratch sessions.
func Run(spec session.LaunchSpec) (string, error) {
	if len(spec.Args) >= 2 && spec.Args[0] == "agent" && spec.Args[1] == "focus" {
		out, err := exec.Command(herdrBin, spec.Args...).CombinedOutput()
		return string(out), err
	}

	inner := innerArgv(spec.Args)
	if len(inner) == 0 {
		return "", fmt.Errorf("no inner command in launch spec for %s", spec.Name)
	}
	startArgs := append([]string{"agent", "start", spec.Name, "--cwd", spec.Cwd, "--no-focus", "--"}, inner...)
	startOut, err := exec.Command(herdrBin, startArgs...).CombinedOutput()
	if err != nil {
		return string(startOut), fmt.Errorf("agent start: %w", err)
	}
	paneID := parsePaneID(startOut)
	if paneID == "" {
		return string(startOut), fmt.Errorf("could not parse pane id from agent start output")
	}
	moveOut, err := exec.Command(herdrBin, "pane", "move", paneID, "--new-tab", "--focus", "--label", spec.Label).CombinedOutput()
	return "start " + paneID + " → new tab: " + string(moveOut), err
}

// innerArgv returns the command after the first "--" in a herd spec's args.
func innerArgv(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func parsePaneID(b []byte) string {
	var r struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &r) == nil {
		return r.Result.Agent.PaneID
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
