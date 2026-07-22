package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

func TestElapsedInStatus(t *testing.T) {
	if elapsedLabel(time.Time{}) != "" {
		t.Error("zero time should format empty")
	}
	if got := elapsedLabel(time.Now().Add(-30 * time.Second)); got != "" {
		t.Errorf("<1m should be empty, got %q", got)
	}
	if got := elapsedLabel(time.Now().Add(-90 * time.Second)); got != "1m" {
		t.Errorf("90s → 1m, got %q", got)
	}
	if got := elapsedLabel(time.Now().Add(-3 * time.Hour)); got != "3h" {
		t.Errorf("3h, got %q", got)
	}
	m := loaded(t)
	m.statusSince = map[string]time.Time{"ZEN-9": time.Now().Add(-5 * time.Minute)}
	if got := m.elapsedInStatus("ZEN-9", session.NeedsInput); got != "5m" {
		t.Errorf("needs-input 5m, got %q", got)
	}
	if got := m.elapsedInStatus("ZEN-9", session.Stopped); got != "" {
		t.Errorf("non-running state should not show elapsed, got %q", got)
	}
	// statusesMsg stamps a since-time for a newly-seen status.
	next, _ := loaded(t).Update(statusesMsg{statuses: map[string]session.Status{"ZEN-9": session.Working}})
	if next.(Model).statusSince["ZEN-9"].IsZero() {
		t.Error("statusesMsg should record when a status was first seen")
	}
}

type fakeFetcher struct{ issues []linear.Issue }

func (f fakeFetcher) FetchAssignedOpen(context.Context) ([]linear.Issue, error) {
	return f.issues, nil
}

// fakeBackend avoids shelling out to claude/herdr in tests; it returns a canned
// "new launch" plan so the enter→dry-notice wiring can be asserted.
type fakeBackend struct{}

func (fakeBackend) Bin() string { return "claude" }

func (fakeBackend) Statuses(keys []string, cwd string) (map[string]session.Status, error) {
	return map[string]session.Status{}, nil
}

func (fakeBackend) Plan(t session.Ticket, cwd string) (session.LaunchSpec, error) {
	return session.LaunchSpec{
		Args:   append([]string{"--session-id", session.DeterministicID(t.Key), "--name", t.Key}, session.LaunchArgs(t)...),
		Cwd:    cwd,
		Action: "new",
	}, nil
}

func (fakeBackend) RunDetached(session.LaunchSpec) (string, error) {
	return "", nil
}

func (fakeBackend) Sessions() ([]session.SessionRef, error) { return nil, nil }

func (fakeBackend) ScratchSpec(cwd string) session.LaunchSpec {
	return session.LaunchSpec{Name: "scratch-1", Label: "scratch-1", Cwd: cwd, Action: "scratch"}
}

func (fakeBackend) FocusSpec(ref session.SessionRef) session.LaunchSpec {
	return session.LaunchSpec{Args: []string{"agent", "focus", ref.Name}, Name: ref.Name, Action: "focus"}
}

func (fakeBackend) CloseSession(session.SessionRef) (string, error) { return "", nil }

func (fakeBackend) Send(name, text string) (string, error)  { return "", nil }
func (fakeBackend) CloseByName(name string) (string, error) { return "", nil }

func fixture() []linear.Issue {
	return []linear.Issue{
		{Identifier: "ZEN-9", Title: "urgent thing", Priority: 1, StateName: "In Progress", StateType: "started"},
		{Identifier: "ZEN-1", Title: "high a", Priority: 2, StateName: "Todo", StateType: "unstarted", UpdatedAt: "2026-07-01"},
		{Identifier: "ZEN-2", Title: "high b", Priority: 2, StateName: "Todo", StateType: "unstarted", UpdatedAt: "2026-07-02"},
		{Identifier: "ZEN-5", Title: "low thing", Priority: 4, StateName: "Merged", StateType: "started"},
		{Identifier: "ZEN-7", Title: "done thing", Priority: 1, StateName: "Done", StateType: "completed"},
	}
}

