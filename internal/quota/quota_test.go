package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every test here runs with a temp HOME so it reads and writes its own cache,
// and every account it polls has credentials that fail locally (missing or
// expired). Nothing in this package's tests may reach the network: the usage
// endpoint is rate-limited per subscription, and a test suite that spends that
// budget breaks the tool it is testing.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	return home
}

func writeCacheFile(t *testing.T, home string, entries map[string]Entry) {
	t.Helper()
	p := filepath.Join(home, ".ticketdeck", "usage.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(struct {
		Accounts map[string]Entry `json:"accounts"`
	}{entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeCreds writes a credentials file whose token expires at exp.
func writeCreds(t *testing.T, dir string, exp time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-test","refreshToken":"rt-test","expiresAt":%d}}`, exp.UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A reading that hasn't aged out is served from the cache without a request.
// This is what makes three decks cost one poll instead of three: the endpoint's
// budget is shared with every Claude Code status line on the machine, and the
// decks arriving together is what turned the usage line into "unavailable".
func TestPollServesFreshCacheWithoutFetching(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	writeCacheFile(t, home, map[string]Entry{
		"support": {Usage: &Usage{FiveHourPct: 12}, FetchedAt: now, RetryAt: now.Add(Every)},
	})
	// A config dir with no credentials: any attempt to fetch would replace the
	// reading with a failure reason, so an intact reading proves none was made.
	got := Poll(context.Background(), []Account{{Name: "support", ConfigDir: filepath.Join(home, "nope")}}, now)
	if e := got["support"]; e.Usage == nil || e.Usage.FiveHourPct != 12 || e.Reason != "" {
		t.Errorf("fresh cache should be served untouched, got %+v", e)
	}
}

// One account's 429 must not silence the others. It used to: a single 429
// pushed one global timer out 15 minutes, so the line went blank for every
// subscription — including the one with headroom you were checking for.
func TestBackoffIsPerAccount(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	writeCacheFile(t, home, map[string]Entry{
		"throttled": {Usage: &Usage{FiveHourPct: 80}, FetchedAt: now.Add(-time.Hour), Reason: "rate limited", RetryAt: now.Add(backoff)},
	})
	got := Poll(context.Background(), []Account{
		{Name: "throttled", ConfigDir: filepath.Join(home, "nope")},
		{Name: "other", ConfigDir: filepath.Join(home, "also-nope")},
	}, now)
	if e := got["throttled"]; e.Usage == nil || e.Usage.FiveHourPct != 80 {
		t.Errorf("throttled account should keep its last reading, got %+v", e)
	}
	if e := got["other"]; e.Reason != "not signed in" {
		t.Errorf("the other account should have been read this round, got %+v", e)
	}
}

// Several due accounts are read concurrently, and a credentials file that fails
// locally returns immediately — so the fetches used to be writing the cache map
// while the spawn loop still read it. Run under -race; without it this is a
// "concurrent map read and map write" fatal error, which kills the deck.
func TestPollReadsManyAccountsWithoutRacing(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	var accts []Account
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		accts = append(accts, Account{Name: name, ConfigDir: filepath.Join(home, "nope-"+name)})
	}
	got := Poll(context.Background(), accts, now)
	for _, a := range accts {
		if got[a.Name].Reason != "not signed in" {
			t.Fatalf("%s: reason = %q, want every account read", a.Name, got[a.Name].Reason)
		}
	}
}

// An idle subscription's token expires and only its own Claude Code refreshes
// it — so the deck detects that locally and skips a request that could only
// come back 401.
func TestExpiredTokenSkipsTheRequest(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	dir := writeCreds(t, filepath.Join(home, ".claude-idle"), now.Add(-time.Hour))
	got := Poll(context.Background(), []Account{{Name: "idle", ConfigDir: dir}}, now)
	e := got["idle"]
	if e.Reason != "token expired" {
		t.Fatalf("reason = %q, want \"token expired\"", e.Reason)
	}
	if wait := e.RetryAt.Sub(now); wait > Every {
		t.Errorf("recheck in %v — a local check costs no request, so it shouldn't wait a full interval", wait)
	}
}

// A 401 and a locally-expired token read the same on the row but must not wait
// the same: the local check is free, so it rechecks in a minute, while a rejected
// token already cost a request and goes back on the normal interval.
func TestRejectedTokenWaitsTheNormalInterval(t *testing.T) {
	if got := retryIn(ErrUnauthorized); got < Every {
		t.Errorf("retryIn(ErrUnauthorized) = %v, want at least Every (%v)", got, Every)
	}
	if got := retryIn(ErrTokenExpired); got >= Every {
		t.Errorf("a free local check shouldn't wait a full interval, got %v", got)
	}
	if reasonFor(ErrUnauthorized) != reasonFor(ErrTokenExpired) {
		t.Errorf("both need the same re-login, so both should read %q", reasonFor(ErrTokenExpired))
	}
}

// A valid-looking token must not be skipped: the expiry guard is there to save
// doomed requests, not to stop the real one.
// Logged out leaves the file in place with both tokens cleared. A session
// launched there stops at its first message asking for /login, so this must
// read as signed out, not as an idle account whose token will refresh.
func TestClearedCredentialsReadAsSignedOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), dir); !errors.Is(err, ErrSignedOut) || reasonFor(err) != "not signed in" {
		t.Errorf("err = %v (%q), want not signed in", err, reasonFor(err))
	}
	// An expired token with a refresh token is just an idle account.
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), dir); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want token expired", err)
	}
}

