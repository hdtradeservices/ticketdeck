// Package herd is an alternative launch backend that drives herdr
// (https://herdr.dev) — an agent-aware terminal multiplexer — instead of
// managing Claude sessions directly. TicketDeck stays the Linear-aware layer;
// herdr owns multiplexing, detach/re-attach persistence, and agent-state.
//
// Commands used (verified against herdr 0.9.1):
//
//	herdr agent list                      — enumerate agents (JSON over socket)
//	herdr agent get <name>                — one agent, incl. interactive_ready
//	herdr tab create --cwd <dir>          — open the pane a session runs in
//	herdr agent start <name> --kind <k> --pane <p> -- <cmd>
//	                                      — run the agent and register its name
//	herdr agent prompt <name> <text>      — type a message and submit it
//	herdr agent focus <name>              — switch the workspace to a session
//
// herdr renames these between minor releases and the deck only finds out at the
// point of use, in front of whoever pressed the key, so keep every one of them
// pinned by a test: the unit tests stub `herdr` and assert the argv, and
// TestLiveSubmit drives the submit path against a real server under
// TICKETDECK_HERDR_LIVE=1.
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
	Name string `json:"name"`
	// Label is herdr's reported-agent field. herdr 0.8.0 dropped `name` from
	// `agent list` and reports the label under `agent` instead, so a build that
	// reads only Name sees every agent as unnamed there: Sessions() skips them
	// all, focusDeck never matches, and CloseByName closes nothing. parseAgents
	// folds this into Name so the rest of the package keeps one field.
	Label       string `json:"agent"`
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

// agentRef renders a session name the way herdr 0.9 requires it. Agent names are
// lowercase there ([a-z0-9_-], 1-32 chars), so a ticket session registers as
// "zen-5138" while the deck shows ZEN-5138 everywhere else. Every herdr call that
// takes a name goes through this; parseAgents folds them back on the way in.
func agentRef(name string) string { return strings.ToLower(name) }

// lowerTicketKeyRE is ticketKeyRE's lowercase twin, matching the names herdr 0.9
// stores for ticket sessions.
var lowerTicketKeyRE = regexp.MustCompile(`^[a-z][a-z0-9]*-[0-9]+$`)

// foldAgentName restores a ticket key's canonical uppercase. Scratch names are
// left alone on purpose: uppercasing "scratch-1" would make ticketKeyRE classify
// an ad-hoc session as a ticket.
func foldAgentName(n string) string {
	if _, isScratch := scratchNum(n); isScratch {
		return n
	}
	if lowerTicketKeyRE.MatchString(n) {
		return strings.ToUpper(n)
	}
	return n
}

func parseAgents(b []byte) ([]Agent, error) {
	var resp agentListResp
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("decode herdr agents json: %w", err)
	}
	for i := range resp.Result.Agents {
		if resp.Result.Agents[i].Name == "" {
			resp.Result.Agents[i].Name = resp.Result.Agents[i].Label
		}
		resp.Result.Agents[i].Name = foldAgentName(resp.Result.Agents[i].Name)
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
				Args:   []string{"agent", "focus", agentRef(t.Key)},
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
	return session.LaunchSpec{Args: []string{"agent", "focus", agentRef(ref.Name)}, Name: ref.Name, Action: "focus"}
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
			if err == nil {
				focusDeck(agents)
			}
			return string(out), err
		}
	}
	return "", nil
}

// hasAgent reports whether an agent named `name` is in the list.
// focusDeck pulls focus back to the deck's pane. herdr 0.8.0 stopped resolving a
// reported agent label as a focus target — `agent focus deck` answers "agent target
// deck not found" there — so the deck is focused by its pane id, which both the old
// and the new targeting accept. A deck that is not in the list (someone closed it)
// is not an error: there is nowhere to put focus, and that is the whole operation.
func focusDeck(agents []Agent) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, "deck") && a.PaneID != "" {
			_ = exec.Command(herdrBin, "agent", "focus", a.PaneID).Run()
			return
		}
	}
}

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
// the pane. Used to fire a message (e.g. "/triage") at a ticket's running Claude
// session from the deck.
func Send(agents []Agent, name, text string) (string, error) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, name) {
			return submit(a.Name, text)
		}
	}
	return "", fmt.Errorf("no running session for %s", name)
}

// Submitting. `agent prompt` is the trust-dialog guard as well as the typist:
// herdr rejects a prompt at a blocked agent with agent_blocked before sending
// anything, so a stray "/triage" cannot answer a dialog by pressing Enter at it.
// Anything that submits by hand instead has to re-earn that.
var (
	readyTimeout = 60 * time.Second       // budget for a fresh session to paint its input box
	pollEvery    = 200 * time.Millisecond // agent re-read interval
)

