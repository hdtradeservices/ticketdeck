package herd

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

func TestAgentNameIsWhatHerdrAccepts(t *testing.T) {
	cases := map[string]string{
		"Deploy notes":         "deploy-notes",
		"  fix: login bug!!  ": "fix-login-bug",
		"ZEN-1234":             "s-zen-1234", // must not pass for that ticket's session
		"deck":                 "s-deck",
		"42 things":            "s-42-things",
		"!!!":                  "",
		"a very long name that goes on and on past the limit": "a-very-long-name-that-goes-on-an",
	}
	for in, want := range cases {
		if got := AgentName(in); got != want {
			t.Errorf("AgentName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUniqueNameAvoidsLiveNames(t *testing.T) {
	live := []session.SessionRef{{Name: "notes"}, {Name: "notes-2"}}
	if got := UniqueName("notes", live); got != "notes-3" {
		t.Errorf("UniqueName = %q, want notes-3", got)
	}
	if got := UniqueName("other", live); got != "other" {
		t.Errorf("UniqueName = %q, want other", got)
	}
}

// A named ad-hoc session runs under its own session id, so the deck can find
// its transcript, its account and its name again after a restart.
func TestScratchSpecCarriesTheNameAndID(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	got := ScratchSpec(nil, session.Ticket{Key: "Deploy notes", SessionID: id, ConfigDir: "/h/.claude-m0"}, "/repos")
	if got.Name != "deploy-notes" || got.Label != "Deploy notes" {
		t.Errorf("name/label = %q/%q", got.Name, got.Label)
	}
	want := "agent start deploy-notes --cwd /repos -- claude --session-id " + id + " --name Deploy notes"
	if strings.Join(got.Args, " ") != want {
		t.Errorf("args = %q\nwant  %q", strings.Join(got.Args, " "), want)
	}
	if got.ConfigDir != "/h/.claude-m0" || got.SessionID != id {
		t.Errorf("account/id not carried: %+v", got)
	}
}

// The account a session runs as rides on the tab's environment, the only
// per-pane environment herdr offers. The claude argv stays exactly what it was.
func TestStartRunsTheSessionAsItsAccount(t *testing.T) {
	log := fakeHerdrScript(t, `
echo "$@" >> %[1]s
case "$1 $2" in
  "tab create") printf '{"result":{"root_pane":{"pane_id":"w1:p9"}}}' ;;
  "agent start") printf '{"result":{}}' ;;
esac
`)
	_, _, err := Start(Launch{Name: "ZEN-7", Cwd: "/r", Label: "ZEN-7  t", Env: []string{"CLAUDE_CONFIG_DIR=/h/.claude-m0"}, Inner: []string{"claude", "--resume", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(log(), "\n")
	if len(lines) != 2 {
		t.Fatalf("calls = %q", lines)
	}
	if want := "tab create --cwd /r --label ZEN-7  t --env CLAUDE_CONFIG_DIR=/h/.claude-m0 --no-focus"; lines[0] != want {
		t.Errorf("tab create = %q\nwant        %q", lines[0], want)
	}
	if want := "agent start zen-7 --kind claude --pane w1:p9 --timeout 120000 -- --resume x"; lines[1] != want {
		t.Errorf("agent start = %q\nwant         %q", lines[1], want)
	}
}

// A session that stops at a trust question or a login is running and named;
// herdr just refuses to call it ready. That is a start, not a failure.
func TestStartTreatsABlockedSessionAsStarted(t *testing.T) {
	fakeHerdrScript(t, `
: %[1]s
case "$1 $2" in
  "tab create") printf '{"result":{"root_pane":{"pane_id":"w1:p3"}}}' ;;
  "agent start") printf '{"error":{"code":"agent_not_ready","message":"blocked during startup"}}'; exit 1 ;;
esac
`)
	pane, _, err := Start(Launch{Name: "ZEN-7", Cwd: "/r", Inner: []string{"claude"}})
	if pane != "w1:p3" || err != ErrBlockedAtStart {
		t.Errorf("Start = %q, %v; want the pane and ErrBlockedAtStart", pane, err)
	}
	spec := session.LaunchSpec{Args: []string{"agent", "start", "ZEN-7", "--cwd", "/r", "--", "claude"}, Name: "ZEN-7", Cwd: "/r"}
	if out, err := Run(spec); err != nil || !strings.Contains(out, "waiting on a prompt") {
		t.Errorf("Run = %q, %v; want a start that says it is waiting", out, err)
	}
}

func TestStopWaitsForTheProcessToExit(t *testing.T) {
	old := unknownPidWait
	unknownPidWait = time.Millisecond
	t.Cleanup(func() { unknownPidWait = old })
	log := fakeHerdrScript(t, `
echo "$@" >> %[1]s
`)
	if err := Stop("/peer.sock", "w2:p4", 0); err != nil {
		t.Fatal(err)
	}
	if got := log(); got != "pane close w2:p4" {
		t.Errorf("stop ran %q", got)
	}
}

func listen(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

// Every layout a deck has used for its server is found, and a socket file
// whose server is gone is not.
func TestPeersFindsEveryDeckLayout(t *testing.T) {
	// Short root: a unix socket path must fit in 108 bytes.
	home, err := os.MkdirTemp("/tmp", "tdh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	own := filepath.Join(home, ".config", "herdr", "herdr.sock")
	t.Setenv("HERDR_SOCKET_PATH", own)

	listen(t, own)
	listen(t, filepath.Join(home, ".config", "herdr", "sessions", "m0", "herdr.sock"))
	listen(t, filepath.Join(home, ".config-support", "herdr", "herdr.sock"))
	// A server that stopped leaves its socket file behind.
	dead := filepath.Join(home, ".config", "herdr", "sessions", "old", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(dead), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dead, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, w := range Peers() {
		names = append(names, w.Name())
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "m0,support" {
		t.Errorf("peers = %q, want m0,support (own and dead excluded)", got)
	}
}
