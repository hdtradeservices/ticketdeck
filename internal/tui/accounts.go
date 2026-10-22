package tui

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/route"
)

// This file is the deck's view of its accounts: what the router sees, the
// header line that shows every account's headroom, and the `A` overlay.

// candidates is every account as the router sees it: its usage reading, what
// its running sessions say about it, and how many it is running.
func (m Model) candidates() []route.Candidate {
	now := time.Now()
	until, authFailed := m.fleet.Limits(now)
	live := m.fleet.LiveOn()
	out := make([]route.Candidate, 0, len(m.accounts))
	for _, a := range m.accounts {
		e := m.quotas[a.Name]
		out = append(out, route.Candidate{
			Name: a.Name, Usage: e.Current(now), Stale: e.Stale(now), Reason: e.Reason,
			LimitedUntil: until[a.Name], AuthFailed: authFailed[a.Name], Live: live[a.Name],
			Home: a.ConfigDir == m.acct.ConfigDir,
		})
	}
	return out
}

// routeFor asks the router where a session should run. sticky is the account
// already holding it ("" for a new one); exclude is one it must leave.
func (m Model) routeFor(sticky, exclude string) route.Decision {
	return route.Pick(m.candidates(), route.Request{Sticky: sticky, Exclude: exclude, Pin: m.settings.Pin, Now: time.Now()})
}

// launchAccount is where a session the user asked to open runs: the router's
// pick, else — when no account can take work — the account it already lives
// on, else this deck's own. Opening is the user's call, so it never refuses;
// it says why when the account it lands on is out.
func (m Model) launchAccount(sticky string) (account.Account, string) {
	d := m.routeFor(sticky, "")
	if a, ok := account.Find(m.accounts, d.Account); ok {
		return a, d.Why
	}
	if a, ok := account.Find(m.accounts, sticky); ok {
		return a, d.Why + " — opening on ⦿" + a.Name + " anyway"
	}
	return m.acct, d.Why + " — opening on ⦿" + m.acct.Name + " anyway"
}

// verdicts is the router's verdict on each account right now.
func (m Model) verdicts() map[string]route.Verdict {
	out := map[string]route.Verdict{}
	for _, r := range m.acctRows() {
		out[r.Name] = r.Verdict
	}
	return out
}

// authFailed reports an account whose running sessions say its login lapsed.
func (m Model) authFailed(name string) bool {
	_, failed := m.fleet.Limits(time.Now())
	return failed[name]
}

// loginCmd is how to log an account back in, with the home directory as ~.
func loginCmd(a account.Account) string {
	return "CLAUDE_CONFIG_DIR=" + tildePath(a.ConfigDir) + " claude /login"
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(p, home); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			return "~" + rest
		}
	}
	return p
}

// accountByName finds one of this machine's accounts.
func (m Model) accountByName(name string) (account.Account, bool) {
	return account.Find(m.accounts, name)
}

// multiAccount reports whether there is more than one account to run under,
// which is when every account-related column, line and key appears.
func (m Model) multiAccount() bool { return len(m.accounts) > 1 }

// hasAccountsLine reports whether the header carries the accounts row, so
// viewportHeight can reserve a line for it. Kept beside accountsLine: if the
// two ever disagree the body is sized wrong and the footer scrolls off.
func (m Model) hasAccountsLine() bool { return m.multiAccount() }

// accountsLine renders every account's headroom on one line under the title:
// both usage windows, how many sessions it runs, and why when it can't take
// work. The account a new session would open under is underlined, so where ⏎
// sends work is visible before pressing it.
func (m Model) accountsLine() string {
	if !m.hasAccountsLine() {
		return ""
	}
	now := time.Now()
	next := m.routeFor("", "").Account
	live := m.fleet.LiveOn()
	until, authFailed := m.fleet.Limits(now)
	chip := func(a account.Account, compact bool) string {
		name := "⦿ " + a.Name
		if compact {
			name = "⦿" + a.Name
		}
		st := m.acctStyle(a.Name)
		if a.Name == next {
			st = st.Bold(true).Underline(true)
		}
		e := m.quotas[a.Name]
		u := e.Current(now)
		var body string
		switch {
		case !until[a.Name].IsZero() && now.Before(until[a.Name]):
			body = errStyle.Render("out until " + route.Clock(until[a.Name], now))
		case authFailed[a.Name]:
			body = errStyle.Render("login expired")
		case e.SignedOut():
			body = errStyle.Render("not signed in")
		case u != nil && compact:
			body = lipgloss.NewStyle().Foreground(quotaColor(u.FiveHourPct)).Render(fmt.Sprintf("%.0f%%", u.FiveHourPct)) +
				dimStyle.Render("·") +
				lipgloss.NewStyle().Foreground(quotaColor(u.SevenDayPct)).Render(fmt.Sprintf("%.0f%%", u.SevenDayPct)) + staleMark(e)
		case u != nil:
			body = quotaPair(u, false) + staleMark(e)
		case e.Usage != nil:
			body = dimStyle.Render(cmp.Or(e.Reason, "no reading") + " · none this week")
		default:
			body = dimStyle.Render(cmp.Or(e.Reason, "usage unavailable"))
		}
		s := st.Render(name) + " " + body
		if n := live[a.Name]; n > 0 {
			s += dimStyle.Render(fmt.Sprintf(" ●%d", n))
		}
		return s
	}
	render := func(compact bool) string {
		parts := make([]string, 0, len(m.accounts))
		for _, a := range m.accounts {
			parts = append(parts, chip(a, compact))
		}
		gap := "   "
		if compact {
			gap = "  "
		}
		return strings.Join(parts, dimStyle.Render(gap))
	}
	line := render(false)
	if m.width > 0 && lipgloss.Width(line)+2 > m.width {
		line = render(true)
	}
	line = "  " + line
	if m.width > 0 && lipgloss.Width(line) > m.width {
		line = truncANSI(line, m.width)
	}
	return line + "\n"
}

