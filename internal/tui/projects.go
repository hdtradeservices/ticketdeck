package tui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// This file holds the Projects section: the top-of-deck rows for the Linear
// projects that are mine, each of which opens a Claude session of its own.
//
// A project row is a peer of a ticket row, not a decoration. It carries the
// same session badge, the same account dot, and the same hotkeys (⏎ open, d
// detail, o browser, p PRs, H hand off, s/P/a writes) — the writes resolving to
// the project-side field in each case. `t` is the one ticket key that does not
// carry over: /triage triages an issue, and there is no project equivalent. Its
// tickets are pulled out of the priority groups below and hang off the project
// instead, folded by default.

// ── derivation ───────────────────────────────────────────────────────────────

// deckProjects is the Projects section before the search filter: my projects
// plus any project holding tickets of mine, each carrying its own tickets, with
// the finished ones that have aged out dropped.
//
// It is also the answer to "which tickets does the Projects section own", which
// is why the unfiltered list exists separately: a ticket whose project is not in
// here belongs back in the priority groups.
func (m Model) deckProjects() []linear.Project {
	if len(m.myProjects) == 0 && !m.anyIssueHasProject() {
		return nil
	}
	return linear.FilterVisibleProjects(linear.AugmentProjects(m.myProjects, linear.FilterVisible(m.allIssues)))
}

// visibleProjects is the render source for the Projects section: deckProjects
// with the search filter applied.
func (m Model) visibleProjects() []linear.Project {
	ps := m.deckProjects()
	if !m.searching() {
		return ps
	}
	q := strings.ToLower(strings.TrimSpace(m.searchQuery))
	out := make([]linear.Project, 0, len(ps))
	for _, p := range ps {
		nameHit := strings.Contains(strings.ToLower(p.Name), q)
		// A project whose name matches keeps all its tickets; one that matched
		// only through a ticket shows just the tickets that matched, so the
		// section answers the query rather than burying it.
		if !nameHit {
			var hits []linear.Issue
			for _, is := range p.Issues {
				if m.searchMatch(is) {
					hits = append(hits, is)
				}
			}
			if len(hits) == 0 {
				continue
			}
			p.Issues = hits
		}
		out = append(out, p)
	}
	return out
}

// anyIssueHasProject reports whether any assigned ticket belongs to a project.
// It is the cheap guard that keeps a deck with no projects at all — and a
// --demo fetcher that has none — rendering exactly as it did before.
func (m Model) anyIssueHasProject() bool {
	for _, is := range m.allIssues {
		if is.ProjectID != "" {
			return true
		}
	}
	return false
}

// ── selection ────────────────────────────────────────────────────────────────

// selectedProject returns the project under the cursor, if the cursor is on a
// project row.
func (m Model) selectedProject() (linear.Project, bool) {
	if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].kind == rowProject {
		return m.rows[m.cursor].project, true
	}
	return linear.Project{}, false
}

// enclosingProjectRow returns the index of the project row the cursor sits
// under, for a ticket nested inside the Projects section. -1 when the cursor is
// not in that section.
func (m Model) enclosingProjectRow() int {
	for i := m.cursor; i >= 0 && i < len(m.rows); i-- {
		switch m.rows[i].kind {
		case rowProject:
			return i
		case rowPrio, rowProjectHeader, rowSessionHeader:
			return -1
		}
	}
	return -1
}

// projectByKey finds a rendered project by its session key.
func (m Model) projectByKey(key string) (linear.Project, bool) {
	for _, p := range m.projects {
		if p.Key() == key {
			return p, true
		}
	}
	return linear.Project{}, false
}

// label turns a session key into what a human should read in a notice: a
// project's name rather than the synthetic "proj-<slug>" key it is stored
// under. Ticket keys are already the right thing to show.
func (m Model) label(key string) string {
	if p, ok := m.projectByKey(key); ok {
		return truncCols(p.Name, 40)
	}
	return key
}

