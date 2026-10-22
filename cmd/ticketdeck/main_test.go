package main

import (
	"os"
	"testing"

	"github.com/hdtradeservices/ticketdeck/internal/tui"
)

func TestResolveBackend(t *testing.T) {
	// claude always resolves.
	if _, err := resolveBackend("claude", false, false); err != nil {
		t.Errorf("claude should always resolve: %v", err)
	}
	// auto falls back to claude when herdr is unavailable, uses herdr when available.
	if b, err := resolveBackend("auto", false, false); err != nil {
		t.Errorf("auto/unavailable err: %v", err)
	} else if _, ok := b.(tui.ClaudeBackend); !ok {
		t.Errorf("auto/unavailable should be ClaudeBackend, got %T", b)
	}
	if b, err := resolveBackend("auto", false, true); err != nil {
		t.Errorf("auto/available err: %v", err)
	} else if _, ok := b.(tui.HerdBackend); !ok {
		t.Errorf("auto/available should be HerdBackend, got %T", b)
	}
	// herdr requested but unavailable (non-dry) → error.
	if _, err := resolveBackend("herdr", false, false); err == nil {
		t.Error("herdr/unavailable/non-dry should error")
	}
	// herdr in dry mode → allowed even when unavailable (preview commands).
	if _, err := resolveBackend("herdr", true, false); err != nil {
		t.Errorf("herdr/dry should be allowed without herdr: %v", err)
	}
	// herdr available → resolves.
	if _, err := resolveBackend("herdr", false, true); err != nil {
		t.Errorf("herdr/available err: %v", err)
	}
	if _, err := resolveBackend("bogus", false, false); err == nil {
		t.Error("unknown backend should error")
	}
}

func TestLogRotatesPastItsCap(t *testing.T) {
	path := t.TempDir() + "/ticketdeck.log"
	w, err := openRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte("0123456789012345678901234567890123456789\n") // 41 bytes
	for range 5 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	cur, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal("the full log should have moved aside to .1")
	}
	if cur.Size() > 100 || old.Size() > 100 {
		t.Errorf("sizes %d and %d, cap 100", cur.Size(), old.Size())
	}
	// A log already past the cap at startup moves aside before anything is written.
	big := t.TempDir() + "/ticketdeck.log"
	if err := os.WriteFile(big, make([]byte, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err = openRotating(big, 100)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if fi, _ := os.Stat(big); fi.Size() != 0 {
		t.Errorf("an oversized log should start fresh, size %d", fi.Size())
	}
}
