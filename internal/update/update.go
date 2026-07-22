// Package update handles version reporting, a cached "newer release available"
// check, and a one-command self-update — so a team can stay current without
// manual steps.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	repo         = "hdtradeservices/ticketdeck"
	installURL   = "https://raw.githubusercontent.com/" + repo + "/main/install.sh"
	latestAPI    = "https://api.github.com/repos/" + repo + "/releases/latest"
	checkEvery   = 24 * time.Hour
	checkTimeout = 3 * time.Second
)

type cache struct {
	CheckedUnix int64  `json:"checked_unix"`
	Latest      string `json:"latest"`
}

func cachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ticketdeck", "update-check.json")
}

// Check returns the latest release tag when it is newer than the running
// version, else "". It caches the result for a day and fails silently (offline,
// rate-limited, dev build) so it never blocks or nags. `now` is injected for
// testability; pass time.Now().
func Check(current string, now time.Time) string {
	if current == "" || current == "dev" {
		return "" // unversioned/local build — nothing to compare against
	}
	if _, ok := parseVer(current); !ok {
		return "" // a `git describe` / source build (e.g. v0.1.0-5-gabc) — don't nag
	}
	latest := cachedLatest(now)
	if latest == "" {
		latest = fetchLatest()
		if latest != "" {
			writeCache(cache{CheckedUnix: now.Unix(), Latest: latest})
		}
	}
	if latest != "" && Newer(latest, current) {
		return latest
	}
	return ""
}

// cachedLatest returns the cached latest tag if the cache is fresh, else "".
func cachedLatest(now time.Time) string {
	p := cachePath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var c cache
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	if now.Sub(time.Unix(c.CheckedUnix, 0)) > checkEvery {
		return ""
	}
	return c.Latest
}

func writeCache(c cache) {
	p := cachePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if b, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
}

func fetchLatest() string {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestAPI, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(res.Body).Decode(&body) != nil {
		return ""
	}
	return body.TagName
}

// Newer reports whether release tag `latest` is a newer version than `current`.
// Both are compared as dotted numeric versions (leading "v" ignored); on any
// parse ambiguity it falls back to simple inequality.
func Newer(latest, current string) bool {
	lp, lok := parseVer(latest)
	cp, cok := parseVer(current)
	if !lok || !cok {
		return latest != current
	}
	for i := 0; i < len(lp) || i < len(cp); i++ {
		var a, b int
		if i < len(lp) {
			a = lp[i]
		}
		if i < len(cp) {
			b = cp[i]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

func parseVer(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// Run performs a self-update by re-running the published installer, which pulls
// the latest release (or builds from source). Output streams to the terminal.
func Run() error {
	fmt.Println("Updating TicketDeck from", installURL, "…")
	c := exec.Command("bash", "-c", "curl -fsSL "+installURL+" | bash")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	return c.Run()
}
