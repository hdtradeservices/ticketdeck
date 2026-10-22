// Package doctor checks the assumptions the deck makes about the tools under
// it, so a release that changes one shows up as a named problem rather than
// as statuses that are quietly wrong.
//
// Several of those assumptions are formats nobody documented: Claude Code's
// live-session registry, its transcripts, the OAuth usage endpoint. Claude Code
// ships most days. The checks compare each one with something that is
// documented, or that a format change would visibly break:
//
//   - each account's registry, against `claude agents --json`;
//   - the transcripts of live sessions, which must be where the deck looks and
//     must parse;
//   - the same conversation running in two processes at once, which forks it;
//   - the usage readings, which say when the endpoint's answer changed shape;
//   - herdr's server version, for what its events can be trusted with;
//   - each account's setup, against the deck's own account (Profile.Drift).
package doctor

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Level is how bad a check's finding is.
type Level int

const (
	OK   Level = iota
	Warn       // worth knowing; the deck still works
	Fail       // an assumption broke, and what the deck shows may be wrong
)

// Check is one finding.
type Check struct {
	Name   string
	Level  Level
	Detail string
}

// Report is one run of every check.
type Report struct {
	Checks []Check
	// ViaAgents marks the accounts, by config dir, whose registry no longer
	// matches `claude agents --json`. The deck reads those through it.
	ViaAgents map[string]bool
	// Profiles is each account's setup, by account name.
	Profiles map[string]account.Profile
	// Claude is the installed Claude Code version, "" when unknown.
	Claude string
	At     time.Time
}

// Failures is every check that failed.
func (r Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Level == Fail {
			out = append(out, c)
		}
	}
	return out
}

// Input is what a run needs.
type Input struct {
	Accounts []account.Account
	Home     account.Account
	Herdr    bool
	// Quotas is the latest usage reading for each account. The check judges
	// the endpoint by what the deck already read; it never calls it.
	Quotas map[string]quota.Entry
	Now    time.Time
}

// The outside world, in vars so tests can run every check without it.
var (
	registry       = session.Registry
	agents         = session.Agents
	procExited     = session.ProcExited
	findTranscript = session.FindTranscript
	tailOf         = session.TailOf
	readProfile    = account.ReadProfile
	herdrVersion   = herd.ServerVersion
	claudeVersion  = func() (string, error) {
		out, err := exec.Command(session.Bin(), "--version").Output()
		if err != nil {
			return "", err
		}
		v, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		return v, nil
	}
)

// settleAfter is how long a session has to have run before its transcript is
// expected on disk.
const settleAfter = 5 * time.Minute

// Run runs every check.
func Run(in Input) Report {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	r := Report{ViaAgents: map[string]bool{}, Profiles: map[string]account.Profile{}, At: in.Now}

	v, err := claudeVersion()
	switch {
	case err != nil:
		r.add("claude", Fail, fmt.Sprintf("can't run `%s --version`: %v", session.Bin(), err))
	default:
		r.Claude = v
		r.add("claude", OK, "Claude Code "+v)
	}

	// Every account's registry against its documented listing, in parallel:
	// each `claude agents` takes a moment.
	type pair struct {
		reg []session.Proc
		ag  []session.Proc
		err error
	}
	pairs := make([]pair, len(in.Accounts))
	var wg sync.WaitGroup
	for i, a := range in.Accounts {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			ag, err := agents(dir)
			pairs[i] = pair{reg: registry(dir), ag: ag, err: err}
		}(i, a.ConfigDir)
	}
	wg.Wait()
	var live []session.Proc
	for i, a := range in.Accounts {
		p := pairs[i]
		name := "sessions ⦿" + a.Name
		live = append(live, p.reg...)
		if p.err != nil {
			r.add(name, Warn, fmt.Sprintf("%d live; can't cross-check with `claude agents --json`: %v", len(p.reg), p.err))
			continue
		}
		seen := map[int]bool{}
		for _, x := range p.reg {
			seen[x.PID] = true
		}
		var missing []string
		for _, x := range p.ag {
			// A session that ended between the two reads is gone from both.
			if !seen[x.PID] && !procExited(x.PID) {
				missing = append(missing, x.SessionID)
			}
		}
		if len(missing) > 0 {
			r.ViaAgents[a.ConfigDir] = true
			live = append(live, p.ag...)
			r.add(name, Fail, fmt.Sprintf("`claude agents` lists %d live session(s) the registry reader can't see — Claude Code changed its session registry; reading this account through `claude agents` instead", len(missing)))
			continue
		}
		r.add(name, OK, fmt.Sprintf("%d live; the registry matches `claude agents --json`", len(p.ag)))
	}

	r.checkTranscripts(live, in.Now)
	r.checkForks(live)
	r.checkUsage(in)
	if in.Herdr {
		r.checkHerdr()
	}

	home := readProfile(in.Home)
	r.Profiles[in.Home.Name] = home
	for _, a := range in.Accounts {
		if a.ConfigDir == in.Home.ConfigDir {
			continue
		}
		p := readProfile(a)
		r.Profiles[a.Name] = p
		if d := home.Drift(p); len(d) > 0 {
			r.add("setup ⦿"+a.Name, Warn, "differs from ⦿"+in.Home.Name+": "+strings.Join(d, " · "))
		}
	}
	return r
}

