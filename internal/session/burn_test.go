package session

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// response is one API response as Claude Code writes it: a line per content
// block, each repeating the response's id and usage.
func response(id, model string, at time.Time, blocks int, usage string) []string {
	var out []string
	for i := 0; i < blocks; i++ {
		out = append(out, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"uuid":"%s-%d","message":{"id":%q,"model":%q,"content":[{"type":"text","text":"x"}],"usage":%s}}`,
			at.UTC().Format(time.RFC3339), id, i, id, model, usage))
	}
	return out
}

func TestBurnCountsEachResponseOnceAtItsModelsRates(t *testing.T) {
	cfg := t.TempDir()
	id := DeterministicID("ZEN-7")
	path := TranscriptPathIn(cfg, id, "/home/x/Repos")
	now := time.Now()

	// Opus 5.5: 1M cache reads at $0.20, 10k 1-hour cache writes at 2×$4,
	// 1k output at $20 → $0.20 + $0.08 + $0.02 = $0.30, written three times.
	var lines []string
	lines = append(lines, response("msg_a", "claude-opus-5-5", now.Add(-time.Hour), 3,
		`{"input_tokens":0,"cache_creation_input_tokens":10000,"cache_read_input_tokens":1000000,"output_tokens":1000,"cache_creation":{"ephemeral_1h_input_tokens":10000,"ephemeral_5m_input_tokens":0}}`)...)
	// Older than the window: not counted.
	lines = append(lines, response("msg_old", "claude-opus-5-5", now.Add(-6*time.Hour), 1,
		`{"input_tokens":0,"cache_read_input_tokens":5000000,"output_tokens":0}`)...)
	// Sonnet 5, cache writes with no breakdown count at the 5-minute rate:
	// 100k × $2 × 1.25 = $0.25.
	lines = append(lines, response("msg_b", "claude-sonnet-5", now.Add(-time.Minute), 2,
		`{"input_tokens":0,"cache_creation_input_tokens":100000,"cache_read_input_tokens":0,"output_tokens":0}`)...)
	writeLines(t, path, lines...)
	// A subagent's own transcript spends the same account: 10k Haiku 5.5
	// output at $0.50 = $0.005.
	writeLines(t, filepath.Join(path[:len(path)-len(".jsonl")], "subagents", "agent-1.jsonl"),
		response("msg_c", "claude-haiku-5-5", now.Add(-time.Minute), 1, `{"input_tokens":0,"output_tokens":10000}`)...)

	a := ActivityOf(cfg, id, "/home/x/Repos", time.Time{})
	if want := 0.30 + 0.25 + 0.005; math.Abs(a.Burn-want) > 1e-9 {
		t.Errorf("burn = $%.4f, want $%.4f", a.Burn, want)
	}
	// Reading again adds nothing: the scan is incremental, and the response
	// ids it has seen stay seen.
	if b := ActivityOf(cfg, id, "/home/x/Repos", time.Time{}).Burn; math.Abs(b-a.Burn) > 1e-9 {
		t.Errorf("second read = $%.4f, first $%.4f", b, a.Burn)
	}
}

func TestUnknownModelsCostAsOpus(t *testing.T) {
	u := usage{OutputTokens: 1_000_000}
	if got := costOf("claude-something-new", u); got != 25 {
		t.Errorf("an unknown model's million output tokens = $%.2f, want Opus 5's $25", got)
	}
	if got := costOf("claude-opus-5-5[1m]", u); got != 20 {
		t.Errorf("claude-opus-5-5[1m] = $%.2f, want Opus 5.5's $20", got)
	}
}
