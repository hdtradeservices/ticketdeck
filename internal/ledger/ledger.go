// Package ledger remembers the sessions the decks on this machine started:
// which account runs each one, its Claude session id, and the name the user
// gave it.
//
// Most of that can be re-derived. A ticket's session id comes from its key, and
// the transcripts on disk say which accounts hold it. An ad-hoc session is the
// exception: its id is random, its name lives only here, and once its process
// exits nothing else ties its transcript back to the deck. The ledger is what
// lets an `n` session survive a restart.
//
// It is a hint, never the authority on where a session lives. A live process
// and the transcripts on disk outrank it, because they can't go stale.
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Kinds of session the deck opens.
const (
	Ticket  = "ticket"
	Project = "project"
	Scratch = "scratch"
)

// Entry is one remembered session.
type Entry struct {
	SessionID string    `json:"session_id"`
	Key       string    `json:"key"`            // ticket/project key; agent name for an ad-hoc session
	Kind      string    `json:"kind"`           // Ticket | Project | Scratch
	Name      string    `json:"name,omitempty"` // what the user called it
	Account   string    `json:"account,omitempty"`
	ConfigDir string    `json:"config_dir,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Started   time.Time `json:"started,omitzero"`
	Updated   time.Time `json:"updated"`
	Moves     []Move    `json:"moves,omitempty"`
	// Handled is the id of the last API error the deck is done with, so one
	// rate limit is acted on once — across deck restarts too.
	Handled string `json:"handled,omitempty"`
	// Carry is the deck's promise to get the session going again after its
	// account cut it off. It is kept until the conversation moves past the
	// cut-off, so a failed move, a restarted deck or a closed tab only delays
	// it.
	Carry *Carry `json:"carry,omitempty"`
	// Forgotten hides an ad-hoc session the user closed for good.
	Forgotten bool `json:"forgotten,omitempty"`
	// Refused remembers each account whose usage limit cut this session off,
	// until the limit lifts. The session moves on and its cut-off goes with
	// it, but the account is still out: forgetting that sent six sessions to
	// an account out for the week (2026-10-28).
	Refused []Refusal `json:"refused,omitempty"`
}

// Refusal is an account's usage limit as a cut-off reported it.
type Refusal struct {
	Account string    `json:"account"`
	Until   time.Time `json:"until"`
	Limit   string    `json:"limit,omitempty"` // "five_hour", "seven_day", …
}

// Refuse records an account's limit, keeping the newest per account and
// dropping those that have lifted.
func (e *Entry) Refuse(r Refusal, now time.Time) {
	var kept []Refusal
	for _, x := range e.Refused {
		if x.Account != r.Account && now.Before(x.Until) {
			kept = append(kept, x)
		}
	}
	e.Refused = append(kept, r)
}

// Carry is a cut-off the deck has promised to carry on from: on another
// account with room, or on the same one once its limit lifts.
type Carry struct {
	Err       string    `json:"err"`  // the API error it answers
	At        time.Time `json:"at"`   // when the cut-off happened
	Kind      string    `json:"kind"` // "rate_limit" or "authentication_failed"
	From      string    `json:"from"` // the account that cut it off
	ResetsAt  time.Time `json:"resets_at,omitzero"`
	LimitType string    `json:"limit_type,omitempty"`
	// Socket is the herdr server of the deck that made the promise. Only that
	// deck keeps it: two decks starting one stopped session would fork it.
	Socket string `json:"socket,omitempty"`
	// Busy: background work was running when it was cut off. A move or a
	// restart ends that work, and the session is told so.
	Busy bool `json:"busy,omitempty"`
	// Told is when the session was last told to carry on, and Tells how many
	// times that has gone unanswered.
	Told  time.Time `json:"told,omitzero"`
	Tells int       `json:"tells,omitempty"`
}

// Move is one hand-off between accounts.
type Move struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	Why  string    `json:"why,omitempty"`
}

// LastMove is the most recent hand-off, if any.
func (e Entry) LastMove() (Move, bool) {
	if len(e.Moves) == 0 {
		return Move{}, false
	}
	return e.Moves[len(e.Moves)-1], true
}

// keepFor is how long an ad-hoc session stays remembered after it was last
// touched. Ticket and project sessions are re-derived from Linear, so theirs
// only need to outlive the ticket's time on the deck.
const keepFor = 30 * 24 * time.Hour

// maxMoves bounds the hand-off history kept per session.
const maxMoves = 10

// dir is overridable in tests.
var dir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ticketdeck")
}

// Path is the ledger file.
func Path() string {
	d := dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "sessions.json")
}

type file struct {
	Sessions map[string]Entry `json:"sessions"`
}

// Load returns every remembered session, keyed by session id. A missing or
// unreadable ledger is an empty one.
func Load() map[string]Entry {
	p := Path()
	if p == "" {
		return map[string]Entry{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return map[string]Entry{}
	}
	var f file
	if json.Unmarshal(b, &f) != nil || f.Sessions == nil {
		return map[string]Entry{}
	}
	return f.Sessions
}

// Sorted returns the entries newest-updated first.
func Sorted(entries map[string]Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// Update applies fn to the entry for a session id, creating it when absent, and
// writes the ledger back. Every deck on the machine shares the file, so the
// read-modify-write runs under a lock.
func Update(id string, fn func(*Entry)) error {
	if id == "" {
		return errors.New("ledger: no session id")
	}
	p := Path()
	if p == "" {
		return errors.New("ledger: no home directory")
	}
	return withLock(p, func() error {
		all := Load()
		e := all[id]
		e.SessionID = id
		fn(&e)
		e.Updated = now()
		if len(e.Moves) > maxMoves {
			e.Moves = e.Moves[len(e.Moves)-maxMoves:]
		}
		all[id] = e
		prune(all, now())
		return write(p, all)
	})
}

// now is overridable in tests.
var now = time.Now

func prune(all map[string]Entry, at time.Time) {
	for id, e := range all {
		if at.Sub(e.Updated) > keepFor {
			delete(all, id)
		}
	}
}

func write(p string, all map[string]Entry) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{Sessions: all}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".sessions-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Lock timing. A write is a file read and a rename, so it holds the lock for
// milliseconds.
var (
	lockWait = 2 * time.Second
	lockPoll = 10 * time.Millisecond
)

// withLock runs fn holding an exclusive flock on the ledger's lock file. The
// kernel drops the lock when its holder exits, however it exits, so a deck
// killed mid-write never leaves one behind — and no deck has to guess that a
// lock is stale and take it over, which is a race two decks can both win.
func withLock(p string, fn func() error) error {
	lp := p + ".lock"
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("ledger: lock %s: %w", lp, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ledger: %s is locked", lp)
		}
		time.Sleep(lockPoll)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
