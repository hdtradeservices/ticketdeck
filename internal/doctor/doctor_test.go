package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

var now = time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)

// world stubs everything a run reaches outside the process.
type world struct {
	reg      map[string][]session.Proc
	ag       map[string][]session.Proc
	agErr    error
	files    map[string]bool // session id → has a transcript
	tails    map[string]session.Tail
	profiles map[string]account.Profile // config dir → setup
	herdr    string
}

func (w world) install(t *testing.T) {
	t.Helper()
	r, a, pe, ft, to, rp, hv, cv := registry, agents, procExited, findTranscript, tailOf, readProfile, herdrVersion, claudeVersion
	t.Cleanup(func() {
		registry, agents, procExited, findTranscript, tailOf, readProfile, herdrVersion, claudeVersion = r, a, pe, ft, to, rp, hv, cv
	})
	registry = func(dir string) []session.Proc { return w.reg[dir] }
	agents = func(dir string) ([]session.Proc, error) { return w.ag[dir], w.agErr }
	procExited = func(int) bool { return false }
	findTranscript = func(_, id string) string {
		if w.files[id] {
			return "/t/" + id + ".jsonl"
		}
		return ""
	}
	tailOf = func(_, id string) session.Tail { return w.tails[id] }
	readProfile = func(a account.Account) account.Profile { return w.profiles[a.ConfigDir] }
	herdrVersion = func() (string, error) { return w.herdr, nil }
	claudeVersion = func() (string, error) { return "2.1.280", nil }
}

var (
	matt = account.Account{Name: "matt", ConfigDir: "/h/.claude"}
	m1   = account.Account{Name: "m1", ConfigDir: "/h/.claude-m1"}
)

func proc(pid int, id, dir string) session.Proc {
	return session.Proc{PID: pid, SessionID: id, ConfigDir: dir, StartedAt: now.Add(-time.Hour)}
}

func find(r Report, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Level: -1}
}

func TestAHealthyMachine(t *testing.T) {
	world{
		reg:   map[string][]session.Proc{matt.ConfigDir: {proc(1, "a", matt.ConfigDir)}},
		ag:    map[string][]session.Proc{matt.ConfigDir: {proc(1, "a", matt.ConfigDir)}},
		files: map[string]bool{"a": true},
		tails: map[string]session.Tail{"a": {Model: "claude-opus-5"}},
		herdr: "0.9.5",
	}.install(t)
	r := Run(Input{Accounts: []account.Account{matt, m1}, Home: matt, Herdr: true, Now: now,
		Quotas: map[string]quota.Entry{"matt": {Usage: &quota.Usage{}}, "m1": {Reason: "token expired"}}})
	if f := r.Failures(); len(f) != 0 {
		t.Errorf("failures on a healthy machine: %+v", f)
	}
	for _, c := range r.Checks {
		if c.Level != OK {
			t.Errorf("%s: %v %s", c.Name, c.Level, c.Detail)
		}
	}
	if r.Claude != "2.1.280" {
		t.Errorf("claude = %q", r.Claude)
	}
}

// A Claude Code release that moves or reshapes the registry shows up as live
// sessions `claude agents` lists and the reader doesn't, and the account is
// then read through `claude agents`.
func TestARegistryThatNoLongerMatches(t *testing.T) {
	world{
		reg:   map[string][]session.Proc{},
		ag:    map[string][]session.Proc{m1.ConfigDir: {proc(7, "b", m1.ConfigDir)}},
		files: map[string]bool{"b": true},
		tails: map[string]session.Tail{"b": {Model: "claude-opus-5"}},
		herdr: "0.9.5",
	}.install(t)
	r := Run(Input{Accounts: []account.Account{matt, m1}, Home: matt, Now: now})
	c := find(r, "sessions ⦿m1")
	if c.Level != Fail || !strings.Contains(c.Detail, "changed its session registry") {
		t.Errorf("m1 check = %+v", c)
	}
	if !r.ViaAgents[m1.ConfigDir] || r.ViaAgents[matt.ConfigDir] {
		t.Errorf("ViaAgents = %v, want only m1", r.ViaAgents)
	}
	// The sessions only `claude agents` saw still have their transcripts checked.
	if c := find(r, "transcripts"); c.Level != OK || !strings.Contains(c.Detail, "1 of 1") {
		t.Errorf("transcripts = %+v", c)
	}
}

