package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// promised records the promise a cut-off on `from` would have left, as the
// deck that made it. mut adjusts it before it is written.
func promised(t *testing.T, m Model, key, from string, resets time.Time, mut func(*ledger.Carry)) Model {
	t.Helper()
	c := &ledger.Carry{
		Err: "err-" + key, At: time.Now().Add(-time.Hour), Kind: "rate_limit", From: from,
		ResetsAt: resets, LimitType: "five_hour", Socket: m.socket,
	}
	if mut != nil {
		mut(c)
	}
	if err := ledger.Update(session.DeterministicID(key), func(e *ledger.Entry) {
		e.Key, e.Kind, e.Carry = key, ledger.Ticket, c
	}); err != nil {
		t.Fatal(err)
	}
	m.ledger = ledger.Load()
	return m
}

// stoppedOn is a session with no process — its tab closed, or herdr restarted
// — whose newest transcript is on acct and ends on its cut-off.
func stoppedOn(m Model, key, acct string) fleet.Session {
	a, _ := account.Find(m.accounts, acct)
	s := fleet.Session{Key: key, ID: session.DeterministicID(key), Kind: ledger.Ticket, Account: a.Name, ConfigDir: a.ConfigDir, Status: session.Stopped}
	s.Activity.Tail.LastUUID = "err-" + key
	return s
}

// idleOn is a running session whose turn is over, its transcript still
// ending on the cut-off — after a move whose prompt never landed, say.
func idleOn(m Model, key, acct string) fleet.Session {
	a, _ := account.Find(m.accounts, acct)
	s := fleet.Session{
		Key: key, ID: session.DeterministicID(key), Kind: ledger.Ticket, Account: a.Name, ConfigDir: a.ConfigDir,
		Status: session.Idle, Live: true, Home: true, PaneID: "w1:p5", PID: 77,
	}
	s.Activity.Tail.LastUUID = "err-" + key
	return s
}

// busyOn is a running session on acct that herdr and Claude call working,
// because agents it started still run, while its lead stopped on its cut-off.
// asked: the lead has sent the API a request since.
func busyOn(m Model, key, acct string, resets time.Time, asked bool) fleet.Session {
	s := limitedOn(m, key, acct, resets)
	s.Status = session.Working
	s.Activity.InFlight.Agents = 3
	cut := time.Now().Add(-2 * time.Minute)
	s.Activity.Tail.LastUUID, s.Activity.Tail.LastAt, s.Activity.Tail.AskedAt = "err-"+key, cut, cut.Add(-time.Second)
	if asked {
		s.Activity.Tail.AskedAt = cut.Add(time.Minute)
	}
	return s
}

// allOut leaves no account with room until an hour from now.
func allOut(m Model) Model {
	for _, a := range []string{"matt", "m0", "support"} {
		m.quotas[a] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 100, FiveHourReset: time.Now().Add(time.Hour)}, FetchedAt: time.Now()}
	}
	return m
}

// The wait-and-resume case across a deck restart: a session parked with
// nowhere to go is still carried on when its limit lifts, however long ago
// that was, because the promise is on disk, not in the deck that made it.
func TestAPromiseOutlivesTheDeckThatMadeIt(t *testing.T) {
	m, rec := threeAccounts(t)
	m = allOut(m)
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))
	m, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 0 || m.ledger[s.ID].Carry == nil {
		t.Fatalf("moves=%v promise=%+v, want it parked under a promise", rec.moves, m.ledger[s.ID].Carry)
	}

	// The deck restarts and the limit lifted three hours ago — long past the
	// window in which a cut-off the deck never promised is revived.
	fresh := New(fakeFetcher{fixture()}, "/repos", false, rec)
	next, _ := fresh.Update(refreshedMsg{issues: fixture()})
	fresh = lapse(t, next.(Model), s.ID, -3*time.Hour)
	fresh.quotas = m.quotas
	s.Status = session.Idle
	s.Activity.Tail.Err.ResetsAt = time.Now().Add(-3 * time.Hour)
	_, cmd = poll(t, fresh, s)
	drainCmd(cmd)
	if rec.sent != "ZEN-9:"+continueAfterReset {
		t.Errorf("sent %q, want the restarted deck to keep the promise", rec.sent)
	}
}

