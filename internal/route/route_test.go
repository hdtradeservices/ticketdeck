package route

import (
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/quota"
)

var now = time.Date(2026, 10, 21, 14, 0, 0, 0, time.Local)

func use(fh, sd float64) *quota.Usage {
	return &quota.Usage{FiveHourPct: fh, SevenDayPct: sd, FiveHourReset: now.Add(2 * time.Hour), SevenDayReset: now.Add(72 * time.Hour)}
}

func names(rs []Ranked) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

// An account is as full as its tighter window: support has the most room once
// the weekly window is counted, though m0's 5-hour window is emptier.
func TestRankByTheTighterWindow(t *testing.T) {
	got := Rank([]Candidate{
		{Name: "m0", Usage: use(1, 56)},
		{Name: "support", Usage: use(9, 44)},
		{Name: "matt", Usage: use(31, 51)},
	}, now)
	if n := names(got); n != "support,matt,m0" {
		t.Errorf("order = %s, want support,matt,m0", n)
	}
}

// Near-equal accounts split the work: sessions on one account share its
// budget, so three busy ones there divide its urgency down.
func TestNearEqualAccountsSpreadSessions(t *testing.T) {
	got := Rank([]Candidate{
		{Name: "a", Usage: use(10, 22), Live: 3, Load: 3},
		{Name: "b", Usage: use(15, 25)},
	}, now)
	if n := names(got); n != "b,a" {
		t.Errorf("order = %s, want b,a", n)
	}
}

// claude-pick's own example: unused weekly allowance is lost at the reset, so
// 40% left that renews tonight outranks 80% left that renews in six days.
func TestSpendTheWeekThatRenewsFirst(t *testing.T) {
	soon := &quota.Usage{FiveHourPct: 10, FiveHourReset: now.Add(3 * time.Hour), SevenDayPct: 60, SevenDayReset: now.Add(10 * time.Hour)}
	late := &quota.Usage{FiveHourPct: 10, FiveHourReset: now.Add(3 * time.Hour), SevenDayPct: 20, SevenDayReset: now.Add(6 * 24 * time.Hour)}
	d := Pick([]Candidate{{Name: "late", Usage: late}, {Name: "soon", Usage: soon}}, Request{Now: now})
	if d.Account != "soon" {
		t.Fatalf("pick = %s (%s), want soon", d.Account, d.Why)
	}
	if !strings.Contains(d.Why, "40% of its week left, renewing") {
		t.Errorf("why = %q, want it to name the week left and its renewal", d.Why)
	}
}

// And its other example: an account at 80% of its 5-hour window with six busy
// sessions is about to stall, whatever its week looks like.
func TestAvoidAPredictedFiveHourStall(t *testing.T) {
	busy := &quota.Usage{FiveHourPct: 80, FiveHourReset: now.Add(4 * time.Hour), SevenDayPct: 10, SevenDayReset: now.Add(20 * time.Hour)}
	calm := &quota.Usage{FiveHourPct: 30, FiveHourReset: now.Add(4 * time.Hour), SevenDayPct: 50, SevenDayReset: now.Add(5 * 24 * time.Hour)}
	got := Rank([]Candidate{{Name: "busy", Usage: busy, Load: 6}, {Name: "calm", Usage: calm, Load: 1}}, now)
	if n := names(got); n != "calm,busy" {
		t.Fatalf("order = %s, want calm,busy", n)
	}
	if got[1].Stall <= 0 || got[1].Score >= 0 {
		t.Errorf("busy: stall %.2fh score %.2f, want a stall and a negative score", got[1].Stall, got[1].Score)
	}
	if !strings.Contains(Fit(got[1], now), "likely to stop a session there") {
		t.Errorf("fit = %q", Fit(got[1], now))
	}
}

