package tui

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// projectFixture: two tickets in a project I lead, one in a project I don't,
// and two with no project at all (which stay in the priority groups).
func projectIssues() []linear.Issue {
	return []linear.Issue{
		{Identifier: "ZEN-1", Title: "in my project", Priority: 1, PrioLabel: "Urgent", StateName: "Triage", StateType: "started",
			ProjectID: "p1", ProjectName: "Mine", ProjectSlugID: "s1"},
		{Identifier: "ZEN-2", Title: "also in my project", Priority: 2, PrioLabel: "High", StateName: "Planned", StateType: "started",
			ProjectID: "p1", ProjectName: "Mine", ProjectSlugID: "s1",
			PRs: []linear.PR{{URL: "https://github.com/o/etp/pull/7", State: "open", Repo: "etp", Number: 7}}},
		{Identifier: "ONB-9", Title: "in someone else's project", Priority: 2, PrioLabel: "High", StateName: "Triage", StateType: "started",
			ProjectID: "p9", ProjectName: "Theirs", ProjectSlugID: "s9", ProjectStatusType: "started"},
		{Identifier: "ZEN-8", Title: "no project", Priority: 1, PrioLabel: "Urgent", StateName: "Triage", StateType: "started"},
		{Identifier: "ZEN-9", Title: "also no project", Priority: 2, PrioLabel: "High", StateName: "Triage", StateType: "started"},
	}
}

func fixtureProjects() []linear.Project {
	return []linear.Project{{
		ID: "p1", Name: "Mine", SlugID: "s1", URL: "https://linear.app/z/project/mine-s1",
		Summary: "the summary", Priority: 2, PrioLabel: "High",
		Progress: 0.5, Scope: 20, StatusName: "In Progress", StatusType: "started", Mine: true,
	}}
}

// projectFetcher is a fakeFetcher that also serves projects.
type projFetcher struct{ fakeFetcher }

func (projFetcher) FetchMyProjects(context.Context) ([]linear.Project, error) {
	return fixtureProjects(), nil
}

func loadedProjects(t *testing.T) Model {
	t.Helper()
	f := projFetcher{fakeFetcher{projectIssues()}}
	m := New(f, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: projectIssues()})
	next, _ = next.(Model).Update(projectsMsg{projects: fixtureProjects()})
	m = next.(Model)
	m.width, m.height = 120, 40
	for k := range m.collapsed {
		m.collapsed[k] = false
	}
	m.regroup()
	m.cursor = m.firstCursorable()
	return m
}

func rowKinds(m Model) []rowKind {
	out := make([]rowKind, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r.kind)
	}
	return out
}

// issueRows returns the ticket identifiers rendered, in order.
func issueRows(m Model) []string {
	var out []string
	for _, r := range m.rows {
		if r.kind == rowIssue {
			out = append(out, r.issue.Identifier)
		}
	}
	return out
}

func TestProjectsSectionLeadsTheDeck(t *testing.T) {
	m := loadedProjects(t)
	kinds := rowKinds(m)
	if len(kinds) == 0 || kinds[0] != rowProjectHeader {
		t.Fatalf("the Projects section must be the first thing on the deck, got %v", kinds)
	}
	// Both the project I lead and the one merely holding my ticket get a row.
	var names []string
	for _, r := range m.rows {
		if r.kind == rowProject {
			names = append(names, r.project.Name)
		}
	}
	if !slices.Contains(names, "Mine") || !slices.Contains(names, "Theirs") {
		t.Errorf("want both projects to have rows, got %v", names)
	}
}

// The whole point of the section: a ticket with a project leaves the priority
// groups.
func TestProjectTicketsLeaveThePrioritySections(t *testing.T) {
	m := loadedProjects(t)
	got := issueRows(m)
	// Projects start folded, so no project ticket should render at all.
	if !slices.Equal(got, []string{"ZEN-8", "ZEN-9"}) {
		t.Errorf("priority sections should hold only the project-less tickets, got %v", got)
	}
	view := m.View()
	for _, key := range []string{"ZEN-1", "ZEN-2", "ONB-9"} {
		if strings.Contains(view, key) {
			t.Errorf("%s has a project and must not appear in the priority sections", key)
		}
	}
}