// A session whose tab closed while it waited — herdr restarted, or the box —
// is started again where it lives once its limit lifts, and told to carry on.
func TestAStoppedSessionStartsAgainWhenItsLimitLifts(t *testing.T) {
	m, rec := threeAccounts(t)
	m = allOut(m)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(-5*time.Minute), nil)
	m, cmd := poll(t, m, stoppedOn(m, "ZEN-9", "m0"))
	m = finish(t, m, cmd)
	if len(rec.moves) != 1 {
		t.Fatalf("moves = %d, want it started again", len(rec.moves))
	}
	mv := rec.moves[0]
	if mv.PaneID != "" || mv.From.Name != "m0" || mv.To.Name != "m0" || mv.Submit != continueAfterReset || mv.Focus {
		t.Errorf("move = %+v, want a start on m0 told the limit reset", mv)
	}
	if c := ledger.Load()[session.DeterministicID("ZEN-9")].Carry; c == nil || c.Told.IsZero() {
		t.Errorf("promise = %+v, want it open and told", c)
	}
	if !strings.Contains(m.notice, "started again on ⦿m0") {
		t.Errorf("notice = %q", m.notice)
	}
}

// Stopped while its limit still holds, it goes where there is room.
func TestAStoppedSessionMovesWhenAnotherAccountHasRoom(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(time.Hour), func(c *ledger.Carry) { c.Busy = true })
	_, cmd := poll(t, m, stoppedOn(m, "ZEN-9", "m0"))
	drainCmd(cmd)
	if len(rec.moves) != 1 || rec.moves[0].To.Name != "support" {
		t.Fatalf("moves = %+v, want one to support", rec.moves)
	}
	if want := continueAfterMove + lostBackgroundWork; rec.moves[0].Submit != want {
		t.Errorf("told %q, want %q", rec.moves[0].Submit, want)
	}
}

// With nowhere to go a stopped session waits; nothing is started early.
func TestAStoppedSessionWaitsWhileNoAccountHasRoom(t *testing.T) {
	m, rec := threeAccounts(t)
	m = allOut(m)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(time.Hour), nil)
	s := stoppedOn(m, "ZEN-9", "m0")
	m, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Fatalf("started with nowhere to go: %+v", rec.moves)
	}
	if note := m.carryNote(s, time.Now()); !strings.Contains(note, "starts again and carries on at") {
		t.Errorf("note = %q", note)
	}
}

// The 2026-10-22 failure: a moved session whose prompt never landed sat idle
// for hours. Its transcript still ends on the cut-off, so it is told again.
func TestAMovedSessionWhosePromptNeverLandedIsToldAgain(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(time.Hour), func(c *ledger.Carry) {
		c.Told, c.Tells = time.Now().Add(-tellWait-time.Second), 1
	})
	_, cmd := poll(t, m, idleOn(m, "ZEN-9", "support"))
	drainCmd(cmd)
	if len(rec.moves) != 0 || rec.sent != "ZEN-9:"+continueAfterMove {
		t.Errorf("moves=%d sent=%q, want it told again where it is", len(rec.moves), rec.sent)
	}
	if c := ledger.Load()[session.DeterministicID("ZEN-9")].Carry; c == nil || c.Tells != 2 {
		t.Errorf("promise = %+v, want the second tell counted", c)
	}
}

// A session told a moment ago gets time to answer before anything else.
func TestAJustToldSessionIsLeftToAnswer(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(time.Hour), func(c *ledger.Carry) {
		c.Told, c.Tells = time.Now().Add(-30*time.Second), 1
	})
	for _, s := range []fleet.Session{idleOn(m, "ZEN-9", "support"), limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))} {
		_, cmd := poll(t, m, s)
		drainCmd(cmd)
	}
	if rec.sent != "" || len(rec.moves) != 0 {
		t.Errorf("sent=%q moves=%d, want it left alone", rec.sent, len(rec.moves))
	}
}

