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
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// ticketKeyRE matches a Linear-style ticket key ("ZEN-3309", "DOPS-12"), used to
// pick out ticket sessions from herdr's agent list.
var ticketKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

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
	TabID       string `json:"tab_id"`
}

// agentListResp is the envelope herdr wraps agent list results in.
type agentListResp struct {
	Result struct {
		Agents []Agent `json:"agents"`
	} `json:"result"`
}

// List enumerates herdr agents (requires a running herdr server).
func List() ([]Agent, error) { return ListAt("") }

// ListAt enumerates the agents of the herdr server listening on socket, rather
// than the one this process's environment points at. That is how a deck sees
// ANOTHER account's sessions: `deck --account NAME` gives each subscription its
// own server socket (scripts/deck), so the ambient list only ever holds this
// deck's own work. An empty socket means the ambient environment.
//
// Only HERDR_SOCKET_PATH is overridden. The client socket stays this process's
// own — pointing it at the peer's would put two clients on one path.
func ListAt(socket string) ([]Agent, error) {
	cmd := exec.Command(herdrBin, "agent", "list")
	if socket != "" {
		cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+socket)
	}
	out, err := cmd.Output()
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
			// return; herdr owns the pane. Carry the label so Run can freshen the
			// tab title (panes opened before the title feature show a bare key).
			return session.LaunchSpec{
				Args:   []string{"agent", "focus", t.Key},
				Cwd:    firstNonEmpty(a.Cwd, defaultCwd),
				Name:   t.Key,
				Label:  session.TabLabel(t),
				Action: "focus",
			}, nil
		}
	}
	// No herdr pane → start one. Args here are the representative command shown in
	// --dry-launch; Run() performs the real (multi-step) new-tab launch.
	args := append([]string{"agent", "start", t.Key, "--cwd", defaultCwd, "--"}, claudeInner(t, defaultCwd)...)
	return session.LaunchSpec{Args: args, Cwd: defaultCwd, Name: t.Key, Label: session.TabLabel(t), Action: "start"}, nil
}

// ScratchSpec builds a launch for an ad-hoc Claude session not tied to any
// ticket: a bare `claude` (no session id, no ticket system prompt) opened in its
// own new tab. The name is the lowest scratch-N not currently live: the live
// scratches are not necessarily scratch-1..scratch-N, and herdr refuses a start
// whose name is still in use (agent_name_taken).
func ScratchSpec(agents []session.SessionRef, cwd string) session.LaunchSpec {
	taken := map[int]bool{}
	for _, a := range agents {
		if i, ok := scratchNum(a.Name); ok {
			taken[i] = true
		}
	}
	n := 1
	for taken[n] {
		n++
	}
	name := fmt.Sprintf("scratch-%d", n)
	args := []string{"agent", "start", name, "--cwd", cwd, "--", "claude"}
	return session.LaunchSpec{Args: args, Cwd: cwd, Name: name, Label: name, Action: "scratch"}
}

// scratchNumRE matches the ad-hoc session names ScratchSpec issues, so a
// user-named agent that merely starts with "scratch-" never reserves a number.
var scratchNumRE = regexp.MustCompile(`^scratch-([0-9]+)$`)

func scratchNum(name string) (int, bool) {
	m := scratchNumRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(name)))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
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
			// Closing a ticket's own tab makes herdr focus the neighbor tab; pull
			// focus back to the deck so a Done/Cancel from the list lands on the
			// ticket list rather than on some adjacent session.
			if err == nil && hasAgent(agents, "deck") {
				_ = exec.Command(herdrBin, "agent", "focus", "deck").Run()
			}
			return string(out), err
		}
	}
	return "", nil
}

// hasAgent reports whether an agent named `name` is in the list.
func hasAgent(agents []Agent, name string) bool {
	for _, a := range agents {
		if strings.EqualFold(a.Name, name) {
			return true
		}
	}
	return false
}

