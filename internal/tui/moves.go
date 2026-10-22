package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/route"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// This file moves sessions: between accounts (H, and automatically when an
// account runs out), and into this deck's workspace from the workspaces older
// per-account decks left running (M).
//
// Every move is the same three steps: stop the session where it runs, copy its
// conversation when it changes account, start it again here with --resume.
// What carries over is the conversation, not the process — a background task
// it had running stops with it — so the deck only moves a session on its own
// once its turn is over.

// The prompts a session gets when the deck sets it going again on its own.
// They say what happened, because the session's last memory is an error.
const (
	continueAfterMove  = "Your last turn was cut off because the account running this session hit its usage limit. The session has moved to another account with room. Please continue where you left off."
	continueAfterLogin = "Your last turn was cut off because the account running this session lost its login. The session has moved to another account. Please continue where you left off."
	continueAfterReset = "The usage limit that cut off your last turn has reset. Please continue where you left off."
	lostBackgroundWork = " Background tasks that were still running when the session stopped did not survive the move — re-run any you still need."
)

// notify tells the user about something the deck did on its own; tests
// replace it.
var notify = herd.Notify

// resetGrace is how long after a limit's stated reset the deck waits before
// telling a session to continue: the reset is the server's estimate, and a
// prompt sent a few seconds early is refused all over again. resumeWindow is
// how long after the reset it still does it — a session cut off yesterday and
// left alone since was left alone on purpose.
const (
	resetGrace   = time.Minute
	resumeWindow = 30 * time.Minute
)

// dismissBeat is how long a dismissed menu gets to clear before a prompt is
// typed where it was.
var dismissBeat = time.Second

// moveDoneMsg is the result of moving a session.
type moveDoneMsg struct {
	key, label string
	newKey     string // the agent name it runs under now, when that changed
	from, to   string // account names
	auto       bool   // the deck moved it on its own
	adopt      bool   // brought in from another deck's workspace
	// blocked: it moved, but stopped at a prompt of its own on the way up — a
	// folder it hasn't trusted on that account, say — so it needs the user in
	// its tab before anything can be sent to it. pending is what to send then.
	blocked bool
	pending string
	why     string
	err     error
	out     string
}

// continuedMsg is the result of telling a session to continue in place.
type continuedMsg struct {
	key         string
	err         error
	afterPrompt bool   // it was held for a startup prompt the user has answered
	errUUID     string // the cut-off it answered, to try again if it failed
}

// retryContinue is how long a continue that failed waits before the next try.
const retryContinue = time.Minute

type moveOpts struct {
	why     string
	submit  string
	auto    bool
	adopt   bool
	focus   bool
	errUUID string // the API error this move answers, recorded so it is acted on once
}

// ticketFor rebuilds a session's identity — what its launch seeds — from the
// deck's view of the ticket or project it belongs to.
func (m Model) ticketFor(s fleet.Session) session.Ticket {
	var t session.Ticket
	switch s.Kind {
	case ledger.Ticket:
		if is, ok := m.issueByKey(s.Key); ok {
			t = toTicket(is)
		} else {
			t = session.Ticket{Key: s.Key}
		}
	case ledger.Project:
		if p, ok := m.projectByKey(s.Key); ok {
			t = toProjectTicket(p)
		} else {
			t = session.Ticket{Key: s.Key, Project: true}
		}
	default:
		t = session.Ticket{Key: s.Key, Title: s.Name, Scratch: true}
	}
	t.SessionID = s.ID
	return t
}

// sessionAccount is the account a session is on, even one that isn't in this
// deck's list any more.
func (m Model) sessionAccount(s fleet.Session) account.Account {
	if a, ok := m.accountByName(s.Account); ok {
		return a
	}
	return account.Account{Name: s.Account, ConfigDir: s.ConfigDir}
}