// A reply after the cut-off is the only thing that keeps a promise.
func TestAnAnswerKeepsThePromise(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(-5*time.Minute), nil)
	s := idleOn(m, "ZEN-9", "m0")
	s.Activity.Tail.LastUUID = "reply-1"
	m, cmd := poll(t, m, s)
	drainCmd(cmd)
	e := m.ledger[s.ID]
	if e.Carry != nil || e.Handled != "err-ZEN-9" {
		t.Errorf("entry = %+v, want the promise closed and the cut-off handled", e)
	}
	if rec.sent != "" {
		t.Errorf("told an answered session to carry on: %q", rec.sent)
	}
}

// Tells that go unanswered stop, and the user hears about it once.
func TestUnansweredTellsStopAndSaySo(t *testing.T) {
	m, rec := threeAccounts(t)
	var notes []string
	notify = func(title, _ string) { notes = append(notes, title) }
	t.Cleanup(func() { notify = func(string, string) {} })
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(-time.Hour), func(c *ledger.Carry) {
		c.Told, c.Tells = time.Now().Add(-time.Hour), maxTells
	})
	s := idleOn(m, "ZEN-9", "m0")
	for range 2 {
		var cmd tea.Cmd
		m, cmd = poll(t, m, s)
		drainCmd(cmd)
	}
	if rec.sent != "" {
		t.Errorf("kept telling: %q", rec.sent)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "isn't carrying on") {
		t.Errorf("notifications = %q, want one saying it isn't carrying on", notes)
	}
	if note := m.carryNote(s, time.Now()); !strings.Contains(note, "open its tab") {
		t.Errorf("note = %q", note)
	}
	// Nor is one that keeps stopping started again and again.
	_, cmd := poll(t, m, stoppedOn(m, "ZEN-9", "m0"))
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Errorf("restarted a session that never answered: %+v", rec.moves)
	}
}

// The 2026-10-28 failure: sessions whose lead a limit stopped were left for
// minutes with another account free, because the agents they started kept
// them reading "working" until each ran into the same limit. A lead that has
// asked nothing since its cut-off moves as soon as it is cut off.
func TestACutOffLeadMovesWhileItsAgentsWindDown(t *testing.T) {
	m, rec := threeAccounts(t)
	_, cmd := poll(t, m, busyOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour), false))
	drainCmd(cmd)
	if len(rec.moves) != 1 || rec.moves[0].To.Name != "support" {
		t.Fatalf("moves = %+v, want one to support", rec.moves)
	}
	if want := continueAfterMove + lostBackgroundWork; rec.moves[0].Submit != want {
		t.Errorf("told %q, want %q: the agents it had running don't carry over", rec.moves[0].Submit, want)
	}
}

// A lead that has asked again since its cut-off is mid-turn — the limit may
// have been raised — and is left to it.
func TestALeadThatAskedAgainIsLeftToIt(t *testing.T) {
	m, rec := threeAccounts(t)
	s := busyOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour), true)
	m, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 0 || m.ledger[s.ID].Carry != nil {
		t.Errorf("moves=%+v promise=%+v, want it left alone", rec.moves, m.ledger[s.ID].Carry)
	}
}