// CurrentTicketKey best-effort resolves the ticket key of the pane that invoked
// a command (e.g. a popup keybind bound to `ticketdeck describe`), so the user
// can view the description of the ticket they're working in without naming it.
// It tries, in order: the active pane herdr exports to custom commands
// (HERDR_ACTIVE_PANE_ID), the currently focused pane (`pane current`), and — if
// exactly one ticket-shaped session is running — that one. Returns "" when it
// can't decide.
func CurrentTicketKey() string {
	agents, err := List()
	if err != nil {
		return ""
	}
	byPane := map[string]string{}
	var tickets []string
	for _, a := range agents {
		if ticketKeyRE.MatchString(a.Name) {
			if a.PaneID != "" {
				byPane[a.PaneID] = a.Name
			}
			tickets = append(tickets, a.Name)
		}
	}
	if k, ok := byPane[os.Getenv("HERDR_ACTIVE_PANE_ID")]; ok {
		return k
	}
	if k, ok := byPane[currentPaneID()]; ok {
		return k
	}
	if len(tickets) == 1 {
		return tickets[0]
	}
	return ""
}

// currentPaneID returns the focused pane's id via `herdr pane current`, or "".
func currentPaneID() string {
	out, err := exec.Command(herdrBin, "pane", "current").Output()
	if err != nil {
		return ""
	}
	var r struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &r) == nil {
		return r.Result.Pane.PaneID
	}
	return ""
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

// Submit timing. Claude's TUI drops an Enter that arrives while it is still
// painting, and treats one coalesced into the same read as the pasted text as a
// literal newline — either way the command lands in the prompt and never runs.
// So sendAndEnter watches the pane instead of firing blind: wait for the typed
// text to show up in the input box, then press Enter until the box clears.
// Shortened by the tests.
var (
	typedTimeout   = 5 * time.Second        // wait for typed text to appear in the input box
	submitTimeout  = 3 * time.Second        // wait for the box to clear after each Enter
	promptTimeout  = 30 * time.Second       // wait for a fresh session to paint its input box
	submitAttempts = 4                      // Enter presses before giving up
	pollEvery      = 200 * time.Millisecond // pane re-read interval
)

// waitForPrompt waits for an empty Claude input box to appear. herdr calls a
// just-spawned agent "idle" within a second — long before Claude paints — so a
// fresh session needs this on top of `agent wait` or the typed text lands in a
// TUI that isn't reading stdin yet.
//
// It reports whether the pane is safe to type into. False means the box was
// readable and stayed occupied for the whole budget — the trust prompt draws its
// own "❯ 1. Yes…" line, and typing a command into that answers a dialog instead.
// A pane we could never read returns true: there is nothing to judge, and one
// blind Enter beats a `t` that silently stops working wherever herdr can't read.
func waitForPrompt(name string) bool {
	sawBox := false
	empty := waitFor(promptTimeout, func() bool {
		in, ok := promptInput(paneText(name))
		sawBox = sawBox || ok
		return ok && in == ""
	})
	return empty || !sawBox
}

// sendAndEnter writes literal text to a named agent then submits it with Enter,
// confirming against the pane that the text actually left the input box.
func sendAndEnter(name, paneID, text string) (string, error) {
	if out, err := exec.Command(herdrBin, "agent", "send", name, text).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("agent send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if paneID == "" {
		return "", fmt.Errorf("no pane id to submit %s", name)
	}
	// Don't press Enter until the text is on screen — an Enter that beats the
	// text is the race this whole dance exists to avoid. If the pane is
	// unreadable (no herdr read, a modal covering the box) this falls through and
	// the single blind Enter below is the old behaviour.
	typed := waitFor(typedTimeout, func() bool { return pending(paneText(name), text) })
	for range submitAttempts {
		out, err := exec.Command(herdrBin, "pane", "send-keys", paneID, "Enter").CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("submit Enter: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if !typed {
			return "sent + Enter (unverified)", nil
		}
		// Success needs the box readable AND empty of our text. "No box in this
		// frame" is not success: a swallowed Enter happens mid-paint, which is
		// exactly when a read comes back without the input box, so accepting that
		// as gone would report the failure this exists to catch as a submit.
		last := boxUnknown
		if waitFor(submitTimeout, func() bool {
			last = readBox(paneText(name), text)
			return last == boxSubmitted
		}) {
			return "sent + Enter", nil
		}
		if last == boxUnknown {
			// The pane stopped being readable, so there is nothing left to judge
			// against — say so rather than pressing Enter at a session we can no
			// longer see.
			return "sent + Enter (unverified)", nil
		}
	}
	return "", fmt.Errorf("%s still sitting unsent in %s's prompt after %d Enters", text, name, submitAttempts)
}

