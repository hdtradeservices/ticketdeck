package tui

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// debugLog is a no-op until SetLog points it at a writer (see --log). Bubble Tea
// owns stdout, so all diagnostics go to a file.
var debugLog = log.New(io.Discard, "", 0)

// SetLog directs TicketDeck's debug log to w.
func SetLog(w io.Writer) { debugLog = log.New(w, "", log.LstdFlags|log.Lmicroseconds) }

// Fetcher is what the model needs from the Linear layer. Kept as an interface
// so --demo mode can supply canned data without an API key.
type Fetcher interface {
	FetchAssignedOpen(ctx context.Context) ([]linear.Issue, error)
}

// statusWriter is the optional write capability: a Fetcher that also transitions
// an issue's workflow state. The live Linear client implements it; --demo does
// not, so status changes are simply unavailable there.
type statusWriter interface {
	MoveState(ctx context.Context, issue linear.Issue, target string) error
}

// assigner is the optional write capability to change an issue's assignee. Live
// Linear implements it; --demo does not.
type assigner interface {
	Users(ctx context.Context) ([]linear.User, error)
	Assign(ctx context.Context, issue linear.Issue, assigneeID string) error
}

const refreshEvery = 60 * time.Second

// triageCmd is the message the `t` hotkey sends to the highlighted session.
const triageCmd = "/triage"

// ── row model ────────────────────────────────────────────────────────────────
// The visible list is a flat slice of rows, one physical terminal line each:
// priority headers, status sub-headers, blank spacers, and selectable issue
// rows. Only issue rows are cursor targets. One row == one line keeps the
// viewport windowing (offset..offset+height) exact.

type rowKind int

const (
	rowPrio rowKind = iota
	rowStatus
	rowIssue
	rowSpacer
	rowSessionHeader // "Other sessions" header
	rowSession       // a non-ticket / off-list session row
)

type row struct {
	kind  rowKind
	text  string             // for headers
	count int                // for rowPrio/rowSessionHeader: item count
	issue linear.Issue       // for rowIssue
	ref   session.SessionRef // for rowSession
}

type refreshedMsg struct {
	issues []linear.Issue
	err    error
}

type statusesMsg struct {
	statuses map[string]session.Status
	err      error
}

type sessionsMsg struct {
	sessions []session.SessionRef
	err      error
}

type execDoneMsg struct{ err error }

// detachedDoneMsg is the result of a fire-and-return command (herdr).
type detachedDoneMsg struct {
	action string
	err    error
	output string
}

type tickMsg struct{}

// statusWriteMsg is the result of a MoveState write.
type statusWriteMsg struct {
	key    string
	target string
	err    error
}

// usersMsg carries the fetched workspace users for the assignee picker.
type usersMsg struct {
	users []linear.User
	err   error
}

// assignWriteMsg is the result of an Assign write.
type assignWriteMsg struct {
	key string
	who string
	err error
}

type Model struct {
	fetch         Fetcher
	root          string         // default working dir for new sessions (repos-root fallback)
	dry           bool           // print the launch command instead of running it
	allIssues     []linear.Issue // last fetched issues (for re-grouping on collapse)
	rows          []row
	cursor        int             // index into rows; a cursorable row (issue, or collapsed header)
	offset        int             // index of the first visible row (scroll position)
	collapsed     map[string]bool // priority label → collapsed
	collapseInit  bool            // whether the start-up default collapse has been applied
	backend       Backend
	sessions      map[string]session.Status // ticket key → session status
	otherSessions []session.SessionRef      // live sessions not tied to a visible ticket
	demoStatuses  map[string]session.Status // --demo override; nil in real use
	detail        *linear.Issue             // non-nil = showing the description overlay
	detailOffset  int                       // scroll offset within the detail overlay
	loading       bool
	underHerdr    bool          // running as a herdr pane (the persistent deck) — q must not kill it
	writer        statusWriter  // non-nil when the backing Fetcher can write status (live Linear)
	assigner      assigner      // non-nil when the backing Fetcher can change assignee (live Linear)
	statusMenu    bool          // status-change overlay is open
	statusPend    string        // chosen target awaiting y/n confirm ("" = still choosing)
	assignMenu    bool          // assignee-picker overlay is open
	assignIssue   linear.Issue  // ticket being reassigned (captured when the picker opens)
	assignQuery   string        // filter text in the assignee picker
	assignCursor  int           // index into the filtered picker options (0 = Unassign)
	users         []linear.User // cached workspace users for the picker
	err           error
	notice        string // transient status line (e.g. dry-run launch plan)
	lastSync      time.Time
	width         int
	height        int
	quitting      bool
}

// demoSessioner lets --demo inject canned session statuses (real sessions won't
// match demo ticket keys), so the badges are visible without a live daemon.
type demoSessioner interface {
	DemoSessions() map[string]session.Status
}

// demoOtherSessioner lets --demo inject canned "other sessions" so the bottom
// section is visible without a live backend.
type demoOtherSessioner interface {
	DemoOtherSessions() []session.SessionRef
}

func New(f Fetcher, root string, dry bool, backend Backend) Model {
	m := Model{fetch: f, root: root, dry: dry, backend: backend, loading: true, sessions: map[string]session.Status{}, collapsed: map[string]bool{}, underHerdr: os.Getenv("HERDR_PANE_ID") != ""}
	if ds, ok := f.(demoSessioner); ok {
		m.demoStatuses = ds.DemoSessions()
	}
	if dos, ok := f.(demoOtherSessioner); ok {
		m.otherSessions = dos.DemoOtherSessions()
	}
	if w, ok := f.(statusWriter); ok {
		m.writer = w
	}
	if a, ok := f.(assigner); ok {
		m.assigner = a
	}
	if backend != nil {
		debugLog.Printf("start: backend=%s root=%q dry=%v", backend.Bin(), root, dry)
	}
	return m
}

// Preview renders a single frame with the fetched data applied, for non-TTY
// verification (see `--preview`). It does not start the event loop. height<=0
// defaults to a tall viewport that shows everything.
func Preview(f Fetcher, height int) (string, error) {
	issues, err := f.FetchAssignedOpen(context.Background())
	if err != nil {
		return "", err
	}
	if height <= 0 {
		height = 40
	}
	m := New(f, "", true, ClaudeBackend{})
	m.height = height
	next, _ := m.Update(refreshedMsg{issues: issues})
	return next.(Model).View(), nil
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick())
}

