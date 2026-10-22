// Package fleet builds the deck's picture of every Claude session on the
// machine: which account runs it, which herdr workspace holds it, what state it
// is in, and what its transcript says about how its last turn ended.
//
// Four sources, joined once per status poll:
//
//   - each account's live-session registry (<config dir>/sessions/<pid>.json),
//     which maps a running claude to its session id and its account — and,
//     through the process's environment, to the herdr pane it runs in;
//   - herdr: this deck's own workspace, and every other herdr server on the
//     machine, which is where the sessions older per-account decks started
//     still run;
//   - the transcripts on disk, which say which accounts hold a session and
//     when each last wrote to it, so a session nobody is running is still
//     attributed;
//   - the ledger, which remembers the ad-hoc sessions the deck started, whose
//     random ids nothing else ties back to it.
//
// Every source fails soft. A workspace that doesn't answer, an account whose
// registry can't be read, a transcript that's gone: each drops out of the
// result rather than failing the poll.
package fleet

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Input is what one poll needs.
type Input struct {
	Accounts []account.Account
	Home     account.Account // the account the deck itself runs as
	Keys     []string        // ticket and project keys on the deck
	Ledger   map[string]ledger.Entry
	Herdr    bool   // herdr is installed: ask the workspaces
	Peers    bool   // also ask the other herdr servers on the machine
	Cwd      string // the launch root, where transcripts are looked for first
	Now      time.Time
	// ViaAgents marks the accounts, by config dir, to read through `claude
	// agents --json` because their registry stopped matching it (see
	// internal/doctor).
	ViaAgents map[string]bool
}

// Session is one Claude session as the deck sees it.
type Session struct {
	Key       string // ticket or project key; an ad-hoc session's agent name
	Kind      string // ledger.Ticket | ledger.Project | ledger.Scratch
	ID        string // Claude session id, "" when unknown
	Account   string // the account running it, or holding its transcript
	ConfigDir string
	Status    session.Status
	Live      bool
	// Home: it runs in this deck's own workspace, so the deck can focus,
	// close, prompt and move it directly.
	Home bool
	// Socket and Workspace locate a live session's herdr server: "" for this
	// deck's own, else another server's socket and the deck it belonged to.
	Socket    string
	Workspace string
	PaneID    string
	TabID     string
	Focused   bool
	PID       int
	ProcStart time.Time
	Cwd       string
	// Name is what to call it: the name the user gave it, else the one Claude
	// generated, else its agent name.
	Name       string
	WaitingFor string
	// Prompting: a dialog of its own is on screen — a permission prompt, a
	// question, or Claude's what-now menu after a usage limit. Typing into it
	// would answer it, so it has to be dismissed before anything is sent.
	Prompting  bool
	Activity   session.Activity
	LastActive time.Time // when it last wrote to its transcript
}

// Snapshot is one poll's result.
type Snapshot struct {
	Sessions []Session
	// OwnErr is set when this deck's own workspace couldn't be listed. The rest
	// of the snapshot still stands.
	OwnErr error
	Peers  []herd.Workspace
	At     time.Time
	// Load is each account's live load as claude-pick counts it, from every
	// session its registry lists — the deck's or not, interactive or
	// background — plus launches too recent to be listed yet.
	Load map[string]float64
}

// The outside world, in vars so tests can drive a poll without herdr, without
// live processes, and without the real filesystem layout.
var (
	listAgents = herd.ListAt
	peers      = herd.Peers
	ownSocket  = herd.OwnSocket
	registry   = session.Registry
	agentsOf   = session.Agents
	activityOf = session.ActivityOf
	activityAt = session.ActivityAt
)

var ticketKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// Poll builds the snapshot.
func Poll(in Input) Snapshot {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	snap := Snapshot{At: in.Now, Load: map[string]float64{}}

	// Registries first: they are what ties a pane to an account.
	type paneKey struct{ socket, pane string }
	var procs []session.Proc
	for _, a := range in.Accounts {
		load := float64(pendingLaunches(a.ConfigDir, in.Now))
		regs := registry(a.ConfigDir)
		if in.ViaAgents[a.ConfigDir] {
			if ps, err := agentsOf(a.ConfigDir); err == nil {
				regs = ps
			}
		}
		for _, p := range regs {
			load += loadOf(p.Status)
			// Under herdr the deck's sessions are interactive panes; Claude's own
			// background sessions run outside any of them. Without herdr those
			// are as much the deck's to show as any other.
			if in.Herdr && p.Kind != "" && p.Kind != "interactive" {
				continue
			}
			procs = append(procs, p)
		}
		snap.Load[a.Name] = load
	}
	own := filepath.Clean(ownSocket())
	byPane := map[paneKey]session.Proc{}
	byID := map[string]session.Proc{}
	for _, p := range procs {
		if p.HerdrPane != "" {
			byPane[paneKey{filepath.Clean(p.HerdrSocket), p.HerdrPane}] = p
		}
		byID[p.SessionID] = p
	}

	// Then every workspace, in parallel: a slow peer must not hold up the rest.
	type listing struct {
		ws     herd.Workspace
		agents []herd.Agent
		err    error
		own    bool
	}
	var lists []listing
	if in.Herdr {
		lists = append(lists, listing{own: true, ws: herd.Workspace{Socket: own}})
		if in.Peers {
			snap.Peers = peers()
			for _, w := range snap.Peers {
				lists = append(lists, listing{ws: w})
			}
		}
		var wg sync.WaitGroup
		for i := range lists {
			wg.Add(1)
			go func(l *listing) {
				defer wg.Done()
				sock := l.ws.Socket
				if l.own {
					sock = ""
				}
				l.agents, l.err = listAgents(sock)
			}(&lists[i])
		}
		wg.Wait()
		if len(lists) > 0 && lists[0].err != nil {
			snap.OwnErr = lists[0].err
		}
	}

	keyed := map[string]bool{}
	for _, k := range in.Keys {
		keyed[k] = true
	}
	ledgerByKey := map[string]ledger.Entry{}
	for _, e := range ledger.Sorted(in.Ledger) {
		if _, seen := ledgerByKey[e.Key]; !seen {
			ledgerByKey[e.Key] = e
		}
	}

	seenProc := map[int]bool{}
	// fallback is each listed session's account when nothing better turns up:
	// the account the workspace it sits in belongs to. Resolved after the disk
	// scan, which answers most of them from cache.
	var fallback []string
	for _, l := range lists {
		for _, a := range l.agents {
			if a.Name == "" || a.Name == "deck" || a.PaneID == "" {
				continue
			}
			sock := own
			if !l.own {
				sock = filepath.Clean(l.ws.Socket)
			}
			s := Session{
				Key:     a.Name,
				Live:    true,
				Home:    l.own,
				PaneID:  a.PaneID,
				TabID:   a.TabID,
				Focused: a.Focused,
				Cwd:     a.Cwd,
				Status:  herd.StatusOf(a),
			}
			s.Prompting = s.Status == session.NeedsInput
			if !l.own {
				s.Socket, s.Workspace = l.ws.Socket, l.ws.Name()
			}
			p, ok := byPane[paneKey{sock, a.PaneID}]
			if !ok && a.SessionID() != "" {
				p, ok = byID[a.SessionID()]
			}
			if ok {
				seenProc[p.PID] = true
				s.ID, s.ConfigDir, s.PID, s.ProcStart = p.SessionID, p.ConfigDir, p.PID, p.ProcStart
				s.WaitingFor = p.WaitingFor
				s.Cwd = firstNonEmpty(p.Cwd, s.Cwd)
				s.Status = refine(s.Status, p.Status)
			}
			if s.ID == "" {
				s.ID = a.SessionID()
			}
			if s.ID == "" {
				s.ID = idForKey(a.Name, ledgerByKey)
			}
			s.Name = a.Title
			snap.Sessions = append(snap.Sessions, s)
			fallback = append(fallback, ambient(in, l.ws, l.own))
		}
	}
	listed := map[string]bool{}
	for _, s := range snap.Sessions {
		if s.ID != "" {
			listed[s.ID] = true
		}
	}

	// A live session outside any workspace the deck can see — a ticket resumed
	// by hand in a plain terminal — still has to stop the deck from forking it.
	// Without herdr every session runs that way, and they are the deck's own.
	for _, p := range procs {
		if seenProc[p.PID] || listed[p.SessionID] {
			continue
		}
		key := keyForID(p.SessionID, in.Keys, in.Ledger)
		if key == "" {
			continue
		}
		s := Session{
			Key: key, ID: p.SessionID, ConfigDir: p.ConfigDir, Live: true,
			Status: refine(session.Idle, p.Status), PID: p.PID, ProcStart: p.ProcStart,
			Cwd: p.Cwd, WaitingFor: p.WaitingFor, Workspace: "a terminal",
			Socket: p.HerdrSocket,
		}
		if !in.Herdr {
			s.Home, s.Workspace, s.Socket = true, "", ""
		}
		snap.Sessions = append(snap.Sessions, s)
		fallback = append(fallback, p.ConfigDir)
	}

	// On disk: every account that holds a transcript for a polled key or a
	// remembered ad-hoc session.
	want := map[string]bool{}
	for _, k := range in.Keys {
		want[session.DeterministicID(k)] = true
	}
	for id, e := range in.Ledger {
		if e.Kind == ledger.Scratch && !e.Forgotten {
			want[id] = true
		}
	}
	for _, s := range snap.Sessions {
		if s.ID != "" {
			want[s.ID] = true
		}
	}
	disk := map[string]map[string]found{} // config dir → id → newest transcript
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range in.Accounts {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			t := transcriptTimes(dir, want)
			mu.Lock()
			disk[dir] = t
			mu.Unlock()
		}(a.ConfigDir)
	}
	wg.Wait()

	liveIDs := map[string]bool{}
	for i := range snap.Sessions {
		s := &snap.Sessions[i]
		// Nothing tied the pane to a process: the account that holds its
		// transcript runs it — a session writes a stub the moment it starts —
		// else the one the ledger remembers, else the workspace's own.
		if s.ConfigDir == "" {
			s.ConfigDir = newestHolder(in.Accounts, disk, s.ID)
		}
		if s.ConfigDir == "" {
			s.ConfigDir = in.Ledger[s.ID].ConfigDir
		}
		if s.ConfigDir == "" {
			s.ConfigDir = fallback[i]
		}
		s.Account = nameOf(in.Accounts, s.ConfigDir)
		s.Kind = kindOf(s.Key, s.ID, in.Ledger)
		if s.ID != "" {
			liveIDs[s.ID] = true
			if f, ok := disk[s.ConfigDir][s.ID]; ok {
				s.LastActive = f.mod
				s.Activity = activityAt(f.path, s.ProcStart)
			} else {
				s.Activity = activityOf(s.ConfigDir, s.ID, in.Cwd, s.ProcStart)
			}
			// An error a move has already answered was the old account's: the
			// copied transcript still ends on it, but it says nothing about the
			// account the session runs on now.
			if e := s.Activity.Tail.Err; e != nil && e.UUID != "" && in.Ledger[s.ID].Handled == e.UUID {
				s.Activity.Tail.Err = nil
			}
			s.Status = settle(s.Status, s.Activity, in.Now)
		}
		s.Name = displayName(*s, in.Ledger)
	}

	for _, k := range in.Keys {
		id := session.DeterministicID(k)
		if liveIDs[id] {
			continue
		}
		if s, ok := onDisk(in, disk, k, id); ok {
			snap.Sessions = append(snap.Sessions, s)
		}
	}
	for id, e := range in.Ledger {
		if e.Kind != ledger.Scratch || e.Forgotten || liveIDs[id] || keyed[e.Key] {
			continue
		}
		if s, ok := onDisk(in, disk, e.Key, id); ok {
			s.Kind = ledger.Scratch
			s.Name = displayName(s, in.Ledger)
			s.Cwd = e.Cwd
			snap.Sessions = append(snap.Sessions, s)
		}
	}
	sort.SliceStable(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].Key < snap.Sessions[j].Key })
	return snap
}

