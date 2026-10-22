// Package route picks the Claude account a session should run under.
//
// It is pure: usage readings and what the deck has seen go in, an account and
// the reason for it come out. Nothing here reads a file or calls an endpoint,
// so every rule is tested as a table rather than trusted.
//
// The rules, in order:
//
//  1. Rule an account out when it can't take work: its usage limit refused a
//     session (until that limit resets), its login lapsed, nobody is signed in,
//     or a window is spent.
//  2. Stick. A session that already lives on an account resumes there unless
//     that account is ruled out or nearly spent and somewhere has real room.
//     Moving costs a transcript copy and a replayed conversation, so it needs a
//     reason.
//  3. Honour a pin: the user can name the account new sessions go to.
//  4. Otherwise take the most room. An account is as full as its tighter
//     window — 5% of the 5-hour window buys nothing on a subscription at 98% of
//     its week. Near-equal accounts (same 10-point band) go to the one running
//     fewer sessions, because sessions on one account share its budget.
//
// An account whose usage can't be read — usually an expired access token,
// which is what an account nobody has used for hours looks like — is usable:
// launching a session under it refreshes the token. It ranks as if it had
// moderate room, behind any account known to have plenty.
package route

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/quota"
)

// Thresholds, in percent of a window used.
const (
	tight5h = 90 // nearly spent: only when nothing else will do
	tight7d = 95
	spent   = 100
)

// unknownRoom is the headroom an unreadable account is ranked with.
const unknownRoom = 35

// Candidate is one account the router may pick.
type Candidate struct {
	Name  string
	Usage *quota.Usage // nil when there is no reading
	// Stale marks a reading old enough that it may no longer hold.
	Stale bool
	// Reason is why the last usage read failed, "" when it worked.
	Reason string
	// LimitedUntil is when a usage limit that refused one of its sessions
	// lifts; zero when none has. This is the account's own word that it is out,
	// and it arrives even when its usage can't be read.
	LimitedUntil time.Time
	// AuthFailed: one of its sessions was refused because its login lapsed.
	AuthFailed bool
	Live       int  // sessions running on it now
	Home       bool // the account the deck itself runs as
}

// Verdict is how usable an account is right now.
type Verdict int

const (
	Usable  Verdict = iota // known to have room
	Unknown                // usable, but its room can't be read
	Tight                  // nearly spent: only when nothing better exists
	Out                    // can't take work
)

// Ranked is a candidate with the router's view of it.
type Ranked struct {
	Candidate
	Verdict  Verdict
	Headroom float64 // percent of the tighter window left; unknownRoom when unread
	Note     string  // a few words on why, for the screen
}

// Request says what the pick is for.
type Request struct {
	Sticky  string // the account already holding the session, "" for a new one
	Exclude string // an account the session must leave
	Pin     string // the account the user pinned for new sessions, "" for auto
	Now     time.Time
}

// Decision is the router's answer.
type Decision struct {
	// Account is the pick, "" when no account can take work at all.
	Account string
	// Why is one line for the screen.
	Why string
	// Ranked is every candidate, best first.
	Ranked []Ranked
	// Stuck reports the session's own account was kept although it is
	// nearly spent, because nothing had more room.
	Stuck bool
}