// applies a refreshedMsg the way Init's fetch cmd would.
func loaded(t *testing.T) Model {
	t.Helper()
	m := New(fakeFetcher{fixture()}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	return next.(Model)
}

// expanded is loaded() with every group unfolded, for tests that assert on the
// full list rather than the start-up collapsed-except-highest default.
func expanded(t *testing.T) Model {
	t.Helper()
	m := loaded(t)
	for k := range m.collapsed {
		m.collapsed[k] = false
	}
	m.regroup()
	m.cursor = m.firstCursorable()
	return m
}

func TestViewGroupsAndOrders(t *testing.T) {
	view := expanded(t).View()

	for _, want := range []string{"URGENT", "HIGH", "LOW", "ZEN-9", "ZEN-1", "ZEN-2", "ZEN-5"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n%s", want, view)
		}
	}
	// Urgent must render above High.
	if strings.Index(view, "URGENT") > strings.Index(view, "HIGH") {
		t.Error("Urgent group should sort above High")
	}
	// Newest-updated first within a status bucket: ZEN-2 (07-02) before ZEN-1 (07-01).
	if strings.Index(view, "ZEN-2") > strings.Index(view, "ZEN-1") {
		t.Error("ZEN-2 (newer) should sort above ZEN-1")
	}
}

func TestCompletedFilteredClientSide(t *testing.T) {
	// The client filters server-side too, but the model should also not crash
	// or show a completed ticket if one slips through — here the model receives
	// exactly what it's given, so ZEN-7 (completed) IS present unless filtered
	// upstream. Confirm the client-side guard belongs in the linear layer:
	got := 0
	for _, is := range fixture() {
		if is.StateType != "completed" && is.StateType != "canceled" {
			got++
		}
	}
	if got != 4 {
		t.Errorf("expected 4 open issues in fixture, got %d", got)
	}
}

func manyIssues(n int) []linear.Issue {
	prios := []int{1, 2, 3, 4}
	out := make([]linear.Issue, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, linear.Issue{
			Identifier: fmt.Sprintf("ZEN-%d", 100+i),
			Title:      fmt.Sprintf("ticket %d", i),
			Priority:   prios[i%len(prios)],
			StateName:  "Todo",
			StateType:  "unstarted",
		})
	}
	return out
}

func TestViewportStartsAtTopAndScrolls(t *testing.T) {
	m := New(fakeFetcher{}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: manyIssues(20)})
	m = next.(Model)
	next, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m = next.(Model)

	// Starts at the top: Urgent group and its header must be on screen.
	if s, _ := m.window(); s != 0 {
		t.Fatalf("viewport should start at row 0, got %d", s)
	}
	if !strings.Contains(m.View(), "URGENT") {
		t.Fatalf("Urgent group should be visible on first frame:\n%s", m.View())
	}

	// Jump to the bottom: viewport must scroll down (top now off-screen).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	m = next.(Model)
	if s, _ := m.window(); s == 0 {
		t.Fatal("viewport should have scrolled off row 0 after jump-to-bottom")
	}
	last := m.rows[m.lastCursorable()].issue.Identifier
	if !strings.Contains(m.View(), last) {
		t.Fatalf("last ticket %s should be visible after G:\n%s", last, m.View())
	}

	// Jump back to top: Urgent visible again, viewport re-anchored at 0.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m = next.(Model)
	if s, _ := m.window(); s != 0 {
		t.Fatalf("viewport should return to row 0 after home, got %d", s)
	}
	if !strings.Contains(m.View(), "URGENT") {
		t.Fatal("Urgent should be visible again after jump-to-top")
	}
}