func TestTranscriptsThatMovedOrStoppedParsing(t *testing.T) {
	live := []session.Proc{proc(1, "a", matt.ConfigDir), proc(2, "b", matt.ConfigDir), proc(3, "c", matt.ConfigDir)}
	world{reg: map[string][]session.Proc{matt.ConfigDir: live}, ag: map[string][]session.Proc{matt.ConfigDir: live}, herdr: "0.9.5"}.install(t)
	if c := find(Run(Input{Accounts: []account.Account{matt}, Home: matt, Now: now}), "transcripts"); c.Level != Fail || !strings.Contains(c.Detail, "moved them") {
		t.Errorf("no transcripts at all = %+v", c)
	}
	world{reg: map[string][]session.Proc{matt.ConfigDir: live}, ag: map[string][]session.Proc{matt.ConfigDir: live},
		files: map[string]bool{"a": true, "b": true, "c": true}, herdr: "0.9.5"}.install(t)
	if c := find(Run(Input{Accounts: []account.Account{matt}, Home: matt, Now: now}), "transcripts"); c.Level != Fail || !strings.Contains(c.Detail, "format") {
		t.Errorf("transcripts that don't parse = %+v", c)
	}
	// A session started a minute ago may not have written yet.
	fresh := proc(4, "d", matt.ConfigDir)
	fresh.StartedAt = now.Add(-time.Minute)
	world{reg: map[string][]session.Proc{matt.ConfigDir: {fresh}}, ag: map[string][]session.Proc{matt.ConfigDir: {fresh}}, herdr: "0.9.5"}.install(t)
	if c := find(Run(Input{Accounts: []account.Account{matt}, Home: matt, Now: now}), "transcripts"); c.Level != OK {
		t.Errorf("a fresh session = %+v", c)
	}
}

func TestUsageAndHerdrAndSetup(t *testing.T) {
	world{
		herdr: "0.9.1",
		profiles: map[string]account.Profile{
			matt.ConfigDir: {Read: true, Model: "claude-opus-5", Hooks: []string{"SessionStart herdr-agent-state.sh"}},
			m1.ConfigDir:   {Read: true, Model: "claude-opus-5"},
		},
	}.install(t)
	r := Run(Input{Accounts: []account.Account{matt, m1}, Home: matt, Herdr: true, Now: now,
		Quotas: map[string]quota.Entry{"matt": {Reason: quota.ReasonFormat}}})
	if c := find(r, "usage"); c.Level != Fail {
		t.Errorf("usage = %+v", c)
	}
	if c := find(r, "herdr"); c.Level != Warn || !strings.Contains(c.Detail, "0.9.1") {
		t.Errorf("herdr = %+v", c)
	}
	if c := find(r, "setup ⦿m1"); c.Level != Warn || c.Detail != "differs from ⦿matt: no hook SessionStart herdr-agent-state.sh" {
		t.Errorf("setup = %+v", c)
	}
}

// The workbench on 2026-10-22 had one conversation live on three accounts at
// once, and another twice on one.
func TestForkedConversations(t *testing.T) {
	m0 := account.Account{Name: "m0", ConfigDir: "/h/.claude-m0"}
	a := proc(1, "9b2c013f-e2db", matt.ConfigDir)
	b := proc(2, "9b2c013f-e2db", m0.ConfigDir)
	c := proc(3, "9b2c013f-e2db", m1.ConfigDir)
	d, e := proc(4, "7a526b7c", m0.ConfigDir), proc(5, "7a526b7c", m0.ConfigDir)
	d.Name = "ZEN-4142"
	world{
		reg: map[string][]session.Proc{matt.ConfigDir: {a}, m0.ConfigDir: {b, d, e}, m1.ConfigDir: {c}},
		ag:  map[string][]session.Proc{matt.ConfigDir: {a}, m0.ConfigDir: {b, d, e}, m1.ConfigDir: {c}},
	}.install(t)
	c2 := find(Run(Input{Accounts: []account.Account{matt, m0, m1}, Home: matt, Now: now}), "forks")
	if c2.Level != Warn || !strings.Contains(c2.Detail, "9b2c013f ×3") || !strings.Contains(c2.Detail, "ZEN-4142 ×2") {
		t.Errorf("forks = %+v", c2)
	}
}
