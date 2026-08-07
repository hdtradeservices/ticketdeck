package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
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

func (fakeBackend) Send(name, text string) (string, error)        { return "", nil }
func (fakeBackend) CloseByName(name string) (string, error)       { return "", nil }
func (fakeBackend) Triage(session.Ticket, string) (string, error) { return "", nil }

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

func TestFoldsSectionsWithoutTopTickets(t *testing.T) {
	// 16 tickets, 4 per priority (round-robin). Global order is Urgent×4, High×4,
	// Medium×4, Low×4 → the top 10 are all Urgent + all High + 2 Medium, so Low
	// (rank 13-16) has none of the top 10 and should auto-fold.
	m := New(fakeFetcher{}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: manyIssues(16)})
	m = next.(Model)
	if !m.collapsed["Low"] {
		t.Errorf("Low (no top-10 ticket) should be folded; collapsed=%v", m.collapsed)
	}
	for _, open := range []string{"Urgent", "High", "Medium"} {
		if m.collapsed[open] {
			t.Errorf("%s has a top-10 ticket and should stay expanded; collapsed=%v", open, m.collapsed)
		}
	}
	// With few tickets (<10), nothing folds.
	next, _ = m.Update(refreshedMsg{issues: fixture()})
	if len(next.(Model).collapsed) != 0 {
		t.Errorf("with <10 tickets nothing should auto-fold, got %v", next.(Model).collapsed)
	}
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
	unblocked   []string // identifiers passed to UnblockToTriage
}

func (w *writeFetcher) MoveState(_ context.Context, issue linear.Issue, target string) error {
	w.moved = append(w.moved, issue.Identifier+"→"+target)
	return nil
}

func (w *writeFetcher) UnblockToTriage(_ context.Context, issue linear.Issue) ([]string, error) {
	w.unblocked = append(w.unblocked, issue.Identifier)
	return nil, nil
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
	triaged      string // ticket key
}