// onDisk is the resumable session for an id: the account that wrote to it last.
func onDisk(in Input, disk map[string]map[string]found, key, id string) (Session, bool) {
	var best Session
	for _, a := range in.Accounts {
		f, ok := disk[a.ConfigDir][id]
		if !ok || (best.ID != "" && !f.mod.After(best.LastActive)) {
			continue
		}
		best = Session{
			Key: key, ID: id, Account: a.Name, ConfigDir: a.ConfigDir,
			Status: session.Stopped, LastActive: f.mod,
		}
	}
	if best.ID == "" {
		return Session{}, false
	}
	best.Kind = kindOf(key, id, in.Ledger)
	return best, true
}

// refine folds Claude's own registry status into herdr's screen-read one.
// Claude knows a question or a permission prompt is open the moment it asks;
// herdr has to see it drawn first.
func refine(st session.Status, reg string) session.Status {
	switch reg {
	case "waiting":
		return session.NeedsInput
	case "busy":
		if st == session.Idle || st == session.Unread {
			return session.Working
		}
	}
	return st
}

// settle refines a session whose lead has stopped using what its transcript
// says: a turn that ended on a rate limit or an API error, or work it left
// running in the background.
func settle(st session.Status, a session.Activity, now time.Time) session.Status {
	// A limit puts up Claude's own what-now menu, so a session cut off by one
	// is usually "blocked" on that menu, not idle. Its turn is just as over.
	if st == session.NeedsInput && a.Tail.Limited(now) {
		return session.Limited
	}
	if st != session.Idle && st != session.Unread {
		return st
	}
	switch {
	case a.Tail.Limited(now):
		return session.Limited
	case a.Tail.Err != nil && !a.Tail.Err.RateLimited():
		return session.Errored
	case a.InFlight.Agents > 0:
		return session.Working
	case a.InFlight.Tasks > 0:
		return session.Background
	}
	return st
}

func idForKey(name string, byKey map[string]ledger.Entry) string {
	if ticketKeyRE.MatchString(name) || strings.HasPrefix(name, linear.ProjectKeyPrefix) {
		return session.DeterministicID(name)
	}
	if e, ok := byKey[name]; ok {
		return e.SessionID
	}
	return ""
}

