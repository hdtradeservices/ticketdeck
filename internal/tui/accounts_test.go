package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// threeAccounts is a live (not dry) deck on a machine with three accounts:
// matt (the deck's own), m0 and support, with usage readings that make
// support the roomiest.
func threeAccounts(t *testing.T) (Model, *recBackend) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{".claude", ".claude-m0", ".claude-support"} {
		p := filepath.Join(home, dir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("TICKETDECK_ACCOUNT", "matt")
	rec := &recBackend{}
	m := New(fakeFetcher{fixture()}, "/repos", false, rec)
	next, _ := m.Update(refreshedMsg{issues: fixture()})
	m = next.(Model)
	now := time.Now()
	m.quotas = map[string]quota.Entry{
		"matt":    {Usage: &quota.Usage{FiveHourPct: 60, SevenDayPct: 40}, FetchedAt: now},
		"m0":      {Usage: &quota.Usage{FiveHourPct: 40, SevenDayPct: 30}, FetchedAt: now},
		"support": {Usage: &quota.Usage{FiveHourPct: 5, SevenDayPct: 10}, FetchedAt: now},
	}
	return m, rec
}

// limitedOn is a session in this deck's workspace that its account's usage
// limit cut off, resetting at resets.
func limitedOn(m Model, key, acct string, resets time.Time) fleet.Session {
	a, _ := account.Find(m.accounts, acct)
	s := fleet.Session{
		Key: key, ID: session.DeterministicID(key), Kind: ledger.Ticket, Account: a.Name, ConfigDir: a.ConfigDir,
		Status: session.Limited, Live: true, Home: true, PaneID: "w1:p5", PID: 77, Cwd: "/repos/etp",
	}
	s.Activity.Tail.Err = &session.APIError{Kind: "rate_limit", LimitType: "five_hour", ResetsAt: resets, UUID: "err-" + key}
	return s
}

func poll(t *testing.T, m Model, sessions ...fleet.Session) (Model, tea.Cmd) {
	t.Helper()
	snap := fleet.Snapshot{Sessions: sessions, At: time.Now()}
	keys := m.sessionKeys()
	visible := map[string]bool{}
	for _, k := range keys {
		visible[k] = true
	}
	st, owners, others := derive(snap, keys, visible)
	next, cmd := m.Update(statusesMsg{statuses: st, owners: owners, snap: &snap, others: others})
	return next.(Model), cmd
}

// The headline: a session cut off by its account's limit moves to the account
// with the most room and is told to carry on — once.
func TestAutoMoveCarriesALimitedSessionOn(t *testing.T) {
	m, rec := threeAccounts(t)
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))
	m, cmd := poll(t, m, s)
	if !m.moving["ZEN-9"] {
		t.Fatal("the cut-off session should be moving")
	}
	drainCmd(cmd)
	if len(rec.moves) != 1 {
		t.Fatalf("moves = %d, want 1", len(rec.moves))
	}
	mv := rec.moves[0]
	if mv.From.Name != "m0" || mv.To.Name != "support" {
		t.Errorf("move %s → %s, want m0 → support (most room)", mv.From.Name, mv.To.Name)
	}
	if mv.Submit != continueAfterMove || mv.PaneID != "w1:p5" || mv.PID != 77 || mv.Cwd != "/repos/etp" {
		t.Errorf("move = %+v", mv)
	}
	if mv.Focus {
		t.Error("an automatic move must not steal focus")
	}
	if e := ledger.Load()[s.ID]; e.Handled != "err-ZEN-9" || e.Account != "support" {
		t.Errorf("ledger = %+v, want the error recorded as handled and the new account", e)
	}

	// The same cut-off seen again — the next poll, or a restarted deck — is
	// not acted on twice.
	m.moving = map[string]bool{}
	m, cmd = poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 1 {
		t.Errorf("the same limit was acted on %d times", len(rec.moves))
	}
	// A new deck on the same machine reads the same ledger.
	fresh := New(fakeFetcher{fixture()}, "/repos", false, rec)
	next, _ := fresh.Update(refreshedMsg{issues: fixture()})
	fresh = next.(Model)
	fresh.quotas = m.quotas
	_, cmd = poll(t, fresh, s)
	drainCmd(cmd)
	if len(rec.moves) != 1 {
		t.Error("a restarted deck acted on a limit the ledger says was handled")
	}
}