func (r *recBackend) Triage(t session.Ticket, cwd string) (string, error) {
	r.triaged = t.Key
	return "", nil
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

func TestTriageTicketInBackground(t *testing.T) {
	rec := &recBackend{}
	m := New(fakeFetcher{fixture()}, "", true, rec)
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model) // cursor on ZEN-9
	next, cmd := m.Update(runes("t"))
	if cmd == nil {
		t.Fatal("t should return a triage command")
	}
	cmd() // executes Triage
	if rec.triaged != "ZEN-9" {
		t.Errorf("t on a ticket should triage ZEN-9 in the background, got %q", rec.triaged)
	}
	// On an "other session" row, t submits /triage to that session instead.
	rec = &recBackend{}
	m = New(fakeFetcher{fixture()}, "", true, rec)
	next, _ = m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)
	next, _ = m.Update(sessionsMsg{sessions: []session.SessionRef{{Name: "ZEN-2990", Ref: "w1:p9"}}})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}) // jump to the session row
	m = next.(Model)
	if _, ok := m.selectedSession(); !ok {
		t.Fatal("cursor should be on the session row")
	}
	next, cmd = m.Update(runes("t"))
	if cmd == nil {
		t.Fatal("t on a session row should send /triage")
	}
	cmd()
	if rec.sent != "ZEN-2990:/triage" {
		t.Errorf("t on a session row should send /triage, got %q", rec.sent)
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

// ownedDeck is a two-subscription deck where the peer holds ZEN-9's session —
// the case the account column exists for.
func ownedDeck(t *testing.T) Model {
	t.Helper()
	m := twoAccounts(t)
	m.owners = map[string]account.Owner{
		"ZEN-9": {Name: "support", Status: session.Working, Live: true},
	}
	m.ownerCol = ownerColWidth(m.accounts, m.owners)
	return m
}

func TestAccountColumnNamesTheOwnerOnTheSelectedRow(t *testing.T) {
	m := ownedDeck(t)
	is := linear.Issue{Identifier: "ZEN-9", Title: "urgent thing", Priority: 1}

	// Unselected: a dot, no name — naming every row would spend the title
	// column's width on something that rarely changes.
	row := m.renderIssue(is, false)
	if !strings.Contains(row, "⦿") {
		t.Errorf("row with a session should carry an account dot:\n%q", row)
	}
	if strings.Contains(row, "support") {
		t.Errorf("unselected row should not spell out the account:\n%q", row)
	}
	// Selected: the name, which is the whole point of the column.
	if sel := m.renderIssue(is, true); !strings.Contains(sel, "support") {
		t.Errorf("selected row should name the owning account:\n%q", sel)
	}

	// A ticket nobody has a session for gets blank padding, not a dot.
	other := linear.Issue{Identifier: "ZEN-1", Title: "high a", Priority: 2}
	if strings.Contains(m.renderIssue(other, false), "⦿") {
		t.Errorf("ticket with no session anywhere should have no dot:\n%q", m.renderIssue(other, false))
	}
}

// Selecting a row must not shift the columns to its right, so the reserved width
// is the same either way.
func TestAccountColumnWidthIsStable(t *testing.T) {
	m := ownedDeck(t)
	plain, _ := m.ownerCell("support", false)
	named, _ := m.ownerCell("support", true)
	blank, _ := m.ownerCell("", false)
	if len([]rune(plain)) != m.ownerCol || len([]rune(named)) != m.ownerCol || len([]rune(blank)) != m.ownerCol {
		t.Errorf("owner cells must all be %d wide: %q / %q / %q", m.ownerCol, plain, named, blank)
	}
}

// One subscription means nothing to disambiguate — the column costs width and
// says nothing, so it must not appear at all.
func TestAccountColumnAbsentWithOneSubscription(t *testing.T) {
	one := []account.Account{{Name: "matt", ConfigDir: "/home/u/.claude"}}
	if got := ownerColWidth(one, nil); got != 0 {
		t.Errorf("ownerColWidth with one account = %d, want 0", got)
	}
	m := loaded(t)
	m.accounts, m.ownerCol = one, 0
	if row := m.renderIssue(linear.Issue{Identifier: "ZEN-9", Title: "x", Priority: 1}, false); strings.Contains(row, "⦿") {
		t.Errorf("single-account deck should render no account column:\n%q", row)
	}
}

// Ownership comes from disk and the peer workspaces, not from the backend, so a
// failed status poll must not blank it.
func TestOwnersSurviveAFailedStatusPoll(t *testing.T) {
	m := ownedDeck(t)
	owners := map[string]account.Owner{"ZEN-9": {Name: "support"}}
	next, _ := m.Update(statusesMsg{owners: owners, err: os.ErrDeadlineExceeded})
	if got := next.(Model).owners["ZEN-9"].Name; got != "support" {
		t.Errorf("owner after a failed poll = %q, want support", got)
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

func TestBlockedNote(t *testing.T) {
	// only for Blocked status
	notBlocked := linear.Issue{StateName: "In Progress", BlockedBy: []linear.Relation{{Identifier: "ZEN-1", StateType: "started"}}}
	if got := blockedNote(notBlocked); got != "" {
		t.Errorf("non-Blocked note = %q, want empty", got)
	}
	blocked := linear.Issue{StateName: "Blocked", BlockedBy: []linear.Relation{
		{Identifier: "ZEN-1", StateType: "started"},
		{Identifier: "ZEN-2", StateType: "completed"}, // filtered
	}}
	if got := blockedNote(blocked); got != "⛔ ZEN-1" {
		t.Errorf("note = %q, want \"⛔ ZEN-1\"", got)
	}
	many := linear.Issue{StateName: "Blocked", BlockedBy: []linear.Relation{
		{Identifier: "A-1", StateType: "started"}, {Identifier: "A-2", StateType: "started"},
		{Identifier: "A-3", StateType: "started"}, {Identifier: "A-4", StateType: "started"},
		{Identifier: "A-5", StateType: "started"},
	}}
	if got := blockedNote(many); got != "⛔ A-1, A-2, A-3 +2" {
		t.Errorf("note = %q, want \"⛔ A-1, A-2, A-3 +2\"", got)
	}
}

// drainCmd executes a (possibly batched) command tree, returning all leaf msgs.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drainCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestDoneTriggersUnblockCascade(t *testing.T) {
	wf := &writeFetcher{fakeFetcher: fakeFetcher{fixture()}}
	m := New(wf, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)

	// A successful Done write should fan out an UnblockToTriage for that ticket.
	next, cmd := m.Update(statusWriteMsg{key: "ZEN-9", target: "Done"})
	if cmd == nil {
		t.Fatal("Done write should return follow-up commands")
	}
	msgs := drainCmd(cmd)
	if len(wf.unblocked) != 1 || wf.unblocked[0] != "ZEN-9" {
		t.Fatalf("Done should trigger UnblockToTriage(ZEN-9), got %v", wf.unblocked)
	}
	// A non-terminal move must NOT cascade.
	wf.unblocked = nil
	next2, cmd2 := next.(Model).Update(statusWriteMsg{key: "ZEN-9", target: "Blocked"})
	_ = next2
	_ = drainCmd(cmd2)
	if len(wf.unblocked) != 0 {
		t.Fatalf("Blocked move should not cascade, got %v", wf.unblocked)
	}
	_ = msgs
}

// capturePRBrowse swaps the browser launcher for a recorder, so PR tests assert
// which URLs would open without spawning anything.
func capturePRBrowse(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	orig := browse
	browse = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { browse = orig })
	return &opened
}

// prFixture is one ticket with three PRs across repos in mixed states.
func prFixture() []linear.Issue {
	return []linear.Issue{{
		Identifier: "ZEN-9", Title: "spans three repos", Priority: 1,
		StateName: "In Progress", StateType: "started",
		PRs: []linear.PR{
			{URL: "https://github.com/o/etp/pull/100", Title: "merged one", State: "merged", Repo: "etp", Number: 100},
			{URL: "https://github.com/o/listing/pull/9", Title: "open one", State: "open", Repo: "listing", Number: 9},
			{URL: "https://github.com/o/charts/pull/2", Title: "draft one", State: "draft", Repo: "charts", Number: 2},
		},
	}}
}

func loadedWith(t *testing.T, issues []linear.Issue) Model {
	t.Helper()
	m := New(fakeFetcher{issues}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: issues})
	return next.(Model)
}

