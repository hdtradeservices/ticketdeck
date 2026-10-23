package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/route"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// ── the info line ────────────────────────────────────────────────────────────

// infoLine says what the highlighted row's session is doing, in more words than
// its badge has room for: which account it is on, what it last did, how full
// its context is, and why it stopped. A row with no session says where ⏎ would
// open it, so the router's choice is visible before it is acted on.
func (m Model) infoLine() string {
	key, label := "", ""
	switch {
	case m.cursor < 0 || m.cursor >= len(m.rows):
		return ""
	case m.rows[m.cursor].kind == rowIssue:
		key = m.rows[m.cursor].issue.Identifier
		label = key
	case m.rows[m.cursor].kind == rowProject:
		key = m.rows[m.cursor].project.Key()
		label = m.label(key)
	case m.rows[m.cursor].kind == rowSession:
		key = m.rows[m.cursor].ref.Name
		label = m.rows[m.cursor].ref.Title
	default:
		return ""
	}
	s, ok := m.fleet.Best(key)
	if !ok {
		if m.demo || !m.multiAccount() || m.rows[m.cursor].kind == rowSession {
			return ""
		}
		a, why := m.launchAccount("")
		return dimStyle.Render("  ⏎ opens "+label+" on ") + m.acctStyle(a.Name).Render("⦿ "+a.Name) + dimStyle.Render(" — "+why)
	}
	return m.describe(s)
}

// describe renders one session's state as a line of prose.
func (m Model) describe(s fleet.Session) string {
	now := time.Now()
	var parts []string
	acct := m.acctStyle(s.Account).Render("⦿ " + s.Account)
	glyph, label, color := sessionStyle(s.Status)
	state := glyph + " " + label
	tail := s.Activity.Tail
	note := m.carryNote(s, now)
	switch {
	case s.Status == session.Limited && tail.Err != nil:
		state = "⊘ out of usage"
		if !tail.Err.ResetsAt.IsZero() {
			state += " until " + route.Clock(tail.Err.ResetsAt, now)
		}
		switch {
		case m.moving[s.Key]:
			state += " · moving it…"
		case !m.settings.autoMove():
			state += " · auto-move is off (A) — H moves it"
		case !s.Home:
			state += " · in the " + s.Workspace + " deck — M brings it here"
		case note != "":
			state += " · " + note
		}
	case s.Status == session.Errored && tail.Err != nil:
		state = "✗ " + firstLine(tail.Err.Text)
		if note != "" {
			state += " · " + note
		}
	case s.Status == session.NeedsInput && s.WaitingFor != "":
		state += ": " + s.WaitingFor
	case !s.Live:
		state = "↻ resumable"
		if e := elapsedLabel(s.LastActive); e != "" {
			state += ", last wrote " + e + " ago"
		}
		if note != "" {
			state += " · " + note
		}
	case note != "":
		state += " · " + note
	}
	parts = append(parts, acct+" "+lipgloss.NewStyle().Foreground(color).Render(state))
	if s.Live && !s.Home {
		where := "in the " + s.Workspace + " deck"
		if s.Workspace == "" {
			where = "outside this deck"
		}
		parts = append(parts, dimStyle.Render(where+" · M brings it here"))
	}
	if in := s.Activity.InFlight; in.Any() {
		var bits []string
		if in.Agents > 0 {
			bits = append(bits, plural(in.Agents, "subagent"))
		}
		if in.Tasks > 0 {
			bits = append(bits, plural(in.Tasks, "background task"))
		}
		parts = append(parts, dimStyle.Render(strings.Join(bits, " + ")+" running"))
	}
	if tail.LastTool != "" && s.Live && s.Status != session.Limited {
		parts = append(parts, dimStyle.Render("last: "+truncCols(tail.LastTool, 50)))
	}
	if n := tail.ContextTokens; n > 0 {
		txt := "context " + kTokens(n)
		if c, big := contextColor(n); big {
			if s.Live && s.Home {
				txt += " — C compacts it"
			}
			parts = append(parts, lipgloss.NewStyle().Foreground(c).Render(txt))
		} else {
			parts = append(parts, dimStyle.Render(txt))
		}
	}
	if b := s.Activity.Burn; s.Live && b >= 0.01 {
		txt := dollars(b) + " in 5h"
		if total := m.spentBy()[s.Account]; total > b {
			txt += fmt.Sprintf(" (%.0f%% of ⦿%s's)", 100*b/total, s.Account)
		}
		parts = append(parts, dimStyle.Render(txt))
	}
	if e, ok := m.ledger[s.ID]; ok {
		if mv, ok := e.LastMove(); ok && now.Sub(mv.At) < 24*time.Hour {
			parts = append(parts, dimStyle.Render(fmt.Sprintf("moved from ⦿%s %s ago", mv.From, cmpElapsed(mv.At))))
		}
	}
	if !s.Live && m.multiAccount() && s.Kind != ledger.Scratch {
		d := m.routeFor(s.Account, "")
		if d.Account != "" && d.Account != s.Account {
			parts = append(parts, dimStyle.Render("⏎ resumes it on ⦿"+d.Account+": "+d.Why))
		}
	}
	return "  " + strings.Join(parts, dimStyle.Render(" · "))
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func cmpElapsed(t time.Time) string {
	if e := elapsedLabel(t); e != "" {
		return e
	}
	return "moments"
}

// ── help (?) ─────────────────────────────────────────────────────────────────

func (m Model) updateHelp(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "?":
		m.help = false
	case "up", "k":
		m.helpOffset = max(m.helpOffset-1, 0)
	case "down", "j":
		m.helpOffset++
	}
	return m, nil
}