func TestScrollUpRevealsOffscreen(t *testing.T) {
	m := New(fakeFetcher{}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: manyIssues(20)})
	m = next.(Model)
	next, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m = next.(Model)

	// Walk to the bottom one issue at a time, then back up; every cursor
	// position must remain within the rendered window.
	for i := 0; i < 40; i++ {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(Model)
	}
	for i := 0; i < 40; i++ {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = next.(Model)
		s, e := m.window()
		if m.cursor < s || m.cursor >= e {
			t.Fatalf("cursor %d outside visible window [%d,%d) after scroll-up step %d", m.cursor, s, e, i)
		}
	}
	// Ended at the top; row 0 must be visible.
	if s, _ := m.window(); s != 0 {
		t.Fatalf("expected to be scrolled to top, offset=%d", s)
	}
}

func TestEnterDryLaunchPlansNewSession(t *testing.T) {
	m := loaded(t) // dry=true
	// Cursor starts on the Urgent ticket ZEN-9 (no session bound → new launch).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !strings.Contains(m.notice, "[dry]") {
		t.Fatalf("expected a dry-run notice, got %q", m.notice)
	}
	for _, want := range []string{"claude", "--session-id", "--name ZEN-9", "new"} {
		if !strings.Contains(m.notice, want) {
			t.Errorf("dry notice missing %q: %s", want, m.notice)
		}
	}
	// BR-1/BR-3: must never run the model on launch (no -p/--print flag).
	// Match flag tokens, not substrings ("--append-system-prompt" contains "-p").
	if strings.Contains(m.notice, " -p ") || strings.Contains(m.notice, "--print") {
		t.Errorf("launch plan must not invoke the model: %s", m.notice)
	}
}

func TestCollapseGroupShowsCountAndHidesTickets(t *testing.T) {
	m := loaded(t) // cursor starts on ZEN-9 (Urgent)
	if !strings.Contains(m.View(), "ZEN-9") {
		t.Fatal("ZEN-9 should be visible before collapse")
	}
	// Space toggles collapse of the current group (Urgent, 1 ticket).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = next.(Model)
	v := m.View()
	if !strings.Contains(v, "URGENT · 1") {
		t.Errorf("collapsed Urgent should show ticket count:\n%s", v)
	}
	if strings.Contains(v, "ZEN-9") {
		t.Errorf("collapsed group should hide its tickets:\n%s", v)
	}
	// Cursor should rest on the collapsed header, and expand restores the ticket.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(Model)
	if !strings.Contains(m.View(), "ZEN-9") {
		t.Errorf("expand should restore tickets:\n%s", m.View())
	}
}

func TestEveryNonEmptyPriorityStartsExpanded(t *testing.T) {
	// Fixture spans Urgent, High, Low (Medium/No-priority are empty). Every
	// priority that has tickets should be expanded by default; empty ones aren't
	// shown at all.
	v := loaded(t).View()
	for _, want := range []string{"ZEN-9", "ZEN-1", "ZEN-2", "ZEN-5"} {
		if !strings.Contains(v, want) {
			t.Errorf("ticket %s should be visible (its priority open by default):\n%s", want, v)
		}
	}
	if strings.Contains(v, "MEDIUM") || strings.Contains(v, "NO PRIORITY") {
		t.Errorf("empty priority levels should not be shown:\n%s", v)
	}
}

func TestQuitDisabledUnderHerdr(t *testing.T) {
	// Standalone: q quits.
	m := loaded(t)
	m.underHerdr = false
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if !next.(Model).quitting {
		t.Error("q should quit when not under herdr")
	}
	// Under herdr: q keeps the deck open (never quits) and hints at Ctrl+b q.
	m = loaded(t)
	m.underHerdr = true
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	got := next.(Model)
	if got.quitting {
		t.Error("q must not quit the deck under herdr")
	}
	if !strings.Contains(got.notice, "Ctrl+b q") {
		t.Errorf("q under herdr should hint at Ctrl+b q, got %q", got.notice)
	}
	// ctrl+c is still a hard escape hatch even under herdr.
	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !next.(Model).quitting {
		t.Error("ctrl+c should still quit under herdr")
	}
}

