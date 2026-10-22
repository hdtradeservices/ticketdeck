package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The on-disk half of a poll: which accounts hold a transcript for each
// session id the deck cares about, and when each last wrote to it.
//
// An account accumulates hundreds of project directories and thousands of
// transcripts, and this runs on the three-second status tick. A directory's
// mtime changes only when an entry is added or removed — not when a transcript
// grows — so each directory's listing is cached against its mtime, and a tick
// costs one stat per directory plus one per wanted transcript.

type dirListing struct {
	mod   time.Time
	names map[string]bool // transcript ids in the directory
}

var (
	listMu   sync.Mutex
	listings = map[string]dirListing{}
)

// transcriptTimes maps each wanted session id to the mtime of its newest
// transcript under configDir, and the path of that transcript.
func transcriptTimes(configDir string, want map[string]bool) map[string]found {
	out := map[string]found{}
	if len(want) == 0 {
		return out
	}
	root := filepath.Join(configDir, "projects")
	ents, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		for id := range idsIn(dir) {
			if !want[id] {
				continue
			}
			p := filepath.Join(dir, id+".jsonl")
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if prev, ok := out[id]; !ok || fi.ModTime().After(prev.mod) {
				out[id] = found{path: p, mod: fi.ModTime()}
			}
		}
	}
	return out
}

type found struct {
	path string
	mod  time.Time
}

// idsIn lists the transcript ids in one project directory, from cache when the
// directory hasn't changed.
func idsIn(dir string) map[string]bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	listMu.Lock()
	l, ok := listings[dir]
	listMu.Unlock()
	if ok && l.mod.Equal(fi.ModTime()) {
		return l.names
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make(map[string]bool, len(ents))
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok && !e.IsDir() {
			names[id] = true
		}
	}
	listMu.Lock()
	listings[dir] = dirListing{mod: fi.ModTime(), names: names}
	listMu.Unlock()
	return names
}
