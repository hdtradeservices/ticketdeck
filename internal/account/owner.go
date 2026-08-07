package account

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Owner is the subscription running a ticket's session — what the deck labels a
// row with, so a ticket already being worked under another account is visible
// without switching decks.
type Owner struct {
	Name       string         // account whose subscription runs the session
	Status     session.Status // live status, when that account's workspace answered
	LastActive time.Time      // transcript mtime: when the session last wrote
	Live       bool           // Status came from a running workspace, not from disk
}

// Owners resolves which subscription holds each ticket's session.
//
// Nothing in the normal status path can answer this: `deck --account NAME`
// isolates both the Claude config dir and the herdr server socket, so a deck's
// backend only ever reports its own sessions. Two per-account sources fill the
// gap:
//
//   - the transcripts on disk (<config dir>/projects/*/<deterministic id>.jsonl),
//     which name every account holding the session and, by mtime, which one
//     wrote to it last;
//   - that account's herdr server, which reports live working/idle/needs-input.
//
// Disk alone attributes a session, so an account whose deck isn't running still
// shows up; herdr only upgrades "last wrote 3m ago" to a live status. Both fail
// soft — an unreadable account or an unreachable server drops out of the result
// rather than failing the refresh.
//
// A ticket held by two accounts (the hand-off case leaves a transcript behind in
// the source) resolves to one owner: a live session wins, else the newest
// transcript.
func Owners(keys []string, accts []Account) map[string]Owner {
	out := make(map[string]Owner, len(keys))
	if len(keys) == 0 || len(accts) == 0 {
		return out
	}

	ids := make(map[string]string, len(keys)) // session id → ticket key
	for _, k := range keys {
		ids[session.DeterministicID(k)] = k
	}

	cur := Current()
	states := make([]acctState, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a Account) {
			defer wg.Done()
			states[i] = readState(a, a.ConfigDir == cur.ConfigDir, keys, ids)
		}(i, a)
	}
	wg.Wait()

	for _, k := range keys {
		id := session.DeterministicID(k)
		var best Owner
		for i, a := range accts {
			st, live := states[i].live[strings.ToLower(k)]
			mod, onDisk := states[i].disk[id]
			if !live && !onDisk {
				continue
			}
			cand := Owner{Name: a.Name, Status: st, LastActive: mod, Live: live}
			if !live {
				// On disk but not in the workspace listing: resumable there.
				cand.Status = session.Stopped
			}
			if outranks(cand, best) {
				best = cand
			}
		}
		if best.Name != "" {
			out[k] = best
		}
	}
	return out
}

// The herdr lookup, in vars so tests can drive it without a herdr on the box.
var (
	listAgents     = herd.ListAt
	herdrAvailable = herd.Available
)

// acctState is one account's view of the polled tickets.
type acctState struct {
	live map[string]session.Status // lowercased ticket key → live status
	disk map[string]time.Time      // session id → transcript mtime
}

// readState collects one account's live and on-disk sessions. own selects the
// ambient herdr socket: this deck's own environment already points at its
// workspace, and a machine can run a herdr setup this package can't derive a
// path for.
func readState(a Account, own bool, keys []string, ids map[string]string) acctState {
	st := acctState{live: map[string]session.Status{}, disk: map[string]time.Time{}}

	socket := ""
	if !own {
		socket = herdSocket(a)
	}
	// Without herdr there is no workspace to ask, and the disk answer stands on
	// its own — don't spawn a doomed process on every status tick.
	if herdrAvailable() && (own || socket != "") {
		if agents, err := listAgents(socket); err == nil {
			for k, s := range herd.Statuses(keys, agents) {
				if s != session.None {
					st.live[strings.ToLower(k)] = s
				}
			}
		}
	}

	for _, dir := range projectDirs(a.ConfigDir) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			id := strings.TrimSuffix(e.Name(), ".jsonl")
			if id == e.Name() {
				continue
			}
			// Stat only the transcripts belonging to a polled ticket — an account
			// accumulates hundreds, and this runs on the fast status tick.
			if _, want := ids[id]; !want {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			if mod := fi.ModTime(); mod.After(st.disk[id]) {
				st.disk[id] = mod
			}
		}
	}
	return st
}

func projectDirs(configDir string) []string {
	ents, err := os.ReadDir(filepath.Join(configDir, "projects"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, filepath.Join(configDir, "projects", e.Name()))
		}
	}
	return out
}

// outranks reports whether candidate c is a better answer to "who is working on
// this ticket" than b. A live session beats a transcript on disk; between two of
// a kind, the one that wrote most recently wins.
func outranks(c, b Owner) bool {
	if b.Name == "" {
		return true
	}
	if cr, br := c.Live && c.Status.Running(), b.Live && b.Status.Running(); cr != br {
		return cr
	}
	return c.LastActive.After(b.LastActive)
}

// herdSocket is where an account's herdr server listens. `deck --account NAME`
// runs that subscription in an isolated workspace under ~/.config-NAME, and the
// default subscription keeps the shared ~/.config (scripts/deck). The path is
// derived from the config dir, not the display name: a name is renameable via
// TICKETDECK_ACCOUNT, and the directory is what the launcher keyed the socket
// on. Returns "" when the socket isn't there, so a peer that has never run a
// deck costs nothing.
func herdSocket(a Account) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cfg := filepath.Join(home, ".config")
	base := filepath.Base(a.ConfigDir)
	if n := strings.TrimPrefix(base, ".claude-"); n != base && n != "" {
		cfg = filepath.Join(home, ".config-"+n)
	}
	sock := filepath.Join(cfg, "herdr", "herdr.sock")
	if _, err := os.Stat(sock); err != nil {
		return ""
	}
	return sock
}