// moveCmd moves one session to an account. A session that isn't running is
// only copied: the next open resumes it where it now lives, and starting it on
// the user's behalf is not what they asked for.
func (m Model) moveCmd(s fleet.Session, to account.Account, o moveOpts) tea.Cmd {
	from := m.sessionAccount(s)
	t := m.ticketFor(s)
	label := m.sessionLabel(s)
	mv := Move{
		Name: s.Key, From: from, To: to, Ticket: t,
		Cwd: firstNonEmpty(s.Cwd, m.root), Focus: o.focus, Submit: o.submit,
	}
	if s.Live {
		mv.Socket, mv.PaneID, mv.PID = s.Socket, s.PaneID, s.PID
	}
	// An ad-hoc session coming in from another workspace may share its agent
	// name with one already here.
	if o.adopt && s.Kind == ledger.Scratch {
		mv.NewName = herd.UniqueName(s.Key, m.homeRefs())
	}
	backend := m.backend
	key, id := s.Key, s.ID
	return func() tea.Msg {
		var out string
		var err error
		if s.Live {
			out, err = backend.Move(mv)
		} else if from.ConfigDir != to.ConfigDir {
			err = account.CopySession(from, to, t.ID(), label)
		}
		if errors.Is(err, account.ErrNoSession) {
			err = nil // nothing written yet: there is nothing to carry over
		}
		msg := moveDoneMsg{key: key, label: label, from: from.Name, to: to.Name, auto: o.auto, adopt: o.adopt, why: o.why, out: out}
		if errors.Is(err, herd.ErrBlockedAtStart) {
			msg.blocked, msg.pending, err = true, o.submit, nil
		}
		msg.err = err
		if err != nil {
			return msg
		}
		msg.newKey = mv.NewName
		_ = ledger.Update(id, func(e *ledger.Entry) {
			e.Key = firstNonEmpty(mv.NewName, key)
			e.Kind = firstNonEmpty(e.Kind, s.Kind)
			e.Account, e.ConfigDir = to.Name, to.ConfigDir
			e.Cwd = firstNonEmpty(e.Cwd, mv.Cwd)
			if from.Name != to.Name {
				e.Moves = append(e.Moves, ledger.Move{From: from.Name, To: to.Name, At: time.Now(), Why: o.why})
			}
			if o.errUUID != "" {
				e.Handled = o.errUUID
			}
		})
		return msg
	}
}

// homeRefs is every session running in this deck's workspace, by agent name.
func (m Model) homeRefs() []session.SessionRef {
	var out []session.SessionRef
	for _, s := range m.fleet.Live() {
		if s.Home {
			out = append(out, session.SessionRef{Name: s.Key})
		}
	}
	return out
}

// sessionLabel is what to call a session in a notice.
func (m Model) sessionLabel(s fleet.Session) string {
	switch s.Kind {
	case ledger.Ticket, ledger.Project:
		return m.label(s.Key)
	}
	return truncCols(firstNonEmpty(s.Name, s.Key), 40)
}

// continuePrompt is what an automatically moved session is told.
func continuePrompt(s fleet.Session) string {
	p := continueAfterMove
	if s.Activity.Tail.Err.AuthFailed() {
		p = continueAfterLogin
	}
	if s.Activity.InFlight.Any() {
		p += lostBackgroundWork
	}
	if custom := strings.TrimSpace(os.Getenv("TICKETDECK_CONTINUE_PROMPT")); custom != "" {
		p = custom
	}
	return p
}

// autoMoves acts on the sessions in this workspace that their account's usage
// limit cut off: each moves to the account with the most room and is told to
// continue. When no account has room it waits, and once the limit lifts it is
// told to continue where it is. Each cut-off is acted on once — recorded here
// and in the ledger, so a restarted deck doesn't act on it again.
//
// Only a session whose turn is over is touched: the limit ended the turn, so
// nothing is in flight to lose. A session someone has since typed into is
// working again and is left alone.
func (m *Model) autoMoves(now time.Time) []tea.Cmd {
	if !m.settings.autoMove() || m.demo || !m.backend.Herdr() || m.dry {
		return nil
	}
	var cmds []tea.Cmd
	cands := m.candidates()
	for _, s := range m.fleet.Sessions {
		tail := s.Activity.Tail
		e := tail.Err
		// Claude's what-now menu after a limit reads as a prompt; the turn is
		// over all the same.
		over := s.Status.Waiting() || (s.Status == session.NeedsInput && e.RateLimited())
		if !s.Live || !s.Home || m.moving[s.Key] || !over {
			continue
		}
		cutOff := e.RateLimited() || e.AuthFailed()
		if !cutOff || e.UUID == "" || m.handled[e.UUID] || m.ledger[s.ID].Handled == e.UUID {
			continue
		}
		if now.Before(m.retryAt[e.UUID]) {
			continue
		}
		// A lapsed login has no reset to wait for: only a move gets it going.
		if e.RateLimited() && !tail.Limited(now) {
			// The limit lifted while it waited.
			if !e.ResetsAt.IsZero() && now.Before(e.ResetsAt.Add(resetGrace)) {
				continue
			}
			if !e.ResetsAt.IsZero() && now.Sub(e.ResetsAt) > resumeWindow {
				m.handled[e.UUID] = true // too old to revive on its own
				continue
			}
			m.handled[e.UUID] = true
			cmds = append(cmds, m.continueCmd(s, e.UUID))
			continue
		}
		d := route.Pick(cands, route.Request{Exclude: s.Account, Now: now})
		to, ok := m.accountByName(d.Account)
		if !ok {
			if _, noted := m.parked[s.Key]; !noted {
				m.notice = fmt.Sprintf("%s is out on ⦿%s — %s; it continues when the limit lifts", m.sessionLabel(s), s.Account, d.Why)
				notify(m.sessionLabel(s)+" is waiting on a usage limit", d.Why)
			}
			m.parked[s.Key] = e.ResetsAt
			continue
		}
		delete(m.parked, s.Key)
		m.handled[e.UUID] = true
		m.moving[s.Key] = true
		for i := range cands {
			if cands[i].Name == to.Name {
				cands[i].Live++ // spread a burst of cut-offs instead of piling them on one account
			}
		}
		why := fmt.Sprintf("⦿%s hit its %s limit", s.Account, limitName(e.LimitType))
		if e.AuthFailed() {
			why = fmt.Sprintf("⦿%s's login expired", s.Account)
		}
		m.notice = fmt.Sprintf("%s: %s — moving it to ⦿%s and carrying on…", m.sessionLabel(s), why, to.Name)
		cmds = append(cmds, m.moveCmd(s, to, moveOpts{auto: true, why: why, submit: continuePrompt(s), errUUID: e.UUID}))
	}
	return cmds
}

