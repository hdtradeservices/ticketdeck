package main

import (
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
