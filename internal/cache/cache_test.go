package cache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
)

// isolate points the shared cache at a temp HOME, so a test never reads or
// writes the real ~/.ticketdeck/tickets.json — which the decks on this machine
// are using while the tests run.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// counter is a live fetch that records how many times it was actually called —
// the whole point of the package is that this number stays at 1.
type counter struct {
	calls int
	val   []string
	err   error
}

func (c *counter) fetch(context.Context) ([]string, error) {
	c.calls++
	return c.val, c.err
}

func TestSecondDeckServesTheCacheInsteadOfCallingLinear(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"ZEN-1"}}

	first, err := get(context.Background(), slotIssues, live.fetch)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	second, err := get(context.Background(), slotIssues, live.fetch)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if live.calls != 1 {
		t.Errorf("two decks should share one call, got %d", live.calls)
	}
	if len(second) != 1 || second[0] != first[0] {
		t.Errorf("cached read should match the fetched one: %v vs %v", second, first)
	}
}

// Each slot has its own interval; the issue list going stale must not drag the
// project list to Linear with it.
func TestSlotsExpireIndependently(t *testing.T) {
	isolate(t)
	issues := &counter{val: []string{"ZEN-1"}}
	projects := &counter{val: []string{"proj"}}

	if _, err := get(context.Background(), slotIssues, issues.fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := get(context.Background(), slotProjects, projects.fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := get(context.Background(), slotIssues, issues.fetch); err != nil {
		t.Fatal(err)
	}

	if issues.calls != 1 || projects.calls != 1 {
		t.Errorf("each slot should have called once, got issues=%d projects=%d", issues.calls, projects.calls)
	}
	if got := readCache()[slotProjects]; len(got.Data) == 0 {
		t.Error("the project slot should have survived the issue slot's read")
	}
}

// The bug this package is meant to end: a deck that starts while the key is
// throttled used to render an empty list, because it had no last good list of
// its own to fall back on. Another deck's is better than nothing.
func TestAFailedFetchFallsBackToWhatAnotherDeckCached(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"ZEN-1"}}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatal(err)
	}
	expire(time.Now()) // force the next read past the cache

	broken := &counter{err: &linear.RateLimitError{RetryAfter: "3600"}}
	got, err := get(context.Background(), slotIssues, broken.fetch)
	if err != nil {
		t.Fatalf("a throttled fetch with a cached list should not error: %v", err)
	}
	if len(got) != 1 || got[0] != "ZEN-1" {
		t.Errorf("should have served the cached list, got %v", got)
	}
}

// With nothing cached anywhere there is nothing to serve, and the deck must be
// told — a silent empty list reads as "no tickets assigned to you".
func TestAFailedFetchWithNothingCachedReportsTheError(t *testing.T) {
	isolate(t)
	broken := &counter{err: errors.New("offline")}
	if _, err := get(context.Background(), slotIssues, broken.fetch); err == nil {
		t.Error("a failed fetch with an empty cache should surface the error")
	}
}

// A 429 is a property of the shared key, so every deck inherits the wait
// through the cache rather than each discovering it with its own request.
func TestRateLimitBacksOffForEveryDeck(t *testing.T) {
	isolate(t)
	broken := &counter{err: &linear.RateLimitError{RetryAfter: "3600"}}
	if _, err := get(context.Background(), slotIssues, broken.fetch); err == nil {
		t.Fatal("expected the rate-limit error")
	}

	e := readCache()[slotIssues]
	if e.Reason == "" {
		t.Error("the cache should record why the last attempt failed")
	}
	wait := time.Until(e.RetryAt)
	if wait <= Every {
		t.Errorf("a 429 should back off further than the normal interval, got %s", wait)
	}
	if wait > maxBackoff {
		t.Errorf("Retry-After: 3600 should be capped at %s, got %s", maxBackoff, wait)
	}
}

func TestRetryInHonoursRetryAfterUpToTheCap(t *testing.T) {
	isolate(t)
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"ordinary failure", errors.New("offline"), Every},
		{"429 with no header", &linear.RateLimitError{}, defaultBackoff},
		{"429 with a short wait", &linear.RateLimitError{RetryAfter: "90"}, 90 * time.Second},
		{"429 with an hour", &linear.RateLimitError{RetryAfter: "3600"}, maxBackoff},
	}
	for _, c := range cases {
		if got := retryIn(c.err); got != c.want {
			t.Errorf("%s: retryIn = %s, want %s", c.name, got, c.want)
		}
	}
}