// limitName turns a rate-limit type into words.
func limitName(t string) string {
	switch t {
	case "five_hour":
		return "5-hour"
	case "seven_day", "seven_day_opus", "seven_day_sonnet":
		return "weekly"
	case "":
		return "usage"
	}
	return strings.ReplaceAll(t, "_", " ")
}

// continueCmd tells a session whose limit has lifted to carry on, where it is.
// The menu Claude put up when the limit hit is dismissed first: herdr won't
// type into a session showing one, and it shouldn't.
func (m Model) continueCmd(s fleet.Session, errUUID string) tea.Cmd {
	backend, key, id, prompting := m.backend, s.Key, s.ID, s.Prompting
	return func() tea.Msg {
		if prompting {
			if err := backend.Keys(key, "esc"); err != nil {
				return continuedMsg{key: key, err: err, errUUID: errUUID}
			}
			time.Sleep(dismissBeat)
		}
		_, err := backend.Send(key, continueAfterReset)
		if err == nil {
			_ = ledger.Update(id, func(e *ledger.Entry) { e.Handled = errUUID })
		}
		return continuedMsg{key: key, err: err, errUUID: errUUID}
	}
}

// handleMoveDone reports a finished move and refreshes the picture.
func (m Model) handleMoveDone(msg moveDoneMsg) (tea.Model, tea.Cmd) {
	delete(m.moving, msg.key)
	m.ledger = ledger.Load()
	if msg.err != nil {
		debugLog.Printf("move %s %s → %s failed: %v (%s)", msg.label, msg.from, msg.to, msg.err, msg.out)
		m.err = fmt.Errorf("moving %s: %v", msg.label, msg.err)
		m.notice = ""
		if msg.auto {
			notify("Couldn't move "+msg.label, msg.err.Error())
		}
		return m, m.refreshStatuses()
	}
	debugLog.Printf("move %s %s → %s ok (auto=%v adopt=%v blocked=%v): %s", msg.label, msg.from, msg.to, msg.auto, msg.adopt, msg.blocked, msg.out)
	m.err = nil
	if msg.blocked {
		// It is running on the new account but stopped at a prompt of its own.
		// What it was to be told waits until someone answers that.
		if msg.pending != "" {
			m.pendingSend[firstNonEmpty(msg.newKey, msg.key)] = msg.pending
		}
		m.notice = fmt.Sprintf("%s moved ⦿%s → ⦿%s but stopped at a prompt in its tab — answer it there and it carries on", msg.label, msg.from, msg.to)
		notify(msg.label+" needs you in its tab", "It moved to ⦿"+msg.to+" and stopped at a startup prompt")
		return m, m.refreshStatuses()
	}
	switch {
	case msg.auto:
		m.notice = fmt.Sprintf("%s moved ⦿%s → ⦿%s and is carrying on (%s)", msg.label, msg.from, msg.to, msg.why)
		notify(msg.label+" moved to ⦿"+msg.to, msg.why+" — it is carrying on there")
	case msg.adopt && msg.from == msg.to:
		m.notice = fmt.Sprintf("%s is in this deck now, still on ⦿%s", msg.label, msg.to)
	case msg.adopt:
		m.notice = fmt.Sprintf("%s is in this deck now, on ⦿%s", msg.label, msg.to)
	default:
		m.notice = fmt.Sprintf("%s → ⦿%s ✓", msg.label, msg.to)
		if strings.Contains(msg.out, "waiting on a prompt") {
			m.notice += " · it is waiting on a prompt in its tab"
		}
	}
	return m, m.refreshStatuses()
}

