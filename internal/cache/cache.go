// Package cache pools the deck's two polled Linear reads across every deck on
// the machine.
//
// Every deck shows the same tickets. There is one LINEAR_API_KEY, one
// workspace, one assignee; the decks differ only in which Claude subscription
// runs the sessions, which the local backend answers and Linear knows nothing
// about. Left to poll independently, N decks make N times the requests against
// one hourly budget of 3,000,000 complexity points, and the assigned-issues
// query — 50 issues, each with its attachments, labels and blocking relations —
// is by far the most expensive thing the deck asks for. Measured on one
// workspace on 2026-09-14, a single deck consumed about half the budget's
// refill rate, so two decks sat at break-even and three or more stayed
// throttled indefinitely. One deck per interval now calls Linear, and the
// others read what it wrote.
//
// This is the pooling internal/quota does for Claude's usage endpoint, for the
// same reason and with the same lock. It differs in one way that matters: a
// blank quota line is a cosmetic loss, but a deck with no ticket list is the
// whole app, so a cold deck waits briefly for the fetching deck rather than
// showing an empty list.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
)

// Every is how long a fetch is served before Linear is asked again. It is a
// machine-wide interval, not a per-deck one, so the deck's own tick can stay at
// a minute: four decks ticking no longer mean four requests.
const Every = 60 * time.Second

// lockStale is when a poll lock is treated as abandoned. Comfortably longer
// than the caller's HTTP timeout, and `deck` kills decks mid-poll by design, so
// a lock must never outlive the process that took it for long.
const lockStale = 2 * time.Minute

// coldWait is how long a deck with nothing cached waits for the deck that holds
// the lock to publish. `deck` restarts every deck on the box at once, so a cold
// start is normally a crowd: without this they would each either fetch (the
// stampede this package exists to stop) or render "no open tickets assigned to
// you", which is a lie about an empty screen.
const coldWait = 10 * time.Second

// coldPoll is how often the waiting deck re-reads the cache file.
const coldPoll = 200 * time.Millisecond

// The write lock around store and expire covers a file read and a rename, so it
// is measured in milliseconds, not the minutes the poll lock allows for.
const (
	writeLockWait  = 2 * time.Second
	writeLockPoll  = 5 * time.Millisecond
	writeLockStale = 5 * time.Second
)

// defaultBackoff is the wait after a 429 that names no Retry-After.
const defaultBackoff = 2 * time.Minute

// maxBackoff caps what a Retry-After can push the whole machine to. Linear
// answers 3600 on a spent complexity budget, but the budget behaved as a leaky
// bucket when measured: it refilled at ~50,000 points a minute against a pooled
// fetch costing ~25,000, so a minute of quiet cleared a throttle that
// Retry-After called an hour (one workspace, 2026-09-14). Honouring 3600
// literally would leave every deck showing an hour-old list to wait out a
// budget that refilled in two.
const maxBackoff = 5 * time.Minute

// slot names — the two reads the deck makes on a timer. The single-issue and
// comment fetches are user-initiated and rare, so they stay unpooled.
const (
	slotIssues   = "assigned_open"
	slotProjects = "my_projects"
)

// Client is a linear.Client whose two polled reads are pooled. The embedded
// client is deliberate: tui.New type-asserts its Fetcher for status writes,
// assignee changes, comments and project writes, so a wrapper that did not
// promote those methods would silently take half the deck's features away.
type Client struct{ *linear.Client }

// Wrap pools c's polled reads. Writes go straight through and then expire the
// cache, so the refresh a write triggers sees the change rather than the list
// from before it.
func Wrap(c *linear.Client) *Client { return &Client{c} }

func (c *Client) FetchAssignedOpen(ctx context.Context) ([]linear.Issue, error) {
	return get(ctx, slotIssues, c.Client.FetchAssignedOpen)
}

func (c *Client) FetchMyProjects(ctx context.Context) ([]linear.Project, error) {
	return get(ctx, slotProjects, c.Client.FetchMyProjects)
}

// ── writes ───────────────────────────────────────────────────────────────────
// Each one calls through and then expires the cache. Without this, the refresh
// the deck fires straight after a write would be served the list from before
// it, and the row would appear to snap back to its old state.

func (c *Client) MoveState(ctx context.Context, issue linear.Issue, target string) error {
	return expireAfter(c.Client.MoveState(ctx, issue, target))
}

// UnblockToTriage is the one write that can half-succeed: it moves dependents
// one at a time and can return an error with some already moved. Keying the
// expiry on the error alone would serve the pre-cascade list to the refresh
// that follows, and the tickets it did unblock would appear to revert.
func (c *Client) UnblockToTriage(ctx context.Context, issue linear.Issue) ([]string, error) {
	keys, err := c.Client.UnblockToTriage(ctx, issue)
	if len(keys) > 0 {
		expire(time.Now())
		return keys, err
	}
	return keys, expireAfter(err)
}