func (m Model) refresh() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		issues, err := m.fetch.FetchAssignedOpen(ctx)
		return refreshedMsg{issues: issues, err: err}
	}
}

// refreshStatuses fetches per-ticket session statuses from the active backend
// (or the demo override). Read-only; no model calls.
func (m Model) refreshStatuses() tea.Cmd {
	keys := m.issueKeys()
	if m.demoStatuses != nil {
		ds := m.demoStatuses
		return func() tea.Msg {
			out := make(map[string]session.Status, len(keys))
			for _, k := range keys {
				out[k] = ds[k]
			}
			return statusesMsg{statuses: out}
		}
	}
	b := m.backend
	cwd := m.root
	return func() tea.Msg {
		st, err := b.Statuses(keys, cwd)
		return statusesMsg{statuses: st, err: err}
	}
}

// refreshSessions lists live backend sessions (for the "other sessions"
// section). Skipped in --demo (no live backend). Read-only.
func (m Model) refreshSessions() tea.Cmd {
	if m.demoStatuses != nil {
		return nil
	}
	b := m.backend
	return func() tea.Msg {
		s, err := b.Sessions()
		return sessionsMsg{sessions: s, err: err}
	}
}

func (m Model) issueKeys() []string {
	keys := make([]string, 0, len(m.rows))
	for _, r := range m.rows {
		if r.kind == rowIssue {
			keys = append(keys, r.issue.Identifier)
		}
	}
	return keys
}

// tick schedules the next background refresh with jitter (BR-2c) so many
// clients don't align on the same second.
func tick() tea.Cmd {
	jitter := time.Duration(rand.Intn(10000)) * time.Millisecond
	return tea.Tick(refreshEvery+jitter, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ensureVisible()

	case tickMsg:
		return m, tea.Batch(m.refresh(), m.refreshStatuses(), m.refreshSessions(), tick())

	case refreshedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err // BR-8: keep showing the last good list on error
			return m, nil
		}
		m.err = nil
		m.lastSync = time.Now()
		m.rebuild(msg.issues)
		return m, tea.Batch(m.refreshStatuses(), m.refreshSessions())

	case statusesMsg:
		if msg.err != nil {
			debugLog.Printf("status refresh error: %v", msg.err)
		} else {
			m.sessions = msg.statuses
		}

	case sessionsMsg:
		if msg.err != nil {
			debugLog.Printf("sessions refresh error: %v", msg.err)
		} else {
			m.otherSessions = msg.sessions
			prevID, _ := m.selectedID()
			prevRef := ""
			if s, ok := m.selectedSession(); ok {
				prevRef = s.Name
			}
			prevIdx := m.cursor
			m.regroup()
			m.restoreCursor(prevID, prevRef, prevIdx)
			m.ensureVisible()
		}

	case execDoneMsg:
		// Returned from a handed-off (foreground) session; refresh badges.
		if msg.err != nil {
			debugLog.Printf("foreground session error: %v", msg.err)
			m.err = msg.err
		} else {
			debugLog.Printf("returned from foreground session")
		}
		return m, m.refreshStatuses()

	case detachedDoneMsg:
		if msg.err != nil {
			debugLog.Printf("%s failed: %v — output: %s", msg.action, msg.err, msg.output)
			m.err = fmt.Errorf("%s failed: %s", msg.action, firstLine(msg.output))
			m.notice = ""
		} else {
			debugLog.Printf("%s ok — output: %s", msg.action, msg.output)
			m.notice = msg.action + " ✓"
		}
		return m, tea.Batch(m.refreshStatuses(), m.refreshSessions())

	case statusWriteMsg:
		if msg.err != nil {
			debugLog.Printf("move %s → %s failed: %v", msg.key, msg.target, msg.err)
			m.err = fmt.Errorf("move %s → %s: %v", msg.key, msg.target, msg.err)
			m.notice = ""
			return m, nil
		}
		debugLog.Printf("move %s → %s ok", msg.key, msg.target)
		m.notice = fmt.Sprintf("%s → %s ✓", msg.key, msg.target)
		m.err = nil
		cmds := []tea.Cmd{m.refresh(), m.refreshStatuses()}
		// A terminal move (Done/Canceled) drops the ticket off the list, so tear
		// down its Claude session too — but only if one is actually running.
		if isTerminalTarget(msg.target) && isRunning(m.sessions[msg.key]) {
			key := msg.key
			backend := m.backend
			cmds = append(cmds, func() tea.Msg {
				out, err := backend.CloseByName(key)
				return detachedDoneMsg{action: "closed " + key, err: err, output: strings.TrimSpace(out)}
			})
		}
		return m, tea.Batch(cmds...)

	case usersMsg:
		if msg.err != nil {
			debugLog.Printf("users fetch error: %v", msg.err)
			m.err = fmt.Errorf("load users: %v", msg.err)
			m.assignMenu = false
			m.notice = ""
		} else {
			m.users = msg.users
			if m.notice == "loading users…" {
				m.notice = ""
			}
		}

	case assignWriteMsg:
		if msg.err != nil {
			debugLog.Printf("assign %s → %s failed: %v", msg.key, msg.who, msg.err)
			m.err = fmt.Errorf("assign %s: %v", msg.key, msg.err)
			m.notice = ""
			return m, nil
		}
		debugLog.Printf("assign %s → %s ok", msg.key, msg.who)
		m.notice = fmt.Sprintf("%s → %s ✓", msg.key, msg.who)
		m.err = nil
		// Reassigning away from me drops it off the assigned list on refresh.
		return m, tea.Batch(m.refresh(), m.refreshStatuses())

	case tea.KeyMsg:
		if m.detail != nil {
			return m.updateDetail(msg)
		}
		if m.statusMenu {
			return m.updateStatus(msg)
		}
		if m.assignMenu {
			return m.updateAssign(msg)
		}
		switch msg.String() {
		case "q":
			// Under herdr the deck is the persistent hub; quitting it orphans
			// tab 1. Keep it open and point at the herdr-native ways to leave.
			if m.underHerdr {
				m.notice = "deck stays open · Ctrl+b q leaves herdr · Ctrl+b 1 returns here"
				break
			}
			m.quitting = true
			return m, tea.Quit
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(1)
		case "d":
			if is, ok := m.selected(); ok {
				c := is
				m.detail = &c
				m.detailOffset = 0
			}
		case "o":
			if is, ok := m.selected(); ok && is.URL != "" {
				m.notice = "opened " + is.Identifier + " in browser"
				return m, openBrowser(is.URL)
			}
		case "p":
			if is, ok := m.selected(); ok {
				return m.openPRFor(is)
			}
		case "s":
			// Open the status-change menu (write). Unavailable in --demo.
			if _, ok := m.selected(); !ok {
				break
			}
			if m.writer == nil {
				m.notice = "status changes need a live Linear connection"
				break
			}
			m.statusMenu = true
			m.statusPend = ""
		case "a":
			// Open the assignee picker (write). Unavailable in --demo.
			is, ok := m.selected()
			if !ok {
				break
			}
			if m.assigner == nil {
				m.notice = "assignee changes need a live Linear connection"
				break
			}
			m.assignMenu = true
			m.assignIssue = is
			m.assignQuery = ""
			m.assignCursor = 0
			if m.users == nil {
				return m, m.fetchUsers()
			}
		case "n":
			// Open an ad-hoc Claude session not tied to any ticket.
			spec := m.backend.ScratchSpec(m.root)
			return m.runSpec(spec, spec.Name)
		case "x":
			// Close the highlighted "other session" (transcript persists).
			if s, ok := m.selectedSession(); ok {
				return m.closeSession(s)
			}
		case "t":
			// Fire a message (default "/triage") at the highlighted session
			// without switching to it. This runs a Claude turn — a user action,
			// so it is consistent with BR-1 (the app itself never spends tokens).
			name := ""
			if is, ok := m.selected(); ok {
				name = is.Identifier
			} else if sr, ok := m.selectedSession(); ok {
				name = sr.Name
			}
			if name != "" {
				return m.sendToSession(name, triageCmd)
			}
		case "pgup", "ctrl+u":
			m.page(-1)
		case "pgdown", "ctrl+d":
			m.page(1)
		case "home", "g":
			m.cursor = m.firstCursorable()
			m.ensureVisible()
		case "end", "G":
			m.cursor = m.lastCursorable()
			m.ensureVisible()
		case " ", "tab":
			m.toggleCollapse("")
		case "left", "h":
			m.toggleCollapse("collapse")
		case "right", "l":
			m.toggleCollapse("expand")
		case "r":
			m.loading = true
			m.notice = ""
			return m, tea.Batch(m.refresh(), m.refreshStatuses())
		case "enter":
			if s, ok := m.selectedSession(); ok {
				return m.runSpec(m.backend.FocusSpec(s), s.Name)
			}
			return m.launchSelected()
		}
	}
	return m, nil
}