// ── H: move a session to another account ────────────────────────────────────

// handoffTargets are the accounts a key's session can move to — every account
// but the one it is on, best first — or, for a key with no session yet, every
// account to open it under.
func (m Model) handoffTargets(key string) []account.Account {
	cur := ""
	if s, ok := m.fleet.Best(key); ok {
		cur = s.Account
	}
	var out []account.Account
	for _, r := range route.Rank(m.candidates(), time.Now()) {
		if r.Name == cur {
			continue
		}
		if a, ok := m.accountByName(r.Name); ok {
			out = append(out, a)
		}
	}
	return out
}

// confirmHandoff moves the overlay's session to the chosen account — or, when
// it has none yet, opens it there.
func (m Model) confirmHandoff(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(m.handoffCands) {
		return m, nil
	}
	key, to := m.handoffKey, m.handoffCands[i]
	// An account nobody is signed in to can't run the session at all; one that
	// is merely spent might be back in minutes, and that call is the user's.
	if e := m.quotas[to.Name]; e.SignedOut() || m.authFailed(to.Name) {
		m.notice = "⦿" + to.Name + " isn't signed in — log in with: " + loginCmd(to)
		return m, nil
	}
	m.handoffKey, m.handoffCands, m.handoffCursor, m.handoffLabel = "", nil, 0, ""

	s, ok := m.fleet.Best(key)
	if !ok {
		return m.openOn(key, to)
	}
	if s.Live && s.PaneID == "" {
		m.notice = m.sessionLabel(s) + " runs in a terminal outside herdr — stop it there, then move it"
		return m, nil
	}
	m.moving[key] = true
	m.notice = fmt.Sprintf("moving %s to ⦿%s…", m.sessionLabel(s), to.Name)
	o := moveOpts{why: "moved by hand", adopt: s.Live && !s.Home}
	// A session parked on a limit was cut off mid-task: moving it is the
	// whole point, and it should pick the task back up.
	if s.Activity.Tail.Err.RateLimited() {
		o.submit, o.errUUID = continuePrompt(s), s.Activity.Tail.Err.UUID
	}
	return m, m.moveCmd(s, to, o)
}

// openOn opens a key's session under a chosen account (H on a ticket with no
// session yet).
func (m Model) openOn(key string, to account.Account) (tea.Model, tea.Cmd) {
	if is, ok := m.issueByKey(key); ok {
		m.forceAcct = &to
		return m.launchIssue(is)
	}
	if p, ok := m.projectByKey(key); ok {
		m.forceAcct = &to
		return m.launchProject(p)
	}
	return m, nil
}

// ── M: bring sessions in from other decks' workspaces ───────────────────────

// peerSessions are the live sessions running in another deck's workspace —
// where the per-account decks of old started them. This deck can see them but
// can't drive them until they are brought in.
func (m Model) peerSessions() []fleet.Session {
	var out []fleet.Session
	for _, s := range m.fleet.Sessions {
		if s.Live && !s.Home && s.Socket != "" && s.PaneID != "" {
			out = append(out, s)
		}
	}
	return out
}

// bringable reports whether a peer session can be brought in right now: its
// turn is over or it is waiting on the user. A working one is queued instead.
func bringable(s fleet.Session) bool {
	return s.Status.Waiting() || s.Status == session.NeedsInput
}

func (m Model) openBring() (tea.Model, tea.Cmd) {
	if !m.backend.Herdr() {
		m.notice = "bringing sessions in needs the herdr backend"
		return m, nil
	}
	if len(m.peerSessions()) == 0 {
		m.notice = "no sessions are running in other decks"
		return m, nil
	}
	m.bringMenu = true
	m.bringCursor = 0
	m.notice = ""
	return m, nil
}

func (m Model) updateBring(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	peers := m.peerSessions()
	if m.bringCursor >= len(peers) {
		m.bringCursor = max(len(peers)-1, 0)
	}
	switch msg.String() {
	case "esc", "q", "M":
		m.bringMenu = false
	case "up", "k":
		if m.bringCursor > 0 {
			m.bringCursor--
		}
	case "down", "j":
		if m.bringCursor < len(peers)-1 {
			m.bringCursor++
		}
	case "enter":
		if len(peers) == 0 {
			m.bringMenu = false
			return m, nil
		}
		next, cmd := m.bring([]fleet.Session{peers[m.bringCursor]})
		nm := next.(Model)
		nm.bringMenu = len(nm.peerSessions()) > 1
		return nm, cmd
	case "a":
		m.bringMenu = false
		return m.bring(peers)
	}
	return m, nil
}

