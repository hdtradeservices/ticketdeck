package fleet

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/route"
)

// loadOf is one live session's share of its account's load, weighted as
// claude-pick weighs it: a busy session spends at the full rate, one waiting on
// a person will again soon, an idle one spends nothing.
func loadOf(status string) float64 {
	switch status {
	case "busy", "shell":
		return 1
	case "waiting":
		return 0.5
	}
	return 0.1
}

// claude-pick leaves a marker for each session it launches, counted as one
// busy session for a minute: a session started seconds ago isn't in the
// registry yet, so back-to-back launches would otherwise all land on one
// account. The deck reads and writes the same markers, so a burst from both
// spreads out too.
const pendingFor = 60 * time.Second

func pendingDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = "/tmp"
	}
	return filepath.Join(base, "claude-pick", "pending")
}

// MarkLaunch records a session just started under the account at configDir.
// Best-effort: a marker that can't be written only means the next pick may not
// know about this launch for a few seconds.
func MarkLaunch(configDir string) {
	dir := pendingDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	name := fmt.Sprintf("%s.%d.%d.%d", filepath.Base(configDir), time.Now().Unix(), os.Getpid(), rand.IntN(32768))
	_ = os.WriteFile(filepath.Join(dir, name), nil, 0o644)
}

// pendingLaunches counts the account's markers younger than pendingFor, and
// clears away older ones as claude-pick does.
func pendingLaunches(configDir string, now time.Time) int {
	matches, _ := filepath.Glob(filepath.Join(pendingDir(), filepath.Base(configDir)+".*"))
	n := 0
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) < pendingFor {
			n++
		} else {
			_ = os.Remove(m)
		}
	}
	return n
}

// Candidates is every account as the router sees it after this poll: its usage
// reading, and what its sessions say about it.
func (s Snapshot) Candidates(accts []account.Account, readings map[string]quota.Entry, home account.Account, now time.Time) []route.Candidate {
	until, authFailed := s.Limits(now)
	live := s.LiveOn()
	out := make([]route.Candidate, 0, len(accts))
	for _, a := range accts {
		e := readings[a.Name]
		out = append(out, route.Candidate{
			Name: a.Name, Usage: e.Current(now), Stale: e.Stale(now), Reason: e.Reason,
			LimitedUntil: until[a.Name], AuthFailed: authFailed[a.Name],
			Live: live[a.Name], Load: s.Load[a.Name],
			Home: a.ConfigDir == home.ConfigDir,
		})
	}
	return out
}