// A write is followed immediately by a refresh. Served the pre-write list, the
// row would appear to snap back to the state it was just moved out of.
func TestASuccessfulWriteExpiresTheCache(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"ZEN-1"}}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatal(err)
	}

	if err := expireAfter(nil); err != nil {
		t.Fatalf("expireAfter should pass a nil error through: %v", err)
	}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatal(err)
	}
	if live.calls != 2 {
		t.Errorf("the refresh after a write should reach Linear, calls=%d", live.calls)
	}
}

// A write that failed changed nothing, so throwing the list away would only
// spend a request re-reading what is already there.
func TestAFailedWriteLeavesTheCacheAlone(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"ZEN-1"}}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatal(err)
	}

	want := errors.New("linear rejected the move")
	if err := expireAfter(want); !errors.Is(err, want) {
		t.Fatalf("expireAfter should pass the error through, got %v", err)
	}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatal(err)
	}
	if live.calls != 1 {
		t.Errorf("a failed write should not force a re-fetch, calls=%d", live.calls)
	}
}

// The mirror of the in-flight case: a fetch issued after the write carries the
// change already, so store must accept it rather than turning it away with the
// same NotBefore that rejects the stale one.
func TestAFetchIssuedAfterTheWriteIsAccepted(t *testing.T) {
	isolate(t)
	wrote := time.Now()
	store(slotIssues, entry{Data: []byte(`["before"]`), FetchedAt: wrote.Add(-time.Minute)})
	expire(wrote)

	after := wrote.Add(time.Second)
	store(slotIssues, entry{Data: []byte(`["after"]`), FetchedAt: after, RetryAt: after.Add(Every)})

	e := readCache()[slotIssues]
	if string(e.Data) != `["after"]` {
		t.Errorf("a fetch that began after the write should be stored, got %s", e.Data)
	}
	if e.RetryAt.IsZero() {
		t.Error("and it should carry its own interval")
	}
}

// A deck killed mid-poll — which is how `deck` restarts them — must not park
// the ticket list for every other deck on the box.
func TestAnAbandonedLockIsTakenOver(t *testing.T) {
	isolate(t)
	lp := cachePath() + "." + slotIssues + ".lock"
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lp, old, old); err != nil {
		t.Fatal(err)
	}

	release := takeLock(slotIssues, time.Now())
	if release == nil {
		t.Fatal("a stale lock should be taken over, not waited on")
	}
	release()
}

func TestALiveLockIsRespected(t *testing.T) {
	isolate(t)
	release := takeLock(slotIssues, time.Now())
	if release == nil {
		t.Fatal("the first deck should get the lock")
	}
	defer release()

	if takeLock(slotIssues, time.Now()) != nil {
		t.Error("a second deck should not fetch while the first holds the lock")
	}
}

// Holding the lock must not stop a deck rendering: anything cached beats
// waiting on the deck that is fetching.
func TestALockedOutDeckStillServesItsCachedList(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"ZEN-1"}}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatal(err)
	}
	expire(time.Now())

	release := takeLock(slotIssues, time.Now())
	if release == nil {
		t.Fatal("could not take the lock")
	}
	defer release()

	got, err := get(context.Background(), slotIssues, live.fetch)
	if err != nil {
		t.Fatalf("a locked-out deck should still render: %v", err)
	}
	if len(got) != 1 || got[0] != "ZEN-1" {
		t.Errorf("should have served the cached list, got %v", got)
	}
	if live.calls != 1 {
		t.Errorf("a locked-out deck should not have called Linear, calls=%d", live.calls)
	}
}