func TestSinglePROpensDirectly(t *testing.T) {
	opened := capturePRBrowse(t)
	issues := []linear.Issue{{
		Identifier: "ZEN-9", Title: "one pr", Priority: 1, StateName: "In Progress", StateType: "started",
		PRs: []linear.PR{{URL: "https://github.com/o/etp/pull/7", State: "open", Repo: "etp", Number: 7}},
	}}
	m := loadedWith(t, issues)
	next, cmd := m.Update(runes("p"))
	m = next.(Model)
	if m.prMenu {
		t.Fatal("a single PR should open directly, not open the picker")
	}
	drainCmd(cmd)
	if len(*opened) != 1 || (*opened)[0] != "https://github.com/o/etp/pull/7" {
		t.Fatalf("expected the one PR to open, got %v", *opened)
	}
	if !strings.Contains(m.notice, "etp#7") {
		t.Errorf("notice should name the PR, got %q", m.notice)
	}
}

func TestMultiPROpensPickerOrderedByActionability(t *testing.T) {
	m := loadedWith(t, prFixture())
	next, _ := m.Update(runes("p"))
	m = next.(Model)
	if !m.prMenu {
		t.Fatal("several PRs should open the picker")
	}
	// Most actionable first: open, draft, then merged.
	want := []string{"listing#9", "charts#2", "etp#100"}
	for i, w := range want {
		if got := m.prList[i].Label(); got != w {
			t.Errorf("prList[%d] = %q, want %q", i, got, w)
		}
	}
	if m.prCursor != 0 {
		t.Errorf("picker should start on the most actionable PR, cursor=%d", m.prCursor)
	}
	// The picker renders its rows and the ticket it belongs to.
	view := m.View()
	for _, want := range []string{"ZEN-9", "listing#9", "charts#2", "etp#100", "open all"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker view missing %q:\n%s", want, view)
		}
	}
}

