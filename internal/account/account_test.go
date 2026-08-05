package account

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// fakeHome builds a home dir holding the named accounts (each with credentials)
// and points HOME + CLAUDE_CONFIG_DIR at it. "default" means ~/.claude.
func fakeHome(t *testing.T, names ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, n := range names {
		dir := filepath.Join(home, ".claude")
		if n != Default {
			dir += "-" + n
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestCurrentNamesTheDefaultAccount(t *testing.T) {
	fakeHome(t, Default)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")

	// The primary deck must still be labelled: an unnamed deck is ambiguous with
	// any other deck, which is the confusion this exists to remove.
	if got := Current().Name; got != Default {
		t.Errorf("Current().Name = %q, want %q", got, Default)
	}
}

func TestCurrentNameFromConfigDirAndEnv(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude-support"))
	t.Setenv("TICKETDECK_ACCOUNT", "")
	if got := Current().Name; got != "support" {
		t.Errorf("derived name = %q, want support", got)
	}

	// The launcher's explicit label wins, so the primary deck can be called
	// something meaningful instead of "default".
	t.Setenv("TICKETDECK_ACCOUNT", "matt")
	if got := Current().Name; got != "matt" {
		t.Errorf("env name = %q, want matt", got)
	}
}

func TestAllAndOthers(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")

	all := All()
	if len(all) != 2 {
		t.Fatalf("All() = %d accounts, want 2: %+v", len(all), all)
	}
	if all[0].Name != Default || all[1].Name != "support" {
		t.Errorf("All() not sorted by name: %+v", all)
	}

	others := Others()
	if len(others) != 1 || others[0].ConfigDir != filepath.Join(home, ".claude-support") {
		t.Errorf("Others() = %+v, want just .claude-support", others)
	}
}

// A renamed current account must not also appear under its derived name, or the
// deck would list itself twice and offer to hand off to itself.
func TestAllDoesNotDuplicateRenamedCurrent(t *testing.T) {
	fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "matt")

	all := All()
	if len(all) != 2 {
		t.Fatalf("All() = %d accounts, want 2: %+v", len(all), all)
	}
	for _, a := range all {
		if a.Name == Default {
			t.Errorf("renamed current also listed as %q: %+v", Default, all)
		}
	}
	if others := Others(); len(others) != 1 || others[0].Name != "support" {
		t.Errorf("Others() = %+v, want just support", others)
	}
}

// A directory without credentials isn't a usable subscription — including it
// would offer a hand-off target that can't run anything.
func TestAllSkipsDirsWithoutCredentials(t *testing.T) {
	home := fakeHome(t, Default)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude-halfsetup"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range All() {
		if a.Name == "halfsetup" {
			t.Errorf("All() included a credential-less dir: %+v", a)
		}
	}
}

func TestColorsAreDistinctPerAccount(t *testing.T) {
	accts := []Account{{Name: "matt"}, {Name: "support"}, {Name: "third"}}
	c := Colors(accts)
	if c["matt"] == c["support"] || c["support"] == c["third"] || c["matt"] == c["third"] {
		t.Errorf("colors collided, defeating the point of the accent: %+v", c)
	}
}

// writeTranscript puts a ticket's transcript under an account's projects tree
// and returns its path.
func writeTranscript(t *testing.T, acct Account, project, key, body string) string {
	t.Helper()
	dir := filepath.Join(acct.ConfigDir, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, session.DeterministicID(key)+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHandOffCopiesTranscriptIntoTargetAccount(t *testing.T) {
	home := t.TempDir()
	from := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude")}
	to := Account{Name: "support", ConfigDir: filepath.Join(home, ".claude-support")}
	writeTranscript(t, from, "-home-matthew-Repos", "ZEN-3356", `{"role":"user"}`)

	if err := HandOff(from, to, "ZEN-3356"); err != nil {
		t.Fatalf("HandOff: %v", err)
	}

	// The destination path must mirror the source's project dir, so the other
	// deck finds it where it already looks for resumable sessions.
	dst := filepath.Join(to.ConfigDir, "projects", "-home-matthew-Repos", session.DeterministicID("ZEN-3356")+".jsonl")
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("transcript not copied to %s: %v", dst, err)
	}
	if string(b) != `{"role":"user"}` {
		t.Errorf("copied content = %q", b)
	}
}

// The same ticket must map to the same session id in every account — that's what
// makes a hand-off a file copy rather than a rewrite.
func TestHandOffKeepsTheSessionID(t *testing.T) {
	home := t.TempDir()
	from := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude")}
	to := Account{Name: "support", ConfigDir: filepath.Join(home, ".claude-support")}
	src := writeTranscript(t, from, "-p", "ZEN-1", "x")
	if err := HandOff(from, to, "ZEN-1"); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(src) != session.DeterministicID("ZEN-1")+".jsonl" {
		t.Fatalf("unexpected source name %s", filepath.Base(src))
	}
	dst := filepath.Join(to.ConfigDir, "projects", "-p", filepath.Base(src))
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("id changed across accounts: %v", err)
	}
}

// Overwriting newer work in the destination would destroy a conversation that
// exists nowhere else, so it must fail loudly instead.
func TestHandOffRefusesToClobberNewerDestination(t *testing.T) {
	home := t.TempDir()
	from := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude")}
	to := Account{Name: "support", ConfigDir: filepath.Join(home, ".claude-support")}
	src := writeTranscript(t, from, "-p", "ZEN-2", "old source")
	dst := writeTranscript(t, to, "-p", "ZEN-2", "newer destination work")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	if err := HandOff(from, to, "ZEN-2"); err == nil {
		t.Fatal("HandOff overwrote newer destination work")
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "newer destination work" {
		t.Errorf("destination was clobbered: %q", b)
	}
}

// The hand-back case: work continued in the source, so the older copy in the
// destination is stale and replacing it is right.
func TestHandOffReplacesOlderDestination(t *testing.T) {
	home := t.TempDir()
	from := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude")}
	to := Account{Name: "support", ConfigDir: filepath.Join(home, ".claude-support")}
	writeTranscript(t, from, "-p", "ZEN-3", "newer source")
	dst := writeTranscript(t, to, "-p", "ZEN-3", "stale")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dst, old, old); err != nil {
		t.Fatal(err)
	}

	if err := HandOff(from, to, "ZEN-3"); err != nil {
		t.Fatalf("HandOff: %v", err)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "newer source" {
		t.Errorf("stale destination not replaced: %q", b)
	}
}

func TestHandOffErrorsWhenNoSourceSession(t *testing.T) {
	home := t.TempDir()
	from := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude")}
	to := Account{Name: "support", ConfigDir: filepath.Join(home, ".claude-support")}
	if err := HandOff(from, to, "ZEN-404"); err == nil {
		t.Error("HandOff succeeded with no source transcript")
	}
}

func TestHandOffRejectsSameAccount(t *testing.T) {
	a := Account{Name: "matt", ConfigDir: t.TempDir()}
	if err := HandOff(a, a, "ZEN-5"); err == nil {
		t.Error("HandOff accepted a move onto the same account")
	}
}

// A session started in a repo subdir lives under that dir's slug, not the deck's
// launch root — so the lookup must not assume any single cwd.
func TestHandOffFindsTranscriptOutsideTheLaunchRoot(t *testing.T) {
	home := t.TempDir()
	from := Account{Name: "matt", ConfigDir: filepath.Join(home, ".claude")}
	to := Account{Name: "support", ConfigDir: filepath.Join(home, ".claude-support")}
	writeTranscript(t, from, "-home-matthew-Repos-etp-catalog", "ZEN-6", "deep")

	if err := HandOff(from, to, "ZEN-6"); err != nil {
		t.Fatalf("HandOff: %v", err)
	}
	dst := filepath.Join(to.ConfigDir, "projects", "-home-matthew-Repos-etp-catalog", session.DeterministicID("ZEN-6")+".jsonl")
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("subdir session not handed off: %v", err)
	}
}