// No ticket may be lost: unfolding its project brings it back, nested.
func TestUnfoldingAProjectRevealsItsTickets(t *testing.T) {
	m := loadedProjects(t)
	// Park the cursor on the "Mine" project row and unfold it.
	for i, r := range m.rows {
		if r.kind == rowProject && r.project.Name == "Mine" {
			m.cursor = i
		}
	}
	next, _ := m.Update(runes(" "))
	m = next.(Model)

	got := issueRows(m)
	for _, want := range []string{"ZEN-1", "ZEN-2"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s should be nested under its project once unfolded, got %v", want, got)
		}
	}
	// Nested rows are indented, so they read as belonging to the project.
	for _, r := range m.rows {
		if r.kind == rowIssue && (r.issue.Identifier == "ZEN-1" || r.issue.Identifier == "ZEN-2") && r.indent == 0 {
			t.Errorf("%s should render indented under its project", r.issue.Identifier)
		}
	}
	// The cursor stays on the project it just unfolded.
	if p, ok := m.selectedProject(); !ok || p.Name != "Mine" {
		t.Error("unfolding should leave the cursor on the project row")
	}
	// Folding it again puts them away.
	next, _ = m.Update(runes(" "))
	if got := issueRows(next.(Model)); slices.Contains(got, "ZEN-1") {
		t.Errorf("folding should hide the project's tickets again, got %v", got)
	}
}

func TestProjectSectionHeaderFolds(t *testing.T) {
	m := loadedProjects(t)
	m.cursor = 0 // the section header
	next, _ := m.Update(runes(" "))
	m = next.(Model)
	if !m.projFolded {
		t.Fatal("space on the section header should fold the whole section")
	}
	for _, r := range m.rows {
		if r.kind == rowProject {
			t.Fatal("a folded section should render no project rows")
		}
	}
	// The header stays reachable so it can be unfolded again.
	if !m.cursorable(m.cursor) {
		t.Error("a folded section header must stay a cursor target")
	}
}

func TestSessionKeysCoverProjects(t *testing.T) {
	m := loadedProjects(t)
	keys := m.sessionKeys()
	for _, want := range []string{"proj-s1", "proj-s9", "ZEN-1", "ZEN-8"} {
		if !slices.Contains(keys, want) {
			t.Errorf("sessionKeys missing %q: %v", want, keys)
		}
	}
}

// A project's own session belongs on its row, never in "Other sessions".
func TestProjectSessionIsNotAnOtherSession(t *testing.T) {
	m := loadedProjects(t)
	next, _ := m.Update(sessionsMsg{sessions: []session.SessionRef{
		{Name: "proj-s1", Status: session.Working},
		{Name: "scratch-1", Status: session.Idle},
	}})
	m = next.(Model)
	for _, r := range m.rows {
		if r.kind == rowSession && r.ref.Name == "proj-s1" {
			t.Fatal("a project's session must be represented by its project row, not as an off-list session")
		}
	}
	if !strings.Contains(m.View(), "scratch-1") {
		t.Error("an unrelated session should still show under Other sessions")
	}
}