// Once its limit lifts, a session whose lead stopped on it is told to carry on
// where it is, even while background work keeps it from reading idle: a
// monitor may run on for good. One whose own continue already went out is
// left to answer it.
func TestABusySessionCarriesOnInPlaceOnceItsLimitLifts(t *testing.T) {
	reset := time.Now().Add(-5 * time.Minute)
	for _, c := range []struct {
		name    string
		session func(Model) fleet.Session
		want    string
	}{
		{"agents running", func(m Model) fleet.Session { return busyOn(m, "ZEN-9", "m0", reset, false) }, "ZEN-9:" + continueAfterReset},
		{"a monitor running", func(m Model) fleet.Session {
			s := idleOn(m, "ZEN-9", "m0")
			s.Status = session.Background
			s.Activity.Tail.Err = &session.APIError{Kind: "rate_limit", ResetsAt: reset, UUID: "err-ZEN-9"}
			return s
		}, "ZEN-9:" + continueAfterReset},
		{"its own continue went out", func(m Model) fleet.Session { return busyOn(m, "ZEN-9", "m0", reset, true) }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, rec := threeAccounts(t)
			m = promised(t, allOut(m), "ZEN-9", "m0", reset, nil)
			_, cmd := poll(t, m, c.session(m))
			drainCmd(cmd)
			if len(rec.moves) != 0 || rec.sent != c.want {
				t.Errorf("moves=%d sent=%q, want %q", len(rec.moves), rec.sent, c.want)
			}
		})
	}
}

// In the minute after a limit lifts, Claude Code's own continue gets its go,
// and agents the session started can work again: it isn't moved then, even
// with another account free.
func TestALiftedLimitIsNotMovedAway(t *testing.T) {
	m, rec := threeAccounts(t)
	reset := time.Now().Add(-10 * time.Second)
	m = promised(t, m, "ZEN-9", "m0", reset, nil)
	_, cmd := poll(t, m, busyOn(m, "ZEN-9", "m0", reset, false))
	drainCmd(cmd)
	if len(rec.moves) != 0 || rec.sent != "" {
		t.Errorf("moves=%+v sent=%q, want it left to carry on where it is", rec.moves, rec.sent)
	}
}

// A cut-off leaves its account's refusal in the ledger, and it outlives the
// promise: six sessions went to an account out for the week once the one
// that found out had moved on (2026-10-28).
func TestACutOffRecordsItsAccountAsOut(t *testing.T) {
	m, _ := threeAccounts(t)
	week := time.Now().Add(40 * time.Hour)
	s := limitedOn(m, "ZEN-9", "m0", week)
	s.Activity.Tail.Err.LimitType = "seven_day"
	m, cmd := poll(t, m, s)
	m = finish(t, m, cmd)
	m.endPromise(s.ID)
	r := ledger.Load()[s.ID].Refused
	if len(r) != 1 || r[0].Account != "m0" || !r[0].Until.Equal(week) || r[0].Limit != "seven_day" {
		t.Errorf("refused = %+v, want m0 out until its weekly reset", r)
	}
}

// A session moved onto an account that turns out to be out opens on Claude's
// limit menu, and sat there until someone came (2026-10-28). The deck clears
// that menu, and no other dialog, and tells it to carry on: the cut-off its
// new account answers with moves it on.
func TestAMovedSessionOnTheLimitMenuIsToldToCarryOn(t *testing.T) {
	for _, menu := range []bool{true, false} {
		m, rec := threeAccounts(t)
		rec.limitMenu = menu
		m = promised(t, m, "ZEN-9", "m0", time.Now().Add(time.Hour), nil)
		s := idleOn(m, "ZEN-9", "support")
		s.Status, s.Prompting = session.NeedsInput, true
		m, cmd := poll(t, m, s)
		m = finish(t, m, cmd)
		if menu {
			if len(rec.keys) != 1 || rec.keys[0] != "ZEN-9:esc" || rec.sent != "ZEN-9:"+continueAfterMove {
				t.Errorf("on the limit menu: keys=%v sent=%q, want it dismissed and told", rec.keys, rec.sent)
			}
			continue
		}
		if len(rec.keys) != 0 || rec.sent != "" {
			t.Errorf("on another dialog: keys=%v sent=%q, want it left for someone", rec.keys, rec.sent)
		}
		if !m.retryAt["ZEN-9"].After(time.Now()) {
			t.Error("a dialog left for someone is read again later, not every poll")
		}
	}
}