// ── folding ──────────────────────────────────────────────────────────────────

// foldProjects handles a fold keypress inside the Projects section and reports
// whether it consumed it. On the section header it folds the whole section; on
// a project row (or a ticket nested under one) it folds that project's tickets.
func (m *Model) foldProjects(mode string) bool {
	i := m.cursor
	if i < 0 || i >= len(m.rows) {
		return false
	}
	switch m.rows[i].kind {
	case rowProjectHeader:
		m.projFolded = applyFold(m.projFolded, mode)
		m.regroup()
		m.cursorToProjectHeader()
		m.ensureVisible()
		return true
	case rowProject:
		m.toggleProject(m.rows[i].project.Key(), mode)
		return true
	case rowIssue:
		if pi := m.enclosingProjectRow(); pi >= 0 {
			// Collapsing from inside a project's ticket list has to land the
			// cursor back on the project, or it would be left pointing at a row
			// that no longer exists.
			m.toggleProject(m.rows[pi].project.Key(), mode)
			return true
		}
	}
	return false
}

// applyFold resolves a fold keypress against a current state: "collapse" and
// "expand" force a direction, "" toggles.
func applyFold(folded bool, mode string) bool {
	switch mode {
	case "collapse":
		return true
	case "expand":
		return false
	}
	return !folded
}

// toggleProject folds or unfolds one project's ticket list, leaving the cursor
// on the project row.
func (m *Model) toggleProject(key, mode string) {
	m.projExpanded[key] = !applyFold(!m.projExpanded[key], mode)
	m.regroup()
	for i, r := range m.rows {
		if r.kind == rowProject && r.project.Key() == key {
			m.cursor = i
			break
		}
	}
	m.ensureVisible()
}

func (m *Model) cursorToProjectHeader() {
	for i, r := range m.rows {
		if r.kind == rowProjectHeader {
			m.cursor = i
			if !m.projFolded {
				m.cursor = m.nearestCursorable(i + 1)
			}
			m.ensureVisible()
			return
		}
	}
	m.cursor = m.firstCursorable()
}

// ── launching ────────────────────────────────────────────────────────────────

// toProjectTicket builds the launch unit for a project session. The identity
// prompt gets the project's summary and the keys of my open tickets in it, so
// the session starts knowing its own scope.
func toProjectTicket(p linear.Project) session.Ticket {
	var ctx strings.Builder
	if s := strings.TrimSpace(p.Summary); s != "" {
		fmt.Fprintf(&ctx, "Summary: %s", strings.TrimSuffix(s, "."))
		ctx.WriteString(". ")
	}
	fmt.Fprintf(&ctx, "Progress: %d%% of scope complete.", p.ProgressPct())
	if p.TargetDate != "" {
		fmt.Fprintf(&ctx, " Target date: %s.", p.TargetDate)
	}
	var keys []string
	for _, is := range p.Issues {
		if !is.IsDone() {
			keys = append(keys, is.Identifier)
		}
	}
	if len(keys) > 0 {
		fmt.Fprintf(&ctx, " My open tickets in it: %s.", strings.Join(keys, ", "))
	}
	return session.Ticket{
		Key:       p.Key(),
		Title:     p.Name,
		URL:       p.URL,
		Status:    p.StatusName,
		PrioLabel: p.PrioLabel,
		Team:      strings.Join(p.TeamKeys, ", "),
		Project:   true,
		Context:   strings.TrimSpace(ctx.String()),
	}
}

// launchProject opens a project's session, taking the same one-deck-per-session
// gate a ticket does: the session id derives from the project key alone, so a
// second deck opening it would fork the transcript exactly as it would a
// ticket's.
func (m Model) launchProject(p linear.Project) (tea.Model, tea.Cmd) {
	if o, ok := m.blockingOwner(p.Key()); ok {
		m.conflict = &conflict{project: &p, owner: o}
		m.notice = ""
		return m, nil
	}
	return m.launchProjectNow(p)
}