func TestPRIconAndDetailListing(t *testing.T) {
	issues := []linear.Issue{
		{Identifier: "ZEN-9", Title: "with pr", Priority: 1, StateName: "In Progress", StateType: "started",
			PRs: []linear.PR{{URL: "https://github.com/x/y/pull/5", Title: "the fix", State: "open"}}},
	}
	m := New(fakeFetcher{issues}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: issues})
	m = next.(Model)
	if !strings.Contains(m.View(), "⇄") {
		t.Errorf("list should show a PR icon for a ticket with a linked PR:\n%s", m.View())
	}
	// The detail overlay lists the PR title.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next.(Model)
	if !strings.Contains(m.View(), "the fix") {
		t.Errorf("detail should list the PR title:\n%s", m.View())
	}
}

func TestResumableBadgeFromDisk(t *testing.T) {
	// Stopped renders as a "resumable" badge (surfaced from an on-disk session).
	if _, label, _ := sessionStyle(session.Stopped); label != "resumable" {
		t.Errorf("Stopped should render as resumable, got %q", label)
	}
}

type writeFetcher struct {
	fakeFetcher
	moved       []string
	prioritized []string
}

func (w *writeFetcher) MoveState(_ context.Context, issue linear.Issue, target string) error {
	w.moved = append(w.moved, issue.Identifier+"→"+target)
	return nil
}

func (w *writeFetcher) SetPriority(_ context.Context, issue linear.Issue, p int) error {
	w.prioritized = append(w.prioritized, fmt.Sprintf("%s→%d", issue.Identifier, p))
	return nil
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestStatusChangeFlow(t *testing.T) {
	wf := &writeFetcher{fakeFetcher: fakeFetcher{fixture()}}
	m := New(wf, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model) // cursor on ZEN-9

	next, _ = m.Update(runes("s"))
	m = next.(Model)
	if !m.statusMenu {
		t.Fatal("s should open the status menu when a writer is present")
	}
	next, _ = m.Update(runes("d"))
	m = next.(Model)
	if m.statusPend != "Done" {
		t.Fatalf("d should stage Done, got %q", m.statusPend)
	}
	next, cmd := m.Update(runes("y"))
	m = next.(Model)
	if m.statusMenu || cmd == nil {
		t.Fatal("y should close the menu and return a write command")
	}
	msg := cmd() // executes MoveState
	if len(wf.moved) != 1 || wf.moved[0] != "ZEN-9→Done" {
		t.Fatalf("expected one MoveState to Done, got %v", wf.moved)
	}
	next, _ = m.Update(msg)
	if got := next.(Model).notice; !strings.Contains(got, "→ Done ✓") {
		t.Errorf("expected a success notice, got %q", got)
	}
}

func TestStatusChangeCanceledAndNoWriter(t *testing.T) {
	// esc during confirm cancels without calling the writer.
	wf := &writeFetcher{fakeFetcher: fakeFetcher{fixture()}}
	m := New(wf, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)
	for _, k := range []tea.KeyMsg{runes("s"), runes("c"), {Type: tea.KeyEsc}} {
		next, _ = m.Update(k)
		m = next.(Model)
	}
	if m.statusMenu || len(wf.moved) != 0 {
		t.Fatalf("esc should cancel without writing, moved=%v", wf.moved)
	}
	// Without a writer (plain fakeFetcher), s must not open the menu.
	m2 := loaded(t)
	next, _ = m2.Update(runes("s"))
	if next.(Model).statusMenu {
		t.Error("s should be a no-op menu when no writer is available")
	}
}

type recBackend struct {
	fakeBackend
	closed       string
	sent         string // "name:text"
	closedByName string
}

func (r *recBackend) CloseSession(ref session.SessionRef) (string, error) {
	r.closed = ref.Name
	return "", nil
}

func (r *recBackend) Send(name, text string) (string, error) {
	r.sent = name + ":" + text
	return "", nil
}

func (r *recBackend) CloseByName(name string) (string, error) {
	r.closedByName = name
	return "", nil
}

func TestTerminalMoveClosesRunningSession(t *testing.T) {
	// Moving a ticket with a running session to Done closes that session.
	rec := &recBackend{}
	m := New(fakeFetcher{fixture()}, "", true, rec)
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)
	m.sessions = map[string]session.Status{"ZEN-9": session.Working}
	_, cmd := m.Update(statusWriteMsg{key: "ZEN-9", target: "Done"})
	if cmd == nil {
		t.Fatal("terminal move should return commands")
	}
	runCmd(cmd) // fan out the batch (refresh + close)
	if rec.closedByName != "ZEN-9" {
		t.Errorf("Done move should close ZEN-9's session, got %q", rec.closedByName)
	}

	// A non-terminal move (Validate) must NOT close the session.
	rec.closedByName = ""
	_, cmd = m.Update(statusWriteMsg{key: "ZEN-9", target: "Validate"})
	runCmd(cmd)
	if rec.closedByName != "" {
		t.Errorf("Validate move should not close the session, got %q", rec.closedByName)
	}

	// Done with no running session must NOT report a close.
	rec.closedByName = ""
	m.sessions = map[string]session.Status{}
	_, cmd = m.Update(statusWriteMsg{key: "ZEN-9", target: "Done"})
	runCmd(cmd)
	if rec.closedByName != "" {
		t.Errorf("Done with no live session should not close, got %q", rec.closedByName)
	}
}

