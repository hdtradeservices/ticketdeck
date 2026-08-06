// Package account resolves which Claude subscription a deck runs as, discovers
// the other subscriptions available on this machine, and moves a ticket's
// session between them.
//
// TicketDeck runs one deck per subscription (`deck --account NAME`), each with
// its own Claude config dir and its own herdr workspace. Two consequences drive
// this package: a deck must name itself unambiguously so you know whose rate
// limits you are spending, and it must be able to hand a session to a
// subscription with headroom when the current one runs out.
package account

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Default labels the plain ~/.claude subscription, which has no --account suffix
// to take a name from. Set TICKETDECK_ACCOUNT to give it a real name.
const Default = "default"

// Account is one Claude subscription, identified by the config dir holding its
// credentials and transcripts.
type Account struct {
	Name      string
	ConfigDir string
}

// Current is the account this deck runs as. The name comes from
// TICKETDECK_ACCOUNT when the launcher set it, else from the config dir — so the
// primary deck is labelled too, rather than left blank and ambiguous with "any
// deck".
func Current() Account {
	dir := session.ConfigDir()
	name := strings.TrimSpace(os.Getenv("TICKETDECK_ACCOUNT"))
	if name == "" {
		return Account{Name: nameFor(dir), ConfigDir: dir}
	}
	// Publish the label into the config dir so PEER decks call this subscription
	// the same thing. TICKETDECK_ACCOUNT only renames this process; without
	// publishing, another deck would label it from its directory and one account
	// would carry two names — and, once those names sort differently, two
	// different accent colors. Best-effort: an unwritable dir just means peers
	// fall back to the directory-derived name.
	if name != readLabel(dir) {
		_ = os.WriteFile(filepath.Join(dir, labelFile), []byte(name+"\n"), 0o644)
	}
	return Account{Name: name, ConfigDir: dir}
}

// LaunchCmd is the command that opens this account's deck. The primary
// subscription takes no --account flag: `deck --account default` would look for
// a ~/.claude-default that doesn't exist.
func (a Account) LaunchCmd() string {
	base := filepath.Base(a.ConfigDir)
	if base == ".claude" {
		return "deck"
	}
	return "deck --account " + strings.TrimPrefix(base, ".claude-")
}

// All lists every subscription with credentials on disk (~/.claude and
// ~/.claude-*), sorted by name. Current is always present even if its
// credentials are unreadable, so a deck can always name itself.
func All() []Account {
	cur := Current()
	byDir := map[string]Account{cur.ConfigDir: cur}
	if home, err := os.UserHomeDir(); err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude*"))
		for _, dir := range matches {
			if _, seen := byDir[dir]; seen || !isAccountDir(home, dir) || !hasCreds(dir) {
				continue
			}
			byDir[dir] = Account{Name: nameFor(dir), ConfigDir: dir}
		}
	}
	out := make([]Account, 0, len(byDir))
	for _, a := range byDir {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// Two dirs can still land on one label (~/.claude and ~/.claude-default, or a
	// stale published label). Names key the color and usage maps, so one would
	// silently shadow the other; disambiguate deterministically instead. The
	// current account keeps its name unconditionally — the deck looks itself up
	// by the name Current() gave it.
	taken := map[string]bool{cur.Name: true}
	for i := range out {
		if out[i].ConfigDir == cur.ConfigDir {
			continue
		}
		base, name := out[i].Name, out[i].Name
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s(%d)", base, n)
		}
		out[i].Name, taken[name] = name, true
	}
	return out
}

// isAccountDir reports whether a path is a subscription dir: ~/.claude or
// ~/.claude-<name>. Judging the directory's shape rather than its derived label
// keeps ~/.claude-default (a real `deck --account default`) while still
// rejecting a stray ~/.claudeX.
func isAccountDir(home, dir string) bool {
	if filepath.Dir(dir) != home {
		return false
	}
	base := filepath.Base(dir)
	if base == ".claude" {
		return true
	}
	n := strings.TrimPrefix(base, ".claude-")
	return n != base && n != ""
}