// ── accounts overlay (A) ─────────────────────────────────────────────────────

func (m Model) openAccounts() (tea.Model, tea.Cmd) {
	m.acctMenu = true
	m.acctCursor = 0
	m.notice = ""
	return m, nil
}

// acctRows is the overlay's rows: every account in the order the router ranks
// them for a new session.
func (m Model) acctRows() []route.Ranked {
	return route.Rank(m.candidates(), time.Now())
}

func (m Model) updateAccounts(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.acctRows()
	switch msg.String() {
	case "esc", "q", "A":
		m.acctMenu = false
	case "up", "k":
		if m.acctCursor > 0 {
			m.acctCursor--
		}
	case "down", "j":
		if m.acctCursor < len(rows)-1 {
			m.acctCursor++
		}
	case "p", "enter":
		if m.acctCursor < len(rows) {
			name := rows[m.acctCursor].Name
			if m.settings.Pin == name {
				m.settings.Pin = ""
				m.notice = "new sessions go wherever there is most room"
			} else {
				m.settings.Pin = name
				m.notice = "new sessions open on ⦿" + name
			}
			saveSettings(m.settings)
		}
	case "a", "0":
		m.settings.Pin = ""
		saveSettings(m.settings)
		m.notice = "new sessions go wherever there is most room"
	case "m":
		on := !m.settings.autoMove()
		m.settings.AutoMove = &on
		saveSettings(m.settings)
		if on {
			m.notice = "auto-move on: a session that runs out moves and keeps going"
		} else {
			m.notice = "auto-move off: a session that runs out waits for you"
		}
	}
	return m, nil
}

func (m Model) renderAccounts() string {
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", titleStyle.Render("Accounts"))
	fmt.Fprintf(&b, "%s\n\n", dimStyle.Render("↑/↓ select · ⏎/p pin for new sessions · a back to auto · m auto-move on/off · esc close"))

	rows := m.acctRows()
	next := m.routeFor("", "").Account
	nameCol := 6
	for _, r := range rows {
		nameCol = max(nameCol, cols(r.Name)+2)
	}
	fmt.Fprintf(&b, "%s\n", dimStyle.Render(fmt.Sprintf("    %-*s  %-12s  %-12s  %-8s  %s", nameCol, "account", "5 hour", "7 day", "sessions", "")))
	for i, r := range rows {
		five, seven := "—", "—"
		if u := r.Usage; u != nil && !m.quotas[r.Name].SignedOut() {
			five = fmt.Sprintf("%3.0f%% %s", u.FiveHourPct, resetsIn(u.FiveHourReset))
			seven = fmt.Sprintf("%3.0f%% %s", u.SevenDayPct, resetsIn(u.SevenDayReset))
		}
		state := ""
		switch r.Verdict {
		case route.Tight:
			state = "nearly spent"
		case route.Unknown:
			state = r.Note + " — usable; opening a session refreshes it"
		case route.Out:
			state = r.Note
			if r.Note == "not signed in" || r.Note == "login expired" {
				if a, ok := m.accountByName(r.Name); ok {
					state += " — " + loginCmd(a)
				}
			}
		}
		var marks []string
		if r.Name == next {
			marks = append(marks, "next")
		}
		if r.Name == m.settings.Pin {
			marks = append(marks, "pinned")
		}
		if r.Home {
			marks = append(marks, "this deck's own")
		}
		mark := ""
		if len(marks) > 0 {
			mark = "  [" + strings.Join(marks, ", ") + "]"
		}
		sessions := fmt.Sprintf("●%d", r.Live)
		line := fmt.Sprintf("%-*s  %-12s  %-12s  %-8s  %s%s", nameCol, "⦿ "+r.Name, five, seven, sessions, state, mark)
		if i == m.acctCursor {
			fmt.Fprintf(&b, "%s\n", selStyle.Render("▶ "+line))
			continue
		}
		color := m.acctColor(r.Name)
		if r.Verdict == route.Out {
			fmt.Fprintf(&b, "  %s\n", dimStyle.Render(line))
			continue
		}
		fmt.Fprintf(&b, "  %s\n", lipgloss.NewStyle().Foreground(color).Render(line))
	}

	pin := "auto — each new session opens where there is most room"
	if m.settings.Pin != "" {
		pin = "pinned to ⦿" + m.settings.Pin
	}
	auto := "on — a session that runs out moves to the account with most room and keeps going"
	if !m.settings.autoMove() {
		auto = "off — a session that runs out waits for you"
	}
	fmt.Fprintf(&b, "\n  %s %s\n", dimStyle.Render("new sessions:"), pin)
	fmt.Fprintf(&b, "  %s %s\n", dimStyle.Render("auto-move:   "), auto)
	if soon := route.Soonest(m.candidates(), now); !soon.IsZero() {
		fmt.Fprintf(&b, "  %s %s\n", dimStyle.Render("next reset:  "), route.Clock(soon, now))
	}
	return b.String()
}