// launchSelected plans and runs (or dry-prints) the session for the cursor's
// ticket.
func (m Model) launchSelected() (tea.Model, tea.Cmd) {
	is, ok := m.selected()
	if !ok {
		return m, nil
	}
	return m.launchIssue(is)
}

// launchIssue plans and runs (or dry-prints) the session for a specific ticket,
// so it can be triggered from the list or from the description overlay.
func (m Model) launchIssue(is linear.Issue) (tea.Model, tea.Cmd) {
	spec, err := m.backend.Plan(toTicket(is), m.root)
	if err != nil {
		debugLog.Printf("plan error for %s: %v", is.Identifier, err)
		m.err = err
		return m, nil
	}
	return m.runSpec(spec, is.Identifier)
}

// runSpec executes a LaunchSpec (ticket, scratch, or focus): dry-prints it,
// hands over the terminal for a foreground spec, or fires-and-returns a detached
// one (herdr new-tab). label names the target for notices/logs.
func (m Model) runSpec(spec session.LaunchSpec, label string) (tea.Model, tea.Cmd) {
	bin := m.backend.Bin()
	debugLog.Printf("launch %s %s: %s %s (cwd=%s fg=%v)", spec.Action, label, bin, strings.Join(spec.Args, " "), spec.Cwd, spec.Foreground)

	if m.dry {
		cmdline := bin + " " + strings.Join(spec.Args, " ")
		if len(cmdline) > 140 { // the appended system prompt can be long
			cmdline = cmdline[:137] + "…"
		}
		m.notice = fmt.Sprintf("[dry] %s  (cwd=%s · %s)", cmdline, spec.Cwd, spec.Action)
		return m, nil
	}

	if spec.Foreground {
		// Interactive claude: hand over the terminal; return on exit.
		c := exec.Command(bin, spec.Args...)
		c.Dir = spec.Cwd
		m.notice = ""
		return m, tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{err: err} })
	}
	// Fire-and-return (herdr): run via the backend (which may open a new tab),
	// capture output, stay in the deck.
	m.notice = spec.Action + " " + label + "…"
	backend := m.backend
	action := spec.Action + " " + label
	return m, func() tea.Msg {
		out, err := backend.RunDetached(spec)
		return detachedDoneMsg{action: action, err: err, output: strings.TrimSpace(out)}
	}
}

// sendToSession fires text at a running session (by name) without switching to
// it. It runs a Claude turn — deliberately, on the user's keypress.
func (m Model) sendToSession(name, text string) (tea.Model, tea.Cmd) {
	m.notice = fmt.Sprintf("sending %q to %s…", text, name)
	backend := m.backend
	return m, func() tea.Msg {
		out, err := backend.Send(name, text)
		return detachedDoneMsg{action: fmt.Sprintf("sent %s to %s", text, name), err: err, output: strings.TrimSpace(out)}
	}
}

// closeSession stops an "other session" (fire-and-return); the transcript stays.
func (m Model) closeSession(s session.SessionRef) (tea.Model, tea.Cmd) {
	m.notice = "closing " + s.Name + "…"
	backend := m.backend
	return m, func() tea.Msg {
		out, err := backend.CloseSession(s)
		return detachedDoneMsg{action: "closed " + s.Name, err: err, output: strings.TrimSpace(out)}
	}
}

