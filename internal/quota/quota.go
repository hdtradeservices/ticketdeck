// Package quota reads Claude Code's usage limits (the 5-hour and 7-day rate
// windows shown in Claude Code's status line) so the deck can surface them.
//
// It uses the same source as Claude Code's status line: the OAuth usage
// endpoint, authenticated with the token in ~/.claude/.credentials.json. This is
// a metadata read — it does NOT spend model tokens, so it's consistent with the
// deck's "no app-side token spend" rule.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const usageURL = "https://api.anthropic.com/api/oauth/usage"

// Usage is the pair of rate-limit windows Claude Code enforces.
type Usage struct {
	FiveHourPct   float64
	FiveHourReset time.Time
	SevenDayPct   float64
	SevenDayReset time.Time
}

// Fetch reads the OAuth token and queries the usage endpoint. Returns an error
// (to be handled silently by the caller) when there's no OAuth token — e.g. an
// API-key setup — or the request fails.
func Fetch(ctx context.Context) (Usage, error) {
	tok, err := oauthToken()
	if err != nil {
		return Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ticketdeck")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Usage{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("usage endpoint: http %d", res.StatusCode)
	}
	var body struct {
		FiveHour struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return Usage{}, err
	}
	return Usage{
		FiveHourPct:   body.FiveHour.Utilization,
		FiveHourReset: parseTime(body.FiveHour.ResetsAt),
		SevenDayPct:   body.SevenDay.Utilization,
		SevenDayReset: parseTime(body.SevenDay.ResetsAt),
	}, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// oauthToken reads the Claude Code OAuth access token.
func oauthToken() (string, error) {
	if t := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		return t, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "", err
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &creds); err != nil {
		return "", err
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no OAuth token (API-key setup?)")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}