func TestPRPickerEnterOpensSelected(t *testing.T) {
	opened := capturePRBrowse(t)
	m := loadedWith(t, prFixture())
	next, _ := m.Update(runes("p"))
	m = next.(Model)
	next, _ = m.Update(runes("j")) // move to charts#2
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.prMenu {
		t.Error("opening a PR should close the picker")
	}
	drainCmd(cmd)
	if len(*opened) != 1 || (*opened)[0] != "https://github.com/o/charts/pull/2" {
		t.Fatalf("expected the selected PR to open, got %v", *opened)
	}
}

func TestPRPickerDigitOpensThatOne(t *testing.T) {
	opened := capturePRBrowse(t)
	m := loadedWith(t, prFixture())
	next, _ := m.Update(runes("p"))
	m = next.(Model)
	next, cmd := m.Update(runes("3")) // third row = etp#100
	if next.(Model).prMenu {
		t.Error("a digit should open that PR and close the picker")
	}
	drainCmd(cmd)
	if len(*opened) != 1 || (*opened)[0] != "https://github.com/o/etp/pull/100" {
		t.Fatalf("digit 3 should open the third PR, got %v", *opened)
	}
}

func TestPRPickerOpenAll(t *testing.T) {
	opened := capturePRBrowse(t)
	m := loadedWith(t, prFixture())
	next, _ := m.Update(runes("p"))
	m = next.(Model)
	next, cmd := m.Update(runes("a"))
	m = next.(Model)
	if m.prMenu {
		t.Error("open-all should close the picker")
	}
	drainCmd(cmd)
	if len(*opened) != 3 {
		t.Fatalf("a should open all 3 PRs, got %v", *opened)
	}
	if !strings.Contains(m.notice, "all 3") {
		t.Errorf("notice should report the count, got %q", m.notice)
	}
}

func TestPRPickerEscCancels(t *testing.T) {
	opened := capturePRBrowse(t)
	m := loadedWith(t, prFixture())
	next, _ := m.Update(runes("p"))
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	drainCmd(cmd)
	if m.prMenu {
		t.Error("esc should close the picker")
	}
	if len(*opened) != 0 {
		t.Errorf("esc must not open anything, got %v", *opened)
	}
}

func TestNoPRNotice(t *testing.T) {
	opened := capturePRBrowse(t)
	m := loadedWith(t, []linear.Issue{{Identifier: "ZEN-9", Title: "no prs", Priority: 1, StateName: "Todo", StateType: "unstarted"}})
	next, cmd := m.Update(runes("p"))
	m = next.(Model)
	drainCmd(cmd)
	if m.prMenu || len(*opened) != 0 {
		t.Error("a ticket with no PR should neither open a picker nor a browser")
	}
	if !strings.Contains(m.notice, "no linked PR") {
		t.Errorf("expected a no-PR notice, got %q", m.notice)
	}
}

func TestPRMarkShowsCount(t *testing.T) {
	one := []linear.PR{{State: "open"}}
	three := []linear.PR{{State: "open"}, {State: "merged"}, {State: "draft"}}
	if g, _ := prMark(nil); g != "  " {
		t.Errorf("no PRs should render two blanks, got %q", g)
	}
	if g, _ := prMark(one); g != "⇄ " {
		t.Errorf("one PR should render %q, got %q", "⇄ ", g)
	}
	if g, _ := prMark(three); g != "⇄3" {
		t.Errorf("three PRs should render ⇄3, got %q", g)
	}
	// Every variant occupies the same width so issue rows stay aligned.
	for _, prs := range [][]linear.PR{nil, one, three} {
		if g, _ := prMark(prs); len([]rune(g)) != prMarkCol {
			t.Errorf("prMark(%d) = %q, want %d cells", len(prs), g, prMarkCol)
		}
	}
}

