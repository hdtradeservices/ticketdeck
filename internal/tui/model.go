package tui

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/session"
	"github.com/hdtradeservices/ticketdeck/internal/update"
)

// Version is the running build's version, set by main (via -ldflags). Used for
// the startup "newer release available" check.
var Version = "dev"

// hideOpenHintPath is the marker file that suppresses the "how to get back"
// reminder shown when opening a ticket session.
func hideOpenHintPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".ticketdeck", "hide-open-hint")
}

func openHintDismissed() bool {
	_, err := os.Stat(hideOpenHintPath())
	return err == nil
}

func dismissOpenHint() {
	p := hideOpenHintPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("1\n"), 0o644)
}

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
	SetPriority(ctx context.Context, issue linear.Issue, priority int) error
	// UnblockToTriage moves the tickets `issue` was blocking to their team's
	// Triage state (the unblock cascade on Done), returning those moved.
	UnblockToTriage(ctx context.Context, issue linear.Issue) ([]string, error)
}

// assigner is the optional write capability to change an issue's assignee. Live
// Linear implements it; --demo does not.
type assigner interface {
	Users(ctx context.Context) ([]linear.User, error)
	Assign(ctx context.Context, issue linear.Issue, assigneeID string) error
}

// commenter is the optional read capability behind the overlay's investigation
// and plan views, which live in the ticket's Linear comments rather than its
// description. Live Linear implements it; --demo does not.
type commenter interface {
	FetchComments(ctx context.Context, issueID string) ([]linear.Comment, error)
}

// projectFetcher is the optional read capability behind the Projects section.
// Live Linear implements it; a Fetcher that doesn't simply gets no section, and
// every ticket keeps grouping by priority as before.
type projectFetcher interface {
	FetchMyProjects(ctx context.Context) ([]linear.Project, error)
}

// projectWriter is the optional write capability for project rows — the
// project-side equivalents of the ticket `s`, `a`, and `P` hotkeys.
type projectWriter interface {
	SetProjectStatus(ctx context.Context, p linear.Project, target string) error
	SetProjectLead(ctx context.Context, p linear.Project, userID string) error
	SetProjectPriority(ctx context.Context, p linear.Project, priority int) error
}

const refreshEvery = 60 * time.Second

// statusRefreshEvery can be far tighter than refreshEvery because the status
// poll only hits the local backend (herdr socket / on-disk claude agents), never
// the rate-limited Linear API.
const statusRefreshEvery = 3 * time.Second

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
	rowProjectHeader // the "Projects" section header
	rowProject       // one of my Linear projects
)

type row struct {
	kind    rowKind
	text    string             // for headers
	count   int                // for rowPrio/rowSessionHeader/rowProjectHeader: item count
	issue   linear.Issue       // for rowIssue
	ref     session.SessionRef // for rowSession
	project linear.Project     // for rowProject
	indent  int                // extra leading columns (a ticket nested under its project)
}

type refreshedMsg struct {
	issues []linear.Issue
	err    error
}

// projectsMsg carries the projects I lead or belong to. It rides its own
// message rather than refreshedMsg because it is a separate Linear query: a
// projects failure must not blank the ticket list, or vice versa.
type projectsMsg struct {
	projects []linear.Project
	err      error
}

// projectWriteMsg is the result of a project status/lead/priority write.
type projectWriteMsg struct {
	key   string // the project's session key, for tearing its session down
	name  string // the project's name, for the notice
	what  string // "status" | "lead" | "priority"
	value string
	err   error
}

// isTerminalProjectStatus reports whether a project status change takes the
// project off the deck (so its session should be closed), mirroring
// isTerminalTarget for tickets.
func isTerminalProjectStatus(status string) bool {
	return status == "Completed" || status == "Canceled"
}

type statusesMsg struct {
	statuses map[string]session.Status
	owners   map[string]account.Owner // ticket key → the subscription running its session
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

// statusTickMsg drives the fast, status-only poll (session badges + other
// sessions), decoupled from the slow tickMsg that also hits the Linear API.
type statusTickMsg struct{}

// updateAvailableMsg carries a newer release tag found by the startup check.
type updateAvailableMsg struct{ latest string }

// quotaMsg carries each account's latest usage entry: the 5h/7d windows when
// there's a reading, and why there isn't when there isn't. Every account asked
// for comes back — "rate limited" and "token expired" are different problems
// with different fixes, and a missing key can say neither.
type quotaMsg struct{ entries map[string]quota.Entry }

// handoffMsg is the result of moving a session to another subscription.
type handoffMsg struct {
	key string
	to  string
	cmd string // how to open the target's deck (not derivable from its name)
	err error
}

// statusWriteMsg is the result of a MoveState write.
type statusWriteMsg struct {
	key    string
	target string
	err    error
}

// triageCascadeMsg reports the result of the unblock cascade after a ticket is
// marked Done (the tickets it was blocking, moved to Triage).
type triageCascadeMsg struct {
	key   string
	moved []string
	err   error
}

// priorityWriteMsg is the result of a SetPriority write.
type priorityWriteMsg struct {
	key   string
	label string
	err   error
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

// commentsMsg carries a ticket's fetched comments, which back the overlay's
// investigation and plan views.
type commentsMsg struct {
	issueID  string
	key      string
	comments []linear.Comment
	err      error
}

type Model struct {
	fetch         Fetcher
	root          string         // default working dir for new sessions (repos-root fallback)
	dry           bool           // print the launch command instead of running it
	allIssues     []linear.Issue // last fetched issues (for re-grouping on collapse)
	myProjects    []linear.Project
	projects      []linear.Project // myProjects + the ones my tickets belong to, sorted
	projFetch     projectFetcher   // non-nil when projects can be fetched (live Linear)
	projWriter    projectWriter    // non-nil when projects can be written (live Linear)
	projExpanded  map[string]bool  // project key → its tickets are unfolded (default: folded)
	projFolded    bool             // the whole Projects section is folded
	detailProj    *linear.Project  // non-nil = showing the project detail overlay
	rows          []row
	cursor        int             // index into rows; a cursorable row (issue, or collapsed header)
	offset        int             // index of the first visible row (scroll position)
	collapsed     map[string]bool // priority label → manually collapsed (empty = all expanded)
	backend       Backend
	sessions      map[string]session.Status   // ticket key → session status
	statusSince   map[string]time.Time        // ticket key → when its current status was first seen
	owners        map[string]account.Owner    // ticket key → which subscription runs its session
	ownerCol      int                         // width of the account column (0 = one account, nothing to label)
	otherSessions []session.SessionRef        // live sessions not tied to a visible ticket
	demoStatuses  map[string]session.Status   // --demo override; nil in real use
	demoOwners    map[string]account.Owner    // --demo override; nil in real use
	detail        *linear.Issue               // non-nil = showing the description overlay
	detailOffset  int                         // scroll offset within the detail overlay
	detailView    linear.Section              // which body the overlay shows: description, investigation, or plan
	commenter     commenter                   // non-nil when comments can be fetched (live Linear)
	comments      map[string][]linear.Comment // issue id → its comments, fetched on first i/P press
	commentsErr   map[string]error            // issue id → why its comment fetch failed
	commentsBusy  map[string]bool             // issue id → a fetch is in flight
	loading       bool
	underHerdr    bool                   // running as a herdr pane (the persistent deck) — q must not kill it
	writer        statusWriter           // non-nil when the backing Fetcher can write status (live Linear)
	assigner      assigner               // non-nil when the backing Fetcher can change assignee (live Linear)
	statusMenu    bool                   // status-change overlay is open
	statusPend    string                 // chosen target awaiting y/n confirm ("" = still choosing)
	priorityMenu  bool                   // priority-change overlay is open
	assignMenu    bool                   // assignee-picker overlay is open
	assignIssue   linear.Issue           // ticket being reassigned (captured when the picker opens)
	assignProj    *linear.Project        // non-nil = the picker is setting a project's lead, not a ticket's assignee
	assignQuery   string                 // filter text in the assignee picker
	assignCursor  int                    // index into the filtered picker options (0 = Unassign)
	searchMode    bool                   // "/" search input is active (captures typing)
	searchQuery   string                 // active ticket-list filter (key/title substring); persists after leaving searchMode
	prMenu        bool                   // multi-PR picker overlay is open
	prIssue       linear.Issue           // ticket whose PRs are being picked
	prList        []linear.PR            // that ticket's PRs, most-actionable-first
	prCursor      int                    // index into prList
	users         []linear.User          // cached workspace users for the picker
	hideOpenHint  bool                   // user chose "don't show again" for the open-session hint
	openHintSpec  *session.LaunchSpec    // pending launch awaiting the open-session hint
	openHintLabel string                 // ticket key for the pending launch
	updateLatest  string                 // newer release tag, if the startup check found one
	acct          account.Account        // the subscription this deck runs as
	accounts      []account.Account      // Claude subscriptions on this machine, resolved once (globs the fs)
	acctColors    map[string]string      // account name → accent color, so two decks never look alike
	quotas        map[string]quota.Entry // account name → its usage reading (or why there is none), so one deck shows every account's headroom
	handoffKey    string                 // ticket awaiting a hand-off confirm ("" = overlay closed)
	handoffCands  []account.Account      // accounts that ticket can be handed to
	handoffCursor int                    // index into handoffCands
	conflict      *conflict              // non-nil = a launch is held back because another deck runs the ticket
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

// demoOwner lets --demo inject canned session ownership, so the account column
// is visible on a machine that only has one subscription.
type demoOwner interface {
	DemoOwners() map[string]account.Owner
}

func New(f Fetcher, root string, dry bool, backend Backend) Model {
	m := Model{fetch: f, root: root, dry: dry, backend: backend, loading: true, sessions: map[string]session.Status{}, statusSince: map[string]time.Time{}, owners: map[string]account.Owner{}, collapsed: map[string]bool{}, projExpanded: map[string]bool{}, underHerdr: os.Getenv("HERDR_PANE_ID") != "", comments: map[string][]linear.Comment{}, commentsErr: map[string]error{}, commentsBusy: map[string]bool{}}
	// Resolved once, not per frame: All() globs the filesystem, and View runs on
	// every keystroke.
	m.acct = account.Current()
	m.accounts = account.All()
	m.acctColors = account.Colors(m.accounts)
	m.quotas = map[string]quota.Entry{}
	if ds, ok := f.(demoSessioner); ok {
		m.demoStatuses = ds.DemoSessions()
	}
	if dos, ok := f.(demoOtherSessioner); ok {
		m.otherSessions = dos.DemoOtherSessions()
	}
	if dow, ok := f.(demoOwner); ok {
		m.demoOwners = dow.DemoOwners()
		m.owners = m.demoOwners
		// --demo runs on whatever machine it's on, usually a one-subscription
		// one. Give its fabricated accounts real accent colors (appended, so the
		// machine's own accounts keep theirs) or every dot would be the same
		// color and the column would demonstrate nothing.
		accts := m.accounts
		var extra []string
		for _, o := range m.owners {
			if _, known := m.acctColors[o.Name]; !known && !slices.Contains(extra, o.Name) {
				extra = append(extra, o.Name)
			}
		}
		slices.Sort(extra) // map order is random; colors must not shuffle per run
		for _, n := range extra {
			accts = append(accts, account.Account{Name: n})
		}
		m.acctColors = account.Colors(accts)
	}
	m.ownerCol = ownerColWidth(m.accounts, m.owners)
	if w, ok := f.(statusWriter); ok {
		m.writer = w
	}
	if a, ok := f.(assigner); ok {
		m.assigner = a
	}
	if c, ok := f.(commenter); ok {
		m.commenter = c
	}
	if p, ok := f.(projectFetcher); ok {
		m.projFetch = p
	}
	if p, ok := f.(projectWriter); ok {
		m.projWriter = p
	}
	m.hideOpenHint = openHintDismissed()
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
	m = next.(Model)
	if m.projFetch != nil {
		// A projects failure degrades to a deck without the section, exactly as
		// it does in the live TUI — it must not take the whole frame down.
		if ps, err := m.projFetch.FetchMyProjects(context.Background()); err == nil {
			next, _ = m.Update(projectsMsg{projects: ps})
			m = next.(Model)
		}
	}
	return m.View(), nil
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.refresh(), m.refreshProjects(), tick(), checkUpdate(), m.fetchQuota()}
	// Name the OS window/tab after the account, so you can tell two decks apart
	// from the taskbar without focusing either one.
	if m.acct.Name != "" {
		cmds = append(cmds, tea.SetWindowTitle("deck ⦿ "+m.acct.Name))
	}
	// The fast status poll only makes sense against a live backend; --demo/--preview
	// resolve statuses synchronously, so don't spin a ticker there.
	if m.demoStatuses == nil {
		cmds = append(cmds, statusTick())
	}
	return tea.Batch(cmds...)
}

// fetchQuota reads Claude's usage limits (metadata only — no token spend).
// Skipped in --demo. Fails silently.
//
// The read goes through quota.Poll, which serves the machine-wide cache and
// calls the endpoint only for accounts whose reading has aged out — and only
// from one deck at a time. Every deck reads every subscription, so calling
// directly meant decks × accounts requests arriving together against a budget
// already shared with every running Claude Code status line.
func (m Model) fetchQuota() tea.Cmd {
	if m.demoStatuses != nil {
		return nil
	}
	accts := make([]quota.Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		accts = append(accts, quota.Account{Name: a.Name, ConfigDir: a.ConfigDir})
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return quotaMsg{entries: quota.Poll(ctx, accts, time.Now())}
	}
}

// checkUpdate runs the cached, non-blocking "newer release available" check.
func checkUpdate() tea.Cmd {
	return func() tea.Msg {
		if latest := update.Check(Version, time.Now()); latest != "" {
			return updateAvailableMsg{latest: latest}
		}
		return nil
	}
}

func (m Model) refresh() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		issues, err := m.fetch.FetchAssignedOpen(ctx)
		return refreshedMsg{issues: issues, err: err}
	}
}