func (m Model) launchProjectNow(p linear.Project) (tea.Model, tea.Cmd) {
	spec, err := m.backend.Plan(toProjectTicket(p), m.root)
	if err != nil {
		debugLog.Printf("plan error for project %s: %v", p.Key(), err)
		m.err = err
		return m, nil
	}
	if !m.dry && !spec.Foreground && !m.hideOpenHint {
		s := spec
		m.openHintSpec = &s
		m.openHintLabel = truncCols(p.Name, 40)
		return m, nil
	}
	return m.runSpec(spec, p.Key())
}

// openProjectPRs opens the PRs linked to a project's tickets — the whole
// project's code review in one place. Reuses the ticket PR picker.
func (m Model) openProjectPRs(p linear.Project) (tea.Model, tea.Cmd) {
	prs := linear.ProjectPRs(p)
	switch len(prs) {
	case 0:
		m.notice = "no linked PRs across " + truncCols(p.Name, 40)
		return m, nil
	case 1:
		m.notice = "opened " + prs[0].Label()
		return m, openBrowser(prs[0].URL)
	}
	// The picker names its subject with prIssue; a synthetic issue carrying the
	// project's name is what makes the shared overlay read correctly.
	m.prMenu = true
	m.prIssue = linear.Issue{Identifier: truncCols(p.Name, 40)}
	m.prList = prs
	m.prCursor = 0
	return m, nil
}

// ── key handling ─────────────────────────────────────────────────────────────

// projectKey handles the action keys on a project row and reports whether it
// consumed the keypress. Navigation, folding, search, refresh and quit return
// false and fall through to the list's own handling, so a project row moves and
// folds exactly like every other row.
func (m Model) projectKey(msg tea.KeyMsg, p linear.Project) (tea.Model, tea.Cmd, bool) {
	switch msg.String() {
	case "enter":
		next, cmd := m.launchProject(p)
		return next, cmd, true
	case "d":
		c := p
		m.detailProj = &c
		m.detailOffset = 0
		return m, nil, true
	case "o":
		if p.URL == "" {
			return m, nil, true
		}
		m.notice = "opened " + truncCols(p.Name, 40) + " in browser"
		return m, openBrowser(p.URL), true
	case "p":
		next, cmd := m.openProjectPRs(p)
		return next, cmd, true
	case "t":
		// /triage is a ticket workflow — it triages one issue. Firing it at a
		// project session would hand the skill a subject it has no procedure
		// for, so the key says so rather than starting something incoherent.
		m.notice = triageCmd + " works on a ticket, not a project — open one of its tickets"
		return m, nil, true
	case "H":
		next, cmd := m.openHandoffFor(p.Key(), truncCols(p.Name, 40))
		return next, cmd, true
	case "s":
		if m.projWriter == nil {
			m.notice = "project status changes need a live Linear connection"
			return m, nil, true
		}
		m.statusMenu = true
		m.statusPend = ""
		return m, nil, true
	case "P":
		if m.projWriter == nil {
			m.notice = "project priority changes need a live Linear connection"
			return m, nil, true
		}
		m.priorityMenu = true
		return m, nil, true
	case "a":
		if m.projWriter == nil || m.assigner == nil {
			m.notice = "project lead changes need a live Linear connection"
			return m, nil, true
		}
		c := p
		m.assignMenu = true
		m.assignProj = &c
		m.assignQuery = ""
		m.assignCursor = 0
		if m.users == nil {
			return m, m.fetchUsers(), true
		}
		return m, nil, true
	}
	return m, nil, false
}

// projectStatusTargets are the statuses `s` offers on a project row, keyed the
// way the ticket menu is where the two overlap (d done, c cancel, b blocked).
var projectStatusTargets = []struct {
	key, target string
}{
	{"p", "Planned"},
	{"i", "In Progress"},
	{"b", "Blocked"},
	{"d", "Completed"},
	{"c", "Canceled"},
}