func (r *Report) add(name string, l Level, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Level: l, Detail: detail})
}

// checkTranscripts looks for the transcripts of sessions old enough to have
// written one, and checks they still parse. One missing is a session nobody
// typed into; all of them missing is a moved directory.
func (r *Report) checkTranscripts(live []session.Proc, now time.Time) {
	var settled, found, parsed int
	for _, p := range live {
		if p.SessionID == "" || p.StartedAt.IsZero() || now.Sub(p.StartedAt) < settleAfter {
			continue
		}
		settled++
		if findTranscript(p.ConfigDir, p.SessionID) == "" {
			continue
		}
		found++
		if t := tailOf(p.ConfigDir, p.SessionID); t.Model != "" || t.Title != "" || t.ContextTokens > 0 {
			parsed++
		}
	}
	switch {
	case settled == 0:
		r.add("transcripts", OK, "no session has run long enough to check")
	case found == 0 && settled >= 2:
		r.add("transcripts", Fail, fmt.Sprintf("none of %d live sessions has a transcript under <account>/projects/*/<id>.jsonl — Claude Code moved them; statuses, limits and moves will be wrong", settled))
	case found >= 3 && parsed == 0:
		r.add("transcripts", Fail, fmt.Sprintf("%d transcripts found, none of them readable — Claude Code changed their format; limits and titles will be missed", found))
	default:
		r.add("transcripts", OK, fmt.Sprintf("%d of %d live sessions' transcripts found and read", parsed, settled))
	}
}

// checkForks finds a conversation running in more than one process at once —
// on two accounts after a copy, or twice on one. Each process appends to its
// own copy of the transcript, and the copies diverge with no way to merge them.
func (r *Report) checkForks(live []session.Proc) {
	byID := map[string][]session.Proc{}
	for _, p := range live {
		if p.SessionID != "" {
			byID[p.SessionID] = append(byID[p.SessionID], p)
		}
	}
	var forks []string
	for id, ps := range byID {
		pids := map[int]bool{}
		for _, p := range ps {
			pids[p.PID] = true
		}
		if len(pids) < 2 {
			continue
		}
		name := id[:min(8, len(id))]
		for _, p := range ps {
			if p.Name != "" {
				name = p.Name
				break
			}
		}
		forks = append(forks, fmt.Sprintf("%s ×%d", name, len(pids)))
	}
	if len(forks) == 0 {
		r.add("forks", OK, "every live conversation runs in one place")
		return
	}
	slices.Sort(forks)
	r.add("forks", Warn, fmt.Sprintf("%d conversation(s) run in more than one place at once, each writing its own copy: %s — close all but one", len(forks), strings.Join(forks, ", ")))
}

// checkUsage judges the usage endpoint by the deck's own readings.
func (r *Report) checkUsage(in Input) {
	var read int
	var reasons []string
	for _, a := range in.Accounts {
		e := in.Quotas[a.Name]
		switch {
		case e.Reason == quota.ReasonFormat:
			r.add("usage", Fail, "the usage endpoint answered in a shape the deck doesn't know — headroom is unknown, so routing and the forecast are blind")
			return
		case e.Usage != nil && e.Reason == "":
			read++
		case e.Reason != "":
			reasons = append(reasons, a.Name+": "+e.Reason)
		}
	}
	switch {
	case len(in.Quotas) == 0:
		r.add("usage", OK, "no reading yet")
	case read == 0 && len(reasons) > 0:
		slices.Sort(reasons)
		r.add("usage", Warn, "no account's usage could be read ("+strings.Join(reasons, "; ")+")")
	default:
		r.add("usage", OK, fmt.Sprintf("read for %d of %d accounts", read, len(in.Accounts)))
	}
}

// checkHerdr checks the running server's version against what the deck relies
// on: events from 0.9.0, and from 0.9.2 events that arrive in full during a
// burst instead of being skipped without a word.
func (r *Report) checkHerdr() {
	v, err := herdrVersion()
	switch {
	case err != nil:
		r.add("herdr", Warn, fmt.Sprintf("can't read herdr's version: %v", err))
	case !herd.AtLeast(v, 0, 9, 0):
		r.add("herdr", Fail, "herdr "+v+" — the deck is built for 0.9; upgrade with `herdr update`")
	case !herd.AtLeast(v, 0, 9, 2):
		r.add("herdr", Warn, "herdr "+v+" can drop events in a burst, which delays a status by up to one poll; 0.9.2 and later report the loss instead")
	default:
		r.add("herdr", OK, "herdr "+v)
	}
}