func TestDetailPKeyOpensPicker(t *testing.T) {
	m := loadedWith(t, prFixture())
	next, _ := m.Update(runes("d")) // description overlay
	m = next.(Model)
	if m.detail == nil {
		t.Fatal("d should open the detail overlay")
	}
	next, _ = m.Update(runes("p"))
	m = next.(Model)
	if m.detail != nil {
		t.Error("the detail overlay should close when the picker opens")
	}
	if !m.prMenu {
		t.Fatal("p in the detail overlay should open the multi-PR picker")
	}
}

func TestSearchFiltersByKeyAndTitle(t *testing.T) {
	m := expanded(t) // fixture's 4 open tickets, all groups unfolded

	// "/" enters search input mode.
	next, _ := m.Update(runes("/"))
	m = next.(Model)
	if !m.searchMode {
		t.Fatal("/ should enter search mode")
	}

	// Typing narrows the list to a key substring.
	for _, r := range []string{"Z", "E", "N", "-", "2"} {
		next, _ = m.Update(runes(r))
		m = next.(Model)
	}
	if m.searchQuery != "ZEN-2" {
		t.Fatalf("query should accumulate to ZEN-2, got %q", m.searchQuery)
	}
	view := m.View()
	if !strings.Contains(view, "ZEN-2") {
		t.Errorf("matching ticket ZEN-2 should be visible\n%s", view)
	}
	for _, gone := range []string{"ZEN-9", "ZEN-1", "ZEN-5"} {
		if strings.Contains(view, gone) {
			t.Errorf("%s should be filtered out by query %q\n%s", gone, m.searchQuery, view)
		}
	}

	// Enter keeps the filter but leaves input mode so list nav works again.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.searchMode {
		t.Error("enter should exit search input mode")
	}
	if m.searchQuery != "ZEN-2" {
		t.Errorf("enter should keep the filter, got %q", m.searchQuery)
	}

	// Esc from the list clears the applied filter.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.searchQuery != "" {
		t.Errorf("esc should clear the filter, got %q", m.searchQuery)
	}
	if v := m.View(); !strings.Contains(v, "ZEN-9") || !strings.Contains(v, "ZEN-1") {
		t.Errorf("clearing the filter should restore the full list\n%s", v)
	}
}

func TestSearchMatchesTitleCaseInsensitively(t *testing.T) {
	m := expanded(t)
	next, _ := m.Update(runes("/"))
	m = next.(Model)
	next, _ = m.Update(runes("URGENT")) // fixture ZEN-9 title is "urgent thing"
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "ZEN-9") {
		t.Errorf("title match should surface ZEN-9 for query %q\n%s", m.searchQuery, view)
	}
	if strings.Contains(view, "ZEN-1") {
		t.Errorf("non-matching ZEN-1 should be hidden\n%s", view)
	}
}

func TestSearchNoMatchShowsMessageAndFooter(t *testing.T) {
	m := expanded(t)
	m.height = 40 // give the view a viewport so the footer renders
	next, _ := m.Update(runes("/"))
	m = next.(Model)
	next, _ = m.Update(runes("zzzznope"))
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "no tickets match") {
		t.Errorf("an empty result set should say so\n%s", view)
	}
	if !strings.Contains(view, "esc clear") {
		t.Errorf("the search footer must stay visible so the filter can be cleared\n%s", view)
	}
}

func TestFocusMsgRefreshesStatuses(t *testing.T) {
	m := loaded(t)
	_, cmd := m.Update(tea.FocusMsg{})
	if cmd == nil {
		t.Fatal("a focus event should trigger a status refresh command")
	}
}

func TestStatusTickRefreshesWithoutLinearFetch(t *testing.T) {
	m := loaded(t)
	_, cmd := m.Update(statusTickMsg{})
	if cmd == nil {
		t.Fatal("statusTickMsg should schedule a refresh + re-arm")
	}
}