// A burst of cut-offs spreads out instead of piling onto one account.
func TestAutoMoveSpreadsABurst(t *testing.T) {
	m, rec := threeAccounts(t)
	// m0 and support both have 80-something percent of room: near enough that
	// the sessions they already run decide.
	m.quotas["m0"] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 8, SevenDayPct: 12}, FetchedAt: time.Now()}
	m.quotas["support"] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 12, SevenDayPct: 15}, FetchedAt: time.Now()}
	reset := time.Now().Add(time.Hour)
	_, cmd := poll(t, m, limitedOn(m, "ZEN-1", "matt", reset), limitedOn(m, "ZEN-2", "matt", reset))
	drainCmd(cmd)
	if len(rec.moves) != 2 {
		t.Fatalf("moves = %d", len(rec.moves))
	}
	if rec.moves[0].To.Name == rec.moves[1].To.Name {
		t.Errorf("both went to ⦿%s; near-equal accounts should share them", rec.moves[0].To.Name)
	}
}

// With nowhere to go the session waits, and says so; once its limit has
// lifted it is told to continue where it is.
func TestAutoMoveWaitsThenContinuesInPlace(t *testing.T) {
	m, rec := threeAccounts(t)
	until := time.Now().Add(time.Hour)
	for _, a := range []string{"matt", "support"} {
		m.quotas[a] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 100, FiveHourReset: until}, FetchedAt: time.Now()}
	}
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))
	m, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Fatalf("moved with nowhere to go: %+v", rec.moves)
	}
	if _, parked := m.parked["ZEN-9"]; !parked || !strings.Contains(m.notice, "continues when the limit lifts") {
		t.Errorf("parked=%v notice=%q", m.parked, m.notice)
	}

	// The limit lifted a few minutes ago: carry on, in place.
	s.Activity.Tail.Err.ResetsAt = time.Now().Add(-5 * time.Minute)
	s.Status = session.Idle
	_, cmd = poll(t, m, s)
	drainCmd(cmd)
	if rec.sent != "ZEN-9:"+continueAfterReset {
		t.Errorf("sent %q, want the continue prompt", rec.sent)
	}
}

// A cut-off from yesterday that someone left alone was left alone on purpose.
func TestAutoMoveLeavesAnOldCutOffAlone(t *testing.T) {
	m, rec := threeAccounts(t)
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(-20*time.Hour))
	s.Status = session.Idle
	_, cmd := poll(t, m, s)
	drainCmd(cmd)
	if rec.sent != "" || len(rec.moves) != 0 {
		t.Errorf("an old cut-off was revived: sent=%q moves=%d", rec.sent, len(rec.moves))
	}
}

func TestAutoMoveRespectsTheSwitchAndTheSessionsState(t *testing.T) {
	m, rec := threeAccounts(t)
	off := false
	m.settings.AutoMove = &off
	_, cmd := poll(t, m, limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour)))
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Error("auto-move off, but it moved")
	}

	m, rec = threeAccounts(t)
	working := limitedOn(m, "ZEN-1", "m0", time.Now().Add(time.Hour))
	working.Status = session.Working // someone typed into it again
	elsewhere := limitedOn(m, "ZEN-2", "m0", time.Now().Add(time.Hour))
	elsewhere.Home, elsewhere.Socket = false, "/peer.sock"
	_, cmd = poll(t, m, working, elsewhere)
	drainCmd(cmd)
	if len(rec.moves) != 0 {
		t.Errorf("moved a working session or one in another deck: %+v", rec.moves)
	}
}