// updateProjectStatus drives the two-step project status change: pick a target,
// then confirm — the same guard the ticket menu uses against a stray keypress.
func (m Model) updateProjectStatus(msg tea.KeyMsg, p linear.Project) (tea.Model, tea.Cmd) {
	// The shared status menu is reached from the ticket path too, so never assume
	// a Fetcher that can write tickets can also write projects.
	if m.projWriter == nil {
		m.statusMenu, m.statusPend = false, ""
		m.notice = "project status changes need a live Linear connection"
		return m, nil
	}
	if m.statusPend == "" {
		for _, t := range projectStatusTargets {
			if msg.String() == t.key {
				m.statusPend = t.target
				return m, nil
			}
		}
		if s := msg.String(); s == "esc" || s == "q" || s == "s" {
			m.statusMenu = false
		}
		return m, nil
	}
	switch msg.String() {
	case "y", "enter":
		target := m.statusPend
		m.statusMenu, m.statusPend = false, ""
		m.notice = fmt.Sprintf("moving %s → %s…", truncCols(p.Name, 40), target)
		w := m.projWriter
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := w.SetProjectStatus(ctx, p, target)
			return projectWriteMsg{key: p.Key(), name: p.Name, what: "status", value: target, err: err}
		}
	case "esc", "n", "q":
		m.statusMenu, m.statusPend = false, ""
		m.notice = "status change canceled"
	}
	return m, nil
}

// updateProjectPriority sets a project's priority on a single keypress, like
// the ticket menu (low-risk and reversible, so no separate confirm).
func (m Model) updateProjectPriority(msg tea.KeyMsg, p linear.Project) (tea.Model, tea.Cmd) {
	if m.projWriter == nil {
		m.priorityMenu = false
		m.notice = "project priority changes need a live Linear connection"
		return m, nil
	}
	prio, label, ok := priorityFor(msg.String())
	if !ok {
		if s := msg.String(); s == "esc" || s == "q" || s == "P" {
			m.priorityMenu = false
		}
		return m, nil
	}
	m.priorityMenu = false
	m.notice = fmt.Sprintf("setting %s → %s…", truncCols(p.Name, 40), label)
	w := m.projWriter
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := w.SetProjectPriority(ctx, p, prio)
		return projectWriteMsg{key: p.Key(), name: p.Name, what: "priority", value: label, err: err}
	}
}

// setProjectLead writes the lead chosen in the assignee picker. A project's
// lead is the closest thing it has to an assignee, so `a` maps to it.
func (m Model) setProjectLead(p linear.Project, userID, who string) (tea.Model, tea.Cmd) {
	if m.projWriter == nil {
		m.notice = "project lead changes need a live Linear connection"
		return m, nil
	}
	m.notice = fmt.Sprintf("setting %s lead → %s…", truncCols(p.Name, 40), who)
	w := m.projWriter
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := w.SetProjectLead(ctx, p, userID)
		return projectWriteMsg{key: p.Key(), name: p.Name, what: "lead", value: who, err: err}
	}
}

// updateProjectDetail handles keys while the project detail overlay is open.
func (m Model) updateProjectDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := *m.detailProj
	switch msg.String() {
	case "esc", "q", "d":
		m.detailProj = nil
		m.detailOffset = 0
	case "enter":
		m.detailProj = nil
		m.detailOffset = 0
		return m.launchProject(p)
	case "o":
		if p.URL != "" {
			return m, openBrowser(p.URL)
		}
	case "p":
		m.detailProj = nil
		m.detailOffset = 0
		return m.openProjectPRs(p)
	case "up", "k":
		if m.detailOffset > 0 {
			m.detailOffset--
		}
	case "down", "j":
		m.detailOffset++
	case "pgup", "ctrl+u":
		m.detailOffset = max(m.detailOffset-10, 0)
	case "pgdown", "ctrl+d":
		m.detailOffset += 10
	}
	return m, nil
}

// ── rendering ────────────────────────────────────────────────────────────────