// The burn rate is measured once some account has an hour of history and real
// load behind it: 40% over two hours at a load of 2 is 10%/h a session. At that
// rate the other account's 35% outlasts the 3-hour horizon; at an assumed
// 30%/h, with nothing to measure, it doesn't.
func TestMeasuredBurnRate(t *testing.T) {
	old := Tune
	t.Cleanup(func() { Tune = old })
	Tune.SessionRate = 30
	measured := &quota.Usage{FiveHourPct: 40, FiveHourReset: now.Add(3 * time.Hour), SevenDayPct: 50, SevenDayReset: now.Add(100 * time.Hour)}
	other := &quota.Usage{FiveHourPct: 65, FiveHourReset: now.Add(270 * time.Minute), SevenDayPct: 50, SevenDayReset: now.Add(100 * time.Hour)}
	stallOf := func(rs []Ranked, name string) float64 {
		for _, r := range rs {
			if r.Name == name {
				return r.Stall
			}
		}
		t.Fatalf("%s not ranked", name)
		return 0
	}
	both := Rank([]Candidate{{Name: "measured", Usage: measured, Load: 2}, {Name: "other", Usage: other}}, now)
	if s := stallOf(both, "other"); s != 0 {
		t.Errorf("other stalls %.2fh at the measured 10%%/h, want none", s)
	}
	alone := Rank([]Candidate{{Name: "other", Usage: other}}, now)
	if s := stallOf(alone, "other"); s < 1.8 || s > 1.9 {
		t.Errorf("other stalls %.2fh at the assumed 30%%/h, want ~1.83h", s)
	}
}

func TestTunablesFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_PICK_SESSION_RATE", "20")
	t.Setenv("CLAUDE_PICK_HORIZON", "nope")
	t.Setenv("CLAUDE_PICK_LOAD_PENALTY", "-1")
	got := tunablesFromEnv()
	if got != (Tunables{SessionRate: 20, Horizon: 3, LoadPenalty: 0.5}) {
		t.Errorf("tunables = %+v: values claude-pick would refuse keep the default", got)
	}
}

func TestVerdicts(t *testing.T) {
	cases := []struct {
		name string
		c    Candidate
		want Verdict
		note string
	}{
		{"room", Candidate{Usage: use(20, 30)}, Usable, "5h 20% · 7d 30%"},
		{"5h nearly spent", Candidate{Usage: use(92, 30)}, Tight, ""},
		{"7d nearly spent", Candidate{Usage: use(10, 96)}, Tight, ""},
		{"5h spent", Candidate{Usage: use(100, 30)}, Out, "5h spent until 4pm"},
		{"refused by its limit", Candidate{Usage: use(40, 30), LimitedUntil: now.Add(50 * time.Minute)}, Out, "out until 2:50pm"},
		{"limit lifted", Candidate{Usage: use(40, 30), LimitedUntil: now.Add(-time.Minute)}, Usable, ""},
		{"login lapsed", Candidate{Usage: use(10, 10), AuthFailed: true}, Out, "login expired"},
		{"not signed in", Candidate{Reason: "not signed in"}, Out, "not signed in"},
		// What an idle account looks like: launching under it refreshes the token.
		{"token expired", Candidate{Reason: "token expired"}, Unknown, "token expired"},
		// The reading predates the 5h reset, so that window has started over.
		{"old reading past its reset", Candidate{Usage: &quota.Usage{FiveHourPct: 100, FiveHourReset: now.Add(-time.Hour), SevenDayPct: 56, SevenDayReset: now.Add(time.Hour)}, Reason: "token expired", Stale: true}, Usable, "5h 0% · 7d 56% ~"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := judge(c.c, now)
			if r.Verdict != c.want {
				t.Errorf("verdict = %v, want %v (%s)", r.Verdict, c.want, r.Note)
			}
			if c.note != "" && r.Note != c.note {
				t.Errorf("note = %q, want %q", r.Note, c.note)
			}
		})
	}
}

func TestPickSticksWhereTheSessionLives(t *testing.T) {
	d := Pick([]Candidate{
		{Name: "m0", Usage: use(60, 70)},
		{Name: "support", Usage: use(5, 5)},
	}, Request{Sticky: "m0", Now: now})
	if d.Account != "m0" || !strings.Contains(d.Why, "already lives") {
		t.Errorf("pick = %s (%s), want m0: moving costs, and m0 has room", d.Account, d.Why)
	}
}

func TestPickMovesOffASpentAccount(t *testing.T) {
	d := Pick([]Candidate{
		{Name: "m0", Usage: use(40, 30), LimitedUntil: now.Add(time.Hour)},
		{Name: "support", Usage: use(5, 5)},
	}, Request{Sticky: "m0", Now: now})
	if d.Account != "support" {
		t.Fatalf("pick = %q (%s), want support", d.Account, d.Why)
	}
	if !strings.Contains(d.Why, "⦿m0 is out until 3pm") || !strings.Contains(d.Why, "⦿support") {
		t.Errorf("why = %q", d.Why)
	}
}