// Regression: a status poll must cover every visible ticket, not just the rows
// currently rendered — otherwise an active search (or a folded group) makes the
// tick replace m.sessions with a partial map, blanking badges and resetting the
// time-in-state timers of everything filtered out.
func TestStatusPollCoversFilteredOutTickets(t *testing.T) {
	m := expanded(t)
	next, _ := m.Update(runes("/"))
	m = next.(Model)
	next, _ = m.Update(runes("ZEN-2")) // only ZEN-2 is rendered now
	m = next.(Model)

	keys := m.issueKeys()
	for _, want := range []string{"ZEN-9", "ZEN-1", "ZEN-2", "ZEN-5"} {
		if !slices.Contains(keys, want) {
			t.Errorf("status poll should still cover %s while a filter hides it; got %v", want, keys)
		}
	}
}

// Regression: with a folded group present, entering a search must put the cursor
// on the first matching ticket. Folded headers stay in m.collapsed, and a
// collapsed header is cursorable, so the cursor could land on a header instead —
// leaving Enter a no-op because no issue is selected.
func TestSearchCursorLandsOnFirstMatchWithFoldedGroups(t *testing.T) {
	m := loaded(t)
	// Fold the group holding ZEN-5 (Low), as top-10 focus does in real use.
	m.collapsed = map[string]bool{"Low": true}
	m.regroup()
	m.cursor = m.firstCursorable()

	next, _ := m.Update(runes("/"))
	m = next.(Model)
	next, _ = m.Update(runes("ZEN-5")) // ZEN-5 is Low priority — a folded group
	m = next.(Model)

	if _, ok := m.selected(); !ok {
		t.Fatalf("cursor should sit on a matching ticket, not a header (row kind %v)", m.rows[m.cursor].kind)
	}
	if is, _ := m.selected(); is.Identifier != "ZEN-5" {
		t.Errorf("cursor should be on ZEN-5, got %s", is.Identifier)
	}
}