// refreshProjects fetches the projects I lead or belong to. Nil when the
// backing Fetcher has no project support, which simply leaves the section off.
func (m Model) refreshProjects() tea.Cmd {
	if m.projFetch == nil {
		return nil
	}
	pf := m.projFetch
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		ps, err := pf.FetchMyProjects(ctx)
		return projectsMsg{projects: ps, err: err}
	}
}

// refreshStatuses fetches per-ticket session statuses from the active backend
// (or the demo override). Read-only; no model calls.
func (m Model) refreshStatuses() tea.Cmd {
	keys := m.sessionKeys()
	if m.demoStatuses != nil {
		ds, owners := m.demoStatuses, m.demoOwners
		return func() tea.Msg {
			out := make(map[string]session.Status, len(keys))
			for _, k := range keys {
				out[k] = ds[k]
			}
			return statusesMsg{statuses: out, owners: owners}
		}
	}
	b := m.backend
	cwd := m.root
	// Ownership is only a question with more than one subscription on the
	// machine; a single-account deck skips the peer polling entirely.
	accts := m.accounts
	if len(accts) < 2 {
		accts = nil
	}
	return func() tea.Msg {
		st, err := b.Statuses(keys, cwd)
		return statusesMsg{statuses: st, owners: account.Owners(keys, accts), err: err}
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

// sessionKeys is the set of session keys to poll statuses for: every visible
// ticket, plus every project (whose rows open sessions of their own). It
// deliberately reads allIssues rather than the rendered rows: a search filter or
// a folded group hides rows, and polling only those would make statusesMsg
// replace m.sessions with a partial map — blanking the hidden tickets' badges
// and resetting their time-in-state timers.
func (m Model) sessionKeys() []string {
	vis := linear.FilterVisible(m.allIssues)
	keys := make([]string, 0, len(vis)+len(m.projects))
	for _, is := range vis {
		keys = append(keys, is.Identifier)
	}
	// m.projects is what regroup last rendered, so the polled set and the badged
	// rows can never drift apart.
	for _, p := range m.projects {
		keys = append(keys, p.Key())
	}
	return keys
}

// tick schedules the next background refresh with jitter (BR-2c) so many
// clients don't align on the same second.
func tick() tea.Cmd {
	jitter := time.Duration(rand.Intn(10000)) * time.Millisecond
	return tea.Tick(refreshEvery+jitter, func(time.Time) tea.Msg { return tickMsg{} })
}

// statusTick schedules the next fast status-only poll. No jitter: it only hits
// the local backend, not the shared Linear API, so there's no herd to spread.
func statusTick() tea.Cmd {
	return tea.Tick(statusRefreshEvery, func(time.Time) tea.Msg { return statusTickMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ensureVisible()

	case tickMsg:
		// Quota rides this tick with no schedule of its own: quota.Poll answers
		// from the cache shared by every deck on the machine and decides for
		// itself when an account is old enough to re-read. Asking every tick is
		// also how a deck that skipped a round — another deck held the poll lock —
		// picks up the answer. A per-deck timer here would only put the decks back
		// in lockstep — the state that earns the 429s.
		return m, tea.Batch(m.refresh(), m.refreshProjects(), m.refreshStatuses(), m.refreshSessions(), tick(), m.fetchQuota())

	case statusTickMsg:
		// Deliberately no Linear fetch or quota call — those stay on the slow tick.
		return m, tea.Batch(m.refreshStatuses(), m.refreshSessions(), statusTick())

	case tea.FocusMsg:
		// The deck pane regained focus (e.g. returning from a ticket's tab under
		// herdr) — refresh badges immediately so they're live on arrival rather
		// than up to statusRefreshEvery stale.
		if m.demoStatuses == nil {
			return m, tea.Batch(m.refreshStatuses(), m.refreshSessions())
		}

	case updateAvailableMsg:
		m.updateLatest = msg.latest

	case handoffMsg:
		if msg.err != nil {
			debugLog.Printf("handoff %s → %s failed: %v", msg.key, msg.to, msg.err)
			m.notice = fmt.Sprintf("hand-off failed: %v", msg.err)
			break
		}
		debugLog.Printf("handoff %s → %s ok", msg.key, msg.to)
		m.notice = fmt.Sprintf("%s → ⦿%s ✓ resume it there (%s)", m.label(msg.key), msg.to, msg.cmd)
		// The session is stopped here now, so refresh badges to show it.
		return m, tea.Batch(m.refreshStatuses(), m.refreshSessions())

	case quotaMsg:
		for name, e := range msg.entries {
			// Log the reason once per change, not once per tick: with the poll on
			// every tick, a throttled account would otherwise write a line a minute
			// for as long as it stays throttled.
			if e.Reason != m.quotas[name].Reason && e.Reason != "" {
				debugLog.Printf("quota %s: %s", name, e.Reason)
			}
			// An entry that failed this round still carries the last good reading,
			// so this keeps the bar rather than blanking one that was fine a moment
			// ago — but never overwrites a reading with nothing.
			if e.Usage == nil {
				e.Usage, e.FetchedAt = m.quotas[name].Usage, m.quotas[name].FetchedAt
			}
			m.quotas[name] = e
		}

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

	case projectsMsg:
		if msg.err != nil {
			// Keep the last good project list, exactly as the ticket list does on
			// error — and don't set m.err, which is the ticket list's status line.
			debugLog.Printf("projects refresh error: %v", msg.err)
			break
		}
		m.myProjects = msg.projects
		prevID, _ := m.selectedID()
		prevKey := ""
		if p, ok := m.selectedProject(); ok {
			prevKey = p.Key()
		}
		prevIdx := m.cursor
		m.regroup()
		m.applyDemoStatuses()
		m.restoreCursor(prevID, prevKey, prevIdx)
		m.ensureVisible()
		// New project rows mean new session keys to badge.
		return m, m.refreshStatuses()

	case projectWriteMsg:
		if msg.err != nil {
			debugLog.Printf("project %s %s → %s failed: %v", msg.name, msg.what, msg.value, msg.err)
			m.err = fmt.Errorf("%s %s: %v", msg.name, msg.what, msg.err)
			m.notice = ""
			return m, nil
		}
		debugLog.Printf("project %s %s → %s ok", msg.name, msg.what, msg.value)
		m.notice = fmt.Sprintf("%s → %s ✓", truncCols(msg.name, 40), msg.value)
		m.err = nil
		cmds := []tea.Cmd{m.refreshProjects(), m.refresh()}
		// Finishing a project drops its row off the deck, so tear its session
		// down too — the same courtesy a ticket moved to Done gets. The
		// transcript persists either way.
		if msg.what == "status" && isTerminalProjectStatus(msg.value) && msg.key != "" && isRunning(m.sessions[msg.key]) {
			key, backend := msg.key, m.backend
			cmds = append(cmds, func() tea.Msg {
				out, err := backend.CloseByName(key)
				return detachedDoneMsg{action: "closed " + msg.name, err: err, output: strings.TrimSpace(out)}
			})
		}
		return m, tea.Batch(cmds...)

	case statusesMsg:
		// Ownership is resolved from disk and the peer workspaces, not from the
		// backend, so it stands even when the backend poll failed.
		if msg.owners != nil {
			m.owners = msg.owners
			m.ownerCol = ownerColWidth(m.accounts, m.owners)
		}
		if msg.err != nil {
			debugLog.Printf("status refresh error: %v", msg.err)
		} else {
			now := time.Now()
			for k, v := range msg.statuses {
				if prev, ok := m.sessions[k]; !ok || prev != v {
					m.statusSince[k] = now // status is new or changed → reset the timer
				}
			}
			for k := range m.statusSince { // forget keys no longer reported
				if _, ok := msg.statuses[k]; !ok {
					delete(m.statusSince, k)
				}
			}
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
			// Prefer the command's output; fall back to the error itself so we
			// never surface a useless "(no output)" (e.g. "no running session").
			detail := strings.TrimSpace(msg.output)
			if detail == "" {
				detail = msg.err.Error()
			}
			m.err = fmt.Errorf("%s failed: %s", msg.action, firstLine(detail))
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
		// Unblock cascade: finishing a ticket sends the tickets it was blocking to
		// Triage so they get picked back up.
		if msg.target == "Done" && m.writer != nil {
			if is, ok := m.issueByKey(msg.key); ok {
				w := m.writer
				cmds = append(cmds, func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
					defer cancel()
					moved, err := w.UnblockToTriage(ctx, is)
					return triageCascadeMsg{key: msg.key, moved: moved, err: err}
				})
			}
		}
		return m, tea.Batch(cmds...)

	case triageCascadeMsg:
		if msg.err != nil {
			debugLog.Printf("unblock cascade %s: moved %v err %v", msg.key, msg.moved, msg.err)
		}
		switch {
		case len(msg.moved) > 0:
			m.notice = fmt.Sprintf("%s done · → Triage: %s", msg.key, strings.Join(msg.moved, ", "))
		case msg.err != nil:
			m.err = fmt.Errorf("triage cascade for %s: %v", msg.key, msg.err)
		}
		if len(msg.moved) > 0 {
			return m, tea.Batch(m.refresh(), m.refreshStatuses())
		}
		return m, nil

	case priorityWriteMsg:
		if msg.err != nil {
			debugLog.Printf("priority %s → %s failed: %v", msg.key, msg.label, msg.err)
			m.err = fmt.Errorf("priority %s: %v", msg.key, msg.err)
			m.notice = ""
			return m, nil
		}
		m.notice = fmt.Sprintf("%s → %s ✓", msg.key, msg.label)
		m.err = nil
		// The ticket regroups into its new priority section on refresh.
		return m, tea.Batch(m.refresh(), m.refreshStatuses())

	case commentsMsg:
		delete(m.commentsBusy, msg.issueID)
		if msg.err != nil {
			debugLog.Printf("comments fetch %s error: %v", msg.key, msg.err)
			m.commentsErr[msg.issueID] = msg.err
		} else {
			m.comments[msg.issueID] = msg.comments
		}

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
		if m.detailProj != nil {
			return m.updateProjectDetail(msg)
		}
		if m.statusMenu {
			return m.updateStatus(msg)
		}
		if m.priorityMenu {
			return m.updatePriority(msg)
		}
		if m.assignMenu {
			return m.updateAssign(msg)
		}
		if m.prMenu {
			return m.updatePR(msg)
		}
		if m.handoffKey != "" {
			return m.updateHandoff(msg)
		}
		if m.conflict != nil {
			return m.updateConflict(msg)
		}
		if m.openHintSpec != nil {
			return m.updateOpenHint(msg)
		}
		if m.searchMode {
			return m.updateSearch(msg)
		}
		// A project row takes the same action keys a ticket row does, resolved
		// against the project. Keys it doesn't own (navigation, folding, search,
		// refresh, quit) fall through to the shared handling below.
		if p, ok := m.selectedProject(); ok {
			if next, cmd, handled := m.projectKey(msg, p); handled {
				return next, cmd
			}
		}
		switch msg.String() {
		case "/":
			m.searchMode = true
			m.notice = ""
		case "esc":
			// Clear an applied filter (when not in the input itself).
			if m.searchQuery != "" {
				m.setSearch("")
			}
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
		case "H":
			if is, ok := m.selected(); ok {
				return m.openHandoff(is)
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
		case "P":
			// Open the priority-change menu (write). Unavailable in --demo.
			if _, ok := m.selected(); !ok {
				break
			}
			if m.writer == nil {
				m.notice = "priority changes need a live Linear connection"
				break
			}
			m.priorityMenu = true
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
			// Triage the highlighted ticket in the background: start its session
			// (unfocused) if needed and submit /triage, staying on the deck. On an
			// "other session" row, just submit /triage to it. Runs a Claude turn —
			// a user action, consistent with BR-1 (the app never spends on its own).
			if is, ok := m.selected(); ok {
				return m.triageTicket(is)
			}
			if sr, ok := m.selectedSession(); ok {
				return m.sendToSession(sr.Name, triageCmd)
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

// conflict is a launch held back because another deck is already running that
// ticket's session. It carries the ticket and which action was asked for, so
// "do it anyway" resumes exactly what was interrupted.
type conflict struct {
	issue linear.Issue
	// project is set instead of issue when the held-back launch was a project
	// row. A project session forks exactly as a ticket's does, so it takes the
	// same gate.
	project *linear.Project
	owner   account.Owner
	triage  bool // the ask was `t` (background /triage), not open
}

// name is how to refer to whatever the conflict is about.
func (c conflict) name() string {
	if c.project != nil {
		return truncCols(c.project.Name, 40)
	}
	return c.issue.Identifier
}

// blockingOwner reports the other deck already running this ticket, when
// starting it here would fork the session instead of attaching to it.
//
// A session this deck already runs is never a conflict: Plan attaches to it.
// The peer's status comes from the 3-second ownership poll, so a session
// started elsewhere in the last few seconds can still slip through — this
// catches the mistake people actually make (opening a ticket another deck has
// been working for minutes), not a race.
func (m Model) blockingOwner(key string) (account.Owner, bool) {
	if isRunning(m.sessions[key]) {
		return account.Owner{}, false
	}
	o := m.owners[key]
	if o.Name == "" || o.Name == m.acct.Name || !o.Live || !o.Status.Running() {
		return account.Owner{}, false
	}
	return o, true
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

// launchIssue opens a ticket's session, first checking that no other deck is
// already running it (see renderConflict for why that matters).
func (m Model) launchIssue(is linear.Issue) (tea.Model, tea.Cmd) {
	if o, ok := m.blockingOwner(is.Identifier); ok {
		m.conflict = &conflict{issue: is, owner: o}
		m.notice = ""
		return m, nil
	}
	return m.launchIssueNow(is)
}

// launchIssueNow plans and runs (or dry-prints) the session for a specific
// ticket, so it can be triggered from the list or from the description overlay.
func (m Model) launchIssueNow(is linear.Issue) (tea.Model, tea.Cmd) {
	spec, err := m.backend.Plan(toTicket(is), m.root)
	if err != nil {
		debugLog.Printf("plan error for %s: %v", is.Identifier, err)
		m.err = err
		return m, nil
	}
	// Opening in a herdr tab switches focus away from the deck; show a one-time
	// (dismissable) reminder of how to get back before launching. Skipped for
	// foreground (claude) launches and in --dry mode.
	if !m.dry && !spec.Foreground && !m.hideOpenHint {
		s := spec
		m.openHintSpec = &s
		m.openHintLabel = is.Identifier
		return m, nil
	}
	return m.runSpec(spec, is.Identifier)
}

// updateConflict handles the "another deck is running this" gate. Cancel is the
// default — Enter and Esc both back out — so a reflexive keypress can't be the
// thing that forks a session. Only "o" overrides.
func (m Model) updateConflict(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := *m.conflict
	switch msg.String() {
	case "o":
		m.conflict = nil
		// A project is never held back for triage — `t` doesn't apply to one.
		if c.project != nil {
			return m.launchProjectNow(*c.project)
		}
		if c.triage {
			return m.triageTicketNow(c.issue)
		}
		return m.launchIssueNow(c.issue)
	case "p":
		m.conflict = nil
		if c.project != nil {
			return m.openProjectPRs(*c.project)
		}
		return m.openPRFor(c.issue)
	default:
		m.conflict = nil
		m.notice = c.name() + " left to ⦿" + c.owner.Name + " · " + m.deckCmd(c.owner.Name)
	}
	return m, nil
}

// deckCmd is the command that opens another account's deck, for pointing the
// user at the deck that owns a session. Falls back to the flag form when that
// account isn't in this deck's list (a peer that appeared mid-run).
func (m Model) deckCmd(name string) string {
	for _, a := range m.accounts {
		if a.Name == name {
			return a.LaunchCmd()
		}
	}
	return "deck --account " + name
}

// renderConflict draws the gate shown when another deck already runs the
// selected ticket's session. Two things go wrong if it opens here anyway, and
// the overlay names both: the session id derives from the ticket key alone, so
// each deck ends up appending to its own copy of one transcript (see
// account.HandOff — divergence has no fix), and two agents work the ticket at
// once.
func (m Model) renderConflict() string {
	c := m.conflict
	glyph, _, _ := sessionStyle(c.owner.Status)
	state := statusPhrase(c.owner.Status)
	if e := elapsedLabel(c.owner.LastActive); e != "" {
		state += ", last wrote " + e + " ago"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", titleStyle.Render("Already open on another deck"), idStyle.Render(c.name()))
	fmt.Fprintf(&b, "  %s   %s\n\n", m.acctStyle(c.owner.Name).Bold(true).Render("⦿ "+c.owner.Name), glyph+" "+state)
	// The blank line stays outside the Render: lipgloss pads every line of a
	// multi-line block to the widest one, including a trailing empty line, and
	// then emits no closing newline — so the next line would start mid-row.
	fmt.Fprintf(&b, "%s\n\n", dimStyle.Render(
		"  A session here would be a second one on the same ticket: both decks\n"+
			"  append to their own copy of one transcript, which then diverge with no\n"+
			"  way to merge them, and two agents work the ticket at once."))
	anyway := "open it"
	if c.triage {
		anyway = "run " + triageCmd
	}
	fmt.Fprintf(&b, "%s\n", selStyle.Render(" ⏎  leave it to ⦿"+c.owner.Name+" "))
	fmt.Fprint(&b, "  p  open its PR here instead\n")
	fmt.Fprintf(&b, "  o  %s here anyway\n", anyway)
	fmt.Fprintf(&b, "\n%s\n", dimStyle.Render("  esc  cancel"))
	fmt.Fprint(&b, dimStyle.Render("  work it where it lives:  "+m.deckCmd(c.owner.Name)))
	return b.String()
}

// statusPhrase spells a peer session's status for prose, where the row badge's
// clipped label ("needs input") reads as a fragment.
func statusPhrase(s session.Status) string {
	switch s {
	case session.Working:
		return "working right now"
	case session.NeedsInput:
		return "waiting on input"
	case session.Idle:
		return "idle"
	default:
		_, label, _ := sessionStyle(s)
		return label
	}
}

// updateOpenHint handles the "how to get back" reminder shown before a session
// opens: Enter proceeds (and keeps showing it next time), "d" proceeds and never
// shows it again, Esc cancels the open.
func (m Model) updateOpenHint(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	spec := *m.openHintSpec
	label := m.openHintLabel
	switch msg.String() {
	case "enter":
		m.openHintSpec = nil
		return m.runSpec(spec, label)
	case "d":
		dismissOpenHint()
		m.hideOpenHint = true
		m.openHintSpec = nil
		return m.runSpec(spec, label)
	case "esc", "q":
		m.openHintSpec = nil
		m.notice = "canceled"
	}
	return m, nil
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

// triageTicket runs /triage against a ticket's session in the background,
// starting the session (unfocused) if it isn't running, without leaving the deck.
func (m Model) triageTicket(is linear.Issue) (tea.Model, tea.Cmd) {
	// Triage starts the session when there isn't one here, so it forks a ticket
	// another deck holds just as an open would.
	if o, ok := m.blockingOwner(is.Identifier); ok {
		m.conflict = &conflict{issue: is, owner: o, triage: true}
		m.notice = ""
		return m, nil
	}
	return m.triageTicketNow(is)
}

func (m Model) triageTicketNow(is linear.Issue) (tea.Model, tea.Cmd) {
	m.notice = "triaging " + is.Identifier + " in the background…"
	backend := m.backend
	t := toTicket(is)
	root := m.root
	return m, func() tea.Msg {
		out, err := backend.Triage(t, root)
		return detachedDoneMsg{action: "triage " + is.Identifier, err: err, output: strings.TrimSpace(out)}
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
	case "esc":
		// Back out one level: a section view returns to the description, the
		// description closes the overlay.
		if m.detailView != linear.SectionDescription {
			m.detailView = linear.SectionDescription
			m.detailOffset = 0
			break
		}
		m.closeDetail()
	case "q", "d":
		m.closeDetail()
	case "i":
		return m.showSection(linear.SectionInvestigation)
	case "P":
		return m.showSection(linear.SectionPlan)
	case "r":
		// Only in a section view: the description comes with the ticket list and
		// refreshes on its own tick.
		if m.detailView != linear.SectionDescription {
			return m.refreshSection()
		}
	case "enter":
		// Navigate straight to this ticket's Claude session from its description.
		is := *m.detail
		m.closeDetail()
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
		// Close the description overlay first: with several PRs this opens the
		// picker, and only one overlay renders at a time.
		is := *m.detail
		m.closeDetail()
		return m.openPRFor(is)
	}
	return m, nil
}

// closeDetail dismisses the overlay, resetting it to the description view so it
// reopens where the reader expects rather than mid-section.
func (m *Model) closeDetail() {
	m.detail = nil
	m.detailOffset = 0
	m.detailView = linear.SectionDescription
}

// showSection switches the overlay to a comment-backed view (the /investigate
// summary or the /plan plan), toggling back to the description when that view is
// already up. The ticket's comments are fetched once, on first use — they aren't
// part of the list refresh, so nothing is spent on tickets nobody opens.
func (m Model) showSection(s linear.Section) (tea.Model, tea.Cmd) {
	if m.detailView == s {
		m.detailView = linear.SectionDescription
		m.detailOffset = 0
		return m, nil
	}
	m.detailView = s
	m.detailOffset = 0
	is := *m.detail
	if m.commenter == nil || is.ID == "" {
		return m, nil
	}
	if _, loaded := m.comments[is.ID]; loaded || m.commentsBusy[is.ID] {
		return m, nil
	}
	m.commentsBusy[is.ID] = true
	delete(m.commentsErr, is.ID)
	return m, m.fetchComments(is)
}

// refreshSection re-reads the open ticket's comments. The fetch is cached for
// the life of the deck, and that cache goes stale while you watch: the agent in
// the next tab posts its write-up minutes after you first looked, so a first
// look that found nothing would otherwise keep saying "none yet" until the deck
// restarts.
func (m Model) refreshSection() (tea.Model, tea.Cmd) {
	is := *m.detail
	if m.commenter == nil || is.ID == "" || m.commentsBusy[is.ID] {
		return m, nil
	}
	// Keep the cached comments until the new ones land. Dropping them first meant
	// a Linear blip blanked a write-up that was on screen a moment ago.
	delete(m.commentsErr, is.ID)
	m.commentsBusy[is.ID] = true
	return m, m.fetchComments(is)
}

// fetchComments loads a ticket's Linear comments in the background.
func (m Model) fetchComments(is linear.Issue) tea.Cmd {
	c := m.commenter
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cs, err := c.FetchComments(ctx, is.ID)
		return commentsMsg{issueID: is.ID, key: is.Identifier, comments: cs, err: err}
	}
}

// openPRFor handles the `p` key for a ticket. A single linked PR opens straight
// away; several open the picker, since a ticket's PRs usually span different
// repos and guessing one is as likely to be wrong as right.
func (m Model) openPRFor(is linear.Issue) (tea.Model, tea.Cmd) {
	switch len(is.PRs) {
	case 0:
		m.notice = "no linked PR for " + is.Identifier
		return m, nil
	case 1:
		m.notice = "opened " + is.PRs[0].Label() + " for " + is.Identifier
		return m, openBrowser(is.PRs[0].URL)
	}
	m.prMenu = true
	m.prIssue = is
	m.prList = linear.SortPRs(is.PRs)
	m.prCursor = 0
	return m, nil
}

// updatePR drives the multi-PR picker: move, open one (⏎ or its digit), open
// every PR at once (a), or back out (esc/q/p).
func (m Model) updatePR(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); s {
	case "esc", "q", "p", "ctrl+c":
		m.prMenu = false
		return m, nil
	case "up", "k":
		if m.prCursor > 0 {
			m.prCursor--
		}
	case "down", "j":
		if m.prCursor < len(m.prList)-1 {
			m.prCursor++
		}
	case "g", "home":
		m.prCursor = 0
	case "G", "end":
		m.prCursor = len(m.prList) - 1
	case "a":
		// Open every linked PR — the "review the whole change" case, where a
		// ticket's work is split across repos.
		cmds := make([]tea.Cmd, 0, len(m.prList))
		for _, pr := range m.prList {
			cmds = append(cmds, openBrowser(pr.URL))
		}
		m.prMenu = false
		m.notice = fmt.Sprintf("opened all %d PRs for %s", len(m.prList), m.prIssue.Identifier)
		return m, tea.Batch(cmds...)
	case "enter":
		return m.openPRAt(m.prCursor)
	default:
		// Digit shortcuts: 1-9 open that row directly.
		if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			return m.openPRAt(int(s[0] - '1'))
		}
	}
	return m, nil
}

// openPRAt opens the i-th PR in the picker and closes the overlay.
func (m Model) openPRAt(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(m.prList) {
		return m, nil
	}
	pr := m.prList[i]
	m.prMenu = false
	m.notice = "opened " + pr.Label() + " for " + m.prIssue.Identifier
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
// setSearch applies a new ticket-list filter and rebuilds the visible rows,
// parking the cursor on the first match so the top result is selected.
func (m *Model) setSearch(q string) {
	m.searchQuery = q
	m.regroup()
	m.cursor = m.firstCursorable()
	m.ensureVisible()
}

// updateSearch drives "/" search: type to filter the list live, Backspace to
// edit, Enter to keep the filter and return to list navigation, Esc to clear it
// and exit. Arrow/navigation keys are inert while typing — press Enter first.
func (m Model) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.searchMode = false
		m.setSearch("")
	case "enter":
		// Apply the filter and drop back to navigation (arrows/enter act on rows).
		m.searchMode = false
	case "backspace":
		if r := []rune(m.searchQuery); len(r) > 0 {
			m.setSearch(string(r[:len(r)-1]))
		}
	default:
		if len(msg.Runes) > 0 {
			m.setSearch(m.searchQuery + string(msg.Runes))
		}
	}
	return m, nil
}

// searching reports whether a filter is currently narrowing the list.
func (m Model) searching() bool { return strings.TrimSpace(m.searchQuery) != "" }

// searchMatch reports whether an issue matches the active search query (a
// case-insensitive substring of its key or title). Empty query matches all.
func (m Model) searchMatch(is linear.Issue) bool {
	q := strings.ToLower(strings.TrimSpace(m.searchQuery))
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(is.Identifier), q) ||
		strings.Contains(strings.ToLower(is.Title), q)
}

// visibleIssues is the render source: always non-terminal (FilterVisible), and
// narrowed to the search query when one is active.
func (m Model) visibleIssues() []linear.Issue {
	vis := linear.FilterVisible(m.allIssues)
	if !m.searching() {
		return vis
	}
	out := make([]linear.Issue, 0, len(vis))
	for _, is := range vis {
		if m.searchMatch(is) {
			out = append(out, is)
		}
	}
	return out
}

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
		m.assignMenu, m.assignProj = false, nil
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
		// A project has a lead where a ticket has an assignee; the picker is the
		// same, only the field it writes differs.
		if proj := m.assignProj; proj != nil {
			if id == "" {
				who = "no lead"
			}
			m.assignMenu, m.assignProj = false, nil
			return m.setProjectLead(*proj, id, who)
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

// updatePriority handles the priority-change menu: a single keypress picks a
// priority and writes it (low-risk and reversible, so no separate confirm).
func (m Model) updatePriority(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if proj, ok := m.selectedProject(); ok {
		return m.updateProjectPriority(msg, proj)
	}
	is, ok := m.selected()
	if !ok {
		m.priorityMenu = false
		return m, nil
	}
	p, label, hit := priorityFor(msg.String())
	if !hit {
		if s := msg.String(); s == "esc" || s == "q" || s == "P" {
			m.priorityMenu = false
		}
		return m, nil // ignore other keys; stay in the menu
	}
	m.priorityMenu = false
	m.notice = fmt.Sprintf("setting %s → %s…", is.Identifier, label)
	w := m.writer
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := w.SetPriority(ctx, is, p)
		return priorityWriteMsg{key: is.Identifier, label: label, err: err}
	}
}

// priorityFor maps a menu keypress to a Linear priority and its label. The same
// scale and the same keys serve tickets and projects.
func priorityFor(key string) (priority int, label string, ok bool) {
	switch key {
	case "u":
		return 1, "Urgent", true
	case "h":
		return 2, "High", true
	case "m":
		return 3, "Medium", true
	case "l":
		return 4, "Low", true
	case "0", "n":
		return 0, "No priority", true
	}
	return 0, "", false
}

// keystrokes are the guard against an accidental write.
func (m Model) updateStatus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if proj, ok := m.selectedProject(); ok {
		return m.updateProjectStatus(msg, proj)
	}
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

// browse launches a URL in the default browser (detached). A package var so
// tests can capture what would be opened instead of spawning a browser.
var browse = func(url string) error { return exec.Command("xdg-open", url).Start() }

// openBrowser opens a URL in the default browser (detached).
func openBrowser(url string) tea.Cmd {
	return func() tea.Msg {
		_ = browse(url)
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

// issueByKey finds a fetched issue by its identifier (the list is small).
func (m Model) issueByKey(key string) (linear.Issue, bool) {
	for _, is := range m.allIssues {
		if is.Identifier == key {
			return is, true
		}
	}
	return linear.Issue{}, false
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
	m.reconcileCollapse()
	m.regroup()

	m.applyDemoStatuses()
	m.restoreCursor(prevID, "", prevIdx)
	m.ensureVisible()
}

// applyDemoStatuses resolves --demo's canned session statuses synchronously, so
// --preview shows badges without an event loop. Real backends populate
// m.sessions asynchronously via statusesMsg, and this is a no-op for them. It
// runs after every regroup, because the key set it fills grows when the project
// rows arrive.
func (m *Model) applyDemoStatuses() {
	if m.demoStatuses == nil {
		return
	}
	keys := m.sessionKeys()
	out := make(map[string]session.Status, len(keys))
	for _, k := range keys {
		out[k] = m.demoStatuses[k]
	}
	m.sessions = out
}

// restoreCursor puts the cursor back on the same ticket (prevID), or on the
// same session or project row (prevName), across a rebuild. When that row is
// gone — e.g. the selected ticket was moved to Done and dropped off the list —
// it stays near the old position (prevIdx) rather than snapping to the top, so
// you keep working down the list.
func (m *Model) restoreCursor(prevID, prevName string, prevIdx int) {
	for i, r := range m.rows {
		if prevID != "" && r.kind == rowIssue && r.issue.Identifier == prevID {
			m.cursor = i
			return
		}
		if prevName == "" {
			continue
		}
		if r.kind == rowSession && r.ref.Name == prevName {
			m.cursor = i
			return
		}
		if r.kind == rowProject && r.project.Key() == prevName {
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

// topFocus is how many top tickets stay in focus; priority sections holding
// none of them are auto-folded.
const topFocus = 10

// reconcileCollapse auto-folds priority sections that contain none of the top
// `topFocus` tickets (in the global priority→status→recency order), so the deck
// stays focused on your highest-priority work. Re-run on every refresh, so as
// tickets close/move the folding follows. A section holding at least one top
// ticket stays expanded; empty sections aren't shown at all.
func (m *Model) reconcileCollapse() {
	// Only the tickets the priority sections actually render count toward focus.
	// Project tickets live under their project row, so letting them consume the
	// top-10 slots would fold priority groups over work that isn't shown there.
	groups := linear.GroupByPriorityThenStatus(linear.IssuesOutsideProjects(linear.FilterVisible(m.allIssues), m.deckProjects()))
	inFocus := map[string]bool{}
	n := 0
	for _, g := range groups {
		for _, sb := range g.Statuses {
			for _, is := range sb.Issues {
				// Recently-done tickets linger for visibility but shouldn't consume
				// focus slots or hold a section open on their own.
				if is.IsDone() {
					continue
				}
				if n < topFocus {
					inFocus[g.PrioLabel] = true
				}
				n++
			}
		}
	}
	m.collapsed = map[string]bool{}
	for _, g := range groups {
		if !inFocus[g.PrioLabel] {
			m.collapsed[g.PrioLabel] = true
		}
	}
}

// regroup rebuilds the visible rows from allIssues, honoring collapsed groups.
// A collapsed priority renders as a single header row (with a ticket count) and
// its statuses/issues are omitted.
func (m *Model) regroup() {
	searching := m.searching()
	var rows []row

	// The Projects section leads the deck: a project is the unit you open a
	// session against, and its tickets are deliberately absent from the priority
	// groups below (they hang off their project row instead, see
	// linear.IssuesOutsideProjects). Set m.projects first — it is what tells the
	// groups below which tickets are already spoken for.
	m.projects = m.visibleProjects()
	if len(m.projects) > 0 {
		rows = append(rows, row{kind: rowProjectHeader, text: "Projects", count: len(m.projects)})
		// While searching, always expand so no match hides inside a fold.
		if !m.projFolded || searching {
			for _, p := range m.projects {
				rows = append(rows, row{kind: rowProject, project: p})
				if !m.projExpanded[p.Key()] && !searching {
					continue
				}
				for _, is := range linear.SortProjectIssues(p.Issues) {
					rows = append(rows, row{kind: rowIssue, issue: is, indent: 2})
				}
			}
		}
		rows = append(rows, row{kind: rowSpacer})
	}

	// Defensive BR-2a: never render Done/Cancelled/Duplicate tickets, whatever
	// the source (the Linear client already filters, but --demo and future
	// feeds might not).
	groups := linear.GroupByPriorityThenStatus(linear.IssuesOutsideProjects(m.visibleIssues(), m.projects))
	for gi, g := range groups {
		if gi > 0 {
			rows = append(rows, row{kind: rowSpacer})
		}
		n := 0
		for _, sb := range g.Statuses {
			n += len(sb.Issues)
		}
		rows = append(rows, row{kind: rowPrio, text: g.PrioLabel, count: n})
		// While searching, always expand so no match hides inside a folded group.
		if m.collapsed[g.PrioLabel] && !searching {
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
	// A project's session is represented by its own row, so it isn't "other".
	for _, p := range m.projects {
		visible[p.Key()] = true
	}
	var others []session.SessionRef
	for _, s := range m.otherSessions {
		if !visible[s.Name] {
			others = append(others, s)
		}
	}
	// The "Other sessions" section is off-list context; hide it while searching so
	// results read cleanly (the query targets the ticket list).
	if len(others) > 0 && !searching {
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
	// A collapsed header is a cursor target so it can be expanded — but search
	// force-expands every group without clearing m.collapsed, so during a search
	// those headers must not be targets or the cursor lands on one instead of the
	// first match (leaving Enter a no-op). A project row is always a target: it
	// opens a session, quite apart from folding its tickets.
	switch r.kind {
	case rowIssue, rowSession, rowProject:
		return true
	case rowPrio:
		return m.collapsed[r.text] && !m.searching()
	case rowProjectHeader:
		return m.projFolded && !m.searching()
	}
	return false
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
	// Inside the Projects section, folding means the project (or the whole
	// section), not a priority group — there is no priority header above these
	// rows to act on.
	if m.foldProjects(mode) {
		return
	}
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
	if m.hasOtherQuotaLine() {
		reserved++ // second header line: the other subscriptions' usage
	}
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
	// recently-done tickets linger struck-through, dimmer still.
	doneRowStyle = lipgloss.NewStyle().Faint(true).Strikethrough(true).Foreground(lipgloss.Color("240"))
	// blocked-by note (why a Blocked ticket is stuck).
	blockedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
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
	best := ""
	for _, p := range prs {
		if linear.PRRank(p.State) > linear.PRRank(best) {
			best = p.State
		}
	}
	return best
}

// prMarkCol is the width of the PR indicator column: the glyph plus a count
// digit for multi-PR tickets. Fixed so every issue row stays aligned.
const prMarkCol = 2

// prMark returns the PR indicator and its color: blank for no PR, "⇄ " for one,
// and "⇄2"/"⇄3"/… for a ticket with several (so a multi-PR ticket is visible on
// the row, where `p` opens the picker). "⇄+" past 9 keeps the column 2 wide.
func prMark(prs []linear.PR) (glyph string, color lipgloss.Color) {
	switch n := len(prs); {
	case n == 0:
		return "  ", lipgloss.Color("240")
	case n == 1:
		return "⇄ ", prColor(pickPRState(prs))
	case n < 10:
		return "⇄" + strconv.Itoa(n), prColor(pickPRState(prs))
	default:
		return "⇄+", prColor(pickPRState(prs))
	}
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
	if m.detailProj != nil {
		return m.renderProjectDetail()
	}
	if m.assignMenu {
		return m.renderAssign()
	}
	if m.prMenu {
		return m.renderPRPicker()
	}
	if m.handoffKey != "" {
		return m.renderHandoff()
	}
	if m.conflict != nil {
		return m.renderConflict()
	}
	if m.openHintSpec != nil {
		return m.renderOpenHint()
	}
	var b strings.Builder

	upd := ""
	if m.updateLatest != "" {
		upd = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(fmt.Sprintf("  ⬆ %s available · ticketdeck update", m.updateLatest))
	}
	fmt.Fprintf(&b, "%s%s%s%s%s\n", titleStyle.Render("TicketDeck"), m.accountSegment(), dimStyle.Render(m.titleMeta()), m.quotaSegment(), upd)
	fmt.Fprint(&b, m.otherQuotaLine())

	if m.loading && len(m.rows) == 0 {
		fmt.Fprint(&b, dimStyle.Render("\n  loading tickets…\n"))
		return b.String()
	}
	if len(m.rows) == 0 && m.err == nil {
		empty := "no open tickets assigned to you 🎉"
		if m.searching() {
			empty = fmt.Sprintf("no tickets match %q", m.searchQuery)
		}
		fmt.Fprint(&b, dimStyle.Render("\n  "+empty+"\n"))
		// Keep the footer so an empty search still shows how to clear it.
		fmt.Fprintf(&b, "\n%s", m.footer())
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
			fmt.Fprintf(&b, "%s\n", m.renderIssue(r.issue, i == m.cursor, r.indent))
		case rowProjectHeader:
			fmt.Fprintf(&b, "%s\n", m.renderProjectHeader(r, i == m.cursor))
		case rowProject:
			fmt.Fprintf(&b, "%s\n", m.renderProject(r.project, i == m.cursor))
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

// otherAccounts is the cached subscriptions minus the one this deck runs as. It
// reads the cached list rather than re-globbing, because View calls it.
func (m Model) otherAccounts() []account.Account {
	var out []account.Account
	for _, a := range m.accounts {
		if a.ConfigDir != m.acct.ConfigDir {
			out = append(out, a)
		}
	}
	return out
}

// openHandoff opens the confirm overlay for moving a ticket's session to
// another subscription. Refuses early when there is nothing to move or nowhere
// to move it, so the overlay never appears without a usable action.
func (m Model) openHandoff(is linear.Issue) (tea.Model, tea.Cmd) {
	return m.openHandoffFor(is.Identifier, is.Identifier)
}

// openHandoffFor is openHandoff addressed by session key, so a project row
// hands off exactly as a ticket does — the transcript move is keyed on the
// session id, which knows nothing about which kind of work it holds. label is
// what to call it on screen, since a project's key is a slug nobody would
// recognise.
func (m Model) openHandoffFor(key, label string) (tea.Model, tea.Cmd) {
	if m.demoStatuses != nil {
		m.notice = "hand-off needs a live backend"
		return m, nil
	}
	cands := m.otherAccounts()
	if len(cands) == 0 {
		m.notice = "only one Claude subscription found — see `deck --account` in SETUP.md"
		return m, nil
	}
	if st := m.sessions[key]; st == session.None {
		m.notice = label + " has no session to hand off"
		return m, nil
	}
	m.handoffKey = key
	m.handoffCands = cands
	m.handoffCursor = 0
	m.notice = ""
	return m, nil
}

func (m Model) updateHandoff(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.handoffKey, m.handoffCands, m.handoffCursor = "", nil, 0
	case "up", "k":
		if m.handoffCursor > 0 {
			m.handoffCursor--
		}
	case "down", "j":
		if m.handoffCursor < len(m.handoffCands)-1 {
			m.handoffCursor++
		}
	case "enter":
		return m.confirmHandoff(m.handoffCursor)
	default:
		// 1-9 picks a row directly.
		if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= len(m.handoffCands) {
			return m.confirmHandoff(n - 1)
		}
	}
	return m, nil
}

func (m Model) confirmHandoff(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(m.handoffCands) {
		return m, nil
	}
	key, to := m.handoffKey, m.handoffCands[i]
	m.handoffKey, m.handoffCands, m.handoffCursor = "", nil, 0
	m.notice = fmt.Sprintf("handing %s to ⦿%s…", m.label(key), to.Name)
	return m, m.handOff(key, to)
}

// handOff stops the ticket's session here, then copies its transcript into the
// target account. Stopping first is mandatory: two accounts appending to their
// own copy of one transcript diverge with no way back.
func (m Model) handOff(key string, to account.Account) tea.Cmd {
	backend, from := m.backend, m.acct
	return func() tea.Msg {
		// Best-effort: a stopped session is already fine, and the built-in claude
		// backend has no daemon to stop one through.
		if out, err := backend.CloseByName(key); err != nil {
			debugLog.Printf("handoff %s: close failed (continuing): %v %s", key, err, out)
		}
		if err := account.HandOff(from, to, key); err != nil {
			return handoffMsg{key: key, to: to.Name, err: err}
		}
		return handoffMsg{key: key, to: to.Name, cmd: to.LaunchCmd()}
	}
}

// accountSegment names the Claude subscription this deck runs as. Every deck
// renders one, including the primary: an unlabelled deck is ambiguous with any
// other deck, which is what the badge exists to prevent. The color is
// per-account so two decks never look alike at a glance.
func (m Model) accountSegment() string {
	if m.acct.Name == "" {
		return ""
	}
	return m.acctStyle(m.acct.Name).Bold(true).Render("  ⦿ " + m.acct.Name)
}

// acctStyle is the accent style for one account name.
func (m Model) acctStyle(name string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(m.acctColor(name))
}

// acctColor is one account's accent color, defaulting for a name with no color
// assigned (a peer that appeared between two All() scans).
func (m Model) acctColor(name string) lipgloss.Color {
	c, ok := m.acctColors[name]
	if !ok {
		c = "111"
	}
	return lipgloss.Color(c)
}

// quotaSegment renders this account's Claude 5h/7d usage for the title bar,
// color-coded by utilization, with a coarse "resets in" hint.
func (m Model) quotaSegment() string {
	e := m.quotas[m.acct.Name]
	if e.Usage == nil {
		if e.Reason == "" {
			return ""
		}
		// Say why. A blank here and a blank while the first poll is still in flight
		// look identical, and the two call for opposite responses: wait, or go log
		// that account back in.
		return dimStyle.Render("  ◷ " + e.Reason)
	}
	return dimStyle.Render("  ◷ ") + quotaPair(e.Usage, true) + staleMark(e)
}

// staleMark flags a reading old enough to distrust — the numbers are real but
// predate the last couple of intervals, which is what a rate-limited or
// logged-out account looks like once its last good reading ages.
func staleMark(e quota.Entry) string {
	if !e.Stale(time.Now()) {
		return ""
	}
	return dimStyle.Render(" ~")
}

// otherQuotaLine renders the OTHER subscriptions' usage on its own line under
// the title. This is why every account is read, not just the active one: when
// this one is throttled, the decision you need is whether another has headroom,
// and that answer must not require switching decks to go look.
// hasOtherQuotaLine reports whether the header carries the other-accounts row,
// so viewportHeight can reserve a line for it. Kept beside otherQuotaLine: if
// the two ever disagree the body is sized wrong and the footer scrolls off.
func (m Model) hasOtherQuotaLine() bool { return len(m.otherAccounts()) > 0 }

func (m Model) otherQuotaLine() string {
	var parts []string
	for _, a := range m.accounts {
		if a.Name == m.acct.Name {
			continue
		}
		// An account whose usage can't be read still gets a row. Dropping it made
		// a rate-limited or logged-out subscription look like it wasn't detected
		// at all, which is the opposite of what this line is for.
		e := m.quotas[a.Name]
		body := dimStyle.Render(cmp.Or(e.Reason, "usage unavailable"))
		if e.Usage != nil {
			body = quotaPair(e.Usage, false) + staleMark(e)
		}
		// "⦿ name", spaced exactly as accountSegment renders the active account —
		// without the space this line's names sit one column left of the title
		// bar's and the two rows visibly fail to stack.
		parts = append(parts, m.acctStyle(a.Name).Render("⦿ "+a.Name)+" "+body)
	}
	if len(parts) == 0 {
		return ""
	}
	return "            " + strings.Join(parts, dimStyle.Render("  ")) + "\n"
}

// quotaPair formats one account's two windows. The "resets in" hint is only
// worth its width for the active account; for the others the percentage alone
// answers "is there room over there?".
func quotaPair(u *quota.Usage, withReset bool) string {
	seg := func(label string, pct float64, reset time.Time) string {
		s := fmt.Sprintf("%s %.0f%%", label, pct)
		if withReset {
			if r := resetsIn(reset); r != "" {
				s += " " + r
			}
		}
		return lipgloss.NewStyle().Foreground(quotaColor(pct)).Render(s)
	}
	return seg("5h", u.FiveHourPct, u.FiveHourReset) +
		dimStyle.Render(" · ") + seg("7d", u.SevenDayPct, u.SevenDayReset)
}

func quotaColor(pct float64) lipgloss.Color {
	switch {
	case pct >= 90:
		return lipgloss.Color("203") // red
	case pct >= 70:
		return lipgloss.Color("214") // orange
	case pct >= 50:
		return lipgloss.Color("220") // yellow
	default:
		return lipgloss.Color("42") // green
	}
}

// resetsIn is a coarse "(2h)" / "(45m)" / "(5d)" until t; empty if unknown/past.
func resetsIn(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Until(t)
	if d <= 0 {
		return ""
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("(%dm)", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("(%dh)", int(d.Hours()))
	default:
		return fmt.Sprintf("(%dd)", int(d.Hours())/24)
	}
}

func (m Model) titleMeta() string {
	meta := "  assigned · open only"
	if s, e := m.window(); s > 0 || e < len(m.rows) {
		meta += "  ↕ more" // there is off-screen content
	}
	return meta
}

// cols is a string's width in terminal columns, which is what every layout sum
// here is actually counting. Rune count is the wrong unit: ⛔ (the blocked-by
// note) and any emoji in a Linear title occupy two columns each, so counting
// runes under-measures them and the row overruns its width.
func cols(s string) int { return runewidth.StringWidth(s) }

// truncCols shortens s to at most max columns, ending in "…" when it had to cut.
// Slicing runes would cut mid-glyph and still overrun on a two-column rune.
func truncCols(s string, max int) string {
	if max <= 0 {
		return ""
	}
	return runewidth.Truncate(s, max, "…")
}

// sessionCol is the fixed width of the badge+label column, sized to the widest
// label ("needs input").
const sessionCol = 17 // fits "◆ needs input 20m" (badge + label + elapsed)

// ownerNameMax caps how much of an account name the row column reserves, so one
// long name can't eat the title column.
const ownerNameMax = 8

// ownerColWidth sizes the account column: a ⦿ dot in the owning subscription's
// accent color on every session row, and its name on the selected row only —
// naming every row would spend the title column's width on something that
// rarely changes. The column is reserved at its full width either way, so moving
// the cursor doesn't shift every column to its right; the name grows leftward
// into space the dot already reserved.
//
// 0 when there is nothing to disambiguate (one subscription on the machine), so
// a single-account deck looks exactly as it did.
func ownerColWidth(accts []account.Account, owners map[string]account.Owner) int {
	names := map[string]bool{}
	if len(accts) > 1 {
		for _, a := range accts {
			names[a.Name] = true
		}
	}
	for _, o := range owners {
		names[o.Name] = true
	}
	if len(names) < 2 {
		return 0
	}
	widest := 0
	for n := range names {
		if l := cols(n); l > widest {
			widest = l
		}
	}
	if widest > ownerNameMax {
		widest = ownerNameMax
	}
	return widest + 3 // name + space + ⦿ + trailing gap
}

// ownerCell renders the account column for a row: right-aligned name (selected
// rows only) then the dot, padded to ownerCol. Returns "" when the column is
// off, and blank padding for a ticket with no session under any account.
func (m Model) ownerCell(name string, selected bool) (string, lipgloss.Color) {
	if m.ownerCol == 0 {
		return "", ""
	}
	if name == "" {
		return strings.Repeat(" ", m.ownerCol), ""
	}
	nameCol := m.ownerCol - 3
	label := ""
	if selected {
		// "…" rather than a bare cut: "averylo…" reads as a shortened name,
		// "averylon" reads as a different account.
		label = truncCols(name, nameCol)
	}
	// Pad by columns, not by fmt's %*s — that counts runes, so a two-column
	// glyph in a name would push the dot out of its column.
	return strings.Repeat(" ", nameCol-cols(label)) + label + " ⦿ ", m.acctColor(name)
}

// elapsedLabel formats how long a session has been in its current state, or ""
// for under a minute (so fresh/transient states stay uncluttered).
func elapsedLabel(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := time.Since(since)
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// elapsedInStatus returns the how-long-in-current-state label for a ticket's
// live session (working/idle/needs-input), so long waits (e.g. a session parked
// waiting on CI, or one needing input a while) are visible. Empty otherwise.
func (m Model) elapsedInStatus(key string, st session.Status) string {
	if !isRunning(st) {
		return ""
	}
	return elapsedLabel(m.statusSince[key])
}

// rowSession resolves which session a ticket row badges, and how long it has
// been in that state.
//
// This deck's own backend answers first. When it has nothing, the ownership poll
// still might: `deck --account NAME` isolates each subscription's workspace, so
// a ticket another deck is working reads as untouched here — the exact mistake
// the account dot was added to prevent, only half-solved, because the dot names
// a deck without saying what that deck is doing. remote marks that second case
// (a session that exists, elsewhere), which renders in its deck's color and
// gates an open (see blockingOwner).
//
// The elapsed figure differs by source, and both are the useful one: locally,
// how long the session has held its current state; remotely, how long since it
// last wrote to its transcript — which is what says whether a "working" peer is
// working or wedged.
func (m Model) rowSession(key string) (st session.Status, elapsed string, remote bool) {
	if local := m.sessions[key]; local != session.None {
		return local, m.elapsedInStatus(key, local), false
	}
	o := m.owners[key]
	if o.Name == "" {
		return session.None, "", false
	}
	return o.Status, elapsedLabel(o.LastActive), o.Name != m.acct.Name
}

// renderIssue draws one ticket row. indent is extra leading columns, used to
// nest a ticket under its project row; 0 is a top-level row in a priority group.
func (m Model) renderIssue(is linear.Issue, selected bool, indent int) string {
	pad := strings.Repeat(" ", indent)
	st, elapsed, remote := m.rowSession(is.Identifier)
	cell, color := sessionCellText(st, elapsed)
	own, ownColor := m.ownerCell(m.owners[is.Identifier].Name, selected)
	if remote {
		// Color the badge like its deck, not like its status: the words already say
		// working/idle, and what a glance needs from a remote row is whose it is.
		color = ownColor
	}
	id := fmt.Sprintf("%-9s", is.Identifier)
	prG, prC := prMark(is.PRs)
	tagText, tagColor := validationTag(is.Labels)
	note := blockedNote(is) // "⛔ ZEN-1, ZEN-2" on a Blocked ticket, else ""

	// Truncate the title to what's left after the fixed columns:
	// indent(2) + owner + badge + space + id(9) + space + prmark(2) + space, minus
	// the trailing validation tag and blocked-by note (each with a leading space).
	avail := m.rowWidth() - (2 + indent + m.ownerCol + sessionCol + 1 + 9 + 1 + prMarkCol + 1)
	if tagText != "" {
		avail -= cols(tagText) + 1
	}
	if note != "" {
		avail -= cols(note) + 1
	}
	if avail < 12 {
		avail = 12
	}
	title := truncCols(is.Title, avail)

	if selected {
		// Plain text (no inner colors) so the selection bg spans the whole row.
		content := fmt.Sprintf("%s▶ %s%s %s %s %s", pad, own, cell, id, prG, title)
		if tagText != "" {
			content += " " + tagText
		}
		if note != "" {
			content += " " + note
		}
		style := selStyle
		if is.IsDone() && !is.IsValidate() {
			style = style.Strikethrough(true)
		}
		return style.Width(m.rowWidth()).Render(content)
	}
	tag := ""
	if tagText != "" {
		tag = " " + lipgloss.NewStyle().Foreground(tagColor).Render(tagText)
	}
	noteR := ""
	if note != "" {
		noteR = " " + blockedStyle.Render(note)
	}
	// Recently-done tickets linger struck-through so finished work stays visible
	// for a while without drawing the eye. Strike only the text tokens, not the
	// column gaps or id padding, so the strikethrough tracks the words. Validate
	// is a completed-type state but an active gate, so it's exempt.
	// The account dot keeps its own accent color in every row style below —
	// dimming or striking it would defeat the one thing it is there to say.
	ownR := lipgloss.NewStyle().Foreground(ownColor).Render(own)
	if is.IsDone() && !is.IsValidate() {
		strike := doneRowStyle
		gap := doneRowStyle.Strikethrough(false)
		idText := strings.TrimRight(id, " ")
		idPad := id[len(idText):]
		row := "  " + pad + ownR + strike.Render(cell) + gap.Render(" ") +
			strike.Render(idText) + gap.Render(idPad+" ") +
			strike.Render(prG) + gap.Render(" ") +
			strike.Render(title)
		return row + tag
	}
	// Working tickets are already being handled — de-emphasize the whole row
	// (uniform dim, no cyan id / bright title) so the eye is drawn to the
	// tickets that still need attention. A peer deck's working row is
	// de-emphasized for the same reason, but its badge keeps the deck color:
	// "someone else is on this" is exactly what must survive the dim.
	if st == session.Working {
		badge := workingRowStyle.Render(cell)
		if remote {
			badge = lipgloss.NewStyle().Foreground(ownColor).Render(cell)
		}
		return "  " + pad + ownR + badge + workingRowStyle.Render(fmt.Sprintf(" %s %s %s", id, prG, title)) + tag + noteR
	}
	badge := lipgloss.NewStyle().Foreground(color).Render(cell)
	pr := lipgloss.NewStyle().Foreground(prC).Render(prG)
	return fmt.Sprintf("  %s%s%s %s %s %s", pad, ownR, badge, idStyle.Render(id), pr, title) + tag + noteR
}

// blockedNote returns a compact "⛔ blocker keys" note for a Blocked ticket that
// has open blockers (up to 3 keys, then "+N"), else "". This is the display of
// "which tickets it's blocked by".
func blockedNote(is linear.Issue) string {
	if !strings.EqualFold(is.StateName, "Blocked") {
		return ""
	}
	blockers := is.OpenBlockers()
	if len(blockers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(blockers))
	for _, b := range blockers {
		keys = append(keys, b.Identifier)
	}
	extra := 0
	if len(keys) > 3 {
		extra = len(keys) - 3
		keys = keys[:3]
	}
	note := "⛔ " + strings.Join(keys, ", ")
	if extra > 0 {
		note += fmt.Sprintf(" +%d", extra)
	}
	return note
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

// renderSession renders an "other sessions" row: its status badge + name. These
// come from this deck's own backend, so the account column always names this
// subscription — the peers' equivalents show up on their own decks.
func (m Model) renderSession(ref session.SessionRef, selected bool) string {
	cell, color := sessionCellText(ref.Status, "")
	own, ownColor := m.ownerCell(m.acct.Name, selected)
	name := ref.Name
	avail := m.rowWidth() - (2 + m.ownerCol + sessionCol + 1)
	if avail > 0 {
		name = truncCols(name, avail)
	}
	if selected {
		return selStyle.Width(m.rowWidth()).Render(fmt.Sprintf("▶ %s%s %s", own, cell, name))
	}
	badge := lipgloss.NewStyle().Foreground(color).Render(cell)
	return fmt.Sprintf("  %s%s %s", lipgloss.NewStyle().Foreground(ownColor).Render(own), badge, name)
}

// sessionCellText returns the badge + status label as plain text padded to
// sessionCol (so the id column stays aligned), plus its color. Splitting text
// from color lets selected rows render a clean full-width highlight.
func sessionCellText(s session.Status, elapsed string) (string, lipgloss.Color) {
	glyph, label, color := sessionStyle(s)
	text := glyph
	if label != "" {
		text += " " + label
	}
	if elapsed != "" {
		text += " " + elapsed
	}
	if pad := sessionCol - cols(text); pad > 0 {
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

// mdRenderer caches the glamour renderer by width. It is created once (not per
// frame) and uses a fixed style — never glamour.WithAutoStyle, which does a
// BLOCKING terminal background-color query that deadlocks with Bubble Tea's
// input reader under a herdr PTY (the overlay would hang, ignoring all keys).
var (
	mdRenderer *glamour.TermRenderer
	mdWidth    int
)

// renderMarkdown renders a ticket's markdown description to styled, word-wrapped
// ANSI via glamour. Falls back to the raw text if rendering fails.
func renderMarkdown(md string, width int) string {
	if width < 20 {
		width = 20
	}
	if mdRenderer == nil || mdWidth != width {
		r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width))
		if err != nil {
			return md
		}
		mdRenderer, mdWidth = r, width
	}
	out, err := mdRenderer.Render(md)
	if err != nil {
		return md
	}
	return out
}

// RenderMarkdown renders markdown to styled, word-wrapped ANSI at the given
// width, reusing the cached renderer. Exported for the `ticketdeck describe`
// subcommand (which shares the deck's description rendering).
func RenderMarkdown(md string, width int) string { return renderMarkdown(md, width) }

// DescribeText formats a ticket's header (key, title, status, priority, links)
// followed by its rendered markdown description, for `ticketdeck describe`. It
// mirrors the deck's `d` overlay but as plain output suitable for a pager/popup.
func DescribeText(is linear.Issue, width int) string {
	var b strings.Builder
	title := lipgloss.NewStyle().Bold(true).Render(is.Identifier + "  " + is.Title)
	b.WriteString(title + "\n")

	meta := is.StateName
	if is.PrioLabel != "" {
		meta += " · " + is.PrioLabel
	}
	if is.TeamName != "" {
		meta += " · " + is.TeamName
	}
	b.WriteString(lipgloss.NewStyle().Faint(true).Render(meta) + "\n")
	if is.URL != "" {
		b.WriteString(is.URL + "\n")
	}
	for _, pr := range is.PRs {
		b.WriteString("PR: " + pr.URL + "\n")
	}
	b.WriteString("\n")

	body := strings.TrimSpace(is.Description)
	if body == "" {
		b.WriteString("(no description)\n")
		return b.String()
	}
	b.WriteString(renderMarkdown(body, width))
	return b.String()
}

// renderDetail draws the description overlay for the selected ticket.
// renderOpenHint draws the "how to get back" reminder shown before a ticket
// session opens in its own tab.
func (m Model) renderOpenHint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", titleStyle.Render("Opening"), idStyle.Render(m.openHintLabel))
	fmt.Fprintf(&b, "%s opens in its own tab. To come back to this ticket list:\n\n", m.openHintLabel)
	fmt.Fprintf(&b, "    %s  — jump straight to the deck (tab 1)\n", noticeStyle.Render("Ctrl+b 1"))
	fmt.Fprintf(&b, "    %s  — search all tabs by name\n\n", noticeStyle.Render("Ctrl+b g"))
	fmt.Fprintf(&b, "%s\n", selStyle.Render(" ⏎  OK "))
	fmt.Fprintf(&b, "%s\n", "  d  OK, don't show this again")
	fmt.Fprintf(&b, "\n%s", dimStyle.Render("esc  cancel"))
	return b.String()
}

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

// renderHandoff draws the hand-off confirm: which subscription the ticket's
// session moves to, and what that costs. It spells out the two things that
// surprise people — the session stops here, and resuming replays context rather
// than continuing a live process.
func (m Model) renderHandoff() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", titleStyle.Render("Hand off session"), idStyle.Render(m.label(m.handoffKey)))
	fmt.Fprintf(&b, "%s\n\n", dimStyle.Render("↑/↓ select · ⏎ hand off · 1-9 pick · esc cancel"))

	fmt.Fprintf(&b, "  from  %s\n\n", m.acctStyle(m.acct.Name).Bold(true).Render("⦿ "+m.acct.Name))
	for i, a := range m.handoffCands {
		num := " "
		if i < 9 {
			num = strconv.Itoa(i + 1)
		}
		row := fmt.Sprintf("%s  to    %s", num, "⦿ "+a.Name)
		if e := m.quotas[a.Name]; e.Usage != nil {
			row += fmt.Sprintf("   %s", quotaPair(e.Usage, true)+staleMark(e))
		}
		if i == m.handoffCursor {
			fmt.Fprintf(&b, "%s\n", selStyle.Render("▶ "+row))
		} else {
			fmt.Fprintf(&b, "  %s\n", row)
		}
	}

	warn := ""
	if m.sessions[m.handoffKey] == session.Working {
		warn = "\n  ⚠ that session is working right now — handing off interrupts it"
	}
	fmt.Fprintf(&b, "\n%s%s\n", dimStyle.Render(
		"  stops the session here, copies its transcript over, and it becomes\n"+
			"  resumable in that deck. Context carries; an in-flight tool call does not."), warn)
	return b.String()
}

// renderPRPicker draws the multi-PR picker: one row per linked PR, ordered
// most-actionable-first, each showing its state, repo#number, and title.
func (m Model) renderPRPicker() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s%s\n", titleStyle.Render("Pull requests"), idStyle.Render(m.prIssue.Identifier), dimStyle.Render("  "+m.prIssue.Title))
	fmt.Fprintf(&b, "%s\n\n", dimStyle.Render("↑/↓ select · ⏎ open · 1-9 open that one · a open all · esc cancel"))

	// Fixed columns so the titles line up: state ("merged" is widest) and
	// repo#number, sized to the widest label in this ticket's set.
	const stateCol = 6
	labelCol := 0
	for _, pr := range m.prList {
		if n := cols(pr.Label()); n > labelCol {
			labelCol = n
		}
	}
	for i, pr := range m.prList {
		num := " "
		if i < 9 {
			num = strconv.Itoa(i + 1)
		}
		state := pr.State
		if state == "" {
			state = "?"
		}
		label := pr.Label()
		title := pr.Title
		if title == "" {
			title = pr.URL
		}
		// Trim the title to the space left after the fixed columns:
		// indent(2) + digit(1) + space + "⇄ " + state + space + label + 2 gap.
		avail := m.width - (2 + 1 + 1 + 2 + stateCol + 1 + labelCol + 2)
		if m.width <= 0 || avail < 12 {
			avail = 12
		}
		title = truncCols(title, avail)

		if i == m.prCursor {
			content := fmt.Sprintf("▶ %s ⇄ %-*s %-*s  %s", num, stateCol, state, labelCol, label, title)
			fmt.Fprintf(&b, "%s\n", selStyle.Width(m.rowWidth()).Render(content))
			continue
		}
		fmt.Fprintf(&b, "  %s %s %-*s  %s\n",
			dimStyle.Render(num),
			lipgloss.NewStyle().Foreground(prColor(pr.State)).Render(fmt.Sprintf("⇄ %-*s", stateCol, state)),
			labelCol, label,
			dimStyle.Render(title))
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

	st := m.sessions[is.Identifier]
	glyph, label, color := sessionStyle(st)
	sess := lipgloss.NewStyle().Foreground(color).Render(glyph + " " + label)
	if e := m.elapsedInStatus(is.Identifier, st); e != "" {
		sess += dimStyle.Render(" · " + e + " in this state")
	}
	if label == "" {
		sess = dimStyle.Render("· no session yet — press ⏎ to start one")
	}
	fmt.Fprintf(&b, "%s\n", sess)
	if tagText, tagColor := validationTag(is.Labels); tagText != "" {
		fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Foreground(tagColor).Render(tagText))
	}
	if blockers := is.OpenBlockers(); len(blockers) > 0 {
		keys := make([]string, 0, len(blockers))
		for _, r := range blockers {
			keys = append(keys, r.Identifier)
		}
		fmt.Fprintf(&b, "%s\n", blockedStyle.Render("⛔ blocked by "+strings.Join(keys, ", ")))
	}
	if is.URL != "" {
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(is.URL))
	}
	prs := linear.SortPRs(is.PRs)
	labelCol := 0
	for _, pr := range prs {
		if n := cols(pr.Label()); n > labelCol {
			labelCol = n
		}
	}
	for _, pr := range prs {
		state := pr.State
		if state == "" {
			state = "PR"
		}
		icon := lipgloss.NewStyle().Foreground(prColor(pr.State)).Render(fmt.Sprintf("⇄ %-6s", state))
		title := pr.Title
		if title == "" {
			title = pr.URL
		}
		fmt.Fprintf(&b, "%s %-*s %s\n", icon, labelCol, pr.Label(), dimStyle.Render(title))
	}
	if len(is.PRs) > 1 {
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(fmt.Sprintf("  p → pick one of %d PRs", len(is.PRs))))
	}
	fmt.Fprint(&b, "\n")

	width := m.width
	if width <= 0 {
		width = 80
	}
	lines := strings.Split(strings.TrimRight(m.detailBody(*is, width-2), "\n"), "\n")

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
	if m.commenter != nil {
		hint += " · i investigation · P plan"
	}
	if m.detailView != linear.SectionDescription {
		hint += " · r refresh"
	}
	hint += " · d/esc back"
	fmt.Fprintf(&b, "\n%s", dimStyle.Render("↑/↓ scroll · "+hint+more))
	return b.String()
}

// detailBody renders the overlay's active view: the ticket description, or the
// /investigate or /plan comment, each with a line naming who wrote it and when.
func (m Model) detailBody(is linear.Issue, width int) string {
	if m.detailView == linear.SectionDescription {
		desc := strings.TrimSpace(is.Description)
		if desc == "" {
			return dimStyle.Render("(no description)")
		}
		return renderMarkdown(desc, width)
	}

	name := m.detailView.Label()
	if m.commenter == nil {
		return dimStyle.Render("the " + name + " needs a live Linear connection")
	}
	// A reading already on screen outranks both the spinner and the error: a
	// refresh that fails must not blank the write-up you were reading. The failure
	// goes in the byline instead.
	_, loaded := m.comments[is.ID]
	if !loaded && m.commentsBusy[is.ID] {
		return dimStyle.Render("loading comments…")
	}
	c, ok := linear.FindSection(m.comments[is.ID], m.detailView)
	if !ok {
		if err := m.commentsErr[is.ID]; err != nil {
			return errStyle.Render("couldn't load comments: " + err.Error())
		}
		return dimStyle.Render("no " + name + " comment on " + is.Identifier + " yet")
	}
	byline := name
	if c.Author != "" {
		byline += " · " + c.Author
	}
	if !c.CreatedAt.IsZero() {
		byline += " · " + c.CreatedAt.Local().Format("2 Jan 15:04")
	}
	if err := m.commentsErr[is.ID]; err != nil {
		byline += " · refresh failed: " + err.Error()
	}
	return dimStyle.Render(byline) + "\n" + renderMarkdown(linear.StripMarkers(c.Body), width)
}

func (m Model) footer() string {
	if m.statusMenu {
		if p, ok := m.selectedProject(); ok {
			name := truncCols(p.Name, 30)
			if m.statusPend == "" {
				return noticeStyle.Render(fmt.Sprintf("  move %s →  p Planned · i In Progress · b Blocked · d Completed · c Cancel · esc", name))
			}
			return noticeStyle.Render(fmt.Sprintf("  move %s → %s?   y confirm · esc cancel", name, m.statusPend))
		}
		is, _ := m.selected()
		if m.statusPend == "" {
			return noticeStyle.Render(fmt.Sprintf("  move %s →  d Done · v Validate · m Monitoring · b Blocked · c Cancel · esc", is.Identifier))
		}
		return noticeStyle.Render(fmt.Sprintf("  move %s → %s?   y confirm · esc cancel", is.Identifier, m.statusPend))
	}
	if m.priorityMenu {
		subject := ""
		if p, ok := m.selectedProject(); ok {
			subject = truncCols(p.Name, 30)
		} else if is, ok := m.selected(); ok {
			subject = is.Identifier
		}
		return noticeStyle.Render(fmt.Sprintf("  priority %s →  u Urgent · h High · m Medium · l Low · 0 None · esc", subject))
	}
	if m.searchMode {
		return noticeStyle.Render(fmt.Sprintf("  /%s▏   type to filter · ⏎ apply · esc clear", m.searchQuery))
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
		statusHint = "s status · P prio · "
	}
	if m.assigner != nil {
		statusHint += "a assign · "
	}
	// Advertise the count when the cursor row has several PRs, so it's clear `p`
	// opens a picker rather than one guessed link. A project's PRs are the union
	// across its tickets.
	prHint := "p PR"
	if is, ok := m.selected(); ok && len(is.PRs) > 1 {
		prHint = fmt.Sprintf("p PRs (%d)", len(is.PRs))
	}
	if p, ok := m.selectedProject(); ok {
		if n := len(linear.ProjectPRs(p)); n > 1 {
			prHint = fmt.Sprintf("p PRs (%d)", n)
		}
		// Name the project-side field each shared write lands on, so `s`/`a` on a
		// project row don't read as the ticket actions they sit beside.
		statusHint = ""
		if m.projWriter != nil {
			statusHint = "s status · P prio · a lead · "
		}
	}
	filterHint := ""
	if m.searchQuery != "" {
		filterHint = fmt.Sprintf("filter %q · esc clear · ", m.searchQuery)
	}
	// Only advertise hand-off when there is somewhere to hand off to, and name
	// the destination when there's exactly one — the common two-subscription case.
	handoffHint := ""
	switch others := m.otherAccounts(); len(others) {
	case 0:
	case 1:
		handoffHint = "H hand to ⦿" + others[0].Name + " · "
	default:
		handoffHint = "H hand off · "
	}
	// `t` only applies to a ticket, so a project row doesn't offer it.
	triageHint := "t " + triageCmd + " · "
	if _, ok := m.selectedProject(); ok {
		triageHint = ""
	}
	help := dimStyle.Render(fmt.Sprintf("%s↑↓ move · ⏎ open · d desc · o web · %s · %s%s%s/ search · n new · ␣/←→ fold · r refresh · %s · synced %s", filterHint, prHint, triageHint, statusHint, handoffHint, quitHint, sync))
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