// A weekly limit resets more than a day out, so Claude offers its own wait on
// the menu instead of arming it, and the menu sat there (2026-10-29). With
// nowhere to move, the deck chooses it, once per cut-off. With somewhere to
// move, it moves.
func TestAParkedSessionWaitsAtItsLimitMenu(t *testing.T) {
	onMenu := func(m Model) fleet.Session {
		s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(40*time.Hour))
		s.Status, s.Prompting = session.NeedsInput, true
		return s
	}

	m, rec := threeAccounts(t)
	m, rec.waitOffered = allOut(m), true
	m, cmd := poll(t, m, onMenu(m))
	m = finishWaits(t, m, cmd)
	if len(rec.waited) != 1 || rec.waited[0] != "ZEN-9" || len(rec.moves) != 0 || rec.sent != "" {
		t.Fatalf("waited=%v moves=%v sent=%q, want only the wait chosen", rec.waited, rec.moves, rec.sent)
	}
	_, cmd = poll(t, m, onMenu(m))
	finishWaits(t, m, cmd)
	if len(rec.waited) != 1 {
		t.Errorf("waited=%v, want the menu answered once per cut-off", rec.waited)
	}

	m, rec = threeAccounts(t)
	rec.waitOffered = true
	_, cmd = poll(t, m, onMenu(m))
	drainCmd(cmd)
	if len(rec.waited) != 0 || len(rec.moves) != 1 {
		t.Errorf("waited=%v moves=%v, want a move when an account has room", rec.waited, rec.moves)
	}
}

// finishWaits feeds a poll's step results back to the deck, as finish does.
func finishWaits(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for _, msg := range drainCmd(cmd) {
		if _, ok := msg.(waitedMsg); ok {
			next, _ := m.Update(msg)
			m = next.(Model)
		}
	}
	return m
}

// A ticket in Validate is still being worked. Linear files the state under
// "completed", and read as done, every cut-off ZEN-6940 had was dropped
// without a word (2026-10-28).
func TestAPromiseStandsForATicketInValidate(t *testing.T) {
	m, rec := threeAccounts(t)
	issues := append(fixture(), linear.Issue{Identifier: "ZEN-8", Title: "being checked", Priority: 2, StateName: "Validate", StateType: "completed"})
	next, _ := m.Update(refreshedMsg{issues: issues})
	m = promised(t, next.(Model), "ZEN-8", "m0", time.Now().Add(-5*time.Minute), nil)
	_, cmd := poll(t, m, idleOn(m, "ZEN-8", "m0"))
	drainCmd(cmd)
	if rec.sent != "ZEN-8:"+continueAfterReset {
		t.Errorf("sent %q, want the Validate ticket carried on", rec.sent)
	}
}

// A ticket that is done no longer wants carrying on.
func TestAPromiseIsCalledOffWhenItsTicketIsDone(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-7", "m0", time.Now().Add(-5*time.Minute), nil)
	m, cmd := poll(t, m, stoppedOn(m, "ZEN-7", "m0"))
	drainCmd(cmd)
	if len(rec.moves) != 0 || m.ledger[session.DeterministicID("ZEN-7")].Carry != nil {
		t.Errorf("moves=%d promise=%+v, want it called off", len(rec.moves), m.ledger[session.DeterministicID("ZEN-7")].Carry)
	}
}

// A poll that began before the deck's last step on a session finished shows
// the session as it was. It is not acted on.
func TestAStalePollIsNotActedOn(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(time.Hour), nil)
	m.stepDone["ZEN-9"] = time.Now().Add(time.Minute)
	_, cmd := poll(t, m, limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour)))
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Errorf("acted on a poll older than the last move: %+v", rec.moves)
	}
}

// Only the deck that made a promise keeps it: two starting one stopped
// session would fork it.
func TestAnotherDecksPromiseIsLeftToIt(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(-5*time.Minute), func(c *ledger.Carry) { c.Socket = "/elsewhere/herdr.sock" })
	_, cmd := poll(t, m, stoppedOn(m, "ZEN-9", "m0"))
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Errorf("kept another deck's promise: %+v", rec.moves)
	}
}