// A cold deck locked out with nothing cached waits for the fetching deck, then
// gives up and fetches rather than showing an empty list forever.
func TestAColdLockedOutDeckFallsThroughRatherThanShowingNothing(t *testing.T) {
	isolate(t)
	release := takeLock(slotIssues, time.Now())
	if release == nil {
		t.Fatal("could not take the lock")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stand in for coldWait elapsing, so the test doesn't sleep for it

	live := &counter{val: []string{"ZEN-1"}}
	got, err := get(ctx, slotIssues, live.fetch)
	if err != nil {
		t.Fatalf("a cold locked-out deck should still end up with a list: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected the fallback fetch's list, got %v", got)
	}
	if live.calls != 1 {
		t.Errorf("expected exactly one fallback call, got %d", live.calls)
	}
}

// The cache is read by other processes mid-write, and they take no lock. A
// half-written file must never be visible.
func TestTheCacheFileIsReplacedAtomically(t *testing.T) {
	isolate(t)
	store(slotIssues, entry{Data: []byte(`["ZEN-1"]`), FetchedAt: time.Now()})

	dir := filepath.Dir(cachePath())
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != filepath.Base(cachePath()) {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

// The case that started all this: every deck cold, the key already throttled,
// nothing cached to fall back on. Each deck must inherit the backoff from the
// one that caught the 429 rather than going to find out for itself — piling
// onto the throttle is what keeps the key pinned.
func TestAThrottledEmptyCacheStopsTheOtherDecksCalling(t *testing.T) {
	isolate(t)
	first := &counter{err: &linear.RateLimitError{RetryAfter: "3600"}}
	if _, err := get(context.Background(), slotIssues, first.fetch); err == nil {
		t.Fatal("expected the rate-limit error")
	}

	second := &counter{err: &linear.RateLimitError{RetryAfter: "3600"}}
	if _, err := get(context.Background(), slotIssues, second.fetch); err == nil {
		t.Error("a deck inside the backoff should still report the failure")
	}
	if second.calls != 0 {
		t.Errorf("a deck inside the backoff should not call Linear, calls=%d", second.calls)
	}
}

// Past the interval the cache stops answering, however good its data — the
// backoff is a pause, not a way to pin a stale list on screen forever.
func TestServedStopsAnsweringOnceTheIntervalPasses(t *testing.T) {
	now := time.Now()
	fresh := entry{Data: []byte(`["ZEN-1"]`), RetryAt: now.Add(Every)}
	if _, _, ok := served[[]string](fresh, now); !ok {
		t.Error("a slot inside its interval should answer from cache")
	}
	stale := entry{Data: []byte(`["ZEN-1"]`), RetryAt: now.Add(-time.Second)}
	if _, _, ok := served[[]string](stale, now); ok {
		t.Error("a slot past its interval should send the caller to Linear")
	}
}

// ── review findings (PR #12) ─────────────────────────────────────────────────

// The deck fires both reads in one batch, so they run concurrently in the same
// process. Sharing one lock made them race, and the loser sat out coldWait
// waiting on a peer that was filling the other slot — a ten-second stall ending
// in the unlocked fetch this package exists to prevent.
func TestOneSlotsFetchDoesNotBlockTheOther(t *testing.T) {
	isolate(t)
	release := takeLock(slotIssues, time.Now())
	if release == nil {
		t.Fatal("could not take the issues lock")
	}
	defer release()

	other := takeLock(slotProjects, time.Now())
	if other == nil {
		t.Fatal("the projects slot should have a lock of its own")
	}
	other() // release it again; the point was that it was available at all

	projects := &counter{val: []string{"proj"}}
	got, err := get(context.Background(), slotProjects, projects.fetch)
	if err != nil {
		t.Fatalf("the projects read should not wait on the issues lock: %v", err)
	}
	if len(got) != 1 || projects.calls != 1 {
		t.Errorf("expected one direct projects fetch, got %v calls=%d", got, projects.calls)
	}
}

// A peer that caught a 429 publishes a reason and no data. A waiter that
// accepted only data sat out the rest of coldWait and then called Linear
// itself — every waiting deck piling onto the throttle the peer had recorded.
func TestAWaiterInheritsThePeersBackoff(t *testing.T) {
	isolate(t)
	store(slotIssues, entry{
		Reason:  "linear: rate limited (retry after 3600)",
		RetryAt: time.Now().Add(maxBackoff),
	})

	v, err, ok := waitForPeer[[]string](context.Background(), slotIssues)
	if !ok {
		t.Fatal("a recorded backoff is an answer; the waiter should take it")
	}
	if err == nil {
		t.Error("the waiter should inherit the peer's failure, not an empty list")
	}
	if v != nil {
		t.Errorf("no data was published, so none should come back: %v", v)
	}
}

// expire runs the moment a write lands, but a fetch already in flight lands
// after it. Re-publishing that pre-write list with a full interval ahead of it
// is the snap-back expiring the cache was meant to prevent.
func TestAnInFlightFetchCannotUndoAnExpiry(t *testing.T) {
	isolate(t)
	started := time.Now()
	store(slotIssues, entry{Data: []byte(`["before"]`), FetchedAt: started, RetryAt: started.Add(Every)})

	expire(started.Add(time.Second)) // the write lands while the next fetch is out

	// That in-flight fetch now returns, carrying data read before the write.
	store(slotIssues, entry{
		Data:      []byte(`["stale"]`),
		FetchedAt: started,
		RetryAt:   started.Add(Every),
	})

	e := readCache()[slotIssues]
	if string(e.Data) == `["stale"]` {
		t.Error("a payload read before the write should not be installed")
	}
	if !e.RetryAt.IsZero() {
		t.Error("the late arrival should not have restored the interval the expiry cleared")
	}
	live := &counter{val: []string{"after"}}
	got, err := get(context.Background(), slotIssues, live.fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "after" {
		t.Errorf("the read after a write should reach Linear, got %v", got)
	}
}

// Only the payload is dropped. A 429 caught by that losing fetch still has to
// be recorded, or the backoff it discovered goes unnoticed.
func TestALateFetchStillRecordsItsFailure(t *testing.T) {
	isolate(t)
	started := time.Now()
	store(slotIssues, entry{Data: []byte(`["before"]`), FetchedAt: started, RetryAt: started.Add(Every)})
	expire(started.Add(time.Second))

	store(slotIssues, entry{
		Data:      []byte(`["stale"]`),
		FetchedAt: started,
		Reason:    "linear: rate limited (retry after 3600)",
		RetryAt:   time.Now().Add(maxBackoff),
	})

	e := readCache()[slotIssues]
	if string(e.Data) == `["stale"]` {
		t.Errorf("the stale payload should not be installed, got %s", e.Data)
	}
	if e.Reason == "" {
		t.Error("the failure that fetch discovered should survive")
	}
}

// ── second review round (PR #12) ─────────────────────────────────────────────

// Making the poll lock per slot let both slots' writers run at once, and each
// read-modify-writes the whole file. Without a second, short lock around that,
// the later writer's snapshot predates the earlier one and drops its slot —
// costing a re-fetch of a list that had just been paid for.
func TestConcurrentSlotWritesKeepBothSlots(t *testing.T) {
	isolate(t)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slot := slotIssues
			if i%2 == 1 {
				slot = slotProjects
			}
			store(slot, entry{
				Data:      []byte(`["x"]`),
				FetchedAt: time.Now(),
				RetryAt:   time.Now().Add(Every),
			})
		}(i)
	}
	wg.Wait()

	slots := readCache()
	for _, slot := range []string{slotIssues, slotProjects} {
		if len(slots[slot].Data) == 0 {
			t.Errorf("%s was dropped by a concurrent write to the other slot", slot)
		}
	}
}

// A payload rejected for being pre-write must not take the slot down with it.
// Blanking Data removes the fallback that keeps a cold deck off an empty
// screen, and zeroing RetryAt throws away the backoff this very call found —
// served stops answering the moment RetryAt passes.
func TestARejectedPayloadKeepsTheListAndTheBackoff(t *testing.T) {
	isolate(t)
	started := time.Now()
	store(slotIssues, entry{Data: []byte(`["good"]`), FetchedAt: started, RetryAt: started.Add(Every)})
	expire(started.Add(time.Second))

	// That in-flight fetch returns late, carrying stale data and a fresh 429.
	backoff := time.Now().Add(maxBackoff)
	store(slotIssues, entry{
		Data:      []byte(`["stale"]`),
		FetchedAt: started,
		Reason:    "linear: rate limited (retry after 3600)",
		RetryAt:   backoff,
	})

	e := readCache()[slotIssues]
	if string(e.Data) != `["good"]` {
		t.Errorf("the last good list should survive, got %s", e.Data)
	}
	if e.RetryAt.IsZero() {
		t.Error("the backoff that call discovered should be recorded, not zeroed")
	}
	if v, _, ok := served[[]string](e, time.Now()); !ok || len(v) != 1 || v[0] != "good" {
		t.Errorf("a deck inside the backoff should be served the last good list, got %v ok=%v", v, ok)
	}
}

// UnblockToTriage moves dependents one at a time and can return an error with
// some already moved. Keying the expiry on the error alone served the
// pre-cascade list to the refresh that follows, and the tickets it did unblock
// appeared to revert.
func TestAPartialUnblockStillExpiresTheCache(t *testing.T) {
	isolate(t)
	started := time.Now()
	store(slotIssues, entry{Data: []byte(`["before"]`), FetchedAt: started, RetryAt: started.Add(Every)})

	// The shape UnblockToTriage returns when it moved some and then failed.
	keys, err := []string{"ZEN-1"}, errors.New("linear rejected the third move")
	if len(keys) > 0 {
		expire(time.Now())
	} else {
		_ = expireAfter(err)
	}

	if !readCache()[slotIssues].RetryAt.IsZero() {
		t.Error("a cascade that moved anything should expire the cache")
	}
}

// ── round 3 ──────────────────────────────────────────────────────────────────

// The cold-start shape of the bug: a deck holds the lock, its call fails slowly,
// and a peer that gave up waiting publishes a good list in the meantime. The
// failure's write carries the snapshot it took before calling — nothing at all —
// and used to overwrite the peer's list, leaving every deck blank and throttled.
func TestALateFailureDoesNotWipeAPeersList(t *testing.T) {
	isolate(t)

	// The peer's successful publish, made after the failing deck started.
	started := time.Now()
	storeLocked(slotIssues, entry{
		Data:      mustJSON(t, []string{"ZEN-1"}),
		FetchedAt: started.Add(time.Second),
		RetryAt:   started.Add(time.Second + Every),
	})

	// The lock holder's failure, written with its own pre-call snapshot (empty).
	storeLocked(slotIssues, entry{
		FetchedAt: started,
		Reason:    "429 Too Many Requests",
		RetryAt:   time.Now().Add(defaultBackoff),
	})

	got := readCache()[slotIssues]
	list, ok := decode[[]string](got)
	if !ok || len(list) != 1 || list[0] != "ZEN-1" {
		t.Fatalf("the peer's list should survive a late failure, got %q", string(got.Data))
	}
	if got.Reason == "" {
		t.Error("the failure still has to be recorded")
	}
}

// The same rule with data on both sides: a failing deck's stale snapshot must
// not roll the slot back to an older list than the one already published.
func TestALateFailureDoesNotRollTheListBack(t *testing.T) {
	isolate(t)
	old, recent := time.Now().Add(-2*time.Minute), time.Now()

	storeLocked(slotIssues, entry{Data: mustJSON(t, []string{"new"}), FetchedAt: recent, RetryAt: recent.Add(Every)})
	storeLocked(slotIssues, entry{
		Data:      mustJSON(t, []string{"old"}),
		FetchedAt: old,
		Reason:    "timeout",
		RetryAt:   time.Now().Add(defaultBackoff),
	})

	list, _ := decode[[]string](readCache()[slotIssues])
	if len(list) != 1 || list[0] != "new" {
		t.Errorf("the newer list should stand, got %v", list)
	}
}

// A successful fetch still has to land — the guard above must not turn into a
// blanket refusal to ever replace the slot.
func TestANewerSuccessStillReplacesTheList(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"first"}}

	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	expire(time.Now())
	live.val = []string{"second"}
	got, err := get(context.Background(), slotIssues, live.fetch)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if len(got) != 1 || got[0] != "second" {
		t.Errorf("a fresh fetch should replace the list, got %v", got)
	}
	if live.calls != 2 {
		t.Errorf("expire should have sent the second read live, calls=%d", live.calls)
	}
}