// M brings sessions in from the old per-account decks: the idle ones now, on
// the account they already run on; the busy ones once their turn ends.
func TestBringSessionsInFromOtherDecks(t *testing.T) {
	m, rec := threeAccounts(t)
	idle := fleet.Session{Key: "scratch-2", ID: "s-2", Kind: ledger.Scratch, Account: "m0", ConfigDir: m.accounts[0].ConfigDir, Status: session.Idle, Live: true, Socket: "/m0.sock", Workspace: "m0", PaneID: "w1:p2", PID: 9}
	for _, a := range m.accounts {
		if a.Name == "m0" {
			idle.ConfigDir = a.ConfigDir
		}
	}
	busy := limitedOn(m, "ZEN-1", "support", time.Time{})
	busy.Status, busy.Home, busy.Socket, busy.Workspace = session.Working, false, "/s.sock", "support"
	busy.Activity.Tail.Err = nil
	home := fleet.Session{Key: "scratch-2", ID: "s-9", Kind: ledger.Scratch, Account: "matt", Status: session.Idle, Live: true, Home: true, PaneID: "w1:p9"}
	m, _ = poll(t, m, idle, busy, home)

	next, _ := m.Update(runes("M"))
	m = next.(Model)
	if !m.bringMenu || !strings.Contains(m.View(), "in the m0 deck") {
		t.Fatalf("M should list the sessions in other decks:\n%s", m.View())
	}
	next, cmd := m.Update(runes("a"))
	m = next.(Model)
	drainCmd(cmd)
	if len(rec.moves) != 1 {
		t.Fatalf("moves = %d, want only the idle one now", len(rec.moves))
	}
	mv := rec.moves[0]
	if mv.From.Name != "m0" || mv.To.Name != "m0" || mv.Socket != "/m0.sock" || mv.PaneID != "w1:p2" {
		t.Errorf("adoption = %+v, want m0 → m0 out of /m0.sock", mv)
	}
	if mv.NewName == "scratch-2" || mv.NewName == "" {
		t.Errorf("its name is taken here, got NewName %q", mv.NewName)
	}
	if !m.adoptQueue["ZEN-1"] {
		t.Fatal("the busy one should be queued")
	}

	// Its turn ends: in it comes.
	busy.Status = session.Idle
	m.moving = map[string]bool{}
	_, cmd = poll(t, m, busy)
	drainCmd(cmd)
	if len(rec.moves) != 2 || rec.moves[1].Name != "ZEN-1" {
		t.Errorf("the queued session should come in once idle, moves = %+v", rec.moves)
	}
}

// A stopped session whose account is out resumes somewhere with room, its
// conversation copied over first.
func TestOpeningASessionStrandedOnASpentAccountMovesIt(t *testing.T) {
	m, _ := threeAccounts(t)
	m.dry = true
	m.quotas["m0"] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 100, FiveHourReset: time.Now().Add(time.Hour)}, FetchedAt: time.Now()}
	var m0 account.Account
	for _, a := range m.accounts {
		if a.Name == "m0" {
			m0 = a
		}
	}
	stopped := fleet.Session{Key: "ZEN-9", ID: session.DeterministicID("ZEN-9"), Kind: ledger.Ticket, Account: "m0", ConfigDir: m0.ConfigDir, Status: session.Stopped, LastActive: time.Now()}
	m, _ = poll(t, m, stopped)

	is, _ := m.issueByKey("ZEN-9")
	next, _ := m.launchIssueNow(is)
	n := next.(Model).notice
	if !strings.Contains(n, "--resume "+session.DeterministicID("ZEN-9")) || !strings.Contains(n, "⦿support") {
		t.Errorf("should resume on support, notice = %q", n)
	}

	// The same on a live run: the conversation is copied onto support before
	// the launch resumes it there.
	id := session.DeterministicID("ZEN-9")
	src := session.TranscriptPathIn(m0.ConfigDir, id, "/repos")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.dry = false
	m.hideOpenHint = true
	rec := &specRec{}
	m.backend = rec
	_, cmd := m.launchIssueNow(is)
	drainCmd(cmd)
	if rec.spec.CopyFrom != m0.ConfigDir || rec.spec.Account != "support" {
		t.Errorf("spec = %+v, want a copy from m0 onto support", rec.spec)
	}
	var support account.Account
	for _, a := range m.accounts {
		if a.Name == "support" {
			support = a
		}
	}
	if session.FindTranscript(support.ConfigDir, id) == "" {
		t.Error("the conversation should be on support before the launch runs")
	}
}