// updateDetail handles keys while the description overlay is open.
func (m Model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "d":
		m.detail = nil
		m.detailOffset = 0
	case "enter":
		// Navigate straight to this ticket's Claude session from its description.
		is := *m.detail
		m.detail = nil
		m.detailOffset = 0
		return m.launchIssue(is)
	case "up", "k":
		if m.detailOffset > 0 {
			m.detailOffset--
		}
	case "down", "j":
		m.detailOffset++
	case "pgup", "ctrl+u":
		m.detailOffset -= 10
		if m.detailOffset < 0 {
			m.detailOffset = 0
		}
	case "pgdown", "ctrl+d":
		m.detailOffset += 10
	case "o":
		if m.detail.URL != "" {
			return m, openBrowser(m.detail.URL)
		}
	case "p":
		return m.openPRFor(*m.detail)
	}
	return m, nil
}

// openPRFor opens the ticket's linked PR in the browser (the most actionable
// one when several are linked), or notes when there is none.
func (m Model) openPRFor(is linear.Issue) (tea.Model, tea.Cmd) {
	if len(is.PRs) == 0 {
		m.notice = "no linked PR for " + is.Identifier
		return m, nil
	}
	best := pickPRState(is.PRs)
	pr := is.PRs[0]
	for _, p := range is.PRs {
		if p.State == best {
			pr = p
			break
		}
	}
	if n := len(is.PRs); n > 1 {
		m.notice = fmt.Sprintf("opened PR for %s (1 of %d)", is.Identifier, n)
	} else {
		m.notice = "opened PR for " + is.Identifier
	}
	return m, openBrowser(pr.URL)
}

// updateStatus drives the two-step status-change overlay: first pick a target
// (d/v/c), then confirm (y). Any escape/other key backs out. The two deliberate
// isTerminalTarget reports whether a status change removes the ticket from the
// deck (so its session should be torn down). Validate stays visible, so it does
// not count.
func isTerminalTarget(target string) bool {
	return target == "Done" || target == "Canceled"
}

// isRunning reports whether a session status corresponds to a live agent (as
// opposed to a resumable-on-disk, completed, or absent one).
func isRunning(st session.Status) bool {
	return st == session.Working || st == session.Idle || st == session.NeedsInput
}

// fetchUsers loads workspace users for the assignee picker (once, then cached).
func (m Model) fetchUsers() tea.Cmd {
	a := m.assigner
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		us, err := a.Users(ctx)
		return usersMsg{users: us, err: err}
	}
}

// filteredUsers returns the workspace users matching the picker's filter text.
func (m Model) filteredUsers() []linear.User {
	q := strings.ToLower(strings.TrimSpace(m.assignQuery))
	if q == "" {
		return m.users
	}
	var out []linear.User
	for _, u := range m.users {
		if strings.Contains(strings.ToLower(u.Label()), q) || strings.Contains(strings.ToLower(u.Email), q) {
			out = append(out, u)
		}
	}
	return out
}

// updateAssign drives the assignee picker: type to filter, ↑/↓ to move, Enter to
// assign (cursor 0 = Unassign), Esc to cancel.
func (m Model) updateAssign(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredUsers()
	n := 1 + len(filtered) // option 0 = Unassign
	if m.assignCursor >= n {
		m.assignCursor = n - 1
	}
	switch msg.String() {
	case "esc":
		m.assignMenu = false
		m.notice = "assignee change canceled"
	case "up", "ctrl+p":
		if m.assignCursor > 0 {
			m.assignCursor--
		}
	case "down", "ctrl+n":
		if m.assignCursor < n-1 {
			m.assignCursor++
		}
	case "backspace":
		if r := []rune(m.assignQuery); len(r) > 0 {
			m.assignQuery = string(r[:len(r)-1])
			m.assignCursor = 0
		}
	case "enter":
		is := m.assignIssue
		id, who := "", "Unassigned"
		if m.assignCursor > 0 && m.assignCursor-1 < len(filtered) {
			u := filtered[m.assignCursor-1]
			id, who = u.ID, u.Label()
		}
		m.assignMenu = false
		m.notice = fmt.Sprintf("assigning %s → %s…", is.Identifier, who)
		a := m.assigner
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := a.Assign(ctx, is, id)
			return assignWriteMsg{key: is.Identifier, who: who, err: err}
		}
	default:
		if len(msg.Runes) == 1 {
			m.assignQuery += string(msg.Runes)
			m.assignCursor = 0
		}
	}
	return m, nil
}

// keystrokes are the guard against an accidental write.
func (m Model) updateStatus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	is, ok := m.selected()
	if !ok {
		m.statusMenu = false
		m.statusPend = ""
		return m, nil
	}
	if m.statusPend == "" { // choosing a target
		switch msg.String() {
		case "d":
			m.statusPend = "Done"
		case "v":
			m.statusPend = "Validate"
		case "m":
			m.statusPend = "Monitoring"
		case "b":
			m.statusPend = "Blocked"
		case "c":
			m.statusPend = "Canceled"
		case "esc", "q", "s":
			m.statusMenu = false
		}
		return m, nil
	}
	// confirming
	switch msg.String() {
	case "y", "enter":
		target := m.statusPend
		m.statusMenu = false
		m.statusPend = ""
		m.notice = fmt.Sprintf("moving %s → %s…", is.Identifier, target)
		w := m.writer
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := w.MoveState(ctx, is, target)
			return statusWriteMsg{key: is.Identifier, target: target, err: err}
		}
	case "esc", "n", "q":
		m.statusMenu = false
		m.statusPend = ""
		m.notice = "status change canceled"
	}
	return m, nil
}

