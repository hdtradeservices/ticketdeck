package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/route"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// This file keeps the deck's promise to a session its account cut off, by a
// usage limit or a login that lapsed: the session gets going again with nobody
// touching it. It moves to another account when one has room, and carries on
// where it is once the limit lifts when none does.
//
// The promise goes into the ledger the moment the deck sees the cut-off, and
// only the session's own transcript ends it: an entry after the cut-off is the
// one proof that it is going again. Until then every poll takes whatever step
// is due — move it, start it again when its tab has gone, tell it to carry
// on — so a move that failed halfway, a restarted deck, or a herdr restart that
// closed every tab only delays it.

// The prompts a session gets when the deck sets it going again. They say what
// happened, because the session's last memory is an error.
const (
	continueAfterMove  = "Your last turn was cut off because the account running this session hit its usage limit. The session has moved to another account with room. Please continue where you left off."
	continueAfterLogin = "Your last turn was cut off because the account running this session lost its login. The session has moved to another account. Please continue where you left off."
	continueAfterReset = "The usage limit that cut off your last turn has reset. Please continue where you left off."
	lostBackgroundWork = " Background tasks that were still running when the session stopped did not carry over — re-run any you still need."
)

const authFailed = "authentication_failed"

// resetGrace is how long after a limit's stated reset the deck waits before
// telling a session to continue: the reset is the server's estimate, a prompt
// sent a few seconds early is refused all over again, and Claude's own
// "continuing automatically" gets the first go. resumeWindow is how long after
// the reset a cut-off the deck is only now seeing still counts — one cut off
// yesterday and left alone since was left alone on purpose.
const (
	resetGrace   = time.Minute
	resumeWindow = 30 * time.Minute
)

// tellWait is how long a session told to carry on has to answer before it is
// told again; after maxTells unanswered tells the deck stops and asks for
// someone. retryStep is the first wait after a step that failed, doubling
// each time it fails again.
const (
	tellWait  = 2 * time.Minute
	maxTells  = 3
	retryStep = time.Minute
)

// dismissBeat is how long a dismissed menu gets to clear before a prompt is
// typed where it was.
var dismissBeat = time.Second