// Pressing the deck's refresh key means "show me now". Without Refresh the
// pooled read answers from the shared list for up to Every, so the key does
// nothing at all for a minute.
func TestRefreshSendsTheNextReadLive(t *testing.T) {
	isolate(t)
	live := &counter{val: []string{"ZEN-1"}}
	c := Wrap(nil)

	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatalf("pooled read: %v", err)
	}
	if live.calls != 1 {
		t.Fatalf("the pooled read should not have called live, calls=%d", live.calls)
	}

	c.Refresh()
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatalf("forced fetch: %v", err)
	}
	if live.calls != 2 {
		t.Errorf("Refresh should send the next read live, calls=%d", live.calls)
	}
}

// The throttle is not the user's to clear. A 429 backoff is a property of the
// shared key, so leaning on the refresh key must not put the decks back on it.
func TestRefreshLeavesAFailureBackoffAlone(t *testing.T) {
	isolate(t)
	retryAt := time.Now().Add(defaultBackoff)
	storeLocked(slotIssues, entry{
		Data:      mustJSON(t, []string{"ZEN-1"}),
		FetchedAt: time.Now(),
		Reason:    "429 Too Many Requests",
		RetryAt:   retryAt,
	})

	Wrap(nil).Refresh()

	if got := readCache()[slotIssues]; !got.RetryAt.Equal(retryAt) {
		t.Errorf("a failure backoff must survive Refresh: %v vs %v", got.RetryAt, retryAt)
	}
	live := &counter{val: []string{"nope"}}
	if _, err := get(context.Background(), slotIssues, live.fetch); err != nil {
		t.Fatalf("read after refresh: %v", err)
	}
	if live.calls != 0 {
		t.Errorf("the backoff should still hold the read off Linear, calls=%d", live.calls)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