// openBrowser opens a URL in the default browser (detached).
func openBrowser(url string) tea.Cmd {
	return func() tea.Msg {
		_ = exec.Command("xdg-open", url).Start()
		return nil
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:99] + "…"
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

func toTicket(is linear.Issue) session.Ticket {
	return session.Ticket{
		Key:       is.Identifier,
		Title:     is.Title,
		URL:       is.URL,
		Branch:    is.Branch,
		Status:    is.StateName,
		PrioLabel: is.PrioLabel,
		Team:      is.TeamName,
	}
}

// rebuild stores the fetched issues, regroups them into rows, and keeps the
// cursor on the same ticket across refreshes when possible.
func (m *Model) rebuild(issues []linear.Issue) {
	prevID, _ := m.selectedID()
	prevIdx := m.cursor
	m.allIssues = issues
	m.applyInitialCollapse()
	m.regroup()

	// Demo statuses resolve synchronously so --preview shows badges; real
	// backends populate m.sessions asynchronously via statusesMsg.
	if m.demoStatuses != nil {
		keys := m.issueKeys()
		out := make(map[string]session.Status, len(keys))
		for _, k := range keys {
			out[k] = m.demoStatuses[k]
		}
		m.sessions = out
	}

	m.restoreCursor(prevID, "", prevIdx)
	m.ensureVisible()
}

// restoreCursor puts the cursor back on the same ticket (prevID) or session
// (prevRef) across a rebuild. When that row is gone — e.g. the selected ticket
// was moved to Done and dropped off the list — it stays near the old position
// (prevIdx) rather than snapping to the top, so you keep working down the list.
func (m *Model) restoreCursor(prevID, prevRef string, prevIdx int) {
	for i, r := range m.rows {
		if prevID != "" && r.kind == rowIssue && r.issue.Identifier == prevID {
			m.cursor = i
			return
		}
		if prevRef != "" && r.kind == rowSession && r.ref.Name == prevRef {
			m.cursor = i
			return
		}
	}
	m.cursor = m.nearestCursorable(prevIdx)
}

// nearestCursorable returns the cursorable row closest to idx, preferring the
// row at or just after idx (which, after a removal, is the next ticket).
func (m Model) nearestCursorable(idx int) int {
	if idx < 0 {
		idx = 0
	}
	for d := 0; d < len(m.rows); d++ {
		if i := idx + d; i < len(m.rows) && m.cursorable(i) {
			return i
		}
		if i := idx - d; i >= 0 && m.cursorable(i) {
			return i
		}
	}
	return m.firstCursorable()
}

// applyInitialCollapse folds every priority group except the highest non-empty
// one on the first load, so the deck opens focused on top-priority work. Runs
// once (collapseInit guard) so later refreshes and manual toggles stick.
func (m *Model) applyInitialCollapse() {
	if m.collapseInit {
		return
	}
	groups := linear.GroupByPriorityThenStatus(linear.FilterVisible(m.allIssues))
	if len(groups) == 0 {
		return // nothing to base the default on yet; try again next load
	}
	for i, g := range groups {
		m.collapsed[g.PrioLabel] = i != 0
	}
	m.collapseInit = true
}

// regroup rebuilds the visible rows from allIssues, honoring collapsed groups.
// A collapsed priority renders as a single header row (with a ticket count) and
// its statuses/issues are omitted.
func (m *Model) regroup() {
	// Defensive BR-2a: never render Done/Cancelled/Duplicate tickets, whatever
	// the source (the Linear client already filters, but --demo and future
	// feeds might not).
	groups := linear.GroupByPriorityThenStatus(linear.FilterVisible(m.allIssues))
	var rows []row
	for gi, g := range groups {
		if gi > 0 {
			rows = append(rows, row{kind: rowSpacer})
		}
		n := 0
		for _, sb := range g.Statuses {
			n += len(sb.Issues)
		}
		rows = append(rows, row{kind: rowPrio, text: g.PrioLabel, count: n})
		if m.collapsed[g.PrioLabel] {
			continue // header only
		}
		for _, sb := range g.Statuses {
			rows = append(rows, row{kind: rowStatus, text: sb.Status})
			for _, is := range sb.Issues {
				rows = append(rows, row{kind: rowIssue, issue: is})
			}
		}
	}

	// "Other sessions": live sessions not represented by a visible ticket — a
	// ticket that dropped off the list (Done/Cancelled), or an ad-hoc scratch
	// session. The deck agent is already excluded by the backend.
	visible := map[string]bool{}
	for _, is := range linear.FilterVisible(m.allIssues) {
		visible[is.Identifier] = true
	}
	var others []session.SessionRef
	for _, s := range m.otherSessions {
		if !visible[s.Name] {
			others = append(others, s)
		}
	}
	if len(others) > 0 {
		rows = append(rows, row{kind: rowSpacer})
		rows = append(rows, row{kind: rowSessionHeader, text: "Other sessions", count: len(others)})
		for _, s := range others {
			rows = append(rows, row{kind: rowSession, ref: s})
		}
	}

	m.rows = rows
}

// cursorable rows are selectable: issue rows, and collapsed priority headers
// (so a collapsed group can be navigated to and expanded).
func (m Model) cursorable(i int) bool {
	if i < 0 || i >= len(m.rows) {
		return false
	}
	r := m.rows[i]
	return r.kind == rowIssue || r.kind == rowSession || (r.kind == rowPrio && m.collapsed[r.text])
}

func (m Model) firstCursorable() int {
	for i := range m.rows {
		if m.cursorable(i) {
			return i
		}
	}
	return 0
}

func (m Model) lastCursorable() int {
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.cursorable(i) {
			return i
		}
	}
	return 0
}

func (m *Model) moveCursor(dir int) {
	n := len(m.rows)
	if n == 0 {
		return
	}
	// Wrap around the ends: scrolling up past the top lands on the bottom, and
	// vice-versa. Scans at most one full loop, so it no-ops if nothing is
	// cursorable.
	i := m.cursor
	for step := 0; step < n; step++ {
		i += dir
		if i < 0 {
			i = n - 1
		} else if i >= n {
			i = 0
		}
		if m.cursorable(i) {
			m.cursor = i
			m.ensureVisible()
			return
		}
	}
}

// page jumps the cursor ~one viewport in the given direction, snapping to the
// nearest cursorable row.
func (m *Model) page(dir int) {
	h := m.viewportHeight()
	if h <= 1 {
		m.moveCursor(dir)
		return
	}
	target := m.cursor + dir*(h-1)
	if target < 0 {
		target = 0
	}
	if target > len(m.rows)-1 {
		target = len(m.rows) - 1
	}
	for target >= 0 && target < len(m.rows) && !m.cursorable(target) {
		target += dir
	}
	if target < 0 {
		target = m.firstCursorable()
	}
	if target >= len(m.rows) {
		target = m.lastCursorable()
	}
	m.cursor = target
	m.ensureVisible()
}

// currentPrioLabel returns the priority group the cursor is in (whether it's on
// an issue row or a collapsed header).
func (m Model) currentPrioLabel() string {
	for i := m.cursor; i >= 0 && i < len(m.rows); i-- {
		if m.rows[i].kind == rowPrio {
			return m.rows[i].text
		}
	}
	return ""
}