// Rank orders the candidates best-first with a verdict on each.
func Rank(cands []Candidate, now time.Time) []Ranked {
	out := make([]Ranked, 0, len(cands))
	for _, c := range cands {
		out = append(out, judge(c, now))
	}
	slices.SortStableFunc(out, func(a, b Ranked) int {
		if c := cmp.Compare(class(a.Verdict), class(b.Verdict)); c != 0 {
			return c
		}
		if c := cmp.Compare(band(b.Headroom), band(a.Headroom)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Live, b.Live); c != 0 {
			return c
		}
		if c := cmp.Compare(weekUsed(a), weekUsed(b)); c != 0 {
			return c
		}
		if a.Home != b.Home {
			if a.Home {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// class groups verdicts: usable and unknown compete on room, tight comes after
// both, out last.
func class(v Verdict) int {
	switch v {
	case Usable, Unknown:
		return 0
	case Tight:
		return 1
	}
	return 2
}

func band(headroom float64) int { return int(math.Floor(headroom / 10)) }

func weekUsed(r Ranked) float64 {
	if r.Usage == nil {
		return 50
	}
	return r.Usage.SevenDayPct
}

func judge(c Candidate, now time.Time) Ranked {
	r := Ranked{Candidate: c, Headroom: unknownRoom}
	switch {
	case c.Reason == "not signed in":
		r.Verdict, r.Note = Out, "not signed in"
		return r
	case c.AuthFailed:
		r.Verdict, r.Note = Out, "login expired"
		return r
	case !c.LimitedUntil.IsZero() && now.Before(c.LimitedUntil):
		r.Verdict, r.Note = Out, "out until "+Clock(c.LimitedUntil, now)
		return r
	}
	if c.Usage == nil {
		r.Verdict, r.Note = Unknown, cmp.Or(c.Reason, "no reading")
		return r
	}
	u := c.Usage
	// A window whose reset has passed since the reading was taken has started
	// over; its old figure says nothing about now.
	fh, sd := u.FiveHourPct, u.SevenDayPct
	if !u.FiveHourReset.IsZero() && !now.Before(u.FiveHourReset) {
		fh = 0
	}
	if !u.SevenDayReset.IsZero() && !now.Before(u.SevenDayReset) {
		sd = 0
	}
	r.Headroom = 100 - max(fh, sd)
	r.Note = fmt.Sprintf("5h %.0f%% · 7d %.0f%%", fh, sd)
	if c.Stale {
		r.Note += " ~"
	}
	switch {
	case fh >= spent:
		r.Verdict, r.Note = Out, "5h spent"+resetNote(u.FiveHourReset, now)
	case sd >= spent:
		r.Verdict, r.Note = Out, "7d spent"+resetNote(u.SevenDayReset, now)
	case fh >= tight5h || sd >= tight7d:
		r.Verdict = Tight
	default:
		r.Verdict = Usable
	}
	return r
}

func resetNote(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " until " + Clock(t, now)
}

// Clock renders a reset time compactly: "2:50pm" when it is within the next
// twelve hours or later today, "Thu 9am" this week, "Oct 30" beyond that.
func Clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	hm := strings.ToLower(t.Format("3:04pm"))
	hm = strings.Replace(hm, ":00", "", 1)
	switch {
	case sameDay(t, now) || (t.After(now) && t.Sub(now) < 12*time.Hour):
		return hm
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon") + " " + hm
	}
	return t.Format("Jan 2")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Pick chooses the account for a session.
func Pick(cands []Candidate, req Request) Decision {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	var pool []Candidate
	for _, c := range cands {
		if req.Exclude != "" && c.Name == req.Exclude {
			continue
		}
		pool = append(pool, c)
	}
	ranked := Rank(pool, now)
	d := Decision{Ranked: ranked}
	byName := func(n string) (Ranked, bool) {
		for _, r := range ranked {
			if r.Name == n {
				return r, true
			}
		}
		return Ranked{}, false
	}
	if len(ranked) == 0 {
		d.Why = "no other account to move to"
		return d
	}
	best := ranked[0]

	if s, ok := byName(req.Sticky); ok {
		switch s.Verdict {
		case Usable, Unknown:
			d.Account, d.Why = s.Name, "resumes on ⦿"+s.Name+", where it already lives"
			return d
		case Tight:
			if best.Verdict == Tight || best.Name == s.Name || best.Headroom <= s.Headroom {
				d.Account, d.Stuck = s.Name, true
				d.Why = "⦿" + s.Name + " is nearly spent (" + s.Note + ") but nothing has more room"
				return d
			}
		}
	}
	if p, ok := byName(req.Pin); ok && p.Verdict != Out {
		d.Account, d.Why = p.Name, "⦿"+p.Name+" is pinned for new sessions"
		return d
	}
	if best.Verdict == Out {
		d.Why = allOut(ranked, now)
		return d
	}
	d.Account = best.Name
	if best.Verdict == Unknown {
		d.Why = fmt.Sprintf("⦿%s's usage can't be read (%s), and no account is known to have more room", best.Name, best.Note)
		if req.Sticky != "" && req.Sticky != best.Name {
			if s, ok := byName(req.Sticky); ok {
				d.Why = fmt.Sprintf("⦿%s is %s — moving it to ⦿%s, whose usage can't be read (%s)", req.Sticky, s.Note, best.Name, best.Note)
			}
		}
		return d
	}
	switch {
	case req.Sticky != "" && req.Sticky != best.Name:
		s, _ := byName(req.Sticky)
		left := "has left"
		if s.Name != "" {
			left = "is " + s.Note
		}
		d.Why = fmt.Sprintf("⦿%s %s — moving it to ⦿%s (%s)", req.Sticky, left, best.Name, best.Note)
	case req.Exclude != "":
		d.Why = fmt.Sprintf("⦿%s has the most room (%s)", best.Name, best.Note)
	case len(ranked) == 1:
		d.Why = "⦿" + best.Name + " is the only account"
	default:
		d.Why = fmt.Sprintf("⦿%s has the most room (%s)", best.Name, best.Note)
	}
	return d
}

// allOut explains a pick with nowhere to go, naming the soonest an account
// comes back.
func allOut(ranked []Ranked, now time.Time) string {
	var soon time.Time
	who := ""
	for _, r := range ranked {
		t := r.LimitedUntil
		if r.Usage != nil {
			if r.Usage.FiveHourPct >= spent && r.Usage.FiveHourReset.After(t) {
				t = r.Usage.FiveHourReset
			}
		}
		if !t.IsZero() && t.After(now) && (soon.IsZero() || t.Before(soon)) {
			soon, who = t, r.Name
		}
	}
	if who == "" {
		return "no account can take work right now"
	}
	return fmt.Sprintf("every account is out — ⦿%s is back first, at %s", who, Clock(soon, now))
}

// Soonest is the earliest time any of the candidates comes back, zero when none
// is waiting on a reset.
func Soonest(cands []Candidate, now time.Time) time.Time {
	var soon time.Time
	for _, c := range cands {
		for _, t := range []time.Time{c.LimitedUntil, resetOf(c)} {
			if !t.IsZero() && t.After(now) && (soon.IsZero() || t.Before(soon)) {
				soon = t
			}
		}
	}
	return soon
}

func resetOf(c Candidate) time.Time {
	if c.Usage == nil {
		return time.Time{}
	}
	switch {
	case c.Usage.FiveHourPct >= spent:
		return c.Usage.FiveHourReset
	case c.Usage.SevenDayPct >= spent:
		return c.Usage.SevenDayReset
	}
	return time.Time{}
}