var helpText = []struct{ key, what string }{
	{"", "Tickets and projects"},
	{"⏎", "open the session: attach, resume where it lives, or start on the best-placed account"},
	{"d", "description · in it: i investigation · P plan · r re-read comments"},
	{"o / p", "open in the browser / open its PR (several: a picker)"},
	{"t", "/triage in the background (tickets only)"},
	{"s · P · a", "status · priority · assignee (a project's lead) — writes to Linear"},
	{"/", "filter the list · esc clears"},
	{"␣ ← →", "fold and unfold sections and projects"},
	{"", "Sessions and accounts"},
	{"H", "move the session to another account (no session yet: open it on one)"},
	{"A", "accounts: usage, sessions, pin new sessions to one, auto-move on/off"},
	{"M", "bring in sessions still running in the old per-account decks"},
	{"n", "new ad-hoc session — name it, pick its account with tab"},
	{"R", "rename an ad-hoc session (Other sessions rows)"},
	{"x", "close an Other sessions row (an ad-hoc one drops off; its conversation stays on disk)"},
	{"C", "compact the session's conversation now, with an optional focus (it must be idle)"},
	{"] [", "jump to the next / previous session waiting on you"},
	{"", "Badges"},
	{"● ◐", "working · lead idle but background work running"},
	{"◆", "needs input — a permission prompt or a question"},
	{"◉", "your turn — it finished a turn you haven't looked at"},
	{"○", "idle"},
	{"⊘", "out of usage on its account — it moves on its own when auto-move is on"},
	{"✗", "stopped on an API error (login expired, API unreachable)"},
	{"↻", "resumable — its conversation is on disk"},
	{"◔640k", "its context is that large: every turn re-reads all of it (orange 300k+, red 600k+)"},
	{"⦿", "the account running it, in that account's color"},
	{"", "herdr"},
	{"Ctrl+b 1", "back to the deck from any session"},
	{"Ctrl+b g", "jump to any session by name"},
	{"Ctrl+b i", "this ticket's description, from inside its session"},
	{"Ctrl+b q", "detach — every session keeps running"},
}