// Others is All minus the account this deck runs as — the candidates a session
// can be handed to.
func Others() []Account {
	cur := Current()
	var out []Account
	for _, a := range All() {
		if a.ConfigDir != cur.ConfigDir {
			out = append(out, a)
		}
	}
	return out
}

// palette holds the per-account accent colors, picked to stay legible and
// distinguishable on both dark and light terminals.
var palette = []string{"111", "213", "43", "214", "141", "84"}

// Colors maps account name to accent color. Assignment is by sorted position
// rather than a hash of the name: a hash can land two accounts on the same
// color, which would defeat the whole point, and sorted position makes every
// deck agree on who is which color.
func Colors(accts []Account) map[string]string {
	out := make(map[string]string, len(accts))
	for i, a := range accts {
		out[a.Name] = palette[i%len(palette)]
	}
	return out
}

// labelFile holds an account's published display name, so every deck agrees on
// what to call that subscription (see Current).
const labelFile = ".ticketdeck-name"

func readLabel(configDir string) string {
	b, err := os.ReadFile(filepath.Join(configDir, labelFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func nameFor(configDir string) string {
	if l := readLabel(configDir); l != "" {
		return l
	}
	base := filepath.Base(configDir)
	if n := strings.TrimPrefix(base, ".claude-"); n != base && n != "" {
		return n
	}
	return Default
}

func hasCreds(configDir string) bool {
	fi, err := os.Stat(filepath.Join(configDir, ".credentials.json"))
	return err == nil && !fi.IsDir()
}

// HandOff moves a ticket's session to another subscription by copying its
// transcript into that account's config dir. The transcript IS the session
// state, and the session id derives from the ticket key alone
// (session.DeterministicID), so the same ticket keeps its identity in every
// account and a copy is the whole move.
//
// The caller MUST stop the session first: two accounts appending to their own
// copy of one transcript diverge with no way to reconcile them.
//
// Resuming replays the conversation, so what carries over is context, not a
// live process — an in-flight tool call is lost.
func HandOff(from, to Account, ticketKey string) error {
	if from.ConfigDir == to.ConfigDir {
		return fmt.Errorf("%s and %s are the same account", from.Name, to.Name)
	}
	id := session.DeterministicID(ticketKey)
	src, err := findTranscript(from.ConfigDir, id)
	if err != nil {
		return fmt.Errorf("no %s session found under ⦿%s", ticketKey, from.Name)
	}
	// Mirror the source's project dir rather than deriving one from a cwd, so a
	// session started in a repo subdir lands where the other deck will look.
	dst := filepath.Join(to.ConfigDir, "projects", filepath.Base(filepath.Dir(src)), id+".jsonl")

	// Refuse when the destination already holds NEWER work than the source:
	// copying would destroy a conversation that only exists there. An older
	// destination is the hand-back case (work continued in `from`), so replacing
	// it is correct and lets a ticket ping-pong between accounts.
	if di, err := os.Stat(dst); err == nil {
		si, err := os.Stat(src)
		if err != nil {
			return err
		}
		if di.ModTime().After(si.ModTime()) {
			return fmt.Errorf("⦿%s already has newer %s work — resume it there instead", to.Name, ticketKey)
		}
	}
	return copyFile(src, dst)
}

// findTranscript locates a session's transcript anywhere under an account's
// projects/ tree, newest first when an id appears under more than one project
// dir.
func findTranscript(configDir, id string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", id+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("no transcript for %s under %s", id, configDir)
	}
	best, bestMod := "", int64(-1)
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if mod := fi.ModTime().UnixNano(); mod > bestMod {
			best, bestMod = p, mod
		}
	}
	if best == "" {
		return "", fmt.Errorf("no readable transcript for %s under %s", id, configDir)
	}
	return best, nil
}

// copyFile writes src to dst via a temp file + rename, so the destination deck
// never observes a half-written transcript. 0600: transcripts hold conversation
// content.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ticketdeck-handoff-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename succeeds
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