// projectChipColor is the Projects section header's accent — deliberately not
// one of the priority colors, so the top section reads as its own thing rather
// than as another priority band.
var projectChipColor = lipgloss.Color("212")

// renderProjectHeader draws the section header, mirroring a priority header so
// the two fold the same way and look like peers.
func (m Model) renderProjectHeader(r row, selected bool) string {
	caret := "▾"
	if m.projFolded {
		caret = "▸"
	}
	body := fmt.Sprintf(" %s PROJECTS · %d ", caret, r.count)
	if selected {
		return selStyle.Width(m.rowWidth()).Render("▶" + body)
	}
	chip := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(projectChipColor)
	return chip.Render(body)
}

// progressBarCells is how many columns the bar occupies. Four cells with
// eighth-block partials give ~3% resolution, which is finer than the percentage
// beside it needs — and keeps the whole cell the width of a ticket's id column.
const progressBarCells = 4

// eighthBlocks are the partial-fill glyphs for the last cell of the bar.
var eighthBlocks = []rune("▏▎▍▌▋▊▉")

// progressBar renders frac (0..1) as a fixed-width block bar.
func progressBar(frac float64) string {
	frac = min(max(frac, 0), 1)
	eighths := int(math.Round(frac * float64(progressBarCells*8)))
	var b strings.Builder
	for i := range progressBarCells {
		switch n := eighths - i*8; {
		case n >= 8:
			b.WriteRune('█')
		case n <= 0:
			b.WriteRune('░')
		default:
			b.WriteRune(eighthBlocks[n-1])
		}
	}
	return b.String()
}

// progressCell is the bar plus its percentage, sized to exactly the width of a
// ticket row's identifier column so project and ticket rows share one grid.
func progressCell(frac float64) string {
	return fmt.Sprintf("%s %3d%%", progressBar(frac), linear.ProgressPct(frac))
}

// progressCellFor is progressCell for a project row, which has one extra case:
// a project discovered through a ticket was never fetched, so its progress is
// unknown rather than zero. An empty bar would assert nothing has been done,
// which is a claim this deck has no basis for.
func progressCellFor(p linear.Project) string {
	if !p.Mine {
		return fmt.Sprintf("%9s", "n/a")
	}
	return progressCell(p.Progress)
}

// projectFlag is the project row's trailing flag, in the slot a ticket uses for
// its validation label. An overdue target date outranks a health signal: the
// date is a fact, the health is someone's last self-report.
func projectFlag(p linear.Project) (string, lipgloss.Color) {
	// A finished project is counting down to dropping off the deck. Neither a
	// missed date nor a stale health self-report is something to act on now, and
	// the row is struck through to say the work is over.
	if p.Finished() {
		return "", lipgloss.Color("")
	}
	if overdue(p.TargetDate) {
		return "⚑ overdue", lipgloss.Color("203")
	}
	switch p.Health {
	case "offTrack":
		return "⚑ off track", lipgloss.Color("203")
	case "atRisk":
		return "⚑ at risk", lipgloss.Color("214")
	}
	return "", lipgloss.Color("")
}

// overdue reports whether a Linear TimelessDate ("2026-09-04") is in the past.
func overdue(date string) bool {
	if date == "" {
		return false
	}
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return false
	}
	return d.Before(time.Now().Truncate(24 * time.Hour))
}