// macOS keeps credentials in the keychain, so the file is missing there by
// design. That must read as unreadable usage — still usable — not as signed
// out, which would rule every account out.
func TestMissingCredentialsOnMacOSAreNotSignedOut(t *testing.T) {
	old := goos
	t.Cleanup(func() { goos = old })
	dir := t.TempDir()
	goos = "darwin"
	if _, err := Fetch(context.Background(), dir); reasonFor(err) == "not signed in" {
		t.Errorf("macOS: %v reads as signed out", err)
	}
	goos = "linux"
	if _, err := Fetch(context.Background(), dir); reasonFor(err) != "not signed in" {
		t.Errorf("linux: %v (%q), want not signed in", err, reasonFor(err))
	}
}

func TestUnexpiredTokenIsUsed(t *testing.T) {
	home := tempHome(t)
	dir := writeCreds(t, filepath.Join(home, ".claude-live"), time.Now().Add(time.Hour))
	if _, err := oauthToken(dir); err != nil {
		t.Errorf("token with an hour left was rejected: %v", err)
	}
}

// While one deck polls, the others serve the cache rather than duplicating the
// call. The simultaneous restart of every deck on the machine is exactly when
// this matters — that burst is what tripped the throttle.
func TestSecondDeckDoesNotPollWhileLockIsHeld(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	lock := filepath.Join(home, ".ticketdeck", "usage.json.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Poll(context.Background(), []Account{{Name: "support", ConfigDir: filepath.Join(home, "nope")}}, now)
	if e := got["support"]; e.Reason != "no reading yet" {
		t.Errorf("a deck that lost the lock should not have fetched, got %+v", e)
	}
}

// A deck killed mid-poll leaves its lock behind, and decks are killed mid-poll
// routinely — `deck` restarts them that way after an update. The lock must not
// outlive the process that took it.
func TestStaleLockIsTakenOver(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	lock := filepath.Join(home, ".ticketdeck", "usage.json.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-lockStale - time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	got := Poll(context.Background(), []Account{{Name: "support", ConfigDir: filepath.Join(home, "nope")}}, now)
	if e := got["support"]; e.Reason != "not signed in" {
		t.Errorf("stale lock should have been taken over, got %+v", e)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("lock should be released after the poll")
	}
}

// A lock that can't be created is not contention. Reading it as contention would
// stop every deck from ever polling again, so an unwritable ~/.ticketdeck costs a
// duplicate request rather than the whole feature.
func TestUnwritableCacheDirStillPolls(t *testing.T) {
	home := tempHome(t)
	// A regular file where the cache directory belongs: MkdirAll can't proceed.
	if err := os.WriteFile(filepath.Join(home, ".ticketdeck"), []byte("not a dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Poll(context.Background(), []Account{{Name: "support", ConfigDir: filepath.Join(home, "nope")}}, time.Now())
	if e := got["support"]; e.Reason != "not signed in" {
		t.Errorf("account should still have been read, got %+v", e)
	}
}

// The cache is what a restarting deck reads instead of re-polling, so a poll
// has to leave one behind.
func TestPollPersistsWhatItLearned(t *testing.T) {
	home := tempHome(t)
	now := time.Now()
	Poll(context.Background(), []Account{{Name: "support", ConfigDir: filepath.Join(home, "nope")}}, now)
	e := readCache()["support"]
	if e.Reason != "not signed in" {
		t.Fatalf("failure not persisted, got %+v", e)
	}
	if !e.RetryAt.After(now) {
		t.Error("persisted entry should carry when to try again")
	}
}

func TestCurrentForgetsWindowsThatHaveReset(t *testing.T) {
	now := time.Now()
	e := Entry{FetchedAt: now.Add(-3 * time.Hour), Usage: &Usage{
		FiveHourPct: 97, FiveHourReset: now.Add(-time.Hour),
		SevenDayPct: 60, SevenDayReset: now.Add(48 * time.Hour),
	}}
	c := e.Current(now)
	if c == nil || c.FiveHourPct != 0 || c.SevenDayPct != 60 {
		t.Errorf("Current = %+v, want the 5h window started over and the week kept", c)
	}
	if e.Usage.FiveHourPct != 97 {
		t.Error("Current must not change the stored reading")
	}
	// Taken before this week began: it says nothing about now.
	e.Usage.SevenDayReset = now.Add(-time.Hour)
	if c := e.Current(now); c != nil {
		t.Errorf("Current = %+v, want nil for last week's reading", c)
	}
}

func TestStaleMarksOldReadingsOnly(t *testing.T) {
	now := time.Now()
	fresh := Entry{Usage: &Usage{}, FetchedAt: now.Add(-Every)}
	old := Entry{Usage: &Usage{}, FetchedAt: now.Add(-3 * Every)}
	if fresh.Stale(now) {
		t.Error("a reading one interval old is not stale")
	}
	if !old.Stale(now) {
		t.Error("a reading three intervals old is stale")
	}
	if (Entry{Reason: "rate limited"}).Stale(now) {
		t.Error("an entry with no reading at all isn't a stale reading")
	}
}

// The deck's reading of matt on 2026-10-22 04:00 UTC: 82% used with 4.7 days
// of its week left is 82% in 2.3 days, so the 18% left goes in about half a
// day.
func TestWeekRunsOutAtThisPace(t *testing.T) {
	read := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	matt := Entry{FetchedAt: read, Usage: &Usage{SevenDayPct: 82, SevenDayReset: read.Add(time.Duration(4.7 * 24 * float64(time.Hour)))}}
	got, ok := matt.WeekRunsOut(read.Add(time.Minute))
	if !ok {
		t.Fatal("matt should run out before its reset")
	}
	if h := got.Sub(read).Hours(); h < 11 || h > 13 {
		t.Errorf("matt runs out %.1fh after the reading, want ~12h", h)
	}
	// A week spent slowly lasts to the reset.
	calm := Entry{FetchedAt: read, Usage: &Usage{SevenDayPct: 20, SevenDayReset: read.Add(3 * 24 * time.Hour)}}
	if _, ok := calm.WeekRunsOut(read); ok {
		t.Error("20% in four days lasts the week")
	}
	// Too early in the week to have a pace.
	fresh := Entry{FetchedAt: read, Usage: &Usage{SevenDayPct: 30, SevenDayReset: read.Add(7*24*time.Hour - 2*time.Hour)}}
	if _, ok := fresh.WeekRunsOut(read); ok {
		t.Error("two hours into the week is no pace to project")
	}
	// Spent is out now.
	spent := Entry{FetchedAt: read, Usage: &Usage{SevenDayPct: 100, SevenDayReset: read.Add(24 * time.Hour)}}
	if got, ok := spent.WeekRunsOut(read); !ok || !got.Equal(read) {
		t.Errorf("a spent week runs out at its reading, got %v %v", got, ok)
	}
	// A week renewed since the reading has no forecast.
	if _, ok := matt.WeekRunsOut(read.Add(5 * 24 * time.Hour)); ok {
		t.Error("the week renewed since the reading")
	}
}

// The endpoint is undocumented. An answer without the two windows is a change
// to it, and must read as one rather than as two empty windows.
func TestFetchNoticesAChangedAnswer(t *testing.T) {
	tempHome(t)
	cfg := t.TempDir()
	creds := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(cfg, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"five_hour":{"utilization":12,"resets_at":"2026-10-22T20:00:00+00:00"},"seven_day":{"utilization":40,"resets_at":"2026-10-25T17:00:00+00:00"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	old := usageURL
	usageURL = srv.URL
	t.Cleanup(func() { usageURL = old })

	u, err := Fetch(context.Background(), cfg)
	if err != nil || u.FiveHourPct != 12 || u.SevenDayPct != 40 {
		t.Fatalf("Fetch = %+v, %v", u, err)
	}
	body = `{"limits":[{"window":"5h","used":12}]}`
	if _, err := Fetch(context.Background(), cfg); !errors.Is(err, ErrFormat) {
		t.Errorf("a changed answer = %v, want ErrFormat", err)
	}
	if got := reasonFor(ErrFormat); got != ReasonFormat {
		t.Errorf("reason = %q", got)
	}
}