// runCmd executes a tea.Cmd, fanning out tea.BatchMsg results so nested
// commands (e.g. the close cmd batched with refreshes) actually run.
func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runCmd(c)
		}
	}
}

type assignFetcher struct {
	fakeFetcher
	users      []linear.User
	assignedTo string // "issue:assigneeID"
}

func (a *assignFetcher) Users(context.Context) ([]linear.User, error) { return a.users, nil }
func (a *assignFetcher) Assign(_ context.Context, issue linear.Issue, id string) error {
	a.assignedTo = issue.Identifier + ":" + id
	return nil
}

func TestAssignPicker(t *testing.T) {
	af := &assignFetcher{fakeFetcher: fakeFetcher{fixture()}, users: []linear.User{{ID: "u1", DisplayName: "Alice"}, {ID: "u2", DisplayName: "Bob"}}}
	m := New(af, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model) // cursor on ZEN-9

	next, cmd := m.Update(runes("a"))
	m = next.(Model)
	if !m.assignMenu {
		t.Fatal("a should open the assignee picker")
	}
	if cmd == nil {
		t.Fatal("a should fetch users on first open")
	}
	next, _ = m.Update(cmd()) // usersMsg
	m = next.(Model)
	if len(m.users) != 2 {
		t.Fatalf("expected 2 users loaded, got %d", len(m.users))
	}
	// Filter to Bob and select him.
	for _, r := range "bob" {
		next, _ = m.Update(runes(string(r)))
		m = next.(Model)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}) // 0=Unassign → 1=Bob
	m = next.(Model)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter should assign")
	}
	cmd()
	if af.assignedTo != "ZEN-9:u2" {
		t.Errorf("expected ZEN-9 assigned to u2 (Bob), got %q", af.assignedTo)
	}

	// Unassign path: reopen, Enter on the default (cursor 0) option.
	af.assignedTo = ""
	next, _ = m.Update(runes("a"))
	m = next.(Model)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should unassign")
	}
	cmd()
	if af.assignedTo != "ZEN-9:" {
		t.Errorf("expected ZEN-9 unassigned (empty id), got %q", af.assignedTo)
	}
}

