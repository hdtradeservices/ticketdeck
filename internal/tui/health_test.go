package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/doctor"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// big is ZEN-9 live on matt with a large context, idle for ten minutes.
func big(t *testing.T, st session.Status) (Model, *recBackend) {
	t.Helper()
	m, rec := threeAccounts(t)
	m = liveOn(m, "ZEN-9", "matt", st)
	s := &m.fleet.Sessions[len(m.fleet.Sessions)-1]
	s.Activity.Tail.ContextTokens = 640_000
	s.LastActive = time.Now().Add(-10 * time.Minute)
	return m, rec
}

func TestCompactAnIdleSession(t *testing.T) {
	m, rec := big(t, session.Idle)
	if !strings.Contains(m.infoLine(), "context 640k — C compacts it") {
		t.Errorf("info line = %q", m.infoLine())
	}
	if !strings.Contains(m.View(), "◔640k") {
		t.Error("the row should flag its 640k context")
	}
	next, _ := m.Update(runes("C"))
	m = next.(Model)
	if m.prompt == nil || m.prompt.kind != promptCompact {
		t.Fatalf("C should open the compact prompt; notice %q", m.notice)
	}
	if line := m.promptLine(); !strings.Contains(line, "compact ZEN-9") || !strings.Contains(line, "from the warm cache") {
		t.Errorf("prompt = %q", line)
	}
	for _, r := range "keep the plan" {
		next, _ = m.Update(runes(string(r)))
		m = next.(Model)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainCmd(cmd)
	if rec.sent != "ZEN-9:/compact keep the plan" {
		t.Errorf("sent %q", rec.sent)
	}
}

func TestCompactWaitsForTheTurnAndForThisDeck(t *testing.T) {
	m, _ := big(t, session.Working)
	next, _ := m.Update(runes("C"))
	if m = next.(Model); m.prompt != nil || !strings.Contains(m.notice, "once its turn is over") {
		t.Errorf("a working session: prompt %v, notice %q", m.prompt, m.notice)
	}
	m, _ = big(t, session.Idle)
	s := &m.fleet.Sessions[len(m.fleet.Sessions)-1]
	s.Home, s.Workspace = false, "m0"
	next, _ = m.Update(runes("C"))
	if m = next.(Model); m.prompt != nil || !strings.Contains(m.notice, "M brings it here first") {
		t.Errorf("a session in another deck: prompt %v, notice %q", m.prompt, m.notice)
	}
}

// Compacting re-reads the context; after an hour idle the cache is gone, and
// the prompt says the read is at full price.
func TestCompactCostSaysWhenTheCacheIsCold(t *testing.T) {
	m, _ := big(t, session.Idle)
	s := m.fleet.Sessions[len(m.fleet.Sessions)-1]
	s.LastActive = time.Now().Add(-2 * time.Hour)
	if got := compactCost(s, time.Now()); !strings.Contains(got, "uncached") {
		t.Errorf("cost = %q", got)
	}
}

// With herdr's events live the timer slows down, an event polls at once, and
// an event during a poll is followed by one more poll rather than lost.
func TestHerdrEventsDriveThePoll(t *testing.T) {
	m, _ := threeAccounts(t)
	if m.pollEvery() != statusRefreshEvery {
		t.Fatalf("without events the poll is the fast one")
	}
	next, _ := m.Update(herdrUpMsg{up: true})
	m = next.(Model)
	if m.pollEvery() != eventPollEvery {
		t.Errorf("with events up the poll should slow to %v", eventPollEvery)
	}
	next, _ = m.Update(herdrChangedMsg{})
	m = next.(Model)
	if !m.pollBusy {
		t.Fatal("an event should start a poll")
	}
	next, _ = m.Update(herdrChangedMsg{})
	m = next.(Model)
	if !m.pollAgain {
		t.Fatal("an event during a poll should queue another")
	}
	// The first poll landed within a second of starting: the next one waits
	// out the gap instead of following straight on.
	next, _ = m.Update(statusesMsg{statuses: map[string]session.Status{}})
	m = next.(Model)
	if m.pollBusy || m.pollAgain || !m.pollQueued {
		t.Fatalf("busy %v again %v queued %v: the follow-up should be queued", m.pollBusy, m.pollAgain, m.pollQueued)
	}
	next, _ = m.Update(herdrChangedMsg{})
	m = next.(Model)
	if m.pollBusy {
		t.Error("an event inside the gap started a poll")
	}
	m.pollStarted = time.Now().Add(-2 * minPollGap)
	next, _ = m.Update(queuedPollMsg{})
	m = next.(Model)
	if !m.pollBusy || m.pollQueued {
		t.Errorf("the queued poll should start once the gap has passed: busy %v queued %v", m.pollBusy, m.pollQueued)
	}
	next, _ = m.Update(herdrUpMsg{up: false})
	if next.(Model).pollEvery() != statusRefreshEvery {
		t.Error("with events down the fast poll comes back")
	}
}

func TestAFailedSelfCheckShowsInTheHeader(t *testing.T) {
	m, _ := threeAccounts(t)
	m.width, m.height = 160, 40
	before := m.viewportHeight()
	next, _ := m.Update(doctorMsg{report: doctor.Report{At: time.Now(), Checks: []doctor.Check{
		{Name: "claude", Level: doctor.OK},
		{Name: "transcripts", Level: doctor.Fail, Detail: "Claude Code moved them"},
	}}})
	m = next.(Model)
	if !strings.Contains(m.View(), "⚠ transcripts: Claude Code moved them") {
		t.Error("the failed check should be named under the header")
	}
	if m.viewportHeight() != before-1 {
		t.Errorf("viewport %d, want %d: the line must be reserved", m.viewportHeight(), before-1)
	}
}

func TestTheHeaderForecastsTheWeek(t *testing.T) {
	m, _ := threeAccounts(t)
	now := time.Now()
	m.quotas["matt"] = quota.Entry{FetchedAt: now, Usage: &quota.Usage{SevenDayPct: 82, SevenDayReset: now.Add(113 * time.Hour)}}
	m.quotas["support"] = quota.Entry{FetchedAt: now, Usage: &quota.Usage{SevenDayPct: 100, SevenDayReset: now.Add(90 * time.Hour)}}
	if line := m.accountsLine(); !strings.Contains(line, "→ out") {
		t.Errorf("accounts line = %q, want matt's run-out time", line)
	}
	// A spent week is spent, not a week that lasts.
	next, _ := m.Update(runes("A"))
	view := next.(Model).View()
	if !strings.Contains(view, "spent") || strings.Count(view, "lasts the week") != 1 {
		t.Errorf("overlay should say support is spent and only m0 lasts:\n%s", view)
	}
}

// A session moved onto an account with a different default keeps its model.
func TestAMoveKeepsTheSessionsModel(t *testing.T) {
	m, rec := threeAccounts(t)
	s := limitedOn(m, "ZEN-1", "matt", time.Now().Add(time.Hour))
	s.Activity.Tail.Model = "claude-opus-5"
	_, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 1 {
		t.Fatalf("moves = %d", len(rec.moves))
	}
	if got := rec.moves[0].Ticket.Model; got != "claude-opus-5" {
		t.Errorf("moved with model %q, want claude-opus-5", got)
	}
}

func TestTheAccountsOverlayShowsSetupAndSpend(t *testing.T) {
	m, _ := threeAccounts(t)
	m = liveOn(m, "ZEN-9", "matt", session.Idle)
	m.fleet.Sessions[len(m.fleet.Sessions)-1].Activity.Burn = 4.2
	m.doctor.Profiles = map[string]account.Profile{
		"matt":    {Read: true, MCP: []string{"Sentry", "linear"}},
		"support": {Read: true, MCP: []string{"linear"}},
		"m0":      {Read: true, MCP: []string{"Sentry", "linear"}},
	}
	next, _ := m.Update(runes("A"))
	view := next.(Model).View()
	for _, want := range []string{"no MCP Sentry", "Spending most", "$4.2", "at this pace"} {
		if !strings.Contains(view, want) {
			t.Errorf("accounts overlay lacks %q", want)
		}
	}
	// And the move overlay warns about the destination's setup.
	next, _ = m.Update(runes("H"))
	m = next.(Model)
	for i, a := range m.handoffCands {
		if a.Name == "support" {
			m.handoffCursor = i
		}
	}
	if !strings.Contains(m.View(), "⦿support is set up differently: no MCP Sentry") {
		t.Errorf("move overlay:\n%s", m.View())
	}
}