// submit types text into the named agent and submits it, without switching to
// its pane.
//
// --wait is deliberately omitted. It requires an observed working or blocked
// state within 5s, and a slash command can finish inside that window: /cost at
// an idle session returned agent_prompt_stalled for a prompt that had in fact
// landed and run (2026-09-21).
func submit(name, text string) (string, error) {
	out, err := exec.Command(herdrBin, "agent", "prompt", agentRef(name), text).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("agent prompt %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return "sent", nil
}

// waitInteractive waits for a freshly started agent to be ready for input, and
// reports whether it got there. herdr calls a just-spawned agent "idle" within a
// second — long before Claude paints — so `agent wait` alone lands text in a TUI
// that is not reading stdin yet. interactive_ready is the flag that tracks the
// input box itself.
func waitInteractive(name string) bool {
	for deadline := time.Now().Add(readyTimeout); ; time.Sleep(pollEvery) {
		if interactiveReady(name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// interactiveReady reads one agent's interactive_ready flag. An unreadable agent
// is reported as not ready: waitInteractive's caller retries, and its timeout is
// the backstop.
func interactiveReady(name string) bool {
	out, err := exec.Command(herdrBin, "agent", "get", agentRef(name)).Output()
	if err != nil {
		return false
	}
	var r struct {
		Result struct {
			Agent struct {
				Ready bool `json:"interactive_ready"`
			} `json:"agent"`
		} `json:"result"`
	}
	return json.Unmarshal(out, &r) == nil && r.Result.Agent.Ready
}

// Triage runs "/triage" against a ticket's session in the background, without
// leaving the deck. If the session is already running it just submits /triage;
// otherwise it starts the session in its own new (unfocused) tab, waits for
// Claude to be ready, submits /triage, and refocuses the deck — so it works
// while you keep triaging other tickets.
func Triage(agents []Agent, t session.Ticket, cwd string) (string, error) {
	for _, a := range agents {
		if strings.EqualFold(a.Name, t.Key) {
			return submit(a.Name, "/triage")
		}
	}
	// Not running → start it in the background.
	inner := claudeInner(t, cwd)
	// Own tab from the start, and never steal focus from the deck.
	_, startOut, err := startAgentPane(t.Key, cwd, session.TabLabel(t), inner, false)
	if err != nil {
		return startOut, err
	}
	// Wait until Claude is up and its prompt is painted, then submit /triage.
	if !waitInteractive(t.Key) {
		focusDeck(agents)
		return "", fmt.Errorf("%s did not finish starting within %s — open its tab to see what it is waiting on, then triage again", t.Key, readyTimeout)
	}
	out, err := submit(t.Key, "/triage")
	// Make sure focus is back on the deck regardless of what the new tab did.
	focusDeck(agents)
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
	paneID, runOut, err := startAgentPane(spec.Name, spec.Cwd, spec.Label, inner, true)
	if err != nil {
		return runOut, err
	}
	return "start " + paneID + " → new tab: " + runOut, nil
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

// parseRootPaneID reads the pane id out of `tab create`/`workspace create` output.
func parseRootPaneID(b []byte) string {
	var r struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &r) == nil {
		return r.Result.RootPane.PaneID
	}
	return ""
}

// startAgentPane opens a new tab at cwd and runs inner in it, returning the pane id.
//
// Two calls, because `agent start` attaches an agent KIND to a pane that already
// exists — it cannot make one, and it has no --cwd. The second call is not
// plumbing: it is what registers the name, and the name is how the deck matches a
// live pane back to its ticket. Run the same argv through `pane run` instead and
// the pane is nameless, so the ticket reads as "resumable" while its session is
// in fact running.
func startAgentPane(name, cwd, label string, inner []string, focus bool) (string, string, error) {
	args := []string{"tab", "create", "--cwd", cwd}
	if label != "" {
		args = append(args, "--label", label)
	}
	if focus {
		args = append(args, "--focus")
	} else {
		args = append(args, "--no-focus")
	}
	out, err := exec.Command(herdrBin, args...).CombinedOutput()
	if err != nil {
		return "", string(out), fmt.Errorf("tab create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	pane := parseRootPaneID(out)
	if pane == "" {
		return "", string(out), fmt.Errorf("could not parse pane id from tab create output")
	}
	startArgs := []string{"agent", "start", agentRef(name), "--kind", inner[0], "--pane", pane, "--timeout", "120000"}
	if len(inner) > 1 {
		startArgs = append(append(startArgs, "--"), inner[1:]...)
	}
	runOut, err := exec.Command(herdrBin, startArgs...).CombinedOutput()
	if err != nil {
		return pane, string(runOut), fmt.Errorf("agent start: %w: %s", err, strings.TrimSpace(string(runOut)))
	}
	return pane, string(runOut), nil
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