// Nearly spent but nowhere better: stay, and say why.
func TestPickStaysOnATightAccountWhenNothingIsBetter(t *testing.T) {
	d := Pick([]Candidate{
		{Name: "m0", Usage: use(93, 30)},
		{Name: "support", Usage: use(95, 10)},
	}, Request{Sticky: "m0", Now: now})
	if d.Account != "m0" || !d.Stuck {
		t.Errorf("pick = %s stuck=%v, want to stay on m0", d.Account, d.Stuck)
	}
}

func TestPickLeavesATightAccountForRealRoom(t *testing.T) {
	d := Pick([]Candidate{
		{Name: "m0", Usage: use(93, 30)},
		{Name: "support", Usage: use(10, 10)},
	}, Request{Sticky: "m0", Now: now})
	if d.Account != "support" {
		t.Errorf("pick = %s, want support", d.Account)
	}
}

func TestPickHonoursAPinForNewSessions(t *testing.T) {
	cands := []Candidate{{Name: "m0", Usage: use(50, 50)}, {Name: "support", Usage: use(5, 5)}}
	if d := Pick(cands, Request{Pin: "m0", Now: now}); d.Account != "m0" {
		t.Errorf("pinned pick = %s, want m0", d.Account)
	}
	// A pin never forces work onto an account that can't take it.
	cands[0].LimitedUntil = now.Add(time.Hour)
	if d := Pick(cands, Request{Pin: "m0", Now: now}); d.Account != "support" {
		t.Errorf("pick with the pin out = %s, want support", d.Account)
	}
}

func TestPickExcludesTheAccountBeingLeft(t *testing.T) {
	d := Pick([]Candidate{
		{Name: "m0", Usage: use(1, 1)},
		{Name: "support", Usage: use(40, 40)},
	}, Request{Exclude: "m0", Now: now})
	if d.Account != "support" {
		t.Errorf("pick = %s, want support", d.Account)
	}
}

func TestPickWithNowhereToGoNamesTheFirstReset(t *testing.T) {
	cands := []Candidate{
		{Name: "m0", LimitedUntil: now.Add(3 * time.Hour)},
		{Name: "support", Usage: use(100, 50)}, // 5h resets in 2h
	}
	d := Pick(cands, Request{Now: now})
	if d.Account != "" {
		t.Errorf("pick = %s, want none", d.Account)
	}
	if !strings.Contains(d.Why, "⦿support is back first, at 4pm") {
		t.Errorf("why = %q", d.Why)
	}
	if got := Soonest(cands, now); !got.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("Soonest = %v", got)
	}
}

// Unknown headroom is not evidence of room, but it isn't evidence of none: an
// account nobody has used for hours has an expired token and an empty window.
func TestUnreadableAccountRanksBehindPlentyAheadOfLittle(t *testing.T) {
	got := Rank([]Candidate{
		{Name: "low", Usage: use(75, 75)},
		{Name: "idle", Reason: "token expired"},
		{Name: "plenty", Usage: use(10, 20)},
	}, now)
	if n := names(got); n != "plenty,idle,low" {
		t.Errorf("order = %s, want plenty,idle,low", n)
	}
}

func TestClock(t *testing.T) {
	cases := map[time.Duration]string{
		50 * time.Minute:    "2:50pm",
		2 * time.Hour:       "4pm",
		26 * time.Hour:      "Thu 4pm",
		11 * time.Hour:      "1am", // tomorrow, but soon enough that the time alone is clear
		10 * 24 * time.Hour: "Oct 31",
	}
	for d, want := range cases {
		if got := Clock(now.Add(d), now); got != want {
			t.Errorf("Clock(+%v) = %q, want %q", d, got, want)
		}
	}
}

// An unreadable account picked because nothing better is known says so,
// rather than claiming it has the most room.
func TestPickingAnUnreadableAccountSaysWhy(t *testing.T) {
	d := Pick([]Candidate{
		{Name: "support", Usage: use(100, 79)},
		{Name: "matt", Reason: "token expired"},
	}, Request{Now: now})
	if d.Account != "matt" || !strings.Contains(d.Why, "can't be read (token expired)") {
		t.Errorf("pick = %s (%s)", d.Account, d.Why)
	}
}
