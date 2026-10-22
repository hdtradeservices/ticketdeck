package account

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The workbench's drift as found on 2026-10-22: m1 had no herdr hook, and only
// some accounts pinned a model. Each account's hooks live under its own dir, so
// the same script must compare equal across them.
func TestReadProfileAndDrift(t *testing.T) {
	home := fakeHome(t, Default, "m1")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{
		"model": "claude-opus-5",
		"remoteControlAtStartup": true,
		"env": {"ZENTAIL_SRC": "/secret/path"},
		"enabledPlugins": {"gopls-lsp@official": true, "off@x": false},
		"hooks": {"SessionStart": [
			{"hooks": [{"type": "command", "command": "bash \"~/.claude/hooks/clone-state-check.sh\""}]},
			{"matcher": "^(startup|resume)$", "hooks": [{"type": "command", "command": "bash '~/.claude/hooks/herdr-agent-state.sh' session"}]}
		]}
	}`)
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"linear": {}, "Sentry": {}}}`)
	writeFile(t, filepath.Join(home, ".claude-m1", "settings.json"), `{
		"autoCompactWindow": 400000,
		"hooks": {"SessionStart": [
			{"hooks": [{"type": "command", "command": "bash \"~/.claude-m1/hooks/clone-state-check.sh\""}]}
		]}
	}`)
	writeFile(t, filepath.Join(home, ".claude-m1", ".claude.json"), `{"mcpServers": {"linear": {}}}`)

	accts := All()
	primary, _ := Find(accts, filepath.Join(home, ".claude"))
	m1, _ := Find(accts, "m1")
	p, q := ReadProfile(primary), ReadProfile(m1)

	if !slices.Equal(p.Env, []string{"ZENTAIL_SRC"}) || !slices.Equal(p.Plugins, []string{"gopls-lsp@official"}) {
		t.Errorf("env %v plugins %v", p.Env, p.Plugins)
	}
	if !slices.Equal(p.MCP, []string{"Sentry", "linear"}) {
		t.Errorf("the primary's MCP servers come from ~/.claude.json, got %v", p.MCP)
	}
	got := p.Drift(q)
	want := []string{
		"model claude-opus-5 → default",
		"auto-compact default → 400000",
		"no Remote Control",
		"no MCP Sentry",
		"no hook SessionStart herdr-agent-state.sh",
		"no plugin gopls-lsp@official",
		"no env ZENTAIL_SRC",
	}
	if !slices.Equal(got, want) {
		t.Errorf("drift =\n %q\nwant\n %q", got, want)
	}
	if d := WithoutModel(got); len(d) != len(want)-1 || slices.ContainsFunc(d, func(s string) bool { return s == want[0] }) {
		t.Errorf("WithoutModel = %q", d)
	}
	if d := p.Drift(p); len(d) != 0 {
		t.Errorf("an account doesn't drift from itself: %q", d)
	}
}

// An account with no settings file can't be compared: it reports nothing rather
// than a list of everything it lacks.
func TestDriftNeedsBothSettings(t *testing.T) {
	home := fakeHome(t, Default, "m0")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"model": "opus"}`)
	accts := All()
	primary, _ := Find(accts, filepath.Join(home, ".claude"))
	m0, _ := Find(accts, "m0")
	if d := ReadProfile(primary).Drift(ReadProfile(m0)); d != nil {
		t.Errorf("drift against an unread account = %q, want none", d)
	}
}

func TestHookName(t *testing.T) {
	for cmd, want := range map[string]string{
		`bash "~/.claude-m0/hooks/mcp-drift-check.sh"`:             "mcp-drift-check.sh",
		`bash '~/.claude/hooks/herdr-agent-state.sh' session`:      "herdr-agent-state.sh",
		`memory-write-reminder.sh`:                                 "memory-write-reminder.sh",
		`/usr/local/bin/herdr pane report-agent --source claude x`: "herdr",
		`jq -r .x`: "jq",
	} {
		if got := hookName(cmd); got != want {
			t.Errorf("hookName(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// support saved its effort for one model only (/effort writes modelSettings),
// so a session on another model runs at the default there.
func TestPerModelSettingsArePartOfTheSetup(t *testing.T) {
	home := fakeHome(t, Default, "support")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TICKETDECK_ACCOUNT", "")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"effortLevel": "high"}`)
	writeFile(t, filepath.Join(home, ".claude-support", "settings.json"), `{"modelSettings": {"claude-opus-5": {"effortLevel": "high", "autoCompactWindow": 400000}}}`)
	accts := All()
	primary, _ := Find(accts, filepath.Join(home, ".claude"))
	support, _ := Find(accts, "support")
	got := ReadProfile(primary).Drift(ReadProfile(support))
	want := []string{"effort high → default (claude-opus-5 high)", "auto-compact default → default (claude-opus-5 400000)"}
	if !slices.Equal(got, want) {
		t.Errorf("drift = %q, want %q", got, want)
	}
}