func TestEnterOnAProjectLaunchesItsSession(t *testing.T) {
	m := loadedProjects(t)
	for i, r := range m.rows {
		if r.kind == rowProject && r.project.Name == "Mine" {
			m.cursor = i
		}
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	// --dry prints the plan; it must be bound to the project's key, and seeded
	// with the project identity rather than a ticket's.
	if !strings.Contains(m.notice, session.DeterministicID("proj-s1")) {
		t.Errorf("enter should launch the project's own session: %q", m.notice)
	}
}

func TestProjectLaunchSeedsProjectIdentity(t *testing.T) {
	p := fixtureProjects()[0]
	p.Issues = []linear.Issue{
		{Identifier: "ZEN-1", StateType: "started"},
		{Identifier: "ZEN-2", StateType: "completed"}, // done: not outstanding work
	}
	tk := toProjectTicket(p)
	if !tk.Project {
		t.Error("a project launch unit must be marked as one")
	}
	if tk.Key != "proj-s1" || tk.Title != "Mine" {
		t.Errorf("launch unit = %+v", tk)
	}
	if !strings.Contains(tk.Context, "ZEN-1") {
		t.Errorf("the session should start knowing its open tickets: %q", tk.Context)
	}
	if strings.Contains(tk.Context, "ZEN-2") {
		t.Errorf("a finished ticket is not outstanding scope: %q", tk.Context)
	}
	if !strings.Contains(tk.Context, "50%") {
		t.Errorf("the session should start knowing the progress: %q", tk.Context)
	}
}

// `p` on a project row opens the PRs across its tickets.
func TestProjectPRKeyUsesTheWholeProject(t *testing.T) {
	opened := capturePRBrowse(t)
	m := loadedProjects(t)
	for i, r := range m.rows {
		if r.kind == rowProject && r.project.Name == "Mine" {
			m.cursor = i
		}
	}
	next, cmd := m.Update(runes("p"))
	m = next.(Model)
	drainCmd(cmd)
	// The project has exactly one PR across its tickets, so it opens directly.
	if m.prMenu {
		t.Fatal("a single project PR should open directly")
	}
	if len(*opened) != 1 || (*opened)[0] != "https://github.com/o/etp/pull/7" {
		t.Fatalf("expected the project's one PR, got %v", *opened)
	}
}

// /triage triages one issue, so `t` is the one ticket key that does not carry
// over to a project row — it must say so rather than starting a session and
// firing an incoherent command at it.
func TestTriageIsTicketOnly(t *testing.T) {
	m := loadedProjects(t)
	for i, r := range m.rows {
		if r.kind == rowProject && r.project.Name == "Mine" {
			m.cursor = i
		}
	}
	next, cmd := m.Update(runes("t"))
	m = next.(Model)
	if cmd != nil {
		t.Error("t on a project must not launch or send anything")
	}
	if !strings.Contains(m.notice, "not a project") {
		t.Errorf("t on a project should explain it doesn't apply, got %q", m.notice)
	}
	// The footer must not advertise a key that does nothing here.
	if strings.Contains(m.View(), "t "+triageCmd) {
		t.Error("the footer should not offer t on a project row")
	}

	// It still works on a nested ticket inside the project.
	m.projExpanded["proj-s1"] = true
	m.regroup()
	for i, r := range m.rows {
		if r.kind == rowIssue && r.issue.Identifier == "ZEN-1" {
			m.cursor = i
		}
	}
	if !strings.Contains(m.footer(), "t "+triageCmd) {
		t.Error("a ticket row, nested or not, should still offer t")
	}
}

func TestProjectDetailOverlay(t *testing.T) {
	m := loadedProjects(t)
	for i, r := range m.rows {
		if r.kind == rowProject && r.project.Name == "Mine" {
			m.cursor = i
		}
	}
	next, _ := m.Update(runes("d"))
	m = next.(Model)
	if m.detailProj == nil {
		t.Fatal("d on a project row should open the project overlay")
	}
	// glamour splits a rendered paragraph across ANSI runs, so compare on the
	// plain text.
	view := projAnsiRe.ReplaceAllString(m.View(), "")
	for _, want := range []string{"Mine", "In Progress", "50%", "the summary", "ZEN-1", "ZEN-2"} {
		if !strings.Contains(view, want) {
			t.Errorf("project overlay missing %q", want)
		}
	}
	// esc closes it.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if next.(Model).detailProj != nil {
		t.Error("esc should close the project overlay")
	}
}

