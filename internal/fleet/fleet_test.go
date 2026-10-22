package fleet

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// world stubs everything Poll reaches outside the process. Every test sets
// one, or Poll would talk to the real herdr and read the real registries on the
// machine running the tests.
type world struct {
	agents map[string][]herd.Agent // socket ("" = own) → agents
	errs   map[string]error
	peers  []herd.Workspace
	procs  map[string][]session.Proc // config dir → live processes
}

func (w world) install(t *testing.T) {
	t.Helper()
	pl, pp, po, pr := listAgents, peers, ownSocket, registry
	listAgents = func(sock string) ([]herd.Agent, error) {
		if err := w.errs[sock]; err != nil {
			return nil, err
		}
		out := append([]herd.Agent(nil), w.agents[sock]...)
		for i := range out {
			out[i].Socket = sock
		}
		return out, nil
	}
	peers = func() []herd.Workspace { return w.peers }
	ownSocket = func() string { return "/h/.config/herdr/herdr.sock" }
	registry = func(dir string) []session.Proc { return w.procs[dir] }
	t.Cleanup(func() { listAgents, peers, ownSocket, registry = pl, pp, po, pr })
}

type accts struct {
	home string
	all  []account.Account
}

func (a accts) dir(name string) string {
	for _, x := range a.all {
		if x.Name == name {
			return x.ConfigDir
		}
	}
	return ""
}

func (a accts) acct(name string) account.Account {
	x, _ := account.Find(a.all, name)
	return x
}

func setup(t *testing.T, names ...string) accts {
	t.Helper()
	home := t.TempDir()
	var out []account.Account
	for i, n := range names {
		dir := filepath.Join(home, ".claude-"+n)
		if i == 0 {
			dir = filepath.Join(home, ".claude")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, account.Account{Name: n, ConfigDir: dir})
	}
	return accts{home: home, all: out}
}