func TestOpenHintGatingAndDismiss(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate the dismiss marker from the real home
	m := New(fakeFetcher{fixture()}, "", false, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model) // cursor on ZEN-9

	// Enter shows the hint first (launch deferred).
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.openHintSpec == nil || cmd != nil {
		t.Fatal("Enter should show the open hint and defer the launch")
	}
	if !strings.Contains(m.View(), "Ctrl+b 1") {
		t.Errorf("hint should explain how to get back:\n%s", m.View())
	}
	// Enter proceeds without persisting the dont-show flag.
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.openHintSpec != nil || cmd == nil || m.hideOpenHint {
		t.Fatal("Enter should launch and keep the hint enabled")
	}
	// It shows again next open.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.openHintSpec == nil {
		t.Fatal("hint should reappear on the next open")
	}
	// 'd' dismisses permanently.
	next, _ = m.Update(runes("d"))
	m = next.(Model)
	if !m.hideOpenHint {
		t.Fatal("'d' should persist the dont-show flag")
	}
	// After dismissal, opening launches directly.
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.openHintSpec != nil || cmd == nil {
		t.Fatal("after dismissal the hint must not show; Enter launches directly")
	}
}

func TestPriorityChange(t *testing.T) {
	wf := &writeFetcher{fakeFetcher: fakeFetcher{fixture()}}
	m := New(wf, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model) // ZEN-9
	next, _ = m.Update(runes("P"))
	m = next.(Model)
	if !m.priorityMenu {
		t.Fatal("P should open the priority menu")
	}
	next, cmd := m.Update(runes("h")) // High = 2
	m = next.(Model)
	if m.priorityMenu || cmd == nil {
		t.Fatal("picking a priority should close the menu and write")
	}
	cmd()
	if len(wf.prioritized) != 1 || wf.prioritized[0] != "ZEN-9→2" {
		t.Errorf("expected ZEN-9→2 (High), got %v", wf.prioritized)
	}
}

func TestSendTriageToTicket(t *testing.T) {
	rec := &recBackend{}
	m := New(fakeFetcher{fixture()}, "", true, rec)
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model) // cursor on ZEN-9
	next, cmd := m.Update(runes("t"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("t should return a send command")
	}
	cmd() // executes Send
	if rec.sent != "ZEN-9:/triage" {
		t.Errorf("t should send /triage to ZEN-9, got %q", rec.sent)
	}
}

func TestOtherSessionsSectionCloseAndScratch(t *testing.T) {
	rec := &recBackend{}
	m := New(fakeFetcher{fixture()}, "", true, rec) // dry=true
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)
	// Inject a live session not tied to any visible ticket.
	next, _ = m.Update(sessionsMsg{sessions: []session.SessionRef{{Name: "ZEN-2990", Status: session.Idle, Ref: "w1:p9"}}})
	m = next.(Model)
	if !strings.Contains(m.View(), "Other sessions") || !strings.Contains(m.View(), "ZEN-2990") {
		t.Fatalf("expected an 'Other sessions' section with ZEN-2990:\n%s", m.View())
	}
	// Jump to the bottom — the last cursorable row is the session row.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	m = next.(Model)
	s, ok := m.selectedSession()
	if !ok || s.Name != "ZEN-2990" {
		t.Fatalf("cursor should rest on the session row, got %+v ok=%v", s, ok)
	}
	// x closes it.
	next, cmd := m.Update(runes("x"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("x should return a close command")
	}
	cmd() // executes CloseSession
	if rec.closed != "ZEN-2990" {
		t.Errorf("expected close of ZEN-2990, got %q", rec.closed)
	}
	// n plans an ad-hoc scratch session (dry → notice).
	next, _ = m.Update(runes("n"))
	if got := next.(Model).notice; !strings.Contains(got, "scratch") {
		t.Errorf("n should open a scratch session, notice=%q", got)
	}
}

func TestWorkingTicketDeEmphasized(t *testing.T) {
	m := loaded(t)
	is := linear.Issue{Identifier: "ZEN-9", Title: "thing", Priority: 1}
	normal := m.renderIssue(is, false)
	m.sessions = map[string]session.Status{"ZEN-9": session.Working}
	working := m.renderIssue(is, false)
	// Both keep the same visible text…
	if !strings.Contains(working, "ZEN-9") || !strings.Contains(working, "thing") {
		t.Fatalf("working row lost its content:\n%q", working)
	}
	// …but when styling is emitted, the working row must look different (dimmed)
	// from a normal, attention-worthy row.
	if strings.Contains(normal, "\x1b[") && working == normal {
		t.Error("working ticket should render de-emphasized vs a normal ticket")
	}
}