// renderProject draws one project row: the same account dot and session badge a
// ticket carries, then its progress where a ticket shows its id, then its PR
// count, name, open-ticket count, and flag.
func (m Model) renderProject(p linear.Project, selected bool) string {
	key := p.Key()
	st, elapsed, remote := m.rowSession(key)
	cell, color := sessionCellText(st, elapsed)
	own, ownColor := m.ownerCell(m.owners[key].Name, selected)
	if remote {
		color = ownColor
	}
	prs := linear.ProjectPRs(p)
	prG, prC := prMark(prs)
	prog := progressCellFor(p)
	flagText, flagColor := projectFlag(p)
	prio := prioMark(p.Priority)

	count := ""
	if n := p.OpenIssueCount(); n > 0 {
		count = fmt.Sprintf("%d open", n)
	}
	caret := "▸"
	if m.projExpanded[key] || m.searching() {
		caret = "▾"
	}
	if len(p.Issues) == 0 {
		caret = " " // nothing to unfold
	}

	// Same column budget as a ticket row, with the progress cell standing in for
	// the identifier so the two line up. The last 4 are the caret and the
	// priority mark, each with its trailing space.
	avail := m.rowWidth() - (2 + m.ownerCol + sessionCol + 1 + 9 + 1 + prMarkCol + 1 + 4)
	if flagText != "" {
		avail -= cols(flagText) + 1
	}
	if count != "" {
		avail -= cols(count) + 1
	}
	if avail < 12 {
		avail = 12
	}
	name := truncCols(p.Name, avail)

	if selected {
		content := fmt.Sprintf("▶ %s%s %s %s %s %s %s", own, cell, prog, prG, caret, prio, name)
		if count != "" {
			content += " " + count
		}
		if flagText != "" {
			content += " " + flagText
		}
		style := selStyle
		if p.Finished() {
			style = style.Strikethrough(true)
		}
		return style.Width(m.rowWidth()).Render(content)
	}

	// A finished project lingers struck through for the rest of its window, the
	// same way a done ticket does, then drops off. Strike the text tokens only —
	// not the column gaps, and never the account dot, which is the one thing on
	// the row that has to stay readable at a glance.
	if p.Finished() {
		strike := doneRowStyle
		gap := doneRowStyle.Strikethrough(false)
		out := "  " + lipgloss.NewStyle().Foreground(ownColor).Render(own) +
			strike.Render(cell) + gap.Render(" ") +
			strike.Render(prog) + gap.Render(" ") +
			strike.Render(prG) + gap.Render(" ") +
			gap.Render(caret+" "+prio+" ") +
			strike.Render(name)
		if count != "" {
			out += gap.Render(" ") + strike.Render(count)
		}
		return out
	}

	nameStyle := lipgloss.NewStyle()
	if !p.Mine {
		// Surfaced only because it holds tickets of mine — real, but not my
		// project. Dimming says so without spending a column on it.
		nameStyle = dimStyle
	}
	out := fmt.Sprintf("  %s%s %s %s %s %s %s",
		lipgloss.NewStyle().Foreground(ownColor).Render(own),
		lipgloss.NewStyle().Foreground(color).Render(cell),
		lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Render(prog),
		lipgloss.NewStyle().Foreground(prC).Render(prG),
		dimStyle.Render(caret),
		lipgloss.NewStyle().Foreground(prioColor(p.PrioLabel)).Render(prio),
		nameStyle.Render(name))
	if count != "" {
		out += " " + dimStyle.Render(count)
	}
	if flagText != "" {
		out += " " + lipgloss.NewStyle().Foreground(flagColor).Render(flagText)
	}
	return out
}

// prioMark is the one-column priority tick on a project row, colored by
// prioColor. The Projects section is ordered by priority and has no priority
// headers to say so — without this the order looks arbitrary. A project with no
// priority set gets a blank, which keeps every row's columns aligned and says
// "nothing here" rather than asserting a level.
func prioMark(priority int) string {
	if priority == 0 {
		return " "
	}
	return "▌"
}