// toggleCollapse folds/unfolds the priority group at the cursor. mode "collapse"
// or "expand" forces a direction; "" toggles. After regrouping, the cursor lands
// on the group's header (collapsed) or its first ticket (expanded).
func (m *Model) toggleCollapse(mode string) {
	label := m.currentPrioLabel()
	if label == "" {
		return
	}
	switch mode {
	case "collapse":
		if m.collapsed[label] {
			return
		}
		m.collapsed[label] = true
	case "expand":
		if !m.collapsed[label] {
			return
		}
		m.collapsed[label] = false
	default:
		m.collapsed[label] = !m.collapsed[label]
	}
	m.regroup()
	m.cursorToPrio(label)
	m.ensureVisible()
}

// cursorToPrio positions the cursor on a priority group's header (if collapsed)
// or its first ticket (if expanded).
func (m *Model) cursorToPrio(label string) {
	headerIdx := -1
	for i, r := range m.rows {
		if r.kind == rowPrio && r.text == label {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		m.cursor = m.firstCursorable()
		return
	}
	if m.collapsed[label] {
		m.cursor = headerIdx
		return
	}
	// expanded: first cursorable at or after the header
	for i := headerIdx; i < len(m.rows); i++ {
		if m.rows[i].kind == rowIssue {
			m.cursor = i
			return
		}
	}
	m.cursor = headerIdx
}

// viewportHeight is the number of body lines available for rows, i.e. the
// terminal height minus the title and footer. Returns 0 when the height is
// unknown (pre-first-resize), meaning "render everything".
func (m Model) viewportHeight() int {
	if m.height <= 0 {
		return 0
	}
	reserved := 1 + 1 + 1 // title + blank spacer + help line
	if m.err != nil || m.notice != "" {
		reserved++ // status line above help
	}
	h := m.height - reserved
	if h < 1 {
		h = 1
	}
	return h
}

// ensureVisible scrolls the viewport so the cursor is on screen, and pulls the
// cursor's group/status headers into view when scrolling up so the selection
// keeps its context.
func (m *Model) ensureVisible() {
	h := m.viewportHeight()
	if h <= 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	} else if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	for m.offset > 0 && m.rows[m.offset-1].kind != rowIssue && m.cursor <= m.offset-1+h-1 {
		m.offset--
	}
	if maxOffset := len(m.rows) - h; m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m Model) selected() (linear.Issue, bool) {
	if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].kind == rowIssue {
		return m.rows[m.cursor].issue, true
	}
	return linear.Issue{}, false
}

// selectedSession returns the session under the cursor, if the cursor is on a
// row in the "other sessions" section.
func (m Model) selectedSession() (session.SessionRef, bool) {
	if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].kind == rowSession {
		return m.rows[m.cursor].ref, true
	}
	return session.SessionRef{}, false
}

func (m Model) selectedID() (string, bool) {
	if is, ok := m.selected(); ok {
		return is.Identifier, true
	}
	return "", false
}

// ── view ─────────────────────────────────────────────────────────────────────

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	statusStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250")).PaddingLeft(1)
	selStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("57"))
	idStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	noticeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	// working tickets recede: uniform faint gray so attention goes elsewhere.
	workingRowStyle = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("240"))
)

// prioColor maps a priority label to a scannable color: red→amber→yellow→blue,
// gray for no-priority. Used to color the group headers.
func prioColor(label string) lipgloss.Color {
	switch label {
	case "Urgent":
		return lipgloss.Color("203") // red
	case "High":
		return lipgloss.Color("214") // orange
	case "Medium":
		return lipgloss.Color("220") // yellow
	case "Low":
		return lipgloss.Color("75") // blue
	default:
		return lipgloss.Color("244") // gray (No priority)
	}
}

// rowWidth is the width to pad a highlighted row to (full-width selection bar).
func (m Model) rowWidth() int {
	if m.width > 1 {
		return m.width
	}
	return 80
}

// prColor maps a PR state to a color (GitHub-ish: merged violet, open green,
// closed red, draft/unknown gray).
func prColor(state string) lipgloss.Color {
	switch state {
	case "merged":
		return lipgloss.Color("141") // violet
	case "open":
		return lipgloss.Color("42") // green
	case "closed":
		return lipgloss.Color("203") // red
	default:
		return lipgloss.Color("244") // draft / unknown
	}
}

// pickPRState summarizes a set of PRs into the most actionable state for the
// row icon: an open PR outranks a draft, then merged, then closed.
func pickPRState(prs []linear.PR) string {
	rank := map[string]int{"open": 4, "draft": 3, "merged": 2, "closed": 1, "": 0}
	best := ""
	for _, p := range prs {
		if rank[p.State] > rank[best] {
			best = p.State
		}
	}
	return best
}

// prMark returns the 1-column PR indicator glyph and its color, or a blank when
// there is no linked PR (kept 1 col wide so issue rows stay aligned).
func prMark(prs []linear.PR) (glyph string, color lipgloss.Color) {
	if len(prs) == 0 {
		return " ", lipgloss.Color("240")
	}
	return "⇄", prColor(pickPRState(prs))
}