func keyForID(id string, keys []string, led map[string]ledger.Entry) string {
	for _, k := range keys {
		if session.DeterministicID(k) == id {
			return k
		}
	}
	if e, ok := led[id]; ok && !e.Forgotten {
		return e.Key
	}
	return ""
}

func kindOf(key, id string, led map[string]ledger.Entry) string {
	if e, ok := led[id]; ok && e.Kind != "" {
		return e.Kind
	}
	switch {
	case strings.HasPrefix(key, linear.ProjectKeyPrefix):
		return ledger.Project
	case ticketKeyRE.MatchString(key):
		return ledger.Ticket
	}
	return ledger.Scratch
}

// newestHolder is the account holding the newest transcript for a session, from
// the poll's disk scan.
func newestHolder(accts []account.Account, disk map[string]map[string]found, id string) string {
	if id == "" {
		return ""
	}
	best, bestMod := "", time.Time{}
	for _, a := range accts {
		if f, ok := disk[a.ConfigDir][id]; ok && (best == "" || f.mod.After(bestMod)) {
			best, bestMod = a.ConfigDir, f.mod
		}
	}
	return best
}

// ambient is the account a workspace's sessions run as when nothing more
// specific is known: the deck's own for its own workspace, else the account the
// old per-account deck that owns the server ran as.
func ambient(in Input, w herd.Workspace, own bool) string {
	if own {
		return in.Home.ConfigDir
	}
	for _, name := range []string{w.Session, w.Overlay} {
		if name == "" {
			continue
		}
		if a, ok := account.Find(in.Accounts, name); ok {
			return a.ConfigDir
		}
		for _, a := range in.Accounts {
			if filepath.Base(a.ConfigDir) == ".claude-"+name {
				return a.ConfigDir
			}
		}
	}
	// The default server belongs to the primary account, ~/.claude.
	for _, a := range in.Accounts {
		if filepath.Base(a.ConfigDir) == ".claude" {
			return a.ConfigDir
		}
	}
	return in.Home.ConfigDir
}

func nameOf(accts []account.Account, dir string) string {
	if a, ok := account.Find(accts, dir); ok {
		return a.Name
	}
	if dir == "" {
		return ""
	}
	return strings.TrimPrefix(filepath.Base(dir), ".claude-")
}

// displayName picks what to call a session: what the user named it here, else
// what it was named in Claude, else what Claude titled it, else its agent name.
func displayName(s Session, led map[string]ledger.Entry) string {
	if e, ok := led[s.ID]; ok && e.Name != "" {
		return e.Name
	}
	t := s.Activity.Tail
	if t.CustomTitle && t.Title != "" {
		return t.Title
	}
	if t.Title != "" && s.Kind == ledger.Scratch {
		return t.Title
	}
	if s.Name != "" && !strings.EqualFold(s.Name, s.Key) && s.Kind == ledger.Scratch {
		return s.Name // the terminal title Claude set
	}
	return s.Key
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Best is the session a key resolves to: one live in this deck's workspace,
// else one live anywhere, else the transcript written most recently.
func (s Snapshot) Best(key string) (Session, bool) {
	var best Session
	found := false
	for _, c := range s.Sessions {
		if c.Key != key {
			continue
		}
		if !found || outranks(c, best) {
			best, found = c, true
		}
	}
	return best, found
}

func outranks(c, b Session) bool {
	if c.Live != b.Live {
		return c.Live
	}
	if c.Home != b.Home {
		return c.Home
	}
	return c.LastActive.After(b.LastActive)
}

// Live is every running session.
func (s Snapshot) Live() []Session {
	var out []Session
	for _, c := range s.Sessions {
		if c.Live {
			out = append(out, c)
		}
	}
	return out
}

// LiveOn counts the running sessions per account.
func (s Snapshot) LiveOn() map[string]int {
	out := map[string]int{}
	for _, c := range s.Sessions {
		if c.Live {
			out[c.Account]++
		}
	}
	return out
}

// Limits is what the running sessions say about their accounts: when a usage
// limit that refused one lifts, and which accounts' logins have lapsed.
func (s Snapshot) Limits(now time.Time) (until map[string]time.Time, authFailed map[string]bool) {
	until, authFailed = map[string]time.Time{}, map[string]bool{}
	for _, c := range s.Sessions {
		e := c.Activity.Tail.Err
		switch {
		case e == nil || !c.Live:
		case c.Activity.Tail.Limited(now):
			t := e.ResetsAt
			if t.IsZero() {
				t = now.Add(time.Hour) // refused, reset unknown: hold off a while
			}
			if t.After(until[c.Account]) {
				until[c.Account] = t
			}
		case e.AuthFailed():
			authFailed[c.Account] = true
		}
	}
	return until, authFailed
}
