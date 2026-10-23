package tui

import (
	"errors"
	"fmt"
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
// once its turn is over. The moves the deck makes on its own are steps in
// keeping a promise (carry.go).

// notify tells the user about something the deck did on its own; tests
// replace it.
var notify = herd.Notify

// moveDoneMsg is the result of moving a session.
type moveDoneMsg struct {
	key, label string
	newKey     string // the agent name it runs under now, when that changed
	from, to   string // account names
	auto       bool   // the deck moved it on its own
	adopt      bool   // brought in from another deck's workspace
	carry      bool   // a step in keeping a promise, which sees the session told
	// blocked: it moved, but stopped at a prompt of its own on the way up — a
	// folder it hasn't trusted on that account, say — so it needs the user in
	// its tab before anything can be sent to it. pending is what to send then.
	blocked bool
	// untold: it moved, but the prompt sent after it never started a turn.
	untold  bool
	pending string
	why     string
	err     error
	out     string
}

// continuedMsg is the result of telling a session to carry on.
type continuedMsg struct {
	key         string
	err         error
	afterPrompt bool // it was held for a startup prompt the user has answered
}

type moveOpts struct {
	why    string
	submit string
	auto   bool
	adopt  bool
	focus  bool
	// start: a session that isn't running is started on the account, not only
	// copied there.
	start bool
	// carry is the cut-off whose promise this move keeps; errUUID is one a move
	// by hand answers outright, recorded so it is acted on once.
	carry   string
	errUUID string
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
// only copied unless o.start says to start it: the next open resumes it where
// it now lives, and starting it on the user's behalf is not what they asked
// for.
func (m Model) moveCmd(s fleet.Session, to account.Account, o moveOpts) tea.Cmd {
	from := m.sessionAccount(s)
	t := m.ticketFor(s)
	if from.ConfigDir != to.ConfigDir || !s.Live {
		// Accounts drift apart in their settings; a session halfway through a
		// task keeps the model it started on, not the new account's default.
		t.Model = s.Activity.Tail.Model
	}
	label := m.sessionLabel(s)
	// A stopped session starts where it was first started: that is where
	// `claude --resume` finds its conversation.
	mv := Move{
		Name: s.Key, From: from, To: to, Ticket: t,
		Cwd: firstNonEmpty(s.Cwd, m.ledger[s.ID].Cwd, m.root), Focus: o.focus, Submit: o.submit,
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
	start := s.Live || o.start
	return func() tea.Msg {
		var out string
		var err error
		if start {
			out, err = backend.Move(mv)
		} else if from.ConfigDir != to.ConfigDir {
			err = account.CopySession(from, to, t.ID(), label)
		}
		if errors.Is(err, account.ErrNoSession) {
			err = nil // nothing written yet: there is nothing to carry over
		}
		msg := moveDoneMsg{key: key, label: label, from: from.Name, to: to.Name, auto: o.auto, adopt: o.adopt, carry: o.carry != "", why: o.why, out: out}
		switch {
		case errors.Is(err, herd.ErrBlockedAtStart):
			msg.blocked, msg.pending, err = true, o.submit, nil
		case errors.Is(err, herd.ErrNoTurn):
			msg.untold, msg.pending, err = true, o.submit, nil
		}
		msg.err = err
		if err != nil {
			return msg
		}
		msg.newKey = mv.NewName
		if start {
			fleet.MarkLaunch(to.ConfigDir)
		}
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
			if o.carry != "" && o.submit != "" && start && !msg.blocked && !msg.untold {
				told(e, o.carry)
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

// handleMoveDone reports a finished move and refreshes the picture.
func (m Model) handleMoveDone(msg moveDoneMsg) (tea.Model, tea.Cmd) {
	delete(m.moving, msg.key)
	now := time.Now()
	m.stepDone[msg.key] = now
	if msg.newKey != "" {
		m.stepDone[msg.newKey] = now
	}
	m.ledger = ledger.Load()
	if msg.err != nil {
		debugLog.Printf("move %s %s → %s failed: %v (%s)", msg.label, msg.from, msg.to, msg.err, msg.out)
		m.err = fmt.Errorf("moving %s: %v", msg.label, msg.err)
		m.notice = ""
		switch {
		case msg.carry:
			// Whatever it got through, the promise picks up from there.
			wait := m.retryLater(msg.key, msg.err)
			m.err = fmt.Errorf("moving %s: %v — trying again in %s", msg.label, msg.err, wait)
		case msg.auto:
			notify("Couldn't move "+msg.label, msg.err.Error())
		}
		return m, m.refreshStatuses()
	}
	debugLog.Printf("move %s %s → %s ok (auto=%v adopt=%v blocked=%v): %s", msg.label, msg.from, msg.to, msg.auto, msg.adopt, msg.blocked, msg.out)
	m.err = nil
	delete(m.fails, msg.key)
	if msg.blocked {
		// It is running on the new account but stopped at a prompt of its own.
		// What it was to be told waits until someone answers that — and a
		// promise tells it then on its own.
		if msg.pending != "" && !msg.carry {
			m.pendingSend[firstNonEmpty(msg.newKey, msg.key)] = msg.pending
		}
		m.notice = fmt.Sprintf("%s moved ⦿%s → ⦿%s but stopped at a prompt in its tab — answer it there and it carries on", msg.label, msg.from, msg.to)
		notify(msg.label+" needs you in its tab", "It moved to ⦿"+msg.to+" and stopped at a startup prompt")
		return m, m.refreshStatuses()
	}
	if msg.untold {
		if msg.pending != "" && !msg.carry {
			m.pendingSend[firstNonEmpty(msg.newKey, msg.key)] = msg.pending
		}
		m.notice = fmt.Sprintf("%s moved ⦿%s → ⦿%s, but telling it to carry on didn't start a turn — trying again", msg.label, msg.from, msg.to)
		return m, m.refreshStatuses()
	}
	switch {
	case msg.auto && msg.from == msg.to:
		m.notice = fmt.Sprintf("%s started again on ⦿%s and is carrying on", msg.label, msg.to)
		notify(msg.label+" is carrying on", "It had stopped while it waited on ⦿"+msg.to+"'s limit")
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
	// whole point, and it should pick the task back up. When the deck has
	// promised that already, the promise sees it through.
	if e := s.Activity.Tail.Err; e.RateLimited() {
		o.submit = continuePrompt(s)
		if c := m.ledger[s.ID].Carry; c != nil && c.Err == e.UUID {
			o.carry = c.Err
		} else {
			o.errUUID = e.UUID
		}
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
			_, err := backend.Prompt(key, text)
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