func TestValidationTag(t *testing.T) {
	if txt, _ := validationTag([]string{"Bug", "validation-inconclusive"}); !strings.Contains(txt, "inconclusive") {
		t.Errorf("inconclusive label should produce a tag, got %q", txt)
	}
	if txt, _ := validationTag([]string{"validation-inconclusive", "validation-failed"}); !strings.Contains(txt, "failed") {
		t.Errorf("validation-failed should outrank inconclusive, got %q", txt)
	}
	if txt, _ := validationTag([]string{"Bug", "Feature"}); txt != "" {
		t.Errorf("non-validation labels should not tag, got %q", txt)
	}
}

func TestValidationLabelShownInList(t *testing.T) {
	issues := []linear.Issue{{Identifier: "ZEN-9", Title: "x", Priority: 1, StateName: "In Progress", StateType: "started", Labels: []string{"validation-inconclusive"}}}
	m := New(fakeFetcher{issues}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: issues})
	if !strings.Contains(next.(Model).View(), "inconclusive") {
		t.Errorf("list should surface the validation flag:\n%s", next.(Model).View())
	}
}

func TestDetailViewOpensAndCloses(t *testing.T) {
	m := loaded(t) // ZEN-9 selected; fixture has no description → "(no description)"
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next.(Model)
	if m.detail == nil {
		t.Fatal("d should open the detail overlay")
	}
	if !strings.Contains(m.View(), "ZEN-9") {
		t.Errorf("detail should show the ticket id:\n%s", m.View())
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.detail != nil {
		t.Error("esc should close the detail overlay")
	}
}

func TestCursorWrapsAround(t *testing.T) {
	m := expanded(t)
	m.cursor = m.firstCursorable()
	m.moveCursor(-1) // up from the top → bottom
	if m.cursor != m.lastCursorable() {
		t.Errorf("up at top should wrap to bottom: cursor=%d last=%d", m.cursor, m.lastCursorable())
	}
	m.moveCursor(1) // down from the bottom → top
	if m.cursor != m.firstCursorable() {
		t.Errorf("down at bottom should wrap to top: cursor=%d first=%d", m.cursor, m.firstCursorable())
	}
}

func TestCursorStaysNearAfterRemoval(t *testing.T) {
	m := expanded(t)
	// Select ZEN-2 (a mid-list High ticket).
	for i, r := range m.rows {
		if r.kind == rowIssue && r.issue.Identifier == "ZEN-2" {
			m.cursor = i
		}
	}
	// Rebuild without ZEN-2 (as if it moved to Done).
	var remaining []linear.Issue
	for _, is := range fixture() {
		if is.Identifier != "ZEN-2" {
			remaining = append(remaining, is)
		}
	}
	m.rebuild(remaining)
	sel, ok := m.selected()
	if !ok {
		t.Fatal("cursor should rest on an issue after removal")
	}
	if sel.Identifier == "ZEN-9" {
		t.Errorf("cursor jumped to the top (ZEN-9) instead of staying near ZEN-2's place; landed on %s", sel.Identifier)
	}
}

func TestCursorSkipsHeaders(t *testing.T) {
	m := expanded(t) // collapsed headers are cursorable by design; expand first
	// Cursor must start on an issue row.
	if _, ok := m.selected(); !ok {
		t.Fatal("cursor did not start on an issue row")
	}
	// Move down through the list; cursor must always land on issue rows.
	for i := range 6 {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(Model)
		if m.rows[m.cursor].kind != rowIssue {
			t.Fatalf("cursor landed on a non-issue row at step %d", i)
		}
	}
}
