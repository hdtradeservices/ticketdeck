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
//  4. Otherwise take the best score, which is claude-pick's (~/claude/scripts,
//     hdtradeservices/claude), so the deck and /dispatch send work to the same
//     place. It trades three things: spend the weekly allowance that renews
//     soonest, since whatever is left at the reset is lost; don't land a session
//     where the 5-hour limit will stop it within the next few hours; and spread
//     sessions, since sessions on one account share its budget.
//
// The deck ports the score instead of running claude-pick: claude-pick
// refreshes an expired login by spending a small model turn, which the deck
// never does, and it reads usage itself rather than from the deck's shared
// cache.
//
// An account whose usage can't be read — usually an expired access token,
// which is what an account nobody has used for hours looks like — is usable:
// launching a session under it refreshes the token. It is scored as if it had
// moderate room.
package route

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
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
	Live       int // sessions running on it now
	// Load is how hard those sessions are spending it, counted as claude-pick
	// does: 1 for a busy session, 0.5 for one waiting on a person, 0.1 for an
	// idle one, and 1 for each launch too recent to show up yet.
	Load float64
	Home bool // the account the deck itself runs as
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
	// Score is claude-pick's score for one more session here: higher is better,
	// below zero when that session is expected to sit stalled. Zero for an
	// account that is out.
	Score float64
	// Stall is how many of the next Horizon hours a session started here now
	// is expected to sit stopped by the 5-hour limit.
	Stall float64
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

