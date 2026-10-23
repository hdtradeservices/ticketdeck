package session

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// Tail is what the end of a session's transcript says that its live status
// can't: whether the last turn ended on an error rather than a reply, what the
// session last did, and what it is called.
//
// herdr reads a session's state off its screen, so a session that stopped
// because its account ran out reads "idle" exactly like one that finished. The
// transcript records the difference as a synthetic assistant message carrying
// the API error, which is what makes moving it to another account safe to
// automate: the turn is over, nothing is in flight, and the conversation says
// exactly where it stopped.
type Tail struct {
	// Err is the API error the last assistant entry carries, nil when the last
	// assistant entry is a real reply.
	Err *APIError
	// Title is the session's name: one the user gave it (/rename, --name) wins
	// over the one Claude generates from the conversation.
	Title       string
	CustomTitle bool
	LastTool    string // e.g. "Bash: go test ./...", "Edit model.go"
	LastToolAt  time.Time
	Model       string
	// ContextTokens is the prompt size of the last reply: how full the context
	// window was when the session last answered.
	ContextTokens int
	UpdatedAt     time.Time // timestamp of the newest dated entry
	// LastUUID is the newest assistant entry of the main conversation, reply
	// or error. While it is a cut-off's own id, nothing has answered it.
	LastUUID string
}

// APIError is a turn that ended on an API error instead of a reply.
type APIError struct {
	Kind      string // "rate_limit", "authentication_failed", "server_error", …
	Text      string // what Claude Code showed the user
	At        time.Time
	UUID      string    // identifies this one occurrence, so it is acted on once
	ResetsAt  time.Time // when a rate limit lifts; zero when unknown
	LimitType string    // "five_hour", "seven_day", … for a rate limit
}

// RateLimited reports whether the turn ended because the account ran out.
func (e *APIError) RateLimited() bool { return e != nil && e.Kind == "rate_limit" }

// AuthFailed reports whether the turn ended because the account's login lapsed.
func (e *APIError) AuthFailed() bool { return e != nil && e.Kind == "authentication_failed" }

// Limited reports whether the session is parked on a rate limit that has not
// lifted yet. A limit with no reset time counts until something else is
// written: the session itself is the only thing that can say it lifted.
func (t Tail) Limited(now time.Time) bool {
	return t.Err.RateLimited() && (t.Err.ResetsAt.IsZero() || now.Before(t.Err.ResetsAt))
}