func (c *Client) Assign(ctx context.Context, issue linear.Issue, assigneeID string) error {
	return expireAfter(c.Client.Assign(ctx, issue, assigneeID))
}

func (c *Client) SetPriority(ctx context.Context, issue linear.Issue, priority int) error {
	return expireAfter(c.Client.SetPriority(ctx, issue, priority))
}

func (c *Client) SetProjectStatus(ctx context.Context, p linear.Project, target string) error {
	return expireAfter(c.Client.SetProjectStatus(ctx, p, target))
}

func (c *Client) SetProjectLead(ctx context.Context, p linear.Project, userID string) error {
	return expireAfter(c.Client.SetProjectLead(ctx, p, userID))
}

func (c *Client) SetProjectPriority(ctx context.Context, p linear.Project, priority int) error {
	return expireAfter(c.Client.SetProjectPriority(ctx, p, priority))
}

// expireAfter expires the cache when the write landed, and leaves it alone when
// it didn't — a failed write changed nothing, so throwing the list away would
// only spend a request re-reading what is already there.
func expireAfter(err error) error {
	if err == nil {
		expire(time.Now())
	}
	return err
}

// ── the pooled read ──────────────────────────────────────────────────────────

// get returns the slot's freshest available value, calling live only when this
// deck is the one that holds the poll lock.
//
// A failed live call falls back to whatever is cached, however old. That is the
// difference between a deck that came up during a 429 showing yesterday's list
// and one showing nothing at all, which is what it used to do: the deck keeps
// its own last good list on a failed refresh, but a deck that just started has
// no last good list to keep.
func get[T any](ctx context.Context, slot string, live func(context.Context) (T, error)) (T, error) {
	var zero T
	now := time.Now()
	cached := readCache()

	if v, err, ok := served[T](cached[slot], now); ok {
		return v, err
	}

	release := takeLock(slot, now)
	if release == nil {
		// Another deck is fetching this very moment. Anything cached beats
		// waiting for it; only a deck with nothing at all has reason to wait.
		if v, ok := decode[T](cached[slot]); ok {
			return v, nil
		}
		if v, err, ok := waitForPeer[T](ctx, slot); ok {
			return v, err
		}
		// The peer never published — it died, or its own call failed. Falling
		// through to fetch is the safe end of that race: one duplicate request
		// costs less than a deck that never shows a list.
	} else {
		defer release()
		// Re-read under the lock: the deck that just released it may have
		// refreshed this very slot while this one waited.
		if cached = readCache(); cached == nil {
			cached = map[string]entry{}
		}
		if v, err, ok := served[T](cached[slot], now); ok {
			return v, err
		}
	}

	// startedAt, not the time the call returns: this is what store compares
	// against the slot's NotBefore, so a write that lands mid-fetch turns this
	// payload away rather than being papered over by it. See expire.
	startedAt := time.Now()
	v, err := live(ctx)
	if err != nil {
		store(slot, entry{
			Data:      cached[slot].Data,
			FetchedAt: cached[slot].FetchedAt,
			Reason:    err.Error(),
			RetryAt:   time.Now().Add(retryIn(err)),
		})
		if v, ok := decode[T](cached[slot]); ok {
			return v, nil
		}
		return zero, err
	}
	b, marshalErr := json.Marshal(v)
	if marshalErr != nil {
		return v, nil // usable in hand; just not shareable
	}
	store(slot, entry{Data: b, FetchedAt: startedAt, RetryAt: startedAt.Add(Every)})
	return v, nil
}

// waitForPeer polls the cache for whatever the fetching deck publishes, which
// is an answer either way. Reports false only if nothing arrives, or if the
// caller's context runs out first.
//
// It waits on served, not on data: a peer that caught a 429 publishes a reason
// and no data, and a waiter that accepted only data would sit out the rest of
// coldWait and then call Linear itself — every waiting deck piling onto the
// throttle that the peer had already recorded.
func waitForPeer[T any](ctx context.Context, slot string) (T, error, bool) {
	var zero T
	deadline := time.Now().Add(coldWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return zero, nil, false
		case <-time.After(coldPoll):
		}
		if v, err, ok := served[T](readCache()[slot], time.Now()); ok {
			return v, err, true
		}
	}
	return zero, nil, false
}