// stamp writes a transcript for id under an account with an explicit mtime,
// which is how a poll decides who wrote last.
func stamp(t *testing.T, configDir, id string, mod time.Time, lines ...string) {
	t.Helper()
	p := filepath.Join(configDir, "projects", "-home-x-Repos", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(append([]string{"{}"}, lines...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	// The directory's mtime moved with the new file, which is what invalidates
	// the listing cache in real use; tests reuse names, so drop it outright.
	listMu.Lock()
	delete(listings, filepath.Dir(p))
	listMu.Unlock()
}

func input(a accts, keys ...string) Input {
	return Input{Accounts: a.all, Home: a.all[0], Keys: keys, Herdr: true, Peers: true, Cwd: "/home/x/Repos"}
}

func best(t *testing.T, s Snapshot, key string) Session {
	t.Helper()
	b, ok := s.Best(key)
	if !ok {
		t.Fatalf("no session for %s in %+v", key, s.Sessions)
	}
	return b
}

// A session nobody is running is attributed from disk alone.
func TestATranscriptOnDiskIsResumableWhereItLives(t *testing.T) {
	a := setup(t, "matt", "support")
	world{}.install(t)
	stamp(t, a.dir("support"), session.DeterministicID("ZEN-1"), time.Now())

	snap := Poll(input(a, "ZEN-1", "ZEN-2"))
	s := best(t, snap, "ZEN-1")
	if s.Account != "support" || s.Live || s.Status != session.Stopped {
		t.Errorf("ZEN-1 = %+v, want resumable on support", s)
	}
	if _, ok := snap.Best("ZEN-2"); ok {
		t.Error("ZEN-2 has no session anywhere")
	}
}

// A hand-off leaves the original transcript behind; whoever wrote last is the
// account working on it now.
func TestTheAccountThatWroteLastOwnsIt(t *testing.T) {
	a := setup(t, "matt", "support")
	world{}.install(t)
	id := session.DeterministicID("ZEN-1")
	stamp(t, a.dir("matt"), id, time.Now().Add(-2*time.Hour))
	stamp(t, a.dir("support"), id, time.Now())
	if s := best(t, Poll(input(a, "ZEN-1")), "ZEN-1"); s.Account != "support" {
		t.Errorf("owner = %s, want support (it wrote last)", s.Account)
	}
}

// A live session outranks a newer transcript: the transcript says who touched
// it last, the workspace says who is running it right now.
func TestALiveSessionOutranksANewerTranscript(t *testing.T) {
	a := setup(t, "matt", "support")
	peer := herd.Workspace{Socket: "/h/.config/herdr/sessions/support/herdr.sock", Session: "support"}
	world{
		peers:  []herd.Workspace{peer},
		agents: map[string][]herd.Agent{peer.Socket: {{Name: "ZEN-1", AgentStatus: "working", PaneID: "w1:p2"}}},
	}.install(t)
	id := session.DeterministicID("ZEN-1")
	stamp(t, a.dir("matt"), id, time.Now())
	stamp(t, a.dir("support"), id, time.Now().Add(-time.Hour))

	s := best(t, Poll(input(a, "ZEN-1")), "ZEN-1")
	if !s.Live || s.Home || s.Status != session.Working {
		t.Fatalf("ZEN-1 = %+v, want a live session in another workspace", s)
	}
	// Nothing tied the pane to a process, so the account comes from where its
	// transcript is newest — the stub it writes the moment it starts.
	if s.Account != "matt" {
		t.Errorf("account = %s, want matt (newest transcript)", s.Account)
	}
	if s.Workspace != "support" {
		t.Errorf("workspace = %q, want support", s.Workspace)
	}
}

// The registry is what says which account a pane really runs: one deck now
// runs every account, so the workspace a pane sits in proves nothing.
func TestTheRegistryNamesAPanesAccount(t *testing.T) {
	a := setup(t, "matt", "m0")
	own := "/h/.config/herdr/herdr.sock"
	world{
		agents: map[string][]herd.Agent{"": {{Name: "ZEN-4", AgentStatus: "idle", PaneID: "w1:p5", TabID: "w1:t5"}}},
		procs: map[string][]session.Proc{a.dir("m0"): {{
			PID: 42, SessionID: session.DeterministicID("ZEN-4"), ConfigDir: a.dir("m0"),
			Status: "waiting", WaitingFor: "permission", HerdrSocket: own, HerdrPane: "w1:p5",
		}}},
	}.install(t)

	s := best(t, Poll(input(a, "ZEN-4")), "ZEN-4")
	if s.Account != "m0" || !s.Home || s.PID != 42 || s.TabID != "w1:t5" {
		t.Errorf("ZEN-4 = %+v, want home on m0", s)
	}
	if s.Status != session.NeedsInput || s.WaitingFor != "permission" {
		t.Errorf("status = %v (%q): Claude's own 'waiting' outranks herdr's idle", s.Status, s.WaitingFor)
	}
}

// An unreachable peer degrades to the disk answer.
func TestAnUnreachablePeerFallsBackToDisk(t *testing.T) {
	a := setup(t, "matt", "support")
	peer := herd.Workspace{Socket: "/p.sock", Session: "support"}
	world{peers: []herd.Workspace{peer}, errs: map[string]error{"/p.sock": os.ErrDeadlineExceeded}}.install(t)
	stamp(t, a.dir("support"), session.DeterministicID("ZEN-1"), time.Now())

	s := best(t, Poll(input(a, "ZEN-1")), "ZEN-1")
	if s.Account != "support" || s.Live || s.Status != session.Stopped {
		t.Errorf("ZEN-1 = %+v, want resumable on support from disk", s)
	}
}

// A session parked on its account's usage limit reads "limited", and the
// account is out until the limit lifts — even with no usage reading at all.
func TestARateLimitedSessionAndItsAccount(t *testing.T) {
	a := setup(t, "matt", "m0")
	id := session.DeterministicID("ZEN-8")
	resets := time.Now().Add(40 * time.Minute).Truncate(time.Second)
	limit := `{"type":"assistant","timestamp":"2026-10-21T14:05:00Z","uuid":"e1","isApiErrorMessage":true,"error":"rate_limit","quotaLimits":{"resetsAt":` + strconv.FormatInt(resets.Unix(), 10) + `,"rateLimitType":"five_hour"},"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit"}]}}`
	stamp(t, a.dir("m0"), id, time.Now(), limit)
	world{
		agents: map[string][]herd.Agent{"": {{Name: "ZEN-8", AgentStatus: "idle", PaneID: "w1:p2"}}},
		procs:  map[string][]session.Proc{a.dir("m0"): {{PID: 7, SessionID: id, ConfigDir: a.dir("m0"), HerdrSocket: "/h/.config/herdr/herdr.sock", HerdrPane: "w1:p2"}}},
	}.install(t)

	snap := Poll(input(a, "ZEN-8"))
	if s := best(t, snap, "ZEN-8"); s.Status != session.Limited {
		t.Errorf("status = %v, want Limited", s.Status)
	}
	until, _ := snap.Limits(time.Now())
	if !until["m0"].Equal(resets) {
		t.Errorf("m0 limited until %v, want %v", until["m0"], resets)
	}
	if n := snap.LiveOn()["m0"]; n != 1 {
		t.Errorf("live on m0 = %d", n)
	}
}

// herdr's "done" is a finished turn nobody has looked at; on the pane the user
// is looking at right now it is just idle.
func TestUnreadTurns(t *testing.T) {
	a := setup(t, "matt")
	world{agents: map[string][]herd.Agent{"": {
		{Name: "ZEN-1", AgentStatus: "done", PaneID: "w1:p1"},
		{Name: "ZEN-2", AgentStatus: "done", PaneID: "w1:p2", Focused: true},
	}}}.install(t)
	snap := Poll(input(a, "ZEN-1", "ZEN-2"))
	if s := best(t, snap, "ZEN-1"); s.Status != session.Unread {
		t.Errorf("ZEN-1 = %v, want Unread", s.Status)
	}
	if s := best(t, snap, "ZEN-2"); s.Status != session.Idle {
		t.Errorf("ZEN-2 = %v, want Idle (it is on screen)", s.Status)
	}
}

// An ad-hoc session the deck started survives a restart: the ledger remembers
// its id, name and account, and its transcript makes it resumable.
func TestARememberedAdHocSessionIsResumable(t *testing.T) {
	a := setup(t, "matt", "m0")
	world{}.install(t)
	id := "11111111-2222-4333-8444-555555555555"
	stamp(t, a.dir("m0"), id, time.Now())
	led := map[string]ledger.Entry{id: {SessionID: id, Key: "deploy-notes", Kind: ledger.Scratch, Name: "Deploy notes", ConfigDir: a.dir("m0"), Account: "m0", Cwd: "/r", Updated: time.Now()}}
	in := input(a)
	in.Ledger = led

	s := best(t, Poll(in), "deploy-notes")
	if s.Status != session.Stopped || s.Account != "m0" || s.Name != "Deploy notes" || s.Kind != ledger.Scratch || s.Cwd != "/r" {
		t.Errorf("session = %+v", s)
	}
}

// A live ad-hoc session is found by its pane, named by what Claude titled it.
func TestALiveAdHocSessionIsNamedFromItsTranscript(t *testing.T) {
	a := setup(t, "matt", "m0")
	id := "22222222-2222-4333-8444-555555555555"
	stamp(t, a.dir("m0"), id, time.Now(), `{"type":"ai-title","aiTitle":"bump the go toolchain"}`)
	world{
		agents: map[string][]herd.Agent{"": {{Name: "scratch-1", AgentStatus: "idle", PaneID: "w1:p3", Title: "scratch-1"}}},
		procs:  map[string][]session.Proc{a.dir("m0"): {{PID: 9, SessionID: id, ConfigDir: a.dir("m0"), HerdrSocket: "/h/.config/herdr/herdr.sock", HerdrPane: "w1:p3"}}},
	}.install(t)
	s := best(t, Poll(input(a)), "scratch-1")
	if s.Name != "bump the go toolchain" || s.Kind != ledger.Scratch || s.ID != id {
		t.Errorf("session = %+v", s)
	}
}

// A ticket resumed by hand in a plain terminal is still running: the deck must
// see it, or opening the ticket would fork it.
func TestASessionOutsideHerdrStillCounts(t *testing.T) {
	a := setup(t, "matt", "m0")
	id := session.DeterministicID("ZEN-3")
	world{procs: map[string][]session.Proc{a.dir("m0"): {{PID: 5, SessionID: id, ConfigDir: a.dir("m0"), Status: "busy"}}}}.install(t)
	s := best(t, Poll(input(a, "ZEN-3")), "ZEN-3")
	if !s.Live || s.Home || s.Account != "m0" || s.Status != session.Working {
		t.Errorf("ZEN-3 = %+v, want a live session elsewhere on m0", s)
	}
}

// A peer workspace's pane with no registry entry and no transcript is
// attributed to the account that old deck ran as.
func TestAPeerWorkspaceNamesItsAccount(t *testing.T) {
	a := setup(t, "matt", "support")
	peer := herd.Workspace{Socket: "/s.sock", Session: "support"}
	world{peers: []herd.Workspace{peer}, agents: map[string][]herd.Agent{"/s.sock": {{Name: "scratch-2", AgentStatus: "idle", PaneID: "w1:p1"}}}}.install(t)
	s := best(t, Poll(input(a)), "scratch-2")
	if s.Account != "support" || s.Workspace != "support" {
		t.Errorf("session = %+v", s)
	}
}

// After a move the copied transcript still ends on the old account's
// rate-limit error. Once the ledger says a move answered it, it says nothing
// about the account the session runs on now.
func TestAnErrorAMoveAnsweredIsTheOldAccounts(t *testing.T) {
	a := setup(t, "matt", "m0")
	id := session.DeterministicID("ZEN-8")
	limit := `{"type":"assistant","timestamp":"2026-10-21T14:05:00Z","uuid":"e1","isApiErrorMessage":true,"error":"rate_limit","quotaLimits":{"resetsAt":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `,"rateLimitType":"five_hour"},"message":{"model":"<synthetic>","content":[{"type":"text","text":"limit"}]}}`
	stamp(t, a.dir("matt"), id, time.Now(), limit)
	world{
		agents: map[string][]herd.Agent{"": {{Name: "ZEN-8", AgentStatus: "idle", PaneID: "w1:p2"}}},
		procs:  map[string][]session.Proc{a.dir("matt"): {{PID: 7, SessionID: id, ConfigDir: a.dir("matt"), HerdrSocket: "/h/.config/herdr/herdr.sock", HerdrPane: "w1:p2"}}},
	}.install(t)
	in := input(a, "ZEN-8")
	in.Ledger = map[string]ledger.Entry{id: {SessionID: id, Handled: "e1", Account: "matt"}}
	snap := Poll(in)
	if s := best(t, snap, "ZEN-8"); s.Status != session.Idle {
		t.Errorf("status = %v, want Idle: the limit was m0's", s.Status)
	}
	if until, _ := snap.Limits(time.Now()); !until["matt"].IsZero() {
		t.Errorf("matt marked out until %v by m0's limit", until["matt"])
	}
}

// A limit puts up Claude's what-now menu, which herdr reads as a prompt. The
// session is cut off all the same, and the deck has to know a dialog is up
// before it types anything there.
func TestALimitMenuReadsAsLimited(t *testing.T) {
	a := setup(t, "matt", "m0")
	id := session.DeterministicID("ZEN-8")
	limit := `{"type":"assistant","timestamp":"2026-10-21T14:05:00Z","uuid":"e1","isApiErrorMessage":true,"error":"rate_limit","quotaLimits":{"resetsAt":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `,"rateLimitType":"seven_day"},"message":{"model":"<synthetic>","content":[{"type":"text","text":"limit"}]}}`
	stamp(t, a.dir("m0"), id, time.Now(), limit)
	world{
		agents: map[string][]herd.Agent{"": {{Name: "ZEN-8", AgentStatus: "blocked", PaneID: "w1:p2"}}},
		procs:  map[string][]session.Proc{a.dir("m0"): {{PID: 7, SessionID: id, ConfigDir: a.dir("m0"), HerdrSocket: "/h/.config/herdr/herdr.sock", HerdrPane: "w1:p2"}}},
	}.install(t)
	s := best(t, Poll(input(a, "ZEN-8")), "ZEN-8")
	if s.Status != session.Limited || !s.Prompting {
		t.Errorf("status = %v prompting = %v, want Limited with the menu up", s.Status, s.Prompting)
	}
}

// A session herdr already listed is not listed again from the registry when
// the two couldn't be tied together (no /proc on macOS, no herdr hook).
func TestASessionIsListedOnce(t *testing.T) {
	a := setup(t, "matt", "m0")
	id := session.DeterministicID("ZEN-4")
	world{
		agents: map[string][]herd.Agent{"": {{Name: "ZEN-4", AgentStatus: "idle", PaneID: "w1:p5"}}},
		procs:  map[string][]session.Proc{a.dir("m0"): {{PID: 42, SessionID: id, ConfigDir: a.dir("m0")}}}, // no pane in its env
	}.install(t)
	snap := Poll(input(a, "ZEN-4"))
	n := 0
	for _, s := range snap.Sessions {
		if s.Key == "ZEN-4" {
			n++
		}
	}
	if n != 1 || snap.LiveOn()["m0"]+snap.LiveOn()["matt"] != 1 {
		t.Errorf("ZEN-4 listed %d times: %+v", n, snap.Sessions)
	}
}

// Without herdr, sessions run in terminals and are the deck's own to attach
// to — Claude's background sessions included.
func TestWithoutHerdrSessionsAreHome(t *testing.T) {
	a := setup(t, "matt")
	id := session.DeterministicID("ZEN-6")
	world{procs: map[string][]session.Proc{a.dir("matt"): {{PID: 8, SessionID: id, ConfigDir: a.dir("matt"), Kind: "background", Status: "busy"}}}}.install(t)
	in := input(a, "ZEN-6")
	in.Herdr = false
	s := best(t, Poll(in), "ZEN-6")
	if !s.Live || !s.Home || s.Status != session.Working {
		t.Errorf("ZEN-6 = %+v, want a working session of the deck's own", s)
	}
}