// renderProjectDetail draws the overlay for a project: its status, progress,
// dates, lead, session, the PRs across its tickets, its tickets, and its
// description.
func (m Model) renderProjectDetail() string {
	p := *m.detailProj
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n", titleStyle.Render(p.Name))

	meta := []string{p.StatusName}
	if p.PrioLabel != "" {
		meta = append(meta, p.PrioLabel)
	}
	if len(p.TeamKeys) > 0 {
		meta = append(meta, strings.Join(p.TeamKeys, "/"))
	}
	if p.LeadName != "" {
		meta = append(meta, "lead "+p.LeadName)
	}
	if !p.Mine {
		meta = append(meta, "not yours — shown because it holds your tickets")
	}
	fmt.Fprintf(&b, "%s\n", dimStyle.Render(strings.Join(meta, " · ")))

	// Progress, then the dates that give it meaning.
	prog := lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Render(progressCellFor(p))
	scope := ""
	if p.Scope > 0 {
		scope = fmt.Sprintf(" of %g points", p.Scope)
	}
	fmt.Fprintf(&b, "%s%s\n", prog, dimStyle.Render(scope))
	dates := []string{}
	if p.StartDate != "" {
		dates = append(dates, "started "+p.StartDate)
	}
	if p.TargetDate != "" {
		d := "target " + p.TargetDate
		if overdue(p.TargetDate) && !p.IsDone() {
			d += " (overdue)"
		}
		dates = append(dates, d)
	}
	if flag, color := projectFlag(p); flag != "" {
		fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Foreground(color).Render(flag))
	}
	if len(dates) > 0 {
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(strings.Join(dates, " · ")))
	}

	st := m.sessions[p.Key()]
	glyph, label, color := sessionStyle(st)
	sess := lipgloss.NewStyle().Foreground(color).Render(glyph + " " + label)
	if label == "" {
		sess = dimStyle.Render("· no session yet — press ⏎ to start one")
	} else if o := m.owners[p.Key()]; o.Name != "" {
		sess += m.acctStyle(o.Name).Render("  ⦿ " + o.Name)
	}
	fmt.Fprintf(&b, "%s\n", sess)
	if p.URL != "" {
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(p.URL))
	}

	prs := linear.ProjectPRs(p)
	switch len(prs) {
	case 0:
	case 1:
		fmt.Fprintf(&b, "%s\n", dimStyle.Render("⇄ "+prs[0].Label()+" — p to open it"))
	default:
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(fmt.Sprintf("⇄ %d PRs across its tickets — p to pick one", len(prs))))
	}

	width := m.width
	if width <= 0 {
		width = 80
	}

	// Body: my tickets in the project, then its description.
	var body strings.Builder
	issues := linear.SortProjectIssues(p.Issues)
	if len(issues) > 0 {
		fmt.Fprintf(&body, "%s\n", sectionStyle.Render(fmt.Sprintf("Your tickets (%d)", len(issues))))
		for _, is := range issues {
			glyph, _, color := sessionStyle(m.sessions[is.Identifier])
			line := fmt.Sprintf("%s %-9s %-11s %s",
				lipgloss.NewStyle().Foreground(color).Render(glyph),
				idStyle.Render(is.Identifier),
				dimStyle.Render(truncCols(is.StateName, 11)),
				truncCols(is.Title, max(width-28, 12)))
			fmt.Fprintf(&body, "%s\n", line)
		}
		body.WriteString("\n")
	}
	desc := strings.TrimSpace(cmpOr(p.Content, p.Summary))
	if desc == "" {
		body.WriteString(dimStyle.Render("(no project description)"))
	} else {
		body.WriteString(renderMarkdown(desc, width-2))
	}

	lines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
	h := m.height - 10
	if h < 1 || m.height <= 0 {
		h = len(lines)
	}
	off := min(max(m.detailOffset, 0), max(len(lines)-1, 0))
	end := min(off+h, len(lines))
	for _, ln := range lines[off:end] {
		fmt.Fprintf(&b, "%s\n", ln)
	}

	more := ""
	if end < len(lines) {
		more = " · ↕ more"
	}
	hint := "⏎ open session · o browser"
	if len(prs) > 0 {
		hint += " · p PRs"
	}
	fmt.Fprintf(&b, "\n%s", dimStyle.Render("↑/↓ scroll · "+hint+" · d/esc back"+more))
	return b.String()
}

// cmpOr returns the first non-empty string.
func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