func (m Model) renderHelp() string {
	var lines []string
	for _, h := range helpText {
		if h.key == "" {
			lines = append(lines, "", sectionStyle.Render(h.what))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s  %s", noticeStyle.Render(fmt.Sprintf("%-9s", h.key)), h.what))
	}
	if m.width > 0 {
		for i, l := range lines {
			lines[i] = truncANSI(l, m.width)
		}
	}
	h := m.height - 3
	if h < 3 || m.height <= 0 {
		h = len(lines)
	}
	off := min(m.helpOffset, max(len(lines)-h, 0))
	end := min(off+h, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", titleStyle.Render("TicketDeck keys"), dimStyle.Render("↑/↓ scroll · esc close"))
	for _, l := range lines[off:end] {
		fmt.Fprintf(&b, "%s\n", l)
	}
	return b.String()
}

// ── what needs you ───────────────────────────────────────────────────────────

// needy reports whether a session is waiting on the user: a question or a
// permission prompt, a finished turn nobody has read, or a stop it can't get
// past alone.
func needy(st session.Status) bool {
	switch st {
	case session.NeedsInput, session.Unread, session.Limited, session.Errored:
		return true
	}
	return false
}

// rowStatus is the status a row badges, whichever section it is in.
func (m Model) rowStatus(r row) session.Status {
	switch r.kind {
	case rowIssue:
		st, _, remote := m.rowSession(r.issue.Identifier)
		if remote {
			return session.None // another deck's to answer
		}
		return st
	case rowProject:
		st, _, remote := m.rowSession(r.project.Key())
		if remote {
			return session.None
		}
		return st
	case rowSession:
		if r.ref.Socket != "" {
			return session.None
		}
		return r.ref.Status
	}
	return session.None
}

// needyCount is how many sessions this deck runs are waiting on the user,
// counted from every ticket, project and Other session — folded ones too.
func (m Model) needyCount() int {
	n := 0
	for _, st := range m.sessions {
		if needy(st) {
			n++
		}
	}
	for _, r := range m.otherSessions {
		if r.Socket == "" && needy(r.Status) {
			n++
		}
	}
	return n
}

// jumpToNeedy moves the cursor to the next (dir 1) or previous (dir -1) row
// whose session is waiting on the user, wrapping around. Rows folded away are
// unfolded to reach them, since the point is to find what needs you.
func (m *Model) jumpToNeedy(dir int) {
	if !m.unfoldNeedy() && m.needyCount() == 0 {
		m.notice = "nothing is waiting on you"
		return
	}
	n := len(m.rows)
	for step, i := 1, m.cursor; step <= n; step++ {
		i = (i + dir + n) % n
		if m.cursorable(i) && needy(m.rowStatus(m.rows[i])) {
			m.cursor = i
			m.ensureVisible()
			return
		}
	}
	m.notice = "nothing is waiting on you"
}

// unfoldNeedy opens the folds hiding a session that waits on the user, and
// reports whether it opened any.
func (m *Model) unfoldNeedy() bool {
	opened := false
	for _, p := range m.projects {
		if m.projExpanded[p.Key()] {
			continue
		}
		for _, is := range p.Issues {
			if needy(m.sessions[is.Identifier]) {
				m.projExpanded[p.Key()] = true
				opened = true
				break
			}
		}
	}
	for _, g := range linear.GroupByPriorityThenStatus(linear.IssuesOutsideProjects(m.visibleIssues(), m.projects)) {
		if !m.collapsed[g.PrioLabel] {
			continue
		}
		for _, sb := range g.Statuses {
			for _, is := range sb.Issues {
				if needy(m.sessions[is.Identifier]) {
					m.collapsed[g.PrioLabel], opened = false, true
				}
			}
		}
	}
	if m.projFolded {
		for _, p := range m.projects {
			if needy(m.sessions[p.Key()]) {
				m.projFolded, opened = false, true
				break
			}
		}
	}
	if opened {
		cur := m.cursor
		m.regroup()
		m.cursor = m.nearestCursorable(cur)
	}
	return opened
}