// waitFor polls cond every pollEvery until it holds or the budget runs out.
func waitFor(budget time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(budget); ; time.Sleep(pollEvery) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// paneText returns the visible text of an agent's pane, "" if unreadable.
func paneText(name string) string {
	out, err := exec.Command(herdrBin, "agent", "read", name, "--source", "visible", "--format", "text").Output()
	if err != nil {
		return ""
	}
	var r struct {
		Result struct {
			Read struct {
				Text string `json:"text"`
			} `json:"read"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &r) != nil {
		return ""
	}
	return r.Result.Read.Text
}

// promptMarkerRE matches a Claude prompt line ("❯ /triage", "> hello", "❯").
var promptMarkerRE = regexp.MustCompile(`^\s*[❯>]\s?(.*)$`)

// promptInput returns what is sitting in Claude's input box, and whether a box
// was found. The box is the LAST prompt line on screen — the ones above it are
// the transcript's echoes of already-submitted messages.
func promptInput(screen string) (string, bool) {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if m := promptMarkerRE.FindStringSubmatch(lines[i]); m != nil {
			return strings.TrimSpace(m[1]), true
		}
	}
	return "", false
}

// boxState is what one frame of the pane says about text we typed. The third
// state is the point: "we couldn't see a box" has to be distinguishable from
// "the box no longer holds it", or an unreadable frame reads as a submit.
type boxState int

const (
	boxUnknown   boxState = iota // no input box in this frame — mid-paint, or a modal over it
	boxPending                   // the text is still sitting in the box
	boxSubmitted                 // the box is readable and the text has left it
)

// readBox classifies one frame. A half-painted box (input is a prefix of text)
// and a box the user had already typed a draft into (text is somewhere inside
// input) both still count as holding the text.
func readBox(screen, text string) boxState {
	input, ok := promptInput(screen)
	switch {
	case !ok:
		return boxUnknown
	case input == "":
		return boxSubmitted
	case strings.Contains(input, text) || strings.HasPrefix(text, input):
		return boxPending
	}
	return boxSubmitted // the box holds something else, so ours went through
}

// pending reports whether text is still sitting unsent in the input box.
func pending(screen, text string) bool { return readBox(screen, text) == boxPending }

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
	if out, err := exec.Command(herdrBin, "pane", "move", paneID, "--new-tab", "--label", session.TabLabel(t)).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("pane move: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Wait until Claude is up and its prompt is painted, then submit /triage.
	_ = exec.Command(herdrBin, "agent", "wait", t.Key, "--status", "idle", "--timeout", "60000").Run()
	if !waitForPrompt(t.Key) {
		_ = exec.Command(herdrBin, "agent", "focus", "deck").Run()
		return "", fmt.Errorf("%s has a prompt of its own open (trust dialog?) — clear it in its tab, then triage again", t.Key)
	}
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
		// Best-effort: give the tab a nice title if we have one (upgrades tabs
		// opened before the title feature). Renaming the tab doesn't touch the
		// agent name, so name-based matching is unaffected.
		if err == nil && spec.Label != "" && spec.Label != spec.Name {
			if agents, e := List(); e == nil {
				for _, a := range agents {
					if strings.EqualFold(a.Name, spec.Name) && a.TabID != "" {
						_ = exec.Command(herdrBin, "tab", "rename", a.TabID, spec.Label).Run()
						break
					}
				}
			}
		}
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