// specRec records the spec a launch ran.
type specRec struct {
	recBackend
	spec session.LaunchSpec
}

func (r *specRec) RunDetached(spec session.LaunchSpec) (string, error) {
	r.spec = spec
	return "", nil
}

// n names a new ad-hoc session, and the deck remembers it — under its own id,
// on the account it opened on.
func TestNewSessionIsNamedAndRemembered(t *testing.T) {
	m, _ := threeAccounts(t)
	m.hideOpenHint = true
	next, _ := m.Update(runes("n"))
	m = next.(Model)
	for _, r := range "Deploy notes" {
		next, _ = m.Update(runes(string(r)))
		m = next.(Model)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter should start the session")
	}
	var found ledger.Entry
	for _, e := range ledger.Load() {
		if e.Kind == ledger.Scratch {
			found = e
		}
	}
	if found.Name != "Deploy notes" || found.Account != "support" || found.SessionID == "" {
		t.Errorf("ledger entry = %+v, want Deploy notes on support (most room)", found)
	}
	if !strings.Contains(m.notice, "Deploy notes") || !strings.Contains(m.notice, "⦿support") {
		t.Errorf("notice = %q", m.notice)
	}
}

// R renames an ad-hoc session: the deck's name, its herdr agent and tab, and
// Claude's own title.
func TestRenameASession(t *testing.T) {
	m, rec := threeAccounts(t)
	s := fleet.Session{Key: "scratch-1", ID: "sid-1", Kind: ledger.Scratch, Account: "matt", Status: session.Idle, Live: true, Home: true, PaneID: "w1:p4"}
	m, _ = poll(t, m, s)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	m = next.(Model)
	if sr, ok := m.selectedSession(); !ok || sr.Name != "scratch-1" {
		t.Fatalf("cursor should be on the ad-hoc session, got %+v", sr)
	}
	next, _ = m.Update(runes("R"))
	m = next.(Model)
	if m.prompt == nil || m.prompt.text != "scratch-1" {
		t.Fatalf("R should open the rename prompt prefilled, got %+v", m.prompt)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = next.(Model)
	for _, r := range "Flaky test" {
		next, _ = m.Update(runes(string(r)))
		m = next.(Model)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainCmd(cmd)
	if e := ledger.Load()["sid-1"]; e.Name != "Flaky test" || e.Key != "flaky-test" {
		t.Errorf("ledger = %+v", e)
	}
	if rec.sent != "flaky-test:/rename Flaky test" {
		t.Errorf("sent %q, want Claude's /rename", rec.sent)
	}
	_ = next
}

// The info line says where ⏎ would open a ticket with no session, before it
// is pressed.
func TestInfoLineShowsWhereANewSessionWouldOpen(t *testing.T) {
	m, _ := threeAccounts(t)
	if line := m.infoLine(); !strings.Contains(line, "⏎ opens ZEN-9 on") || !strings.Contains(line, "⦿ support") {
		t.Errorf("info line = %q", line)
	}
}

// A poll still running is not stacked on by the timer.
func TestStatusTickDoesNotStackPolls(t *testing.T) {
	m, _ := threeAccounts(t)
	next, _ := m.Update(statusTickMsg{})
	m = next.(Model)
	if !m.pollBusy {
		t.Fatal("the first tick should start a poll")
	}
	started := m.pollStarted
	next, _ = m.Update(statusTickMsg{})
	if !next.(Model).pollStarted.Equal(started) {
		t.Error("a tick during a poll started another one")
	}
	// The reply clears the way for the next tick.
	next, _ = next.(Model).Update(statusesMsg{statuses: map[string]session.Status{}})
	if next.(Model).pollBusy {
		t.Error("the poll's reply should clear the busy flag")
	}
	// A reply that never came can't stall polling for good.
	m.pollStarted = time.Now().Add(-time.Minute)
	next, _ = m.Update(statusTickMsg{})
	if next.(Model).pollStarted.Equal(m.pollStarted) {
		t.Error("a poll lost for a minute should be retried")
	}
}

// A session whose account's login lapsed is stuck there for good; it moves and
// carries on like one cut off by a limit.
func TestAutoMoveCarriesOnPastALapsedLogin(t *testing.T) {
	m, rec := threeAccounts(t)
	s := limitedOn(m, "ZEN-9", "m0", time.Time{})
	s.Status = session.Errored
	s.Activity.Tail.Err = &session.APIError{Kind: "authentication_failed", UUID: "auth-1", Text: "Login expired · Please run /login"}
	_, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 1 || rec.moves[0].To.Name != "support" || rec.moves[0].Submit != continueAfterLogin {
		t.Errorf("moves = %+v, want one to support told why", rec.moves)
	}
}

// A moved session that stopped at a startup prompt is told to carry on once
// someone answers the prompt — not before, when the text would land in it.
func TestAMoveHeldAtAPromptCarriesOnOnceAnswered(t *testing.T) {
	m, rec := threeAccounts(t)
	next, _ := m.Update(moveDoneMsg{key: "ZEN-9", label: "ZEN-9", from: "m0", to: "support", auto: true, blocked: true, pending: continueAfterMove})
	m = next.(Model)
	if !strings.Contains(m.notice, "stopped at a prompt") || m.pendingSend["ZEN-9"] == "" {
		t.Fatalf("notice=%q pending=%v", m.notice, m.pendingSend)
	}
	held := fleet.Session{Key: "ZEN-9", ID: session.DeterministicID("ZEN-9"), Kind: ledger.Ticket, Account: "support", Status: session.NeedsInput, Live: true, Home: true, PaneID: "w1:p3"}
	m, cmd := poll(t, m, held)
	drainCmd(cmd)
	if rec.sent != "" {
		t.Fatalf("sent %q into a session still at its prompt", rec.sent)
	}
	held.Status = session.Idle
	_, cmd = poll(t, m, held)
	drainCmd(cmd)
	if rec.sent != "ZEN-9:"+continueAfterMove {
		t.Errorf("sent %q, want the continue prompt once it is idle", rec.sent)
	}
}

// Claude's what-now menu after a limit is a prompt herdr won't type into, and
// typing into it would answer it. It is dismissed before the session is told
// to carry on.
func TestContinuingDismissesTheLimitMenuFirst(t *testing.T) {
	m, rec := threeAccounts(t)
	for _, a := range []string{"matt", "support"} {
		m.quotas[a] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 100, FiveHourReset: time.Now().Add(time.Hour)}, FetchedAt: time.Now()}
	}
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(-5*time.Minute))
	s.Status, s.Prompting = session.NeedsInput, true
	_, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.keys) != 1 || rec.keys[0] != "ZEN-9:esc" {
		t.Fatalf("keys = %v, want the menu dismissed", rec.keys)
	}
	if rec.sent != "ZEN-9:"+continueAfterReset {
		t.Errorf("sent %q", rec.sent)
	}
}

