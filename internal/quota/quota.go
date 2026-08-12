// Package quota reads Claude Code's usage limits (the 5-hour and 7-day rate
// windows shown in Claude Code's status line) so the deck can surface them.
//
// It uses the same source as Claude Code's status line: the OAuth usage
// endpoint, authenticated with the token in ~/.claude/.credentials.json. This is
// a metadata read — it does NOT spend model tokens, so it's consistent with the
// deck's "no app-side token spend" rule.
//
// Reading is pooled machine-wide through ~/.ticketdeck/usage.json. Every deck
// on the box wants the same few numbers, every deck reads every subscription,
// and every running Claude Code session polls this same endpoint against the
// same per-account budget. Left to poll independently, N decks × M accounts
// arrive together — hardest right after `deck` restarts them all at once — and
// the endpoint answers 429, which is what "usage unavailable" was. So one deck
// per interval actually calls, and the others serve what its call wrote.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

const usageURL = "https://api.anthropic.com/api/oauth/usage"

// Every is how long a reading is served before an account is re-read.
// Utilization moves slowly and the budget is shared with every Claude Code
// status line on the machine, so this is deliberately slack.
const Every = 5 * time.Minute

// backoff is how long an account waits after its own 429. Longer than Every so
// a throttled account stops adding to the pile. Per account, not global: one
// subscription's throttle used to silence the whole line for 15 minutes, which
// hid exactly the headroom the line exists to show.
const backoff = 15 * time.Minute

// expiredRecheck is the wait after a locally-detected expired token. Shorter
// than Every because that check costs no request — it re-reads the file, which
// the account's own Claude Code may have refreshed in the meantime.
const expiredRecheck = time.Minute

// lockStale is when a poll lock is treated as abandoned (a deck killed
// mid-poll). Comfortably longer than the HTTP timeouts the caller sets.
const lockStale = 2 * time.Minute

// ErrRateLimited reports that the endpoint refused the request for rate reasons.
// The budget is shared with Claude Code's own status line, so a deck that keeps
// asking on its normal schedule just extends the throttle — callers back off.
var ErrRateLimited = errors.New("usage endpoint: rate limited")

// ErrTokenExpired reports that the account's stored OAuth token is past its
// expiry. Only that account's own Claude Code refreshes it, so an idle
// subscription — precisely the one you want headroom for — goes stale and every
// request for it is a guaranteed 401. The deck skips the call and says why
// rather than spending a request on it. It deliberately does not refresh the
// token itself: that would race the owning Claude Code over a rotating refresh
// token and could log the account out.
var ErrTokenExpired = errors.New("token expired")

// ErrUnauthorized is the endpoint refusing the token the file said was still
// good — revoked, or rotated by a re-login elsewhere. Same fix for the reader as
// ErrTokenExpired, so it reads the same on the row, but a different wait: this
// one cost a request, so it goes back on the normal interval rather than the
// free local recheck.
var ErrUnauthorized = errors.New("token rejected")

// Usage is the pair of rate-limit windows Claude Code enforces.
type Usage struct {
	FiveHourPct   float64
	FiveHourReset time.Time
	SevenDayPct   float64
	SevenDayReset time.Time
}

// Account is one subscription to read: the name to file it under and the config
// dir holding its credentials.
type Account struct {
	Name      string
	ConfigDir string
}

// Entry is one account's last reading and the state of the last attempt. Usage
// survives a failed attempt: a rate-limited account keeps showing the numbers
// from ten minutes ago, which answers "is there room over there?" far better
// than a blank does.
type Entry struct {
	Usage     *Usage    `json:"usage,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"` // when Usage was read; zero if never
	Reason    string    `json:"reason,omitempty"`    // why the last attempt failed; "" when it worked
	RetryAt   time.Time `json:"retry_at,omitzero"`   // don't call for this account before this
}

// Stale reports whether the reading predates the last two intervals — old
// enough that the numbers are worth marking rather than showing plain.
func (e Entry) Stale(now time.Time) bool {
	return e.Usage != nil && now.Sub(e.FetchedAt) > 2*Every
}

// Poll returns every account's latest entry, refreshing the ones that are due.
// Due accounts are fetched only by the one deck that holds the poll lock; any
// other deck returns the shared cache untouched, so adding decks doesn't add
// requests. Accounts that have never been read successfully come back with a
// Reason and no Usage.
func Poll(ctx context.Context, accts []Account, now time.Time) map[string]Entry {
	cached := readCache()
	var due []Account
	for _, a := range accts {
		if now.Before(cached[a.Name].RetryAt) {
			continue
		}
		due = append(due, a)
	}
	if len(due) == 0 {
		return forAccounts(accts, cached, now)
	}
	release := takeLock(now)
	if release == nil {
		// Another deck is polling right now. Its result lands in the cache within
		// seconds and this deck picks it up on its next tick — cheaper than two
		// decks asking the same question at the same moment.
		return forAccounts(accts, cached, now)
	}
	defer release()
	// Re-read under the lock: the deck that just released it may have refreshed
	// these very accounts while this one waited to acquire.
	if cached = readCache(); cached == nil {
		cached = map[string]Entry{}
	}
	// Settle the list of accounts to read before spawning anything: the fetches
	// only ever read `cached`, and it is written once they have all finished.
	// Interleaving those writes with this loop's reads is a concurrent map access,
	// which the runtime kills the deck for — and a credentials file that fails
	// locally returns fast enough to make that the common case, not the rare one.
	var fetch []Account
	for _, a := range due {
		if !now.Before(cached[a.Name].RetryAt) {
			fetch = append(fetch, a)
		}
	}
	var wg sync.WaitGroup
	read := make([]Entry, len(fetch))
	for i, a := range fetch {
		wg.Add(1)
		go func(i int, a Account) {
			defer wg.Done()
			e := cached[a.Name]
			u, err := Fetch(ctx, a.ConfigDir)
			if err != nil {
				e.Reason, e.RetryAt = reasonFor(err), now.Add(retryIn(err))
			} else {
				e.Usage, e.FetchedAt, e.Reason, e.RetryAt = &u, now, "", now.Add(Every)
			}
			read[i] = e
		}(i, a)
	}
	wg.Wait()
	for i, a := range fetch {
		cached[a.Name] = read[i]
	}
	writeCache(cached)
	return forAccounts(accts, cached, now)
}