// The top-10 focus rule must count only what the priority sections render;
// project tickets live elsewhere and must not fold a group over work it holds.
func TestAutoFoldIgnoresProjectTickets(t *testing.T) {
	var issues []linear.Issue
	// 12 Urgent tickets, all inside a project — they must not consume the focus
	// budget that keeps the High group open.
	for i := range 12 {
		issues = append(issues, linear.Issue{
			Identifier: "PRJ-" + string(rune('A'+i)), Title: "in a project", Priority: 1, PrioLabel: "Urgent",
			StateName: "Triage", StateType: "started", ProjectID: "p1", ProjectName: "Mine", ProjectSlugID: "s1",
		})
	}
	issues = append(issues, linear.Issue{
		Identifier: "ZEN-9", Title: "bare high", Priority: 2, PrioLabel: "High", StateName: "Triage", StateType: "started",
	})
	m := New(projFetcher{fakeFetcher{issues}}, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: issues})
	m = next.(Model)
	if m.collapsed["High"] {
		t.Error("the only priority group with tickets in it should not be auto-folded")
	}
}

func TestProgressBarAndCellWidth(t *testing.T) {
	for _, tc := range []struct {
		frac float64
		want string
	}{
		{0, "░░░░"},
		{1, "████"},
		{0.5, "██░░"},
		{0.25, "█░░░"},
	} {
		if got := progressBar(tc.frac); got != tc.want {
			t.Errorf("progressBar(%v) = %q, want %q", tc.frac, got, tc.want)
		}
	}
	// The cell must be exactly the width of a ticket row's id column, or project
	// and ticket rows stop lining up.
	for _, f := range []float64{0, 0.07, 0.5, 0.999, 1} {
		if got := runewidth.StringWidth(progressCell(f)); got != 9 {
			t.Errorf("progressCell(%v) is %d columns, want 9", f, got)
		}
	}
	// A project we never fetched says so rather than claiming 0%.
	cell := progressCellFor(linear.Project{Mine: false})
	if !strings.Contains(cell, "n/a") {
		t.Errorf("an unfetched project should not assert a progress figure, got %q", cell)
	}
	if runewidth.StringWidth(cell) != 9 {
		t.Errorf("the n/a cell must keep the column width, got %d", runewidth.StringWidth(cell))
	}
}

var projAnsiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// A project row must respect the row width for the same reason a ticket row
// does: an overrun wraps and breaks the whole list's alignment.
func TestProjectRowNeverOverrunsItsWidth(t *testing.T) {
	m := loadedProjects(t)
	m.width = 90
	long := linear.Project{
		Name:     "A project with a deliberately very long name 🚀 that should be truncated well before the edge",
		SlugID:   "s1",
		Mine:     true,
		Progress: 0.42,
		Health:   "offTrack",
		Issues:   []linear.Issue{{Identifier: "ZEN-1", StateType: "started"}},
	}
	for _, sel := range []bool{false, true} {
		got := runewidth.StringWidth(projAnsiRe.ReplaceAllString(m.renderProject(long, sel), ""))
		if got > m.rowWidth() {
			t.Errorf("project row (selected=%v) rendered %d columns, row width is %d", sel, got, m.rowWidth())
		}
	}
}

func TestProjectFlagPrefersTheDateOverSelfReportedHealth(t *testing.T) {
	overdueAtRisk := linear.Project{TargetDate: "2020-01-01", Health: "atRisk"}
	if got, _ := projectFlag(overdueAtRisk); got != "⚑ overdue" {
		t.Errorf("an overdue project should say so, got %q", got)
	}
	if got, _ := projectFlag(linear.Project{Health: "offTrack"}); got != "⚑ off track" {
		t.Errorf("off track flag = %q", got)
	}
	if got, _ := projectFlag(linear.Project{Health: "onTrack"}); got != "" {
		t.Errorf("a healthy project needs no flag, got %q", got)
	}
	// A finished project is not overdue.
	done := linear.Project{TargetDate: "2020-01-01", StatusType: "completed"}
	if got, _ := projectFlag(done); got == "⚑ overdue" {
		t.Error("a completed project should not be flagged overdue")
	}
}