// served reports what the cache can answer with on its own, and whether it can
// answer at all.
//
// A slot still inside its interval answers with its data — or, when the last
// attempt failed and left no data to answer with, with the reason it failed.
// That second case is the one that matters: a throttled key with an empty cache
// is precisely when four decks would each decide to go and find out for
// themselves, and pile onto the 429 that put them there.
func served[T any](e entry, now time.Time) (T, error, bool) {
	var zero T
	if !now.Before(e.RetryAt) {
		return zero, nil, false // past the interval; go and refresh it
	}
	if v, ok := decode[T](e); ok {
		return v, nil, true
	}
	if e.Reason != "" {
		return zero, errors.New(e.Reason), true
	}
	return zero, nil, false
}

func decode[T any](e entry) (T, bool) {
	var v T
	if len(e.Data) == 0 {
		return v, false
	}
	if json.Unmarshal(e.Data, &v) != nil {
		return v, false
	}
	return v, true
}

// retryIn is how long the whole machine waits before asking again. A 429 is a
// property of the shared key, not of the deck that happened to catch it, so
// every deck inherits the wait through the cache.
func retryIn(err error) time.Duration {
	var rl *linear.RateLimitError
	if !errors.As(err, &rl) {
		return Every // an ordinary failure (offline, a timeout) retries normally
	}
	secs, convErr := strconv.Atoi(rl.RetryAfter)
	if convErr != nil || secs <= 0 {
		return defaultBackoff
	}
	return min(time.Duration(secs)*time.Second, maxBackoff)
}

// ── shared file ──────────────────────────────────────────────────────────────

// entry is one slot's payload and the state of the last attempt. Data survives
// a failed attempt: a list from ten minutes ago answers "what am I working on?"
// far better than a blank does.
type entry struct {
	Data      json.RawMessage `json:"data,omitempty"`
	FetchedAt time.Time       `json:"fetched_at,omitzero"` // when the fetch was ISSUED, not when it returned
	Reason    string          `json:"reason,omitempty"`    // why the last attempt failed; "" when it worked
	RetryAt   time.Time       `json:"retry_at,omitzero"`   // don't call again before this
	NotBefore time.Time       `json:"not_before,omitzero"` // reject data read before this; set by expire
}

func cachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ticketdeck", "tickets.json")
}

func readCache() map[string]entry {
	p := cachePath()
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var c struct {
		Slots map[string]entry `json:"slots"`
	}
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return c.Slots
}

// store replaces one slot, leaving the other alone. Read-modify-write under the
// poll lock everywhere it matters; the un-held path is a deck publishing a fetch
// it made because the lock holder went silent, where losing the race means one
// slot is re-fetched a minute later.
//
// Which payload survives is keepPayload's call, and a rejected one costs only
// the payload: the failure bookkeeping on that same write still has to land, or
// a 429 caught by the losing fetch would go unnoticed.
func store(slot string, e entry) {
	withWriteLock(func() { storeLocked(slot, e) })
}

func storeLocked(slot string, e entry) {
	slots := readCache()
	if slots == nil {
		slots = map[string]entry{}
	}
	prev := slots[slot]
	e.NotBefore = prev.NotBefore
	if !keepPayload(e, prev) {
		if e.Reason == "" {
			return // nothing to record but a payload that isn't the one to keep
		}
		// Keep what is in the slot rather than blanking it. Dropping it would
		// take the last good list away from every deck — the fallback that
		// keeps a cold deck off an empty screen — and zeroing RetryAt would
		// throw away the backoff this very call discovered, since served stops
		// answering once RetryAt passes.
		e.Data, e.FetchedAt = prev.Data, prev.FetchedAt
	}
	slots[slot] = e
	writeCache(slots)
}

// keepPayload reports whether e's payload should replace prev's. Two things
// disqualify it, and only the payload — the Reason and RetryAt on that same
// call are bookkeeping the slot still needs either way.
//
// It was read before the last write landed. expire runs the moment a write
// lands, but a fetch already in flight lands after it and carries the pre-write
// list, which would otherwise go back into the slot with a full interval ahead
// of it — the snap-back expiring the cache was meant to prevent.
//
// Or it is no newer than what is already there. A deck whose call fails writes
// back the snapshot it took before calling, which on a cold start is empty; a
// peer that gave up waiting and fetched successfully may have published a good
// list in the meantime. Without this, that late failure hands every deck an
// empty list and the backoff to go with it.
func keepPayload(e, prev entry) bool {
	if len(e.Data) == 0 {
		return false
	}
	if !e.NotBefore.IsZero() && e.FetchedAt.Before(e.NotBefore) {
		return false
	}
	return len(prev.Data) == 0 || e.FetchedAt.After(prev.FetchedAt)
}