// When this deck's own workspace didn't answer the poll, a session that looks
// stopped may be running there. Nothing is started.
func TestNothingStartsWhileTheWorkspaceIsUnreadable(t *testing.T) {
	m, rec := threeAccounts(t)
	m = promised(t, m, "ZEN-9", "m0", time.Now().Add(-5*time.Minute), nil)
	snap := fleet.Snapshot{Sessions: []fleet.Session{stoppedOn(m, "ZEN-9", "m0")}, At: time.Now(), OwnErr: errors.New("herdr agent list: exit status 1")}
	_, cmd := m.Update(statusesMsg{snap: &snap, err: snap.OwnErr})
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Errorf("started a session it couldn't see: %+v", rec.moves)
	}
}

// A move that fails is tried again later from wherever it got to, not
// dropped: here it stopped the session and then couldn't start it.
func TestAFailedMoveIsTakenUpAgain(t *testing.T) {
	m, rec := threeAccounts(t)
	rec.moveErr = errors.New("tab create: exit status 1")
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))
	m, cmd := poll(t, m, s)
	m = finish(t, m, cmd)
	if m.ledger[s.ID].Carry == nil || !m.retryAt["ZEN-9"].After(time.Now()) {
		t.Fatalf("promise=%+v retryAt=%v, want it open and a retry booked", m.ledger[s.ID].Carry, m.retryAt["ZEN-9"])
	}
	rec.moveErr = nil
	m.retryAt["ZEN-9"] = time.Now().Add(-time.Second)
	_, cmd = poll(t, m, stoppedOn(m, "ZEN-9", "m0"))
	drainCmd(cmd)
	if len(rec.moves) != 2 || rec.moves[1].To.Name != "support" || rec.moves[1].PaneID != "" {
		t.Errorf("moves = %+v, want the stopped session started on support", rec.moves)
	}
}

// A move that landed but whose prompt didn't start a turn is still a move:
// the ledger follows the session, and the next poll tells it again there.
func TestAMoveWhosePromptDidNotTakeIsToldAgain(t *testing.T) {
	m, rec := threeAccounts(t)
	rec.moveErr = fmt.Errorf("zen-9: %w", herd.ErrNoTurn)
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))
	m, cmd := poll(t, m, s)
	m = finish(t, m, cmd)
	e := m.ledger[s.ID]
	if e.Account != "support" || e.Carry == nil || !e.Carry.Told.IsZero() || m.err != nil {
		t.Fatalf("entry = %+v (carry %+v), err = %v: want it on support, promised and untold", e, e.Carry, m.err)
	}
	rec.moveErr = nil
	_, cmd = poll(t, m, idleOn(m, "ZEN-9", "support"))
	drainCmd(cmd)
	if rec.sent != "ZEN-9:"+continueAfterMove || len(rec.moves) != 1 {
		t.Errorf("sent=%q moves=%d, want it told on support, not moved again", rec.sent, len(rec.moves))
	}
}

// The prompt names what happened: a move, a lapsed login, or a reset; and
// background work that a new process lost.
func TestCarryPrompt(t *testing.T) {
	limit := ledger.Carry{Kind: "rate_limit", Busy: true}
	login := ledger.Carry{Kind: authFailed}
	for _, c := range []struct {
		c              ledger.Carry
		moved, restart bool
		want           string
	}{
		{limit, true, true, continueAfterMove + lostBackgroundWork},
		{limit, false, false, continueAfterReset},
		{limit, false, true, continueAfterReset + lostBackgroundWork},
		{login, true, true, continueAfterLogin},
	} {
		if got := carryPrompt(c.c, c.moved, c.restart); got != c.want {
			t.Errorf("carryPrompt(%+v, moved=%v, restarted=%v) = %q, want %q", c.c, c.moved, c.restart, got, c.want)
		}
	}
}