// Search reaches into the Projects section: by project name, and by the tickets
// inside it.
func TestSearchMatchesProjectsAndTheirTickets(t *testing.T) {
	m := loadedProjects(t)
	m.setSearch("Mine")
	var names []string
	for _, r := range m.rows {
		if r.kind == rowProject {
			names = append(names, r.project.Name)
		}
	}
	if !slices.Equal(names, []string{"Mine"}) {
		t.Errorf("searching a project name should narrow to it, got %v", names)
	}

	m = loadedProjects(t)
	m.setSearch("ONB-9")
	names = nil
	for _, r := range m.rows {
		if r.kind == rowProject {
			names = append(names, r.project.Name)
		}
	}
	if !slices.Equal(names, []string{"Theirs"}) {
		t.Errorf("searching a ticket should surface its project, got %v", names)
	}
	if !slices.Contains(issueRows(m), "ONB-9") {
		t.Error("a matching ticket should be visible under its project during a search")
	}
}

// The deck must look exactly as it did for someone with no projects at all.
func TestNoProjectsRendersUnchanged(t *testing.T) {
	m := loaded(t)
	for _, r := range m.rows {
		if r.kind == rowProject || r.kind == rowProjectHeader {
			t.Fatal("a deck with no projects must render no Projects section")
		}
	}
	if strings.Contains(m.View(), "PROJECTS") {
		t.Error("no project rows, no section header")
	}
}

// A finished project drops off the deck when its linger window is up — and its
// still-open tickets must land back in the priority sections rather than
// disappearing with the row.
func TestFinishedProjectDropsOffAndReturnsItsTickets(t *testing.T) {
	issues := []linear.Issue{
		{Identifier: "ZEN-1", Title: "still open", Priority: 1, PrioLabel: "Urgent", StateName: "Triage", StateType: "started",
			ProjectID: "p1", ProjectName: "Mine", ProjectSlugID: "s1"},
	}
	done := fixtureProjects()
	done[0].StatusName, done[0].StatusType = "Completed", "completed"
	done[0].CompletedAt = time.Now().Add(-2 * linear.DoneVisibleFor)

	f := projFetcher{fakeFetcher{issues}}
	m := New(f, "", true, fakeBackend{})
	next, _ := m.Update(refreshedMsg{issues: issues})
	next, _ = next.(Model).Update(projectsMsg{projects: done})
	m = next.(Model)
	m.width, m.height = 120, 40
	for k := range m.collapsed {
		m.collapsed[k] = false
	}
	m.regroup()

	for _, r := range m.rows {
		if r.kind == rowProject {
			t.Fatalf("a project finished %v ago should be off the deck", linear.DoneVisibleFor*2)
		}
	}
	if got := issueRows(m); !slices.Equal(got, []string{"ZEN-1"}) {
		t.Errorf("the ticket should be back in the priority sections, got %v", got)
	}
}

// Inside the window it stays, struck through like a done ticket.
func TestRecentlyFinishedProjectRendersStruckThrough(t *testing.T) {
	// Under `go test` there is no TTY, so lipgloss renders every style as plain
	// text and the assertion below would pass on any row. Force a profile that
	// emits the attribute.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prev)

	done := fixtureProjects()[0]
	done.StatusName, done.StatusType = "Completed", "completed"
	done.CompletedAt = time.Now().Add(-time.Hour)

	m := loadedProjects(t)
	if row := m.renderProject(done, false); !struckThrough(row) {
		t.Errorf("a just-finished project row should be struck through: %q", row)
	}
	if struckThrough(m.renderProject(fixtureProjects()[0], false)) {
		t.Error("a live project row must not be struck through")
	}
}

// strikeRE matches SGR 9 (strikethrough) wherever it sits in an escape
// sequence: lipgloss folds it in with the faint and color attributes, so the
// bare "\x1b[9m" a row never actually carries is the wrong thing to look for.
var strikeRE = regexp.MustCompile("\x1b\\[(?:[0-9]+;)*9m")

func struckThrough(s string) bool { return strikeRE.MatchString(s) }