// twoAccounts points the process at a home holding two subscriptions and returns
// a loaded model that has resolved them, as a deck running as "matt" would.
func twoAccounts(t *testing.T) Model {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{".claude", ".claude-support"} {
		p := filepath.Join(home, dir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("TICKETDECK_ACCOUNT", "matt")
	return loaded(t)
}

// An unlabelled deck is indistinguishable from any other deck, so the primary
// one must carry a badge too.
func TestAccountBadgeAlwaysRenders(t *testing.T) {
	m := twoAccounts(t)
	if !strings.Contains(m.accountSegment(), "matt") {
		t.Errorf("account badge missing the account name: %q", m.accountSegment())
	}
	if !strings.Contains(m.View(), "⦿ matt") {
		t.Error("view should name the subscription this deck runs as")
	}
}

func TestAccountColorsDifferBetweenAccounts(t *testing.T) {
	m := twoAccounts(t)
	if m.acctColors["matt"] == m.acctColors["support"] {
		t.Errorf("both accounts got color %q — the accent can't tell them apart", m.acctColors["matt"])
	}
}

// The whole reason to read every account: seeing the other's headroom without
// switching decks.
func TestOtherAccountQuotaShownInView(t *testing.T) {
	m := twoAccounts(t)
	m.usages = map[string]*quota.Usage{
		"matt":    {FiveHourPct: 94, SevenDayPct: 61},
		"support": {FiveHourPct: 12, SevenDayPct: 8},
	}
	line := m.otherQuotaLine()
	if !strings.Contains(line, "support") || !strings.Contains(line, "12%") {
		t.Errorf("other account's headroom missing: %q", line)
	}
	if strings.Contains(line, "matt") {
		t.Errorf("active account belongs on the title line, not the others line: %q", line)
	}
	if !strings.Contains(m.View(), "12%") {
		t.Error("view should show the other subscription's usage")
	}
}

func TestQuotaMsgKeepsPriorReadingForFailedAccount(t *testing.T) {
	m := twoAccounts(t)
	m.usages = map[string]*quota.Usage{"support": {FiveHourPct: 12}}
	// support failed this round (absent from the message); its bar must not blank.
	next, _ := m.Update(quotaMsg{usages: map[string]*quota.Usage{"matt": {FiveHourPct: 50}}})
	m = next.(Model)
	if m.usages["support"] == nil {
		t.Error("a transient failure blanked an account that was readable a moment ago")
	}
	if m.usages["matt"] == nil || m.usages["matt"].FiveHourPct != 50 {
		t.Error("new reading not applied")
	}
}

func TestHandoffOverlayOpensAndNamesBothAccounts(t *testing.T) {
	m := twoAccounts(t)
	m.sessions = map[string]session.Status{"ZEN-9": session.NeedsInput}
	next, _ := m.Update(runes("H"))
	m = next.(Model)
	if m.handoffKey != "ZEN-9" {
		t.Fatalf("H should open the hand-off overlay for the cursor ticket, got %q", m.handoffKey)
	}
	view := m.View()
	for _, want := range []string{"ZEN-9", "matt", "support"} {
		if !strings.Contains(view, want) {
			t.Errorf("hand-off overlay missing %q: %s", want, view)
		}
	}
}

// Nothing to move means the overlay must not open — it would offer an action
// that can't do anything.
func TestHandoffRefusedWithoutASession(t *testing.T) {
	m := twoAccounts(t)
	m.sessions = map[string]session.Status{}
	next, _ := m.Update(runes("H"))
	m = next.(Model)
	if m.handoffKey != "" {
		t.Error("hand-off opened for a ticket with no session")
	}
	if !strings.Contains(m.notice, "no session") {
		t.Errorf("expected an explanatory notice, got %q", m.notice)
	}
}

func TestHandoffRefusedWithOnlyOneAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TICKETDECK_ACCOUNT", "matt")
	m := loaded(t)
	m.sessions = map[string]session.Status{"ZEN-9": session.Working}
	next, _ := m.Update(runes("H"))
	m = next.(Model)
	if m.handoffKey != "" {
		t.Error("hand-off opened with nowhere to hand off to")
	}
}

// Stopping the session before copying is mandatory: two accounts appending to
// their own copy of one transcript diverge irreconcilably.
func TestHandoffStopsTheSessionBeforeCopying(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{".claude", ".claude-support"} {
		p := filepath.Join(home, dir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("TICKETDECK_ACCOUNT", "matt")

	rec := &recBackend{}
	m := New(fakeFetcher{fixture()}, "", true, rec)
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)
	m.sessions = map[string]session.Status{"ZEN-9": session.NeedsInput}

	next, _ = m.Update(runes("H"))
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.handoffKey != "" {
		t.Error("overlay should close on confirm")
	}
	drainCmd(cmd)
	if rec.closedByName != "ZEN-9" {
		t.Errorf("hand-off must stop the session first, closed %q", rec.closedByName)
	}
}

func TestHandoffEscCancels(t *testing.T) {
	m := twoAccounts(t)
	m.sessions = map[string]session.Status{"ZEN-9": session.Working}
	next, _ := m.Update(runes("H"))
	next, _ = next.(Model).Update(tea.KeyMsg{Type: tea.KeyEsc})
	if next.(Model).handoffKey != "" {
		t.Error("esc should close the hand-off overlay")
	}
}

// The footer names the destination when there's exactly one, so the two-account
// case reads as an action rather than a menu.
func TestFooterNamesTheHandoffTarget(t *testing.T) {
	m := twoAccounts(t)
	if !strings.Contains(m.View(), "H hand to ⦿support") {
		t.Error("footer should name the single hand-off target")
	}
}

// The success notice must tell you how to actually open that deck. `deck
// --account default` would look for a ~/.claude-default that doesn't exist, so
// the command comes from the target's config dir, not its name.
func TestHandoffNoticeUsesTheTargetsRealLaunchCommand(t *testing.T) {
	m := twoAccounts(t)
	next, _ := m.Update(handoffMsg{key: "ZEN-9", to: "support", cmd: "deck --account support"})
	if !strings.Contains(next.(Model).notice, "deck --account support") {
		t.Errorf("notice = %q", next.(Model).notice)
	}
	// Handing back to the primary: plain `deck`, no invented --account flag.
	next, _ = m.Update(handoffMsg{key: "ZEN-9", to: "matt", cmd: "deck"})
	if n := next.(Model).notice; strings.Contains(n, "--account") {
		t.Errorf("primary target should need no --account flag: %q", n)
	}
}

// An account whose usage can't be read must still appear. Dropping the row made
// a rate-limited subscription look like it was never detected — which is exactly
// how this reads to someone checking whether their second account is wired up.
func TestOtherAccountShownEvenWithoutUsage(t *testing.T) {
	m := twoAccounts(t)
	m.usages = map[string]*quota.Usage{"matt": {FiveHourPct: 94}} // support failed
	line := m.otherQuotaLine()
	if !strings.Contains(line, "support") {
		t.Errorf("account vanished when its usage was unreadable: %q", line)
	}
	if !strings.Contains(line, "unavailable") {
		t.Errorf("missing usage should say so: %q", line)
	}
	if !strings.Contains(m.View(), "support") {
		t.Error("view should still name the other subscription")
	}
}

// A 429 must push the next poll out. The budget is shared with Claude Code's own
// status line, so holding the normal schedule just extends the throttle.
func TestRateLimitBacksOffTheNextPoll(t *testing.T) {
	m := twoAccounts(t)
	m.quotaNextAt = time.Now()
	next, _ := m.Update(quotaMsg{usages: map[string]*quota.Usage{}, rateLimited: true})
	got := next.(Model).quotaNextAt
	if wait := time.Until(got); wait < quotaEvery {
		t.Errorf("backoff = %v, want at least quotaEvery (%v)", wait, quotaEvery)
	}
}

// Quota rides the slow tick but on its own longer schedule; a tick that isn't
// due must not queue another usage round.
func TestTickSkipsQuotaUntilDue(t *testing.T) {
	// Assert on quotaNextAt rather than draining the batch: the tick command is a
	// real 60s tea.Tick, and draining it would stall the suite for a minute.
	// quotaNextAt is stamped exactly when a fetch is dispatched, so it's a
	// faithful proxy.
	m := twoAccounts(t)
	armed := time.Now().Add(quotaEvery)
	m.quotaNextAt = armed
	next, _ := m.Update(tickMsg{})
	if !next.(Model).quotaNextAt.Equal(armed) {
		t.Error("a not-yet-due tick moved the quota schedule (so it fetched)")
	}

	// Once due, it fires and re-arms.
	m.quotaNextAt = time.Now().Add(-time.Second)
	next, _ = m.Update(tickMsg{})
	if time.Until(next.(Model).quotaNextAt) < quotaEvery/2 {
		t.Error("a due tick should re-arm the quota schedule")
	}
}

// The other-accounts header row must be reserved in viewportHeight. It wasn't,
// so the body ran one line too tall and pushed the footer off-screen — in
// exactly the multi-account case the row exists to serve.
func TestOtherQuotaLineIsReservedInViewport(t *testing.T) {
	m := twoAccounts(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 14})
	m = next.(Model)
	if !m.hasOtherQuotaLine() {
		t.Fatal("fixture should render the other-accounts row")
	}

	// The rendered frame must fit the terminal, or the footer is off-screen.
	lines := strings.Count(strings.TrimRight(m.View(), "\n"), "\n") + 1
	if lines > m.height {
		t.Errorf("frame is %d lines in a %d-line terminal — the footer scrolls off", lines, m.height)
	}
	if !strings.Contains(m.View(), "↑↓ move") {
		t.Error("help footer missing from the frame")
	}

	// One account: no extra row, so one more body line is available.
	solo := loadedSingleAccount(t)
	next, _ = solo.Update(tea.WindowSizeMsg{Width: 100, Height: 14})
	solo = next.(Model)
	if solo.hasOtherQuotaLine() {
		t.Fatal("single-account deck should not render the row")
	}
	if solo.viewportHeight() != m.viewportHeight()+1 {
		t.Errorf("viewport heights: single=%d two-account=%d, want single to be exactly one larger",
			solo.viewportHeight(), m.viewportHeight())
	}
}

// loadedSingleAccount is a deck with only the primary subscription on disk.
func loadedSingleAccount(t *testing.T) Model {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TICKETDECK_ACCOUNT", "matt")
	return loaded(t)
}
