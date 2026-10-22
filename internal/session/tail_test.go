package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines trimmed from real Claude Code 2.1.307 transcripts (2026-10-21).
const (
	replyLine = `{"type":"assistant","timestamp":"2026-10-21T14:00:00Z","uuid":"r1","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...\ngo vet ./...","description":"Run the tests"}}],"usage":{"input_tokens":12,"cache_creation_input_tokens":2000,"cache_read_input_tokens":140000,"output_tokens":90}}}`
	limitLine = `{"type":"assistant","timestamp":"2026-10-21T14:05:00Z","uuid":"e1","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"quotaLimits":{"status":"rejected","resetsAt":1792630200,"rateLimitType":"five_hour"},"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit · resets 2:50pm (America/New_York)"}]}}`
	authLine  = `{"type":"assistant","timestamp":"2026-10-21T14:06:00Z","uuid":"e2","isApiErrorMessage":true,"error":"authentication_failed","message":{"model":"<synthetic>","content":[{"type":"text","text":"Login expired · Please run /login"}]}}`
	aiTitle   = `{"type":"ai-title","aiTitle":"fix the shipworks sweep","sessionId":"s"}`
	userTitle = `{"type":"custom-title","customTitle":"sweep fix","sessionId":"s"}`
	toolOut   = `{"type":"user","timestamp":"2026-10-21T14:01:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok \"type\":\"assistant\" in output"}]},"toolUseResult":{"stdout":"ok"}}`
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTailSeesARateLimitAndItsReset(t *testing.T) {
	cfg := t.TempDir()
	id := DeterministicID("ZEN-1")
	path := TranscriptPathIn(cfg, id, "/home/x/Repos")
	writeLines(t, path, replyLine, toolOut, limitLine)

	tail := TailOf(cfg, id)
	if !tail.Err.RateLimited() {
		t.Fatalf("Err = %+v, want a rate limit", tail.Err)
	}
	if got := tail.Err.ResetsAt.Unix(); got != 1792630200 {
		t.Errorf("ResetsAt = %d, want 1792630200", got)
	}
	if tail.Err.LimitType != "five_hour" || tail.Err.UUID != "e1" {
		t.Errorf("limit = %+v", tail.Err)
	}
	if !tail.Limited(time.Unix(1792630200-60, 0)) {
		t.Error("a limit that resets in a minute is still in force")
	}
	if tail.Limited(time.Unix(1792630200+1, 0)) {
		t.Error("a limit past its reset time no longer holds the session")
	}
	// The reply before the limit still says what the session was doing.
	if tail.LastTool != "Bash: Run the tests" {
		t.Errorf("LastTool = %q", tail.LastTool)
	}
	if tail.Model != "claude-opus-5-5" {
		t.Errorf("Model = %q, want the real model, not <synthetic>", tail.Model)
	}
	if tail.ContextTokens != 142012 {
		t.Errorf("ContextTokens = %d, want 142012", tail.ContextTokens)
	}

	// A real reply after the limit means the session got going again.
	writeLines(t, path, strings.Replace(replyLine, "r1", "r2", 1))
	if tail := TailOf(cfg, id); tail.Err != nil {
		t.Errorf("a reply after the error must clear it, got %+v", tail.Err)
	}
}

func TestTailKeepsTheLatestErrorKind(t *testing.T) {
	cfg := t.TempDir()
	id := DeterministicID("ZEN-2")
	writeLines(t, TranscriptPathIn(cfg, id, "/r"), replyLine, limitLine, authLine)
	tail := TailOf(cfg, id)
	if !tail.Err.AuthFailed() || tail.Err.RateLimited() {
		t.Errorf("Err = %+v, want the login failure that came last", tail.Err)
	}
	if !strings.Contains(tail.Err.Text, "Login expired") {
		t.Errorf("Text = %q", tail.Err.Text)
	}
}

func TestTailTitlePrefersTheOneTheUserGave(t *testing.T) {
	cfg := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	path := TranscriptPathIn(cfg, id, "/r")
	writeLines(t, path, aiTitle)
	if got := TailOf(cfg, id).Title; got != "fix the shipworks sweep" {
		t.Errorf("AI title = %q", got)
	}
	writeLines(t, path, userTitle, strings.Replace(aiTitle, "fix the shipworks sweep", "later ai title", 1))
	tail := TailOf(cfg, id)
	if tail.Title != "sweep fix" || !tail.CustomTitle {
		t.Errorf("title = %q (custom %v), want the user's name to stick", tail.Title, tail.CustomTitle)
	}
}

func TestSummarizeTool(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"Bash", `{"command":"go test ./...\nmore"}`, "Bash: go test ./..."},
		{"Bash", `{"command":"x","description":"Run tests"}`, "Bash: Run tests"},
		{"Edit", `{"file_path":"/a/b/model.go"}`, "Edit: model.go"},
		{"Grep", `{"pattern":"TODO"}`, "Grep: TODO"},
		{"Agent", `{"description":"review the diff"}`, "Agent: review the diff"},
		{"Skill", `{"skill":"triage"}`, "Skill: /triage"},
		{"mcp__claude_ai_Linear__get_issue", `{}`, "get_issue: Linear"},
		{"TodoWrite", `{}`, "TodoWrite"},
	}
	for _, c := range cases {
		if got := summarizeTool(c.name, []byte(c.input)); got != c.want {
			t.Errorf("summarizeTool(%s, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
}

// fakeProc builds a /proc stand-in holding one process and points procRoot at
// it. starttime is field 22 of the stat line.
func fakeProc(t *testing.T, pid, starttime string, env ...string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// pid (comm with spaces) state, then fields 4..21, then starttime.
	stat := pid + " (claude code) S" + strings.Repeat(" 0", 18) + " " + starttime + " 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(strings.Join(env, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3\nbtime 1792600000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

func writeRegistry(t *testing.T, cfg, pid, body string) {
	t.Helper()
	dir := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pid+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryReadsLiveSessionsWithTheirPane(t *testing.T) {
	fakeProc(t, "4242", "9176865", "HOME=/h", "HERDR_SOCKET_PATH=/h/.config/herdr/herdr.sock", "HERDR_PANE_ID=w1:p7")
	cfg := t.TempDir()
	writeRegistry(t, cfg, "4242", `{"pid":4242,"sessionId":"abc","cwd":"/r","startedAt":1792636109205,"procStart":"9176865","kind":"interactive","name":"scratch-2","status":"waiting","waitingFor":"permission"}`)
	// Exited without cleaning up: its pid is gone from /proc.
	writeRegistry(t, cfg, "999", `{"pid":999,"sessionId":"dead","procStart":"1"}`)

	procs := Registry(cfg)
	if len(procs) != 1 {
		t.Fatalf("Registry = %+v, want only the live process", procs)
	}
	p := procs[0]
	if p.SessionID != "abc" || p.Status != "waiting" || p.WaitingFor != "permission" || p.Name != "scratch-2" {
		t.Errorf("proc = %+v", p)
	}
	if p.HerdrSocket != "/h/.config/herdr/herdr.sock" || p.HerdrPane != "w1:p7" {
		t.Errorf("pane = %q %q", p.HerdrSocket, p.HerdrPane)
	}
	if want := time.Unix(1792600000, 0).Add(9176865 * time.Second / clockTicks); !p.ProcStart.Equal(want) {
		t.Errorf("ProcStart = %v, want %v", p.ProcStart, want)
	}
	if p.ConfigDir != cfg {
		t.Errorf("ConfigDir = %q", p.ConfigDir)
	}
}

// A pid is reused once its process exits. A registry file left behind by a
// killed session must not claim whatever process now holds its pid.
func TestRegistrySkipsAReusedPid(t *testing.T) {
	fakeProc(t, "4242", "5555")
	cfg := t.TempDir()
	writeRegistry(t, cfg, "4242", `{"pid":4242,"sessionId":"abc","procStart":"9176865"}`)
	if procs := Registry(cfg); len(procs) != 0 {
		t.Errorf("Registry = %+v, want none: the pid belongs to a newer process", procs)
	}
}

func TestProcExited(t *testing.T) {
	fakeProc(t, "4242", "1")
	if ProcExited(4242) {
		t.Error("a process in /proc has not exited")
	}
	if !ProcExited(4243) {
		t.Error("a process missing from /proc has exited")
	}
}

func TestAccountEnvClearsAnotherAccountsToken(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/h/.claude")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")
	if got := strings.Join(AccountEnv("/h/.claude-support"), " "); got != "CLAUDE_CONFIG_DIR=/h/.claude-support CLAUDE_CODE_OAUTH_TOKEN=" {
		t.Errorf("other account env = %q", got)
	}
	// Its own account inherits: naming the dir would switch Claude to another
	// .claude.json, and the token is this account's own.
	if got := AccountEnv("/h/.claude"); got != nil {
		t.Errorf("own account env = %q, want nothing (inherit)", got)
	}
	if AccountEnv("") != nil {
		t.Error("no config dir, no env")
	}
}

func TestClaudeArgsResumeOnlyWhereTheTranscriptIs(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	tk := Ticket{Key: "ZEN-5", Title: "t", ConfigDir: a}
	writeLines(t, TranscriptPathIn(a, tk.ID(), "/r"), replyLine)

	if got := ClaudeArgs(tk); got[0] != "--resume" || got[1] != tk.ID() {
		t.Errorf("account holding the transcript should resume, got %v", got[:2])
	}
	tk.ConfigDir = b
	if got := ClaudeArgs(tk); got[0] != "--session-id" {
		t.Errorf("an account without it starts fresh, got %v", got[:2])
	}
	s := Ticket{Key: "notes", Scratch: true, SessionID: "11111111-2222-4333-8444-555555555555", ConfigDir: b}
	got := ClaudeArgs(s)
	if strings.Contains(strings.Join(got, " "), "--append-system-prompt") {
		t.Errorf("an ad-hoc session seeds no ticket identity: %v", got)
	}
	if got[1] != s.SessionID {
		t.Errorf("an ad-hoc session runs under its own id: %v", got)
	}
}

func TestNewIDIsAValidUniqueV4(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b {
		t.Fatal("two ids collided")
	}
	if len(a) != 36 || a[14] != '4' || !strings.ContainsRune("89ab", rune(a[19])) {
		t.Errorf("not a v4 uuid: %s", a)
	}
}