type tailLine struct {
	Type              string    `json:"type"`
	Timestamp         time.Time `json:"timestamp"`
	UUID              string    `json:"uuid"`
	IsSidechain       bool      `json:"isSidechain"`
	IsAPIErrorMessage bool      `json:"isApiErrorMessage"`
	Error             string    `json:"error"`
	QuotaLimits       *struct {
		ResetsAt      int64  `json:"resetsAt"` // epoch seconds
		RateLimitType string `json:"rateLimitType"`
	} `json:"quotaLimits"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	Message     struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *usage `json:"usage"`
	} `json:"message"`
}

// usage is one API response's token counts.
type usage struct {
	InputTokens         int `json:"input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreation       *struct {
		OneHour  int `json:"ephemeral_1h_input_tokens"`
		FiveMins int `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
}

var (
	assistantMark = []byte(`"type":"assistant"`)
	titleMarks    = [][]byte{[]byte(`"type":"custom-title"`), []byte(`"type":"ai-title"`)}
)

// apply folds one transcript line into the tail. Only assistant entries and
// title records are parsed; everything else — tool results especially, which
// can run to megabytes — is skipped on a byte search. An API response's line
// also returns what the response spent. Claude Code writes a line per content
// block, each repeating the response's usage, so the caller counts each
// response id once.
func (t *Tail) apply(line []byte) (spent spend, ok bool) {
	isTitle := bytes.Contains(line, titleMarks[0]) || bytes.Contains(line, titleMarks[1])
	if !isTitle && !bytes.Contains(line, assistantMark) {
		return spend{}, false
	}
	var l tailLine
	if json.Unmarshal(line, &l) != nil {
		return spend{}, false
	}
	if l.Type == "assistant" && l.Message.Usage != nil && l.Message.ID != "" && !l.IsAPIErrorMessage {
		spent, ok = spend{id: l.Message.ID, at: l.Timestamp, usd: costOf(l.Message.Model, *l.Message.Usage)}, true
	}
	if !l.Timestamp.IsZero() && l.Timestamp.After(t.UpdatedAt) {
		t.UpdatedAt = l.Timestamp
	}
	switch l.Type {
	case "custom-title":
		if s := strings.TrimSpace(l.CustomTitle); s != "" {
			t.Title, t.CustomTitle = s, true
		}
		return spent, ok
	case "ai-title":
		if s := strings.TrimSpace(l.AITitle); s != "" && !t.CustomTitle {
			t.Title = s
		}
		return spent, ok
	case "assistant":
	default:
		return spent, ok
	}
	if l.IsSidechain {
		return spent, ok
	}
	if l.UUID != "" {
		t.LastUUID = l.UUID
	}
	if l.IsAPIErrorMessage {
		e := &APIError{Kind: l.Error, At: l.Timestamp, UUID: l.UUID}
		if e.Kind == "" {
			e.Kind = "api_error"
		}
		for _, c := range l.Message.Content {
			if c.Type == "text" {
				e.Text += c.Text
			}
		}
		if q := l.QuotaLimits; q != nil {
			if q.ResetsAt > 0 {
				e.ResetsAt = time.Unix(q.ResetsAt, 0)
			}
			e.LimitType = q.RateLimitType
		}
		t.Err = e
		return spent, ok
	}
	// A real reply: whatever error came before it has been answered.
	t.Err = nil
	if m := l.Message.Model; m != "" && m != "<synthetic>" {
		t.Model = m
	}
	if u := l.Message.Usage; u != nil {
		if n := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens; n > 0 {
			t.ContextTokens = n
		}
	}
	for _, c := range l.Message.Content {
		if c.Type == "tool_use" && c.Name != "" {
			t.LastTool, t.LastToolAt = summarizeTool(c.Name, c.Input), l.Timestamp
		}
	}
	return spent, ok
}

// summarizeTool renders a tool call as a few words: the tool, and the one input
// that says what it is doing.
func summarizeTool(name string, input json.RawMessage) string {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		NotebookPth string `json:"notebook_path"`
		Path        string `json:"path"`
		Pattern     string `json:"pattern"`
		Query       string `json:"query"`
		URL         string `json:"url"`
		Skill       string `json:"skill"`
		Prompt      string `json:"prompt"`
	}
	_ = json.Unmarshal(input, &in)
	detail := ""
	switch name {
	case "Bash":
		detail = firstNonEmpty(in.Description, firstLineOf(in.Command))
	case "Read", "Edit", "Write", "MultiEdit":
		detail = filepath.Base(in.FilePath)
	case "NotebookEdit":
		detail = filepath.Base(in.NotebookPth)
	case "Grep", "Glob":
		detail = in.Pattern
	case "WebSearch":
		detail = in.Query
	case "WebFetch":
		detail = in.URL
	case "Agent", "Task":
		detail = in.Description
	case "Skill":
		detail = "/" + strings.TrimPrefix(in.Skill, "/")
	default:
		if server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__"); ok && strings.HasPrefix(name, "mcp__") {
			name = tool
			detail = strings.TrimPrefix(server, "claude_ai_")
		}
	}
	detail = strings.TrimSpace(detail)
	if detail == "" || detail == "." || detail == "/" {
		return name
	}
	return name + ": " + detail
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// TailOf reads the tail of a session's transcript under one account.
func TailOf(configDir, id string) Tail {
	path := FindTranscript(configDir, id)
	if path == "" {
		return Tail{}
	}
	return scanFile(path, time.Now()).tail
}