// renderPrio draws a priority header, color-coded by priority, always showing
// the ticket count: "▾ Urgent (3)" expanded, "▸ Urgent (3)" collapsed. Collapsed
// headers are cursorable, so they get the full-width selection bar when selected.
func (m Model) renderPrio(r row, selected bool) string {
	caret := "▾"
	if m.collapsed[r.text] {
		caret = "▸"
	}
	// A bold uppercase chip (priority color as background) makes each priority a
	// clearly-delineated section header. Count is kept inside the chip.
	body := fmt.Sprintf(" %s %s · %d ", caret, strings.ToUpper(r.text), r.count)
	if selected {
		return selStyle.Width(m.rowWidth()).Render("▶" + body)
	}
	chip := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(prioColor(r.text))
	return chip.Render(body)
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if m.detail != nil {
		return m.renderDetail()
	}
	if m.assignMenu {
		return m.renderAssign()
	}
	var b strings.Builder

	fmt.Fprintf(&b, "%s%s\n", titleStyle.Render("TicketDeck"), dimStyle.Render(m.titleMeta()))

	if m.loading && len(m.rows) == 0 {
		fmt.Fprint(&b, dimStyle.Render("\n  loading tickets…\n"))
		return b.String()
	}
	if len(m.rows) == 0 && m.err == nil {
		fmt.Fprint(&b, dimStyle.Render("\n  no open tickets assigned to you 🎉\n"))
		return b.String()
	}

	start, end := m.window()
	for i := start; i < end; i++ {
		switch r := m.rows[i]; r.kind {
		case rowPrio:
			fmt.Fprintf(&b, "%s\n", m.renderPrio(r, i == m.cursor))
		case rowStatus:
			fmt.Fprintf(&b, "%s\n", statusStyle.Render("▏ "+strings.ToUpper(r.text)))
		case rowIssue:
			fmt.Fprintf(&b, "%s\n", m.renderIssue(r.issue, i == m.cursor))
		case rowSessionHeader:
			fmt.Fprintf(&b, "%s\n", sectionStyle.Render(fmt.Sprintf("Other sessions (%d)", r.count)))
		case rowSession:
			fmt.Fprintf(&b, "%s\n", m.renderSession(r.ref, i == m.cursor))
		case rowSpacer:
			fmt.Fprint(&b, "\n")
		}
	}

	fmt.Fprintf(&b, "\n%s", m.footer())
	return b.String()
}

// window returns the [start,end) row range to render given the viewport.
// h<=0 (height unknown) means render everything.
func (m Model) window() (int, int) {
	h := m.viewportHeight()
	if h <= 0 {
		return 0, len(m.rows)
	}
	start := m.offset
	if start < 0 {
		start = 0
	}
	if start > len(m.rows) {
		start = len(m.rows)
	}
	end := start + h
	if end > len(m.rows) {
		end = len(m.rows)
	}
	return start, end
}

func (m Model) titleMeta() string {
	meta := "  assigned · open only"
	if s, e := m.window(); s > 0 || e < len(m.rows) {
		meta += "  ↕ more" // there is off-screen content
	}
	return meta
}

// sessionCol is the fixed width of the badge+label column, sized to the widest
// label ("needs input").
const sessionCol = 13

func (m Model) renderIssue(is linear.Issue, selected bool) string {
	st := m.sessions[is.Identifier]
	cell, color := sessionCellText(st)
	id := fmt.Sprintf("%-9s", is.Identifier)
	prG, prC := prMark(is.PRs)
	tagText, tagColor := validationTag(is.Labels)

	// Truncate the title to what's left after the fixed columns:
	// indent(2) + badge + space + id(9) + space + prmark(1) + space, minus the
	// trailing validation tag (with its leading space) when present.
	avail := m.rowWidth() - (2 + sessionCol + 1 + 9 + 1 + 1 + 1)
	if tagText != "" {
		avail -= len([]rune(tagText)) + 1
	}
	if avail < 12 {
		avail = 12
	}
	title := is.Title
	if len([]rune(title)) > avail {
		title = string([]rune(title)[:avail-1]) + "…"
	}

	if selected {
		// Plain text (no inner colors) so the selection bg spans the whole row.
		content := fmt.Sprintf("▶ %s %s %s %s", cell, id, prG, title)
		if tagText != "" {
			content += " " + tagText
		}
		return selStyle.Width(m.rowWidth()).Render(content)
	}
	tag := ""
	if tagText != "" {
		tag = " " + lipgloss.NewStyle().Foreground(tagColor).Render(tagText)
	}
	// Working tickets are already being handled — de-emphasize the whole row
	// (uniform dim, no cyan id / bright title) so the eye is drawn to the
	// tickets that still need attention.
	if st == session.Working {
		return workingRowStyle.Render(fmt.Sprintf("  %s %s %s %s", cell, id, prG, title)) + tag
	}
	badge := lipgloss.NewStyle().Foreground(color).Render(cell)
	pr := lipgloss.NewStyle().Foreground(prC).Render(prG)
	return fmt.Sprintf("  %s %s %s %s", badge, idStyle.Render(id), pr, title) + tag
}

// validationTag returns a compact flag + color for a ticket carrying a
// validation label — validation-failed (red) outranks validation-inconclusive
// (amber). Empty when neither is present.
func validationTag(labels []string) (string, lipgloss.Color) {
	inconclusive := false
	for _, l := range labels {
		switch strings.ToLower(l) {
		case "validation-failed":
			return "⚑ validation failed", lipgloss.Color("203")
		case "validation-inconclusive":
			inconclusive = true
		}
	}
	if inconclusive {
		return "⚑ inconclusive", lipgloss.Color("214")
	}
	return "", lipgloss.Color("")
}

// renderSession renders an "other sessions" row: its status badge + name.
func (m Model) renderSession(ref session.SessionRef, selected bool) string {
	cell, color := sessionCellText(ref.Status)
	name := ref.Name
	avail := m.rowWidth() - (2 + sessionCol + 1)
	if avail > 0 && len([]rune(name)) > avail {
		name = string([]rune(name)[:avail-1]) + "…"
	}
	if selected {
		return selStyle.Width(m.rowWidth()).Render(fmt.Sprintf("▶ %s %s", cell, name))
	}
	badge := lipgloss.NewStyle().Foreground(color).Render(cell)
	return fmt.Sprintf("  %s %s", badge, name)
}

// sessionCellText returns the badge + status label as plain text padded to
// sessionCol (so the id column stays aligned), plus its color. Splitting text
// from color lets selected rows render a clean full-width highlight.
func sessionCellText(s session.Status) (string, lipgloss.Color) {
	glyph, label, color := sessionStyle(s)
	text := glyph
	if label != "" {
		text += " " + label
	}
	if pad := sessionCol - len([]rune(text)); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return text, color
}

// sessionStyle maps a status to its glyph, short label, and color.
func sessionStyle(s session.Status) (glyph, label string, color lipgloss.Color) {
	switch s {
	case session.Working:
		return "●", "working", lipgloss.Color("42") // green
	case session.NeedsInput:
		return "◆", "needs input", lipgloss.Color("214") // amber
	case session.Idle:
		return "○", "idle", lipgloss.Color("81") // cyan
	case session.Completed:
		return "✓", "done", lipgloss.Color("71") // muted green
	case session.Stopped:
		return "↻", "resumable", lipgloss.Color("245") // gray — on disk, not running
	default:
		return "·", "", lipgloss.Color("238") // no session
	}
}

