package ledger

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func tempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	old := dir
	dir = func() string { return d }
	t.Cleanup(func() { dir = old })
	return d
}

func TestUpdateCreatesAndAmends(t *testing.T) {
	tempDir(t)
	if err := Update("s1", func(e *Entry) {
		e.Key, e.Kind, e.Name, e.Account = "notes", Scratch, "Deploy notes", "m0"
	}); err != nil {
		t.Fatal(err)
	}
	if err := Update("s1", func(e *Entry) {
		e.Moves = append(e.Moves, Move{From: "m0", To: "support", Why: "5h limit"})
		e.Account = "support"
	}); err != nil {
		t.Fatal(err)
	}
	e := Load()["s1"]
	if e.Name != "Deploy notes" || e.Account != "support" || e.Kind != Scratch {
		t.Errorf("entry = %+v", e)
	}
	if m, ok := e.LastMove(); !ok || m.To != "support" {
		t.Errorf("last move = %+v", m)
	}
	if e.Updated.IsZero() {
		t.Error("Update must stamp the entry")
	}
}

func TestOldEntriesArePruned(t *testing.T) {
	tempDir(t)
	old := now
	t.Cleanup(func() { now = old })

	now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	if err := Update("stale", func(e *Entry) { e.Kind = Scratch }); err != nil {
		t.Fatal(err)
	}
	now = func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) }
	if err := Update("fresh", func(e *Entry) { e.Kind = Scratch }); err != nil {
		t.Fatal(err)
	}
	all := Load()
	if _, ok := all["stale"]; ok {
		t.Error("an entry untouched for two months should be pruned")
	}
	if _, ok := all["fresh"]; !ok {
		t.Error("the fresh entry was lost")
	}
}

// Every deck on the machine writes the same file; concurrent updates must all
// land rather than the last writer winning.
func TestConcurrentUpdatesAllLand(t *testing.T) {
	tempDir(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Update(string(rune('a'+i)), func(e *Entry) { e.Kind = Ticket }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(Load()); n != 20 {
		t.Errorf("entries = %d, want 20", n)
	}
}

// A lock file a killed deck left behind holds no lock: flock dies with its
// holder, so the next write goes straight through.
func TestALeftoverLockFileDoesNotBlock(t *testing.T) {
	d := tempDir(t)
	if err := os.WriteFile(filepath.Join(d, "sessions.json.lock"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Update("s", func(e *Entry) {}); err != nil {
		t.Fatalf("a lock file left by a killed deck must not block writes: %v", err)
	}
}

// A lock someone holds is waited for, and the wait gives up rather than hang.
func TestAHeldLockIsWaitedFor(t *testing.T) {
	d := tempDir(t)
	old := lockWait
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
	f, err := os.OpenFile(filepath.Join(d, "sessions.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Skip("flock unavailable:", err)
	}
	if err := Update("s", func(e *Entry) {}); err == nil {
		t.Error("Update went ahead while another holder had the lock")
	}
}

func TestSortedNewestFirst(t *testing.T) {
	a := Entry{SessionID: "a", Updated: time.Unix(10, 0)}
	b := Entry{SessionID: "b", Updated: time.Unix(20, 0)}
	got := Sorted(map[string]Entry{"a": a, "b": b})
	if got[0].SessionID != "b" {
		t.Errorf("order = %v, want b first", []string{got[0].SessionID, got[1].SessionID})
	}
}
