package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hdtradeservices/ticketdeck/internal/doctor"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/route"
)

// This file is the deck keeping an eye on itself and on what it spends: herdr's
// event stream, the self-check, and the forecasts and spend figures the header
// and the accounts overlay show.

// herdrChangedMsg is herdr saying something in this deck's workspace changed.
type herdrChangedMsg struct{}

// herdrUpMsg says whether the event subscription is live.
type herdrUpMsg struct{ up bool }

// doctorMsg carries a finished self-check.
type doctorMsg struct{ report doctor.Report }

// queuedPollMsg is an event-driven poll that waited out minPollGap.
type queuedPollMsg struct{}

// minPollGap is the least time between two polls started by events. A busy
// workspace can report changes faster than a poll is worth running, and a
// poll that follows the last one within a second learns little more.
const minPollGap = time.Second

// pollOnEvent polls for a change herdr reported: at once, after the poll in
// flight, or once minPollGap has passed since the last one started.
func (m Model) pollOnEvent() (Model, tea.Cmd) {
	since := time.Since(m.pollStarted)
	switch {
	case m.pollBusy && since <= 30*time.Second:
		m.pollAgain = true
		return m, nil
	case since < minPollGap:
		if m.pollQueued {
			return m, nil
		}
		m.pollQueued = true
		return m, tea.Tick(minPollGap-since, func(time.Time) tea.Msg { return queuedPollMsg{} })
	}
	m.pollBusy, m.pollStarted = true, time.Now()
	return m, m.refreshStatuses()
}

// doctorEvery is how often the self-check runs again: often enough to catch a
// Claude Code upgrade in a deck that runs for days, rarely enough that its
// `claude agents` calls don't add up.
const doctorEvery = 10 * time.Minute

// watchHerdr starts following this deck's herdr server and returns the command
// that waits for its first message.
func (m Model) watchHerdr() tea.Cmd {
	if m.events == nil {
		return nil
	}
	ch := m.events
	herd.SetWatchLog(func(s string) { debugLog.Print(s) })
	go herd.Watch(context.Background(), herd.OwnSocket(),
		func() {
			select {
			case ch <- herdrChangedMsg{}:
			default: // a poll is already queued; it will see this change too
			}
		},
		func(up bool) { ch <- herdrUpMsg{up: up} },
	)
	return m.waitHerdr()
}

// waitHerdr waits for the watcher's next message. Each one re-arms it.
func (m Model) waitHerdr() tea.Cmd {
	if m.events == nil {
		return nil
	}
	ch := m.events
	return func() tea.Msg { return <-ch }
}

// runDoctor runs the self-check in the background.
func (m Model) runDoctor() tea.Cmd {
	in := doctor.Input{
		Accounts: m.accounts, Home: m.acct,
		Herdr:  m.backend != nil && m.backend.Herdr(),
		Quotas: m.quotas, Now: time.Now(),
	}
	return func() tea.Msg { return doctorMsg{report: doctor.Run(in)} }
}

// hasDoctorLine reports whether the header carries a failed self-check, so
// viewportHeight can reserve a line for it.
func (m Model) hasDoctorLine() bool { return len(m.doctor.Failures()) > 0 }

// doctorLine names the first failed self-check under the header: the deck's
// statuses may be wrong while it stands, and this says which assumption broke.
func (m Model) doctorLine() string {
	f := m.doctor.Failures()
	if len(f) == 0 {
		return ""
	}
	line := errStyle.Render("  ⚠ " + f[0].Name + ": " + f[0].Detail)
	if len(f) > 1 {
		line += errStyle.Render(fmt.Sprintf(" (+%d more)", len(f)-1))
	}
	line += dimStyle.Render(" · ticketdeck doctor")
	if m.width > 0 && lipgloss.Width(line) > m.width {
		line = truncANSI(line, m.width)
	}
	return line + "\n"
}

// forecast is when an account's week runs out at the pace it has been spent,
// "" when it lasts until it renews. A spent week isn't forecast: the account
// already says it is out.
func forecast(e quota.Entry, now time.Time) string {
	u := e.Current(now)
	if u == nil || u.SevenDayPct >= 100 {
		return ""
	}
	t, ok := e.WeekRunsOut(now)
	if !ok {
		return ""
	}
	if !t.After(now) {
		return "now"
	}
	return route.Clock(t, now)
}

// spender is one session's spend in the last five hours.
type spender struct {
	s   fleet.Session
	usd float64
}

// spenders is every live session that spent anything in the burn window,
// biggest first.
func (m Model) spenders() []spender {
	var out []spender
	for _, s := range m.fleet.Sessions {
		if s.Live && s.Activity.Burn > 0 {
			out = append(out, spender{s, s.Activity.Burn})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].usd > out[j].usd })
	return out
}

// spentBy is each account's live sessions' spend in the burn window.
func (m Model) spentBy() map[string]float64 {
	out := map[string]float64{}
	for _, sp := range m.spenders() {
		out[sp.s.Account] += sp.usd
	}
	return out
}

// dollars renders an API-rate figure compactly.
func dollars(usd float64) string {
	switch {
	case usd >= 100:
		return fmt.Sprintf("$%.0f", usd)
	case usd >= 0.995:
		return fmt.Sprintf("$%.1f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// Context sizes past which a session is flagged: every turn re-reads the whole
// context, so a session this large spends its account fast even when its
// turns are short.
const (
	bigContext  = 300_000
	hugeContext = 600_000
)

// contextColor is the color a session's context size shows in, and whether it
// is large enough to show at all.
func contextColor(tokens int) (lipgloss.Color, bool) {
	switch {
	case tokens >= hugeContext:
		return lipgloss.Color("203"), true
	case tokens >= bigContext:
		return lipgloss.Color("214"), true
	}
	return "", false
}

func kTokens(n int) string { return fmt.Sprintf("%dk", (n+500)/1000) }

// setupDrift is what changes for a session that moves from one account to
// another, from the last self-check's reading of their settings.
func (m Model) setupDrift(from, to string) []string {
	p, okp := m.doctor.Profiles[from]
	q, okq := m.doctor.Profiles[to]
	if !okp || !okq {
		return nil
	}
	return p.Drift(q)
}

func joinDrift(d []string) string { return strings.Join(d, " · ") }

// contextTag flags a row whose live session carries a context large enough to
// make every one of its turns expensive.
func (m Model) contextTag(key string) (string, lipgloss.Color) {
	s, ok := m.fleet.Best(key)
	if !ok || !s.Live {
		return "", ""
	}
	n := s.Activity.Tail.ContextTokens
	c, big := contextColor(n)
	if !big {
		return "", ""
	}
	return "◔" + kTokens(n), c
}