// Refresh puts every slot back on the interval's due side, so the next read
// goes to Linear. The deck calls it when the user presses the refresh key: that
// keypress means "show me now", and without it a manual refresh would be
// answered from the pool for up to Every — the deck's own refresh key no longer
// refreshing anything.
//
// A slot whose last call failed is left alone. Its RetryAt is the throttle
// telling every deck on this key to stop asking, and a key a user can lean on
// is the last thing that should be able to clear it. Nothing else is touched:
// the payload stays as the fallback, and NotBefore stays put because no write
// landed here to make an in-flight fetch stale.
func (c *Client) Refresh() {
	withWriteLock(func() {
		slots := readCache()
		if slots == nil {
			return
		}
		due := false
		for name, e := range slots {
			if e.Reason != "" || e.RetryAt.IsZero() {
				continue
			}
			e.RetryAt = time.Time{}
			slots[name] = e
			due = true
		}
		if due {
			writeCache(slots)
		}
	})
}

// expire marks every slot due, so the next read goes to Linear, and records
// `at` as the point before which a payload is stale. A fetch already in flight
// carries pre-write data and lands after this runs, so clearing RetryAt alone
// would not hold: store consults NotBefore to turn that late arrival away.
func expire(at time.Time) {
	withWriteLock(func() { expireLocked(at) })
}

func expireLocked(at time.Time) {
	slots := readCache()
	if slots == nil {
		return
	}
	for name, e := range slots {
		e.RetryAt, e.NotBefore = time.Time{}, at
		slots[name] = e
	}
	writeCache(slots)
}

// withWriteLock serializes the read-modify-write that store and expire perform
// on the whole file.
//
// The poll lock cannot do this job. It is per slot — so that one slot's fetch
// never blocks the other's — and it is held across an HTTP call, which means
// both slots' writers are in flight at once by design. Each would otherwise
// read the file, add its own slot, and write back a snapshot taken before the
// other's, dropping a slot that had just been fetched. This lock is held for
// microseconds instead, around no I/O but the file itself.
//
// It blocks rather than backing off, and runs unsynchronised rather than not at
// all if the lock can never be taken: losing a slot costs a re-fetch, but
// skipping the write entirely would lose the fetch that was just paid for.
func withWriteLock(fn func()) {
	p := cachePath()
	if p == "" {
		fn()
		return
	}
	lp := p + ".write.lock"
	if os.MkdirAll(filepath.Dir(lp), 0o755) != nil {
		fn()
		return
	}
	deadline := time.Now().Add(writeLockWait)
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			defer func() { _ = os.Remove(lp) }()
			fn()
			return
		}
		if !errors.Is(err, os.ErrExist) {
			break
		}
		if fi, statErr := os.Stat(lp); statErr == nil && time.Since(fi.ModTime()) > writeLockStale {
			_ = os.Remove(lp) // a deck killed between taking it and writing
			continue
		}
		time.Sleep(writeLockPoll)
	}
	fn()
}

// writeCache replaces the cache atomically. Readers take no lock, so a
// half-written file would look to them like no list at all.
func writeCache(slots map[string]entry) {
	p := cachePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, err := json.Marshal(struct {
		Slots map[string]entry `json:"slots"`
	}{slots})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tickets-*")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return
	}
	if tmp.Close() != nil {
		return
	}
	_ = os.Rename(tmp.Name(), p)
}

// takeLock claims the right to call Linear for one slot this round, returning
// nil only when another deck holds that slot's lock. A lock older than
// lockStale is taken over — a deck killed mid-poll (which is how `deck`
// restarts them) must not park the ticket list for everyone.
//
// Per slot, not per file: the deck fires both reads in one batch, so they run
// concurrently in the same process. A single lock made them race each other,
// and the loser then sat out coldWait waiting for a peer that was filling the
// other slot and would never publish its own — a ten-second stall on a cold
// start, ending in the unlocked fetch this package exists to prevent.
//
// A lock that can't be created at all — no home dir, an unwritable
// ~/.ticketdeck, a full disk — polls unlocked instead of not at all. Read as
// contention, that would be permanent: no deck would ever call Linear again.
func takeLock(slot string, now time.Time) func() {
	p := cachePath()
	if p == "" {
		return func() {}
	}
	lp := p + "." + slot + ".lock"
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return func() {}
	}
	for range 2 {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lp) }
		}
		if !errors.Is(err, os.ErrExist) {
			return func() {}
		}
		fi, statErr := os.Stat(lp)
		if statErr != nil || now.Sub(fi.ModTime()) < lockStale {
			return nil // another deck is polling right now
		}
		_ = os.Remove(lp)
	}
	return nil
}
