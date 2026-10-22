package session

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BurnWindow is how far back a session's spend is summed: the length of the
// usage window that stops sessions mid-task.
const BurnWindow = 5 * time.Hour

// spend is what one API response cost.
type spend struct {
	id  string
	at  time.Time
	usd float64
}

// price is a model's API rate in dollars per million tokens. The deck uses it
// to weigh token counts against each other, since a cache read and an output
// token differ in cost by two orders of magnitude, and so its totals are what
// the same work would cost on usage credits, which bill at these rates.
type price struct{ in, out, read float64 }

// prices are Anthropic's first-party rates as published on 2026-10-06. Cache
// writes cost 1.25× input for the 5-minute TTL and 2× for the hour.
var prices = []struct {
	prefix string
	price
}{
	{"claude-fable-5-1", price{10, 50, 0.25}},
	{"claude-mythos-5-1", price{10, 50, 0.25}},
	{"claude-fable-5", price{10, 50, 1}},
	{"claude-mythos-5", price{10, 50, 1}},
	{"claude-opus-5-5", price{4, 20, 0.20}},
	{"claude-opus-5", price{5, 25, 0.50}},
	{"claude-opus-4", price{5, 25, 0.50}},
	{"claude-sonnet-5-5", price{2, 10, 0.20}},
	{"claude-sonnet-5", price{2, 10, 0.20}},
	{"claude-sonnet-4", price{3, 15, 0.30}},
	{"claude-haiku-5-5", price{0.10, 0.50, 0.01}},
	{"claude-haiku-4", price{1, 5, 0.10}},
}

// fallbackPrice is Opus 5's, for a model the table doesn't know: a session
// with an unrecognized model is still counted, at a plausible Opus rate.
var fallbackPrice = price{5, 25, 0.50}

func priceOf(model string) price {
	for _, p := range prices {
		if strings.HasPrefix(model, p.prefix) {
			return p.price
		}
	}
	return fallbackPrice
}

func costOf(model string, u usage) float64 {
	p := priceOf(model)
	hour, five := 0, u.CacheCreationTokens
	if c := u.CacheCreation; c != nil {
		hour, five = c.OneHour, c.FiveMins
	}
	tokens := float64(u.InputTokens)*p.in +
		float64(five)*p.in*1.25 + float64(hour)*p.in*2 +
		float64(u.CacheReadTokens)*p.read +
		float64(u.OutputTokens)*p.out
	return tokens / 1e6
}

// addSpend records one response, once: the lines of one response arrive
// together, so the last few ids are enough to tell a repeat.
func (s *bgScan) addSpend(sp spend) {
	for i := len(s.spends) - 1; i >= 0 && i >= len(s.spends)-8; i-- {
		if s.spends[i].id == sp.id {
			return
		}
	}
	s.spends = append(s.spends, sp)
}

// burn sums what the transcript recorded spending in the last BurnWindow, and
// forgets older responses.
func (s *bgScan) burn(now time.Time) float64 {
	cut := now.Add(-BurnWindow)
	keep := s.spends[:0]
	var usd float64
	for _, sp := range s.spends {
		if sp.at.Before(cut) {
			continue
		}
		keep = append(keep, sp)
		usd += sp.usd
	}
	// The dropped tail of the backing array still holds old entries; clear the
	// reference so a long-lived scan doesn't pin them.
	clear(s.spends[len(keep):])
	s.spends = keep
	return usd
}

// subagentBurn is what a session's subagents spent in the last BurnWindow,
// read from their own transcripts beside the session's. Only files written
// inside the window are read.
func subagentBurn(path string, now time.Time) float64 {
	files, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	var usd float64
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || now.Sub(fi.ModTime()) > BurnWindow {
			continue
		}
		usd += scanFile(f, now).burn
	}
	return usd
}