// renderDetail draws the description overlay for the selected ticket.
// renderAssign draws the assignee picker overlay: a filter line, an Unassign
// option, then the matching workspace users, windowed to the viewport.
func (m Model) renderAssign() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s%s\n", titleStyle.Render("Assign"), idStyle.Render(m.assignIssue.Identifier), dimStyle.Render("  "+m.assignIssue.Title))
	fmt.Fprintf(&b, "%s\n", dimStyle.Render("type to filter · ↑/↓ select · ⏎ assign · esc cancel"))
	if m.users == nil {
		fmt.Fprint(&b, dimStyle.Render("\n  loading users…"))
		return b.String()
	}
	fmt.Fprintf(&b, "\nfilter: %s\n\n", m.assignQuery)

	labels := []string{"— Unassign —"}
	for _, u := range m.filteredUsers() {
		labels = append(labels, u.Label())
	}
	h := m.height - 7
	if h < 3 || m.height <= 0 {
		h = len(labels)
	}
	start := 0
	if m.assignCursor >= h {
		start = m.assignCursor - h + 1
	}
	end := start + h
	if end > len(labels) {
		end = len(labels)
	}
	for i := start; i < end; i++ {
		if i == m.assignCursor {
			fmt.Fprintf(&b, "%s\n", selStyle.Render("▶ "+labels[i]))
		} else {
			fmt.Fprintf(&b, "  %s\n", labels[i])
		}
	}
	if end < len(labels) {
		fmt.Fprint(&b, dimStyle.Render("  ↓ more"))
	}
	return b.String()
}

func (m Model) renderDetail() string {
	is := m.detail
	var b strings.Builder

	fmt.Fprintf(&b, "%s %s\n", idStyle.Render(is.Identifier), titleStyle.Render(is.Title))

	// Priority (colored) · status · team, then the session state on its own line.
	meta := []string{}
	if is.StateName != "" {
		meta = append(meta, is.StateName)
	}
	if is.TeamName != "" {
		meta = append(meta, is.TeamName)
	}
	line := ""
	if is.PrioLabel != "" {
		line = lipgloss.NewStyle().Bold(true).Foreground(prioColor(is.PrioLabel)).Render(is.PrioLabel)
	}
	if len(meta) > 0 {
		if line != "" {
			line += dimStyle.Render(" · ")
		}
		line += dimStyle.Render(strings.Join(meta, " · "))
	}
	fmt.Fprintf(&b, "%s\n", line)

	glyph, label, color := sessionStyle(m.sessions[is.Identifier])
	sess := lipgloss.NewStyle().Foreground(color).Render(glyph + " " + label)
	if label == "" {
		sess = dimStyle.Render("· no session yet — press ⏎ to start one")
	}
	fmt.Fprintf(&b, "%s\n", sess)
	if tagText, tagColor := validationTag(is.Labels); tagText != "" {
		fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Foreground(tagColor).Render(tagText))
	}
	if is.URL != "" {
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(is.URL))
	}
	for _, pr := range is.PRs {
		label := pr.State
		if label == "" {
			label = "PR"
		}
		icon := lipgloss.NewStyle().Foreground(prColor(pr.State)).Render("⇄ " + label)
		title := pr.Title
		if title == "" {
			title = pr.URL
		}
		fmt.Fprintf(&b, "%s %s\n", icon, dimStyle.Render(title))
	}
	fmt.Fprint(&b, "\n")

	desc := strings.TrimSpace(is.Description)
	if desc == "" {
		desc = "(no description)"
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	wrapped := lipgloss.NewStyle().Width(width - 2).Render(desc)
	lines := strings.Split(wrapped, "\n")

	// window the body to the available height (header: title, meta, session,
	// url, PR lines, blank; plus the blank+footer at the bottom)
	h := m.height - 7 - len(is.PRs)
	if h < 1 || m.height <= 0 {
		h = len(lines)
	}
	off := m.detailOffset
	if off > len(lines)-1 {
		off = len(lines) - 1
	}
	if off < 0 {
		off = 0
	}
	end := off + h
	if end > len(lines) {
		end = len(lines)
	}
	for _, ln := range lines[off:end] {
		fmt.Fprintf(&b, "%s\n", ln)
	}

	more := ""
	if end < len(lines) {
		more = " · ↕ more"
	}
	hint := "⏎ open session · o browser"
	if len(is.PRs) > 0 {
		hint += " · p open PR"
	}
	hint += " · d/esc back"
	fmt.Fprintf(&b, "\n%s", dimStyle.Render("↑/↓ scroll · "+hint+more))
	return b.String()
}

func (m Model) footer() string {
	if m.statusMenu {
		is, _ := m.selected()
		if m.statusPend == "" {
			return noticeStyle.Render(fmt.Sprintf("  move %s →  d Done · v Validate · m Monitoring · b Blocked · c Cancel · esc", is.Identifier))
		}
		return noticeStyle.Render(fmt.Sprintf("  move %s → %s?   y confirm · esc cancel", is.Identifier, m.statusPend))
	}
	sync := "never"
	if !m.lastSync.IsZero() {
		sync = m.lastSync.Format("15:04:05")
	}
	quitHint := "q quit"
	if m.underHerdr {
		quitHint = "Ctrl+b q leave" // q keeps the deck open under herdr
	}
	// On an "other session" row the actions differ (open/close), so show those.
	if _, ok := m.selectedSession(); ok {
		return dimStyle.Render(fmt.Sprintf("↑↓ move · ⏎ open · t %s · x close · n new · r refresh · %s · synced %s", triageCmd, quitHint, sync))
	}
	statusHint := ""
	if m.writer != nil {
		statusHint = "s status · "
	}
	if m.assigner != nil {
		statusHint += "a assign · "
	}
	help := dimStyle.Render(fmt.Sprintf("↑↓ move · ⏎ open · d desc · o web · p PR · t %s · %sn new · ␣/←→ fold · r refresh · %s · synced %s", triageCmd, statusHint, quitHint, sync))
	var status string
	switch {
	case m.err != nil:
		status = errStyle.Render("  " + m.err.Error())
	case m.notice != "":
		status = noticeStyle.Render("  " + m.notice)
	}
	if status != "" {
		return status + "\n" + help
	}
	return help
}
