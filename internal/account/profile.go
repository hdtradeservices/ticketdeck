package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Profile is the part of an account's Claude Code setup a session notices when
// it changes account: which model it runs, which tools and hooks it has, and
// when it compacts. Accounts are configured one by one, so they drift apart —
// and a session moved onto one that differs changes model or loses tools
// halfway through a task.
type Profile struct {
	Model string // "" is Claude Code's default
	// Effort is the effort level, with any saved for one model after it.
	Effort string
	// MCP is the user-scope MCP servers, by name.
	MCP []string
	// Hooks is each hook as "<event> <script>", the script by file name: every
	// account keeps its own copy of the same script under its own dir.
	Hooks   []string
	Plugins []string
	// Env is the names set in the settings' env block, never their values.
	Env           []string
	RemoteControl bool
	// AutoCompact is the auto-compact window, "off" when auto-compact is off,
	// "" for Claude Code's default.
	AutoCompact string
	// Read reports that the settings file was there to read. An account without
	// one has nothing to compare.
	Read bool
}

// ReadProfile reads one account's setup from its settings.json and, for its
// MCP servers, the .claude.json its sessions use.
func ReadProfile(a Account) Profile {
	var p Profile
	var s struct {
		Model              string          `json:"model"`
		EffortLevel        string          `json:"effortLevel"`
		Env                map[string]any  `json:"env"`
		EnabledPlugins     map[string]bool `json:"enabledPlugins"`
		RemoteControl      bool            `json:"remoteControlAtStartup"`
		AutoCompactEnabled *bool           `json:"autoCompactEnabled"`
		AutoCompactWindow  json.RawMessage `json:"autoCompactWindow"`
		ModelSettings      map[string]struct {
			EffortLevel       string          `json:"effortLevel"`
			AutoCompactWindow json.RawMessage `json:"autoCompactWindow"`
		} `json:"modelSettings"`
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				URL     string `json:"url"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if b, err := os.ReadFile(filepath.Join(a.ConfigDir, "settings.json")); err == nil && json.Unmarshal(b, &s) == nil {
		p.Read = true
	}
	p.Model, p.Effort, p.RemoteControl = s.Model, s.EffortLevel, s.RemoteControl
	for k := range s.Env {
		p.Env = append(p.Env, k)
	}
	for k, on := range s.EnabledPlugins {
		if on {
			p.Plugins = append(p.Plugins, k)
		}
	}
	for event, entries := range s.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				what := hookName(h.Command)
				if what == "" {
					what = strings.TrimSpace(h.Type + " " + h.URL)
				}
				p.Hooks = append(p.Hooks, event+" "+what)
			}
		}
	}
	switch {
	case s.AutoCompactEnabled != nil && !*s.AutoCompactEnabled:
		p.AutoCompact = "off"
	case len(s.AutoCompactWindow) > 0:
		p.AutoCompact = strings.Trim(string(s.AutoCompactWindow), `"`)
	}
	// A level or window saved for one model (by /effort or /autocompact) wins
	// over the top-level one for that model, so it is part of the setup too.
	var efforts, windows []string
	for model, ms := range s.ModelSettings {
		if ms.EffortLevel != "" {
			efforts = append(efforts, model+" "+ms.EffortLevel)
		}
		if len(ms.AutoCompactWindow) > 0 {
			windows = append(windows, model+" "+strings.Trim(string(ms.AutoCompactWindow), `"`))
		}
	}
	p.Effort = withPerModel(p.Effort, efforts)
	if p.AutoCompact != "off" {
		p.AutoCompact = withPerModel(p.AutoCompact, windows)
	}
	var g struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if b, err := os.ReadFile(globalConfigPath(a.ConfigDir)); err == nil && json.Unmarshal(b, &g) == nil {
		for k := range g.MCPServers {
			p.MCP = append(p.MCP, k)
		}
	}
	for _, l := range [][]string{p.Env, p.Plugins, p.Hooks, p.MCP} {
		slices.Sort(l)
	}
	p.Hooks = slices.Compact(p.Hooks)
	return p
}

// globalConfigPath is the .claude.json the account's sessions read. Claude Code
// keeps it in the home dir while CLAUDE_CONFIG_DIR is unset and in the config
// dir once it is set, and the deck sets it for every account but its own (see
// session.AccountEnv).
func globalConfigPath(configDir string) string {
	if session.AccountEnv(configDir) == nil && os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".claude.json")
		}
	}
	return filepath.Join(configDir, ".claude.json")
}

// hookName is the script a hook command runs, by file name: `bash
// "~/.claude-m0/hooks/x.sh" session` and `bash "~/.claude/hooks/x.sh" session`
// are the same hook installed in two accounts.
func hookName(cmd string) string {
	fields := strings.Fields(cmd)
	for _, f := range fields {
		f = strings.Trim(f, `"'`)
		if strings.ContainsRune(f, '/') || strings.HasSuffix(f, ".sh") || strings.HasSuffix(f, ".py") {
			return filepath.Base(f)
		}
	}
	if len(fields) > 0 {
		return filepath.Base(strings.Trim(fields[0], `"'`))
	}
	return ""
}

// Drift lists what changes for a session that moves from an account set up as
// p to one set up as q, a few words each, empty when nothing does. Unread
// settings on either side compare as nothing: a missing file is not a
// difference anyone can act on.
func (p Profile) Drift(q Profile) []string {
	if !p.Read || !q.Read {
		return nil
	}
	var out []string
	if p.Model != q.Model {
		out = append(out, fmt.Sprintf("model %s → %s", orDefault(p.Model), orDefault(q.Model)))
	}
	if p.Effort != q.Effort {
		out = append(out, fmt.Sprintf("effort %s → %s", orDefault(p.Effort), orDefault(q.Effort)))
	}
	if p.AutoCompact != q.AutoCompact {
		out = append(out, fmt.Sprintf("auto-compact %s → %s", orDefault(p.AutoCompact), orDefault(q.AutoCompact)))
	}
	if p.RemoteControl && !q.RemoteControl {
		out = append(out, "no Remote Control")
	}
	out = append(out, lacks("MCP", p.MCP, q.MCP)...)
	out = append(out, lacks("hook", p.Hooks, q.Hooks)...)
	out = append(out, lacks("plugin", p.Plugins, q.Plugins)...)
	out = append(out, lacks("env", p.Env, q.Env)...)
	return out
}

// WithoutModel is d less its model change: a move keeps the session's model,
// so a different default on the destination doesn't reach it.
func WithoutModel(d []string) []string {
	return slices.DeleteFunc(slices.Clone(d), func(s string) bool { return strings.HasPrefix(s, "model ") })
}

func lacks(what string, have, other []string) []string {
	var missing []string
	for _, h := range have {
		if !slices.Contains(other, h) {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{"no " + what + " " + strings.Join(missing, ", ")}
}

// withPerModel renders a setting and its per-model overrides: "high",
// "default (claude-opus-5 high)".
func withPerModel(top string, perModel []string) string {
	if len(perModel) == 0 {
		return top
	}
	slices.Sort(perModel)
	return orDefault(top) + " (" + strings.Join(perModel, ", ") + ")"
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
