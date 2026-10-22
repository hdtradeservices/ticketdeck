// Package account resolves the Claude subscriptions on this machine — which one
// this process runs as, and every other one a session can run under — and moves
// a session between them.
//
// One deck runs sessions under every subscription: each session's pane gets its
// account's CLAUDE_CONFIG_DIR. The subscription a deck itself runs as is only
// its home: what it falls back to when nothing better is known. Two needs drive
// this package: every account needs one stable name and color, so you can see
// whose rate limits a session is spending, and a session must be able to move
// to a subscription with headroom when the one it is on runs out.
package account

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// All lists every subscription on this machine, sorted by name: each of ~/.claude
// and ~/.claude-* that holds credentials, plus any ~/.claude-accounts names (on
// macOS the credentials live in the keychain, not in the directory). Current is
// always present even if its credentials are unreadable, so a deck can always
// name itself.
func All() []Account {
	cur := Current()
	byDir := map[string]Account{cur.ConfigDir: cur}
	if home, err := os.UserHomeDir(); err == nil {
		listed := map[string]bool{}
		for _, dir := range listedDirs(home) {
			listed[dir] = true
		}
		matches, _ := filepath.Glob(filepath.Join(home, ".claude*"))
		for _, dir := range matches {
			if _, seen := byDir[dir]; seen || !isAccountDir(home, dir) || (!hasCreds(dir) && !listed[dir]) {
				continue
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				continue
			}
			byDir[dir] = Account{Name: nameFor(dir), ConfigDir: dir}
		}
	}
	out := make([]Account, 0, len(byDir))
	for _, a := range byDir {
		out = append(out, a)
	}
	// Order on (name, dir), not name alone: out comes from map iteration and
	// sort.Slice isn't stable, so equal names would order arbitrarily and the
	// disambiguator below would hand the bare name to a different account run to
	// run. ConfigDir is unique, which makes this a total order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ConfigDir < out[j].ConfigDir
	})

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
	if base != ".claude" {
		// A config dir outside the ~/.claude family (CLAUDE_CONFIG_DIR pointed
		// somewhere else): its own name is the only honest label.
		if n := strings.TrimPrefix(strings.TrimPrefix(base, "."), "claude-"); n != "" {
			return n
		}
	}
	if p := primaryName(); p != "" {
		return p
	}
	return Default
}

// accountsFile lists this machine's accounts, one name per line: the first owns
// ~/.claude, the rest ~/.claude-<name>. rdeck reads it too, to know which
// account is the primary.
const accountsFile = ".claude-accounts"

func listedNames() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, accountsFile))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Fields(line)[0])
	}
	return out
}

// primaryName is the name ~/.claude-accounts gives ~/.claude, "" without one.
func primaryName() string {
	if names := listedNames(); len(names) > 0 {
		return names[0]
	}
	return ""
}

func listedDirs(home string) []string {
	var out []string
	for i, n := range listedNames() {
		if i == 0 {
			out = append(out, filepath.Join(home, ".claude"))
			continue
		}
		out = append(out, filepath.Join(home, ".claude-"+n))
	}
	return out
}

// Find returns the account with this name, or the one at this config dir.
func Find(accts []Account, nameOrDir string) (Account, bool) {
	for _, a := range accts {
		if a.Name == nameOrDir {
			return a, true
		}
	}
	clean := filepath.Clean(nameOrDir)
	for _, a := range accts {
		if filepath.Clean(a.ConfigDir) == clean {
			return a, true
		}
	}
	return Account{}, false
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
	return CopySession(from, to, session.DeterministicID(ticketKey), ticketKey)
}

// ErrNoSession reports that the account holds no transcript for the session.
// A session nobody has typed into yet may have none at all: Claude writes the
// file only once there is something to record.
var ErrNoSession = errors.New("nothing to copy")

// CopySession is HandOff by session id, for a session whose id doesn't derive
// from a ticket key (an ad-hoc one). label names it in errors. Alongside the
// transcript it copies the session's own directory — its subagents' transcripts
// and the large tool outputs the transcript points at — so the copy stands on
// its own.
func CopySession(from, to Account, id, label string) error {
	if from.ConfigDir == to.ConfigDir {
		return fmt.Errorf("%s and %s are the same account", from.Name, to.Name)
	}
	src, err := findTranscript(from.ConfigDir, id)
	if err != nil {
		return fmt.Errorf("%w: no %s session found under ⦿%s", ErrNoSession, label, from.Name)
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
			return fmt.Errorf("⦿%s already has newer %s work — resume it there instead", to.Name, label)
		}
	}
	// The directory first: once the transcript lands, the destination is a
	// session that can be resumed, and it should be whole by then.
	if err := copyTree(strings.TrimSuffix(src, ".jsonl"), strings.TrimSuffix(dst, ".jsonl")); err != nil {
		return err
	}
	return copyFile(src, dst)
}

// copyTree copies a session directory, replacing files that differ. A missing
// source directory is not an error: a session that never ran a subagent or
// saved a large tool output has none.
func copyTree(src, dst string) error {
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if si, err := d.Info(); err == nil {
			if di, err := os.Stat(target); err == nil && di.Size() == si.Size() && !di.ModTime().Before(si.ModTime()) {
				return nil // already there
			}
		}
		return copyFile(p, target)
	})
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