// A session sitting on the limit menu moves like one that went idle.
func TestAutoMoveTakesASessionOffTheLimitMenu(t *testing.T) {
	m, rec := threeAccounts(t)
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(time.Hour))
	s.Status, s.Prompting = session.NeedsInput, true
	_, cmd := poll(t, m, s)
	drainCmd(cmd)
	if len(rec.moves) != 1 || rec.moves[0].To.Name != "support" {
		t.Errorf("moves = %+v", rec.moves)
	}
}

// A move to an account nobody is signed in to would strand the session at its
// first message; the overlay refuses it and says how to log the account in.
func TestMoveRefusesASignedOutAccount(t *testing.T) {
	m, rec := threeAccounts(t)
	m.quotas["support"] = quota.Entry{Reason: "not signed in"}
	m = liveOn(m, "ZEN-9", "matt", session.Idle)
	next, _ := m.Update(runes("H"))
	m = next.(Model)
	if m.handoffCands[m.handoffCursor].Name == "support" {
		t.Fatal("the cursor should start on an account that can take work")
	}
	for i, a := range m.handoffCands {
		if a.Name == "support" {
			m.handoffCursor = i
		}
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	drainCmd(cmd)
	if len(rec.moves) != 0 || !strings.Contains(m.notice, "claude /login") {
		t.Errorf("moves=%d notice=%q, want a refusal that says how to log in", len(rec.moves), m.notice)
	}
}

// A second ⏎ while the first launch is still starting doesn't start it twice.
func TestASecondEnterWaitsForTheFirstLaunch(t *testing.T) {
	m, _ := threeAccounts(t)
	m.hideOpenHint = true
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil || !m.launching["ZEN-9"] {
		t.Fatalf("first ⏎ should launch, launching=%v", m.launching)
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil || !strings.Contains(m.notice, "still opening") {
		t.Errorf("second ⏎ should wait: cmd=%v notice=%q", cmd != nil, m.notice)
	}
	next, _ = m.Update(detachedDoneMsg{action: "start ZEN-9", key: "ZEN-9"})
	if next.(Model).launching["ZEN-9"] {
		t.Error("the launch finishing should clear the mark")
	}
}

// A session brought in under a new name must not leave its old name marked as
// moving: the next session to take that name would be stuck behind the mark.
func TestAMoveUnderANewNameClearsTheOldMark(t *testing.T) {
	m, _ := threeAccounts(t)
	m.moving["scratch-1"] = true
	next, _ := m.Update(moveDoneMsg{key: "scratch-1", newKey: "scratch-1-2", label: "notes", from: "m0", to: "m0", adopt: true})
	if next.(Model).moving["scratch-1"] {
		t.Error("the old name is still marked as moving")
	}
}

// Every line of the frame fits the terminal: one that wraps grows the frame
// past the screen and pushes the title off the top.
func TestNoLineWrapsANarrowTerminal(t *testing.T) {
	m, _ := threeAccounts(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = next.(Model)
	for i, line := range strings.Split(m.View(), "\n") {
		if w := runewidth.StringWidth(ansiRe.ReplaceAllString(line, "")); w > 60 {
			t.Errorf("line %d is %d columns wide in a 60-column terminal: %q", i, w, ansiRe.ReplaceAllString(line, ""))
		}
	}
}

// A finished turn you have looked at stays seen, though herdr — which only
// learns of a look from an explicit focus — keeps calling it unread.
func TestAFinishedTurnOnceSeenStaysSeen(t *testing.T) {
	m, _ := threeAccounts(t)
	s := fleet.Session{Key: "ZEN-9", ID: session.DeterministicID("ZEN-9"), Kind: ledger.Ticket, Account: "matt", Status: session.Working, Live: true, Home: true, PaneID: "w1:p3"}
	m, _ = poll(t, m, s)
	s.Status = session.Unread
	m, _ = poll(t, m, s)
	if m.sessions["ZEN-9"] != session.Unread {
		t.Fatalf("a freshly finished turn should read as unread, got %v", m.sessions["ZEN-9"])
	}
	s.Focused = true // the user switched to its tab
	m, _ = poll(t, m, s)
	s.Focused = false // and back to the deck; herdr still says done
	m, _ = poll(t, m, s)
	if m.sessions["ZEN-9"] != session.Idle {
		t.Errorf("a turn the user has seen should read idle, got %v", m.sessions["ZEN-9"])
	}
	// The next turn it finishes is new again.
	s.Status = session.Working
	m, _ = poll(t, m, s)
	time.Sleep(time.Millisecond)
	s.Status = session.Unread
	m, _ = poll(t, m, s)
	if m.sessions["ZEN-9"] != session.Unread {
		t.Errorf("a new finished turn should read unread again, got %v", m.sessions["ZEN-9"])
	}
}

// A turn that started and finished between two polls is still new.
func TestAQuickTurnBetweenPollsIsStillUnread(t *testing.T) {
	m, _ := threeAccounts(t)
	s := fleet.Session{Key: "ZEN-9", ID: session.DeterministicID("ZEN-9"), Kind: ledger.Ticket, Account: "matt", Status: session.Idle, Live: true, Home: true, PaneID: "w1:p3", Focused: true}
	m, _ = poll(t, m, s)
	s.Focused = false
	m, _ = poll(t, m, s)
	time.Sleep(time.Millisecond)
	s.Status = session.Unread // went idle → working → done between polls
	m, _ = poll(t, m, s)
	if m.sessions["ZEN-9"] != session.Unread {
		t.Errorf("got %v, want Unread", m.sessions["ZEN-9"])
	}
}

// ] finds the next session waiting on you, unfolding what hides it.
func TestJumpToTheNextSessionThatNeedsYou(t *testing.T) {
	m, _ := threeAccounts(t)
	// ZEN-5 sits in the Low section, which starts folded.
	m.collapsed["Low"] = true
	m.regroup()
	m.sessions = map[string]session.Status{"ZEN-5": session.NeedsInput, "ZEN-1": session.Working}
	if !strings.Contains(m.View(), "◆ 1 need you") {
		t.Errorf("the title bar should count what needs you:\n%s", m.View())
	}
	next, _ := m.Update(runes("]"))
	m = next.(Model)
	if id, _ := m.selectedID(); id != "ZEN-5" {
		t.Errorf("] landed on %q, want ZEN-5", id)
	}
	m.sessions = map[string]session.Status{}
	next, _ = m.Update(runes("]"))
	if n := next.(Model).notice; !strings.Contains(n, "nothing is waiting") {
		t.Errorf("notice = %q", n)
	}
}

// A session running in a plain terminal can't be stopped from here; moving it
// would start a second copy of its conversation.
func TestMoveRefusesASessionRunningInATerminal(t *testing.T) {
	m, rec := threeAccounts(t)
	m.fleet.Sessions = append(m.fleet.Sessions, fleet.Session{Key: "ZEN-9", ID: session.DeterministicID("ZEN-9"), Kind: ledger.Ticket, Account: "matt", Status: session.Working, Live: true, PID: 99, Workspace: "a terminal"})
	m.owners["ZEN-9"] = account.Owner{Name: "matt", Status: session.Working, Live: true, Workspace: "a terminal"}
	next, _ := m.Update(runes("H"))
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainCmd(cmd)
	if len(rec.moves) != 0 || !strings.Contains(next.(Model).notice, "terminal") {
		t.Errorf("moves=%d notice=%q", len(rec.moves), next.(Model).notice)
	}
	if _, err := (HerdBackend{}).Move(Move{PID: 99}); err != errRunsInATerminal {
		t.Errorf("the backend must refuse it too, got %v", err)
	}
}

// A continue that failed is tried again a minute later, not dropped.
func TestAFailedContinueIsRetried(t *testing.T) {
	m, rec := threeAccounts(t)
	for _, a := range []string{"matt", "support"} {
		m.quotas[a] = quota.Entry{Usage: &quota.Usage{FiveHourPct: 100, FiveHourReset: time.Now().Add(time.Hour)}, FetchedAt: time.Now()}
	}
	s := limitedOn(m, "ZEN-9", "m0", time.Now().Add(-5*time.Minute))
	s.Status = session.Idle
	m, _ = poll(t, m, s)
	if !m.handled[s.Activity.Tail.Err.UUID] {
		t.Fatal("the reset should have been acted on")
	}
	next, _ := m.Update(continuedMsg{key: "ZEN-9", err: errors.New("agent_prompt_stalled"), errUUID: s.Activity.Tail.Err.UUID})
	m = next.(Model)
	if m.handled[s.Activity.Tail.Err.UUID] {
		t.Fatal("a failed continue must not count as handled")
	}
	rec.sent = ""
	m, cmd := poll(t, m, s)
	drainCmd(cmd)
	if rec.sent != "" {
		t.Error("retried before the wait was up")
	}
	m.retryAt[s.Activity.Tail.Err.UUID] = time.Now().Add(-time.Second)
	_, cmd = poll(t, m, s)
	drainCmd(cmd)
	if rec.sent != "ZEN-9:"+continueAfterReset {
		t.Errorf("sent %q after the wait, want the continue again", rec.sent)
	}
}