// Rank orders the candidates best-first with a verdict on each: every account
// that can take work by score, then the ones that can't.
func Rank(cands []Candidate, now time.Time) []Ranked {
	out := make([]Ranked, 0, len(cands))
	for _, c := range cands {
		out = append(out, judge(c, now))
	}
	score(out, now, Tune)
	slices.SortStableFunc(out, func(a, b Ranked) int {
		if c := cmp.Compare(outLast(a.Verdict), outLast(b.Verdict)); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
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

func outLast(v Verdict) int {
	if v == Out {
		return 1
	}
	return 0
}

// Tunables are claude-pick's knobs.
type Tunables struct {
	// SessionRate is the %/hour of the 5-hour limit one busy session burns,
	// used until some account has the history to measure it.
	SessionRate float64
	// Horizon is how many hours a new session is expected to run.
	Horizon float64
	// LoadPenalty is how fast live load divides an account's urgency down.
	LoadPenalty float64
}

// Tune is what Rank scores with: claude-pick's defaults, overridden by the
// variables claude-pick reads, so a change made for one changes both.
var Tune = tunablesFromEnv()

func tunablesFromEnv() Tunables {
	t := Tunables{SessionRate: 12, Horizon: 3, LoadPenalty: 0.5}
	read := func(name string, dst *float64, ok func(float64) bool) {
		if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64); err == nil && ok(v) {
			*dst = v
		}
	}
	read("CLAUDE_PICK_SESSION_RATE", &t.SessionRate, func(v float64) bool { return v > 0 })
	read("CLAUDE_PICK_HORIZON", &t.Horizon, func(v float64) bool { return v >= 0 })
	read("CLAUDE_PICK_LOAD_PENALTY", &t.LoadPenalty, func(v float64) bool { return v >= 0 })
	return t
}

// window is one account as the score sees it: each limit's use in percent and
// the hours until it renews, and its live load.
type window struct {
	s, sr, w, wr, load float64
	read               bool // from a real reading, not assumed
}

// An account whose usage can't be read is scored as having unknownRoom of its
// week left, half way through it.
const unknownWeekLeft = 84.0

func windowOf(r Ranked, now time.Time) window {
	u := r.Usage
	if u == nil {
		return window{s: 0, sr: 5, w: 100 - unknownRoom, wr: unknownWeekLeft, load: r.Load}
	}
	hoursTo := func(t time.Time, full float64) float64 {
		if t.IsZero() || !t.After(now) {
			return full // not started, or renewed since the reading
		}
		return t.Sub(now).Hours()
	}
	fh, sd := pcts(u, now)
	return window{
		s: fh, sr: hoursTo(u.FiveHourReset, 5),
		w: sd, wr: hoursTo(u.SevenDayReset, 168),
		load: r.Load, read: true,
	}
}

// pcts is a reading's two windows as they stand now: a window whose reset has
// passed since the reading was taken has started over, and its old figure says
// nothing about now.
func pcts(u *quota.Usage, now time.Time) (fh, sd float64) {
	fh, sd = u.FiveHourPct, u.SevenDayPct
	if !u.FiveHourReset.IsZero() && !now.Before(u.FiveHourReset) {
		fh = 0
	}
	if !u.SevenDayReset.IsZero() && !now.Before(u.SevenDayReset) {
		sd = 0
	}
	return fh, sd
}

// score sets Score and Stall on every account that can take work. It is
// claude-pick's SCORE_JQ for one more session on each account; the comments
// there carry the reasoning.
func score(rs []Ranked, now time.Time, t Tunables) {
	var idx []int
	var ws []window
	for i := range rs {
		if rs[i].Verdict == Out {
			continue
		}
		idx = append(idx, i)
		ws = append(ws, windowOf(rs[i], now))
	}
	if len(ws) == 0 {
		return
	}
	// The per-session 5-hour burn, measured where an account has an hour of
	// history and real load behind it; the upper median of those, as jq takes it.
	var rates []float64
	for _, w := range ws {
		if w.read && 5-w.sr >= 1 && w.load >= 1 && w.s >= 5 {
			rates = append(rates, w.s/(5-w.sr)/w.load)
		}
	}
	rate := t.SessionRate
	if len(rates) > 0 {
		slices.Sort(rates)
		rate = rates[len(rates)/2]
	}
	var loadSum float64
	for _, w := range ws {
		loadSum += w.load + 1
	}
	stallWeight := 20 / (1 + t.LoadPenalty*loadSum/float64(len(ws)))
	for k, w := range ws {
		m := w.load + 1
		down := downtime(w, m, rate, t.Horizon)
		urgency := max((100-w.w)/max(w.wr, 1), 0)
		r := &rs[idx[k]]
		r.Stall = down
		r.Score = urgency/(1+t.LoadPenalty*m) - stallWeight*down
	}
}

// downtime is how many hours a session added at load m would sit stopped by a
// limit, each window cut at its own reset. A weekly stall only counts once the
// week is spent: a session that reaches it sooner moves on.
func downtime(w window, m, rate, horizon float64) float64 {
	sh, wh := min(horizon, w.sr), min(horizon, w.wr)
	sdown := sh
	if w.s < 100 {
		sdown = max(sh-(100-w.s)/(rate*m), 0)
	}
	var wdown float64
	if w.w >= 100 {
		wdown = wh
	}
	return max(sdown, wdown)
}

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
	fh, sd := pcts(u, now)
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
		d.Why = fmt.Sprintf("⦿%s %s — moving it to ⦿%s (%s)", req.Sticky, left, best.Name, Fit(best, now))
	case len(ranked) == 1 && req.Exclude == "":
		d.Why = "⦿" + best.Name + " is the only account"
	default:
		d.Why = fmt.Sprintf("⦿%s is the best fit: %s", best.Name, Fit(best, now))
	}
	return d
}

// Fit says what makes an account a good place for a session, in the terms the
// score weighs: the week it has left and when that renews, how full its
// 5-hour window is, and whether a session there is expected to stall.
func Fit(r Ranked, now time.Time) string {
	if r.Usage == nil {
		return r.Note
	}
	fh, sd := pcts(r.Usage, now)
	s := fmt.Sprintf("%.0f%% of its week left", 100-sd)
	if t := r.Usage.SevenDayReset; t.After(now) {
		s += ", renewing " + Clock(t, now)
	}
	s += fmt.Sprintf(", 5h %.0f%%", fh)
	if r.Stall >= 0.25 {
		s += fmt.Sprintf(", but its 5-hour limit is likely to stop a session there for ~%s", hoursText(r.Stall))
	}
	if r.Stale {
		s += " ~"
	}
	return s
}

func hoursText(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%dm", int(h*60+0.5))
	}
	return fmt.Sprintf("%.1fh", h)
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