// bring adopts sessions now, or queues the busy ones to come in once their
// turn is over.
func (m Model) bring(ss []fleet.Session) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	now, queued := 0, 0
	for _, s := range ss {
		if m.moving[s.Key] {
			continue
		}
		if !bringable(s) {
			m.adoptQueue[s.Key] = true
			queued++
			continue
		}
		delete(m.adoptQueue, s.Key)
		m.moving[s.Key] = true
		cmds = append(cmds, m.moveCmd(s, m.sessionAccount(s), moveOpts{adopt: true, why: "brought in from the " + s.Workspace + " deck"}))
		now++
	}
	switch {
	case now > 0 && queued > 0:
		m.notice = fmt.Sprintf("bringing %d session(s) in; %d busy one(s) follow once their turn ends", now, queued)
	case now > 0:
		m.notice = fmt.Sprintf("bringing %d session(s) in…", now)
	case queued > 0:
		m.notice = fmt.Sprintf("%d busy session(s) will come in once their turn ends", queued)
	}
	return m, tea.Batch(cmds...)
}

// sendPending delivers what a moved session was to be told once it is ready for
// it: running here, past the startup prompt that held it, and at its input.
func (m *Model) sendPending() []tea.Cmd {
	var cmds []tea.Cmd
	for key, text := range m.pendingSend {
		s, ok := m.fleet.Best(key)
		if !ok || !s.Live || !s.Home {
			continue
		}
		if s.Status != session.Idle && s.Status != session.Unread && s.Status != session.Limited {
			continue
		}
		delete(m.pendingSend, key)
		backend := m.backend
		cmds = append(cmds, func() tea.Msg {
			_, err := backend.Send(key, text)
			return continuedMsg{key: key, err: err, afterPrompt: true}
		})
	}
	return cmds
}

// adoptQueued brings in the queued sessions whose turn has ended.
func (m *Model) adoptQueued() []tea.Cmd {
	var cmds []tea.Cmd
	for key := range m.adoptQueue {
		s, ok := m.fleet.Best(key)
		if !ok || !s.Live || s.Home {
			delete(m.adoptQueue, key) // gone, or already here
			continue
		}
		if !bringable(s) || m.moving[key] {
			continue
		}
		delete(m.adoptQueue, key)
		m.moving[key] = true
		cmds = append(cmds, m.moveCmd(s, m.sessionAccount(s), moveOpts{adopt: true, why: "brought in from the " + s.Workspace + " deck"}))
	}
	return cmds
}

func (m Model) renderBring() string {
	var b strings.Builder
	peers := m.peerSessions()
	fmt.Fprintf(&b, "%s\n", titleStyle.Render("Sessions running in other decks"))
	fmt.Fprintf(&b, "%s\n\n", dimStyle.Render("↑/↓ select · ⏎ bring this one here · a bring them all · esc close"))
	fmt.Fprintf(&b, "%s\n\n", dimStyle.Render(
		"  These run in the workspaces the per-account decks started. Bringing one\n"+
			"  here stops it there and resumes it in this deck on the same account, so\n"+
			"  this deck can open, move and watch it. A busy one waits for its turn to end."))
	nameCol, acctCol := 10, 4
	for _, s := range peers {
		nameCol = max(nameCol, min(cols(m.sessionLabel(s)), 40))
		acctCol = max(acctCol, cols(s.Account))
	}
	for i, s := range peers {
		cell, color := sessionCellText(s.Status, "")
		when := ""
		switch {
		case m.moving[s.Key]:
			when = "moving…"
		case m.adoptQueue[s.Key]:
			when = "comes in when its turn ends"
		case !bringable(s):
			when = "busy — ⏎ queues it"
		}
		line := fmt.Sprintf("%s %-*s  in the %s deck  %s", cell, nameCol, truncCols(m.sessionLabel(s), 40), s.Workspace, when)
		acct := fmt.Sprintf("⦿ %-*s", acctCol, s.Account)
		if i == m.bringCursor {
			fmt.Fprintf(&b, "%s\n", selStyle.Render("▶ "+acct+" "+line))
			continue
		}
		fmt.Fprintf(&b, "  %s %s\n", m.acctStyle(s.Account).Render(acct), lipgloss.NewStyle().Foreground(color).Render(line))
	}
	return b.String()
}