// forAccounts narrows the cache to the accounts asked for, filling in a reason
// for any that have never been read — the caller renders a row per account
// either way, and "no reading yet" must not look the same as "0%".
func forAccounts(accts []Account, cached map[string]Entry, now time.Time) map[string]Entry {
	out := make(map[string]Entry, len(accts))
	for _, a := range accts {
		e, ok := cached[a.Name]
		if !ok {
			e.Reason = "no reading yet"
		} else if e.Usage == nil && e.Reason == "" {
			e.Reason = "no reading yet"
		}
		out[a.Name] = e
	}
	return out
}

// reasonFor turns a fetch error into the few words the header line has room
// for. The distinction earns its keep: rate-limited fixes itself, an expired
// token needs you to open that account's Claude Code, and offline is neither.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, ErrRateLimited):
		return "rate limited"
	case errors.Is(err, ErrTokenExpired), errors.Is(err, ErrUnauthorized):
		return "token expired"
	case errors.Is(err, os.ErrNotExist):
		return "not signed in"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "no response"
	}
	return "usage unavailable"
}

func retryIn(err error) time.Duration {
	switch {
	case errors.Is(err, ErrRateLimited):
		return backoff
	case errors.Is(err, ErrTokenExpired):
		return expiredRecheck
	}
	return Every
}

// ── shared cache ─────────────────────────────────────────────────────────────

func cachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ticketdeck", "usage.json")
}

func readCache() map[string]Entry {
	p := cachePath()
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var c struct {
		Accounts map[string]Entry `json:"accounts"`
	}
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return c.Accounts
}

// writeCache replaces the cache atomically. Readers take no lock, so a
// half-written file would look to them like no readings at all.
func writeCache(accounts map[string]Entry) {
	p := cachePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, err := json.Marshal(struct {
		Accounts map[string]Entry `json:"accounts"`
	}{accounts})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".usage-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return
	}
	if tmp.Close() != nil {
		return
	}
	_ = os.Rename(tmp.Name(), p)
}

// takeLock claims the right to call the endpoint this round, returning nil only
// when another deck holds the lock. A lock older than lockStale is taken over —
// a deck killed mid-poll (which is how decks restart) must not park the usage
// line for everyone.
//
// A lock that can't be created at all — no home dir, an unwritable ~/.ticketdeck,
// a full disk — polls unlocked instead of not at all. Read as contention, that
// would be permanent: no deck would ever call the endpoint again, and the line
// would sit empty for good. A duplicate request is the cheaper failure.
func takeLock(now time.Time) func() {
	p := cachePath()
	if p == "" {
		return func() {}
	}
	lp := p + ".lock"
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

// ── endpoint ─────────────────────────────────────────────────────────────────

// Fetch reads the OAuth token from configDir and queries the usage endpoint.
// Returns an error (to be handled silently by the caller) when there's no OAuth
// token — e.g. an API-key setup — or the request fails.
//
// configDir is explicit rather than implied so one deck can read every
// subscription's headroom, not just its own: knowing the other account has room
// is the whole reason to look when this one runs out.
//
// Prefer Poll: a bare Fetch is one more unpooled request against the shared
// per-account budget.
func Fetch(ctx context.Context, configDir string) (Usage, error) {
	tok, err := oauthToken(configDir)
	if err != nil {
		return Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ticketdeck")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Usage{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return Usage{}, ErrRateLimited
	}
	if res.StatusCode == http.StatusUnauthorized {
		// The clock said the token was still good, so this is the credentials file
		// having moved on (a re-login elsewhere) — same fix as an expired one.
		return Usage{}, ErrUnauthorized
	}
	if res.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("usage endpoint: http %d", res.StatusCode)
	}
	var body struct {
		FiveHour struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return Usage{}, err
	}
	return Usage{
		FiveHourPct:   body.FiveHour.Utilization,
		FiveHourReset: parseTime(body.FiveHour.ResetsAt),
		SevenDayPct:   body.SevenDay.Utilization,
		SevenDayReset: parseTime(body.SevenDay.ResetsAt),
	}, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// oauthToken reads the Claude Code OAuth access token out of configDir.
func oauthToken(configDir string) (string, error) {
	// The env token belongs to the active account only — using it for another
	// account's dir would report this subscription's usage under that one's name.
	if t := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); t != "" && configDir == session.ConfigDir() {
		return t, nil
	}
	b, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		return "", err
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // epoch milliseconds
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &creds); err != nil {
		return "", err
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no OAuth token (API-key setup?)")
	}
	if exp := creds.ClaudeAiOauth.ExpiresAt; exp > 0 && time.Now().After(time.UnixMilli(exp)) {
		return "", ErrTokenExpired
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}
