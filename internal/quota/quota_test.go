package quota

import (
	"context"
	"encoding/json"
	"fmt"
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
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-test","expiresAt":%d}}`, exp.UnixMilli())
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
