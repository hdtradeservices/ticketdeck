package herd

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Workspace is one herdr server on this machine.
//
// A deck used to run its own server per account: `deck --account NAME` gave
// each subscription an overlay XDG_CONFIG_HOME with its own socket, and `rdeck`
// gives each one a named session. Sessions started by those decks live on in
// their servers, so a deck that runs every account has to look in all of them
// to know what is already running.
type Workspace struct {
	Socket string
	// Session is the named herdr session the server runs ("" for a default
	// server, including an overlay deck's).
	Session string
	// Overlay is the account name of a `deck --account NAME` overlay server
	// (~/.config-NAME), "" otherwise.
	Overlay string
}

// Name says which deck the workspace belonged to, for the screen.
func (w Workspace) Name() string {
	switch {
	case w.Session != "":
		return w.Session
	case w.Overlay != "":
		return w.Overlay
	}
	return "default"
}

// OwnSocket is the socket of the server this process talks to: the one its pane
// runs in, else herdr's default.
func OwnSocket() string {
	if s := os.Getenv("HERDR_SOCKET_PATH"); s != "" {
		return filepath.Clean(s)
	}
	return defaultSocket()
}

func defaultSocket() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "herdr", "herdr.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

// dialTimeout bounds the liveness probe. A live server answers in well under a
// millisecond; a stale socket file refuses at once. The bound is for a wedged
// server, which must not stall the status poll.
var dialTimeout = 300 * time.Millisecond

// Peers lists the other herdr servers on this machine that are accepting
// connections: the default server, every named session, and every per-account
// overlay, minus the one this process runs in. A socket file whose server is
// gone is skipped — that is what every server stopped by a restart leaves.
func Peers() []Workspace {
	own := OwnSocket()
	var out []Workspace
	for _, w := range candidates() {
		if w.Socket == own || !alive(w.Socket) {
			continue
		}
		out = append(out, w)
	}
	return out
}

func candidates() []Workspace {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Workspace
	add := func(w Workspace) {
		w.Socket = filepath.Clean(w.Socket)
		if seen[w.Socket] {
			return
		}
		if _, err := os.Stat(w.Socket); err != nil {
			return
		}
		seen[w.Socket] = true
		out = append(out, w)
	}
	bases := []string{filepath.Join(home, ".config")}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.Clean(x) != bases[0] {
		bases = append(bases, x)
	}
	for _, base := range bases {
		add(Workspace{Socket: filepath.Join(base, "herdr", "herdr.sock")})
		names, _ := filepath.Glob(filepath.Join(base, "herdr", "sessions", "*", "herdr.sock"))
		for _, s := range names {
			add(Workspace{Socket: s, Session: filepath.Base(filepath.Dir(s))})
		}
	}
	overlays, _ := filepath.Glob(filepath.Join(home, ".config-*", "herdr", "herdr.sock"))
	for _, s := range overlays {
		acct := strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(s))), ".config-")
		add(Workspace{Socket: s, Overlay: acct})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Socket < out[j].Socket })
	return out
}

// Alive reports whether a herdr server is accepting connections on socket.
func Alive(socket string) bool { return alive(socket) }

func alive(socket string) bool {
	c, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