// carryOn keeps the deck's promises. It makes one for each session in this
// deck that a limit or a lapsed login has just cut off, and takes every open
// one a step further.
func (m *Model) carryOn(now time.Time) []tea.Cmd {
	if !m.settings.autoMove() || m.demo || !m.backend.Herdr() || m.dry {
		return nil
	}
	// Moves and tells write to the ledger from their own goroutines, so the
	// copy the last poll brought may be behind.
	m.ledger = ledger.Load()
	m.promiseCutOffs(now)
	cands := m.candidates()
	var cmds []tea.Cmd
	for _, e := range ledger.Sorted(m.ledger) {
		if e.Carry == nil || e.Carry.Socket != m.socket {
			continue
		}
		if cmd := m.keepPromise(e, cands, now); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// promiseCutOffs records a promise for each session in this deck whose last
// turn a usage limit or a lapsed login ended.
func (m *Model) promiseCutOffs(now time.Time) {
	wrote := false
	for _, s := range m.fleet.Sessions {
		e := s.Activity.Tail.Err
		if !s.Live || !s.Home || s.ID == "" || m.busy(s.Key) || m.stale(s.Key) {
			continue
		}
		if (!e.RateLimited() && !e.AuthFailed()) || e.UUID == "" {
			continue
		}
		le := m.ledger[s.ID]
		if le.Handled == e.UUID || (le.Carry != nil && le.Carry.Err == e.UUID) {
			continue
		}
		if !turnOver(s) {
			m.noteHeld(s, e.UUID, s.Account)
			continue
		}
		old := e.RateLimited() && !e.ResetsAt.IsZero() && now.Sub(e.ResetsAt) > resumeWindow
		c := &ledger.Carry{
			Err: e.UUID, At: e.At, Kind: e.Kind, From: s.Account, ResetsAt: e.ResetsAt, LimitType: e.LimitType,
			Socket: m.socket, Busy: s.Activity.InFlight.Any(),
		}
		_ = ledger.Update(s.ID, func(x *ledger.Entry) {
			if x.Handled == e.UUID || (x.Carry != nil && x.Carry.Err == e.UUID) {
				return // another write got there first
			}
			x.Key = firstNonEmpty(x.Key, s.Key)
			x.Kind = firstNonEmpty(x.Kind, s.Kind)
			x.Cwd = firstNonEmpty(x.Cwd, s.Cwd)
			if x.Account == "" {
				x.Account, x.ConfigDir = s.Account, s.ConfigDir
			}
			if old {
				x.Handled = e.UUID
				return
			}
			// A newer cut-off answers the one before it.
			if x.Carry != nil {
				x.Handled = x.Carry.Err
			}
			x.Carry = c
			if e.RateLimited() {
				x.Refuse(ledger.Refusal{Account: s.Account, Until: liftsAt(*c), Limit: e.LimitType}, now)
			}
		})
		wrote = true
	}
	if wrote {
		m.ledger = ledger.Load()
	}
}

// keepPromise takes one promise the step it is due, if any.
func (m *Model) keepPromise(e ledger.Entry, cands []route.Candidate, now time.Time) tea.Cmd {
	c, key := *e.Carry, e.Key
	if m.unwanted(e) {
		debugLog.Printf("%s is finished, off the deck or closed for good: its cut-off on ⦿%s is not carried on", key, c.From)
		m.endPromise(e.SessionID)
		return nil
	}
	if m.busy(key) || m.stale(key) || now.Before(m.retryAt[key]) {
		return nil
	}
	s, ok := m.fleet.Best(key)
	if !ok || s.ID != e.SessionID {
		return nil
	}
	if t := s.Activity.Tail; answered(c, t) {
		if t.Err != nil && t.Err.UUID == t.LastUUID {
			debugLog.Printf("%s was cut off again, on ⦿%s, before it carried on from ⦿%s", key, s.Account, c.From)
		} else {
			debugLog.Printf("%s answered its cut-off on ⦿%s: promise kept", key, c.From)
		}
		m.endPromise(e.SessionID)
		delete(m.parked, key)
		return nil
	}
	switch {
	case s.Live && !s.Home:
		return nil // another deck runs it now
	case s.Live && !turnOver(s) && !onDialog(s):
		m.noteHeld(s, c.Err, c.From) // working again, or waiting on someone
		return nil
	case !s.Live && m.fleet.OwnErr != nil:
		return nil // this workspace didn't answer: it may be running there
	case !c.Told.IsZero() && now.Sub(c.Told) < tellWait:
		return nil // just told: its answer isn't in yet
	}
	label := m.sessionLabel(s)
	if c.Tells >= maxTells {
		if !m.stuckNoted[key] {
			m.stuckNoted[key] = true
			debugLog.Printf("%s was told to carry on %d times and never answered", key, c.Tells)
			notify(label+" isn't carrying on", fmt.Sprintf("Told it %d times with no answer — open its tab", c.Tells))
		}
		return nil
	}
	if s.Account == c.From && stillOut(c, now) {
		if lifted(c, now) {
			// Its agents can work again, and a move would end them. It carries
			// on here once Claude Code's own continue has had its go.
			return nil
		}
		d := route.Pick(cands, route.Request{Exclude: c.From, Now: now})
		to, ok := m.accountByName(d.Account)
		if !ok {
			if _, noted := m.parked[key]; !noted {
				m.notice = fmt.Sprintf("%s is out on ⦿%s — %s; it carries on when the limit lifts", label, c.From, d.Why)
				notify(label+" is waiting on a usage limit", d.Why)
			}
			m.parked[key] = liftsAt(c)
			if s.Live && s.Status == session.NeedsInput && m.waited[key] != c.Err {
				return m.waitCmd(s, c)
			}
			return nil
		}
		delete(m.parked, key)
		for i := range cands {
			if cands[i].Name == to.Name {
				// Spread a burst of cut-offs instead of piling them on one account:
				// each is told to carry on, so it lands busy.
				cands[i].Live++
				cands[i].Load++
			}
		}
		why := cutOffWhy(c)
		m.notice = fmt.Sprintf("%s: %s — moving it to ⦿%s and carrying on…", label, why, to.Name)
		return m.carryMove(s, to, c, why)
	}
	delete(m.parked, key)
	if !s.Live {
		to := m.sessionAccount(s)
		m.notice = fmt.Sprintf("%s stopped while it waited — starting it again on ⦿%s to carry on…", label, to.Name)
		return m.carryMove(s, to, c, "started again to carry on")
	}
	return m.tellCmd(s, c)
}

// turnOver reports whether a session's last turn has ended, so nothing in
// flight is lost by moving or prompting it. Claude's what-now menu after a
// limit reads as a prompt; the turn is over all the same. So is the turn of a
// lead its account cut off that has asked nothing since, however busy the
// background work it started keeps the session looking: once a limit lifts,
// a monitor may run on for good.
func turnOver(s fleet.Session) bool {
	t := s.Activity.Tail
	switch {
	case s.Status.Waiting():
		return true
	case !t.Err.RateLimited() && !t.Err.AuthFailed():
		return false
	case s.Status == session.NeedsInput:
		return t.Err.RateLimited()
	}
	return s.Status == session.Background || t.Stopped()
}

// noteHeld logs, once per cut-off, a session the deck is leaving alone
// because it doesn't look stopped, and why. A hold nobody can see is how a
// cut-off sits out its limit unnoticed.
func (m *Model) noteHeld(s fleet.Session, errID, from string) {
	if m.heldNoted[s.Key] == errID {
		return
	}
	m.heldNoted[s.Key] = errID
	t := s.Activity.Tail
	why := "it is mid-turn"
	switch {
	case s.Status == session.NeedsInput:
		why = "a dialog is up"
	case t.Err != nil && t.AskedAt.IsZero():
		why = "it reads busy, and its transcript doesn't record requests"
	case t.Err != nil:
		why = "its lead has asked the API since"
	}
	debugLog.Printf("%s was cut off on ⦿%s, but %s: waiting for its turn to end", s.Key, from, why)
}

// answered reports whether a session's conversation has moved past its
// cut-off: a reply, or a newer error. An unread transcript has not.
func answered(c ledger.Carry, t session.Tail) bool {
	return t.LastUUID != "" && t.LastUUID != c.Err
}

// stillOut reports whether the account that cut a session off still can't take
// it back. A lapsed login has no reset to wait for: only a move gets it going.
func stillOut(c ledger.Carry, now time.Time) bool {
	return c.Kind == authFailed || now.Before(liftsAt(c).Add(resetGrace))
}

// lifted reports whether a cut-off's usage limit has already lifted. A lapsed
// login never does.
func lifted(c ledger.Carry, now time.Time) bool {
	return c.Kind != authFailed && !now.Before(liftsAt(c))
}

// liftsAt is when a cut-off's limit lifts. One that didn't say gets the hour
// fleet.Limits gives it.
func liftsAt(c ledger.Carry) time.Time {
	if c.ResetsAt.IsZero() {
		return c.At.Add(time.Hour)
	}
	return c.ResetsAt
}

func cutOffWhy(c ledger.Carry) string {
	if c.Kind == authFailed {
		return fmt.Sprintf("⦿%s's login expired", c.From)
	}
	return fmt.Sprintf("⦿%s hit its %s limit", c.From, limitName(c.LimitType))
}

// carryMove moves a session to an account, or starts it there when it has
// stopped, and tells it to carry on.
func (m *Model) carryMove(s fleet.Session, to account.Account, c ledger.Carry, why string) tea.Cmd {
	m.moving[s.Key] = true
	moved := to.Name != c.From
	return m.moveCmd(s, to, moveOpts{
		auto: true, why: why, start: true, carry: c.Err, focus: s.Focused,
		submit: carryPrompt(c, moved, moved || !s.Live),
	})
}

// onDialog reports a session on a dialog its transcript doesn't explain: one
// moved under a promise, its cut-off left behind on the account it came from.
// Claude opens on its limit menu when the account it landed on is out too,
// and then waits for someone who isn't coming.
func onDialog(s fleet.Session) bool {
	return s.Status == session.NeedsInput && s.Activity.Tail.Err == nil
}

// dialogRecheck is how long a session on some other dialog waits before its
// screen is read again.
const dialogRecheck = time.Minute

// tellCmd tells a session that is ready to carry on to do so. A menu Claude
// put up when the limit hit is dismissed first: herdr won't type into a
// session showing one, and it shouldn't. A dialog the transcript doesn't
// explain is dismissed only when its screen shows that menu: Esc on a trust
// dialog would exit the session.
func (m *Model) tellCmd(s fleet.Session, c ledger.Carry) tea.Cmd {
	m.telling[s.Key] = true
	backend, key, id, prompting := m.backend, s.Key, s.ID, s.Prompting
	verify := onDialog(s)
	text := carryPrompt(c, s.Account != c.From, false)
	return func() tea.Msg {
		if verify && !backend.LimitMenu(key) {
			return continuedMsg{key: key, held: true}
		}
		if prompting {
			if err := backend.Keys(key, "esc"); err != nil {
				return continuedMsg{key: key, err: err}
			}
			time.Sleep(dismissBeat)
		}
		_, err := backend.Prompt(key, text)
		if err == nil {
			_ = ledger.Update(id, func(e *ledger.Entry) { told(e, c.Err) })
		}
		return continuedMsg{key: key, err: err}
	}
}

// waitCmd answers the limit menu of a session with nowhere to move with
// "Wait here, then continue automatically". Claude offers that, rather than
// arming it, when the limit resets more than a day out: left alone the menu
// sits there, and Claude doesn't carry on at the reset.
func (m *Model) waitCmd(s fleet.Session, c ledger.Carry) tea.Cmd {
	m.telling[s.Key] = true
	backend, key := m.backend, s.Key
	return func() tea.Msg {
		ok, err := backend.WaitAtLimit(key)
		return waitedMsg{key: key, errID: c.Err, ok: ok, err: err}
	}
}

// told notes on a promise that its session was just told to carry on.
func told(e *ledger.Entry, errID string) {
	if e.Carry != nil && e.Carry.Err == errID {
		e.Carry.Told = time.Now()
		e.Carry.Tells++
	}
}

// carryPrompt is what a session is told when the deck sets it going again.
// moved: it runs on another account than the one that cut it off. restarted:
// its process is new, so background work it had running is gone.
func carryPrompt(c ledger.Carry, moved, restarted bool) string {
	if custom := strings.TrimSpace(os.Getenv("TICKETDECK_CONTINUE_PROMPT")); custom != "" {
		return custom
	}
	p := continueAfterReset
	switch {
	case moved && c.Kind == authFailed:
		p = continueAfterLogin
	case moved:
		p = continueAfterMove
	}
	if restarted && c.Busy {
		p += lostBackgroundWork
	}
	return p
}

// continuePrompt is what a session cut off mid-task is told when it is moved
// by hand.
func continuePrompt(s fleet.Session) string {
	var c ledger.Carry
	if e := s.Activity.Tail.Err; e != nil {
		c.Kind = e.Kind
	}
	c.Busy = s.Activity.InFlight.Any()
	return carryPrompt(c, true, true)
}

// unwanted reports a promise nobody wants kept any more: its ticket or project
// is done or off the deck, or it is an ad-hoc session closed for good. Before
// the lists have loaded there is no telling, and the promise stands.
func (m Model) unwanted(e ledger.Entry) bool {
	switch {
	case e.Forgotten:
		return true
	case e.Kind == ledger.Ticket && !m.lastSync.IsZero():
		// Validate is completed-type yet still active, as everywhere else on
		// the deck. Read as done, it silently ended every promise ZEN-6940
		// was made, and each of its cut-offs waited for someone to type
		// "resume" (2026-10-25 to 28).
		is, ok := m.issueByKey(e.Key)
		return !ok || finishedTicket(is)
	case e.Kind == ledger.Project && m.myProjects != nil:
		p, ok := m.projectByKey(e.Key)
		return !ok || p.StatusType == "completed" || p.StatusType == "canceled"
	}
	return false
}

// endPromise closes a session's promise, kept or not, and marks its cut-off
// as dealt with.
func (m *Model) endPromise(id string) {
	_ = ledger.Update(id, func(e *ledger.Entry) {
		if e.Carry != nil {
			e.Handled = e.Carry.Err
			e.Carry = nil
		}
	})
	m.ledger = ledger.Load()
}

// busy reports a step for the session already in flight.
func (m Model) busy(key string) bool {
	return m.moving[key] || m.telling[key] || m.launching[key]
}

// stale reports whether the last poll began before the deck's last step on a
// session finished. It shows the session as it was before that step, and
// acting on it acts on a session that isn't there any more: a move decided on
// one sent a session that had already left ⦿m0 "from ⦿m0" (2026-10-22).
func (m Model) stale(key string) bool {
	done, ok := m.stepDone[key]
	return ok && m.fleet.At.Before(done)
}

// retryLater puts off the next try at a step that failed, twice as long each
// time it fails again, and says so once when it keeps failing.
func (m *Model) retryLater(key string, err error) time.Duration {
	m.fails[key]++
	n := m.fails[key]
	wait := retryStep << min(n-1, 4)
	m.retryAt[key] = time.Now().Add(wait)
	if n == 3 {
		notify("Can't get "+m.label(key)+" going again", err.Error()+" — still trying")
	}
	return wait
}

// carryNote says what the deck has promised a cut-off session, for its row.
func (m Model) carryNote(s fleet.Session, now time.Time) string {
	e, ok := m.ledger[s.ID]
	if !ok || e.Carry == nil || !m.settings.autoMove() {
		return ""
	}
	c := *e.Carry
	switch {
	case m.moving[s.Key]:
		return "moving it…"
	case c.Tells >= maxTells:
		return fmt.Sprintf("told to carry on %d times with no answer — open its tab", c.Tells)
	case !c.Told.IsZero():
		return "told to carry on"
	case s.Account == c.From && c.Kind == authFailed:
		return "no other account has room; it moves when one does"
	case s.Account == c.From && stillOut(c, now):
		if !s.Live {
			return "starts again and carries on at " + route.Clock(liftsAt(c).Add(resetGrace), now)
		}
		return "no account has room; it carries on at " + route.Clock(liftsAt(c).Add(resetGrace), now)
	}
	return "carries on next"
}
