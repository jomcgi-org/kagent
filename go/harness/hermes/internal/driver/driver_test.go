package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/kagent-dev/kagent/go/harness/hermes/internal/fakehermes"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestMain(m *testing.M) {
	fakehermes.RunIfRequested()
	os.Exit(m.Run())
}

func TestRenderConfigDisablesToolsMemoryAndApprovals(t *testing.T) {
	raw, err := RenderConfig(Settings{Model: "deepseek/deepseek-v4.1-flash", BaseURL: "https://openrouter.ai/api/v1"})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{
		"default": "deepseek/deepseek-v4.1-flash", "provider": "custom",
		"base_url": "https://openrouter.ai/api/v1", "api_key": CredentialPlaceholder,
	}, cfg["model"])
	require.Equal(t, []any{}, cfg["toolsets"])
	require.Equal(t, map[string]any{"api_server": []any{}}, cfg["platform_toolsets"])
	disabled := cfg["agent"].(map[string]any)["disabled_toolsets"].([]any)
	require.Len(t, disabled, len(DisabledToolsets))
	require.Contains(t, disabled, "terminal")
	memory := cfg["memory"].(map[string]any)
	require.Equal(t, false, memory["memory_enabled"])
	require.Equal(t, false, memory["user_profile_enabled"])
	approvals := cfg["approvals"].(map[string]any)
	for _, mode := range []string{"cron_mode", "single_query_mode", "unattended_mode"} {
		require.Equal(t, "deny", approvals[mode], mode)
	}
	require.Equal(t, false, cfg["security"].(map[string]any)["allow_lazy_installs"])

	_, err = RenderConfig(Settings{BaseURL: "https://openrouter.ai/api/v1"})
	require.ErrorContains(t, err, "model and base URL are required")
}

func TestWriteHomeReplacesConfigAndRemovesEnvFile(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".env"), []byte("HERMES_YOLO_MODE=1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte("toolsets: [terminal]\n"), 0o600))
	require.NoError(t, WriteHome(home, Settings{Model: "m", BaseURL: "https://example.com/v1", APIKey: "kagent-credential-injected"}))
	_, err := os.Stat(filepath.Join(home, ".env"))
	require.True(t, os.IsNotExist(err), ".env must be removed")
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "toolsets: []\n")
	hindsight, err := os.ReadFile(filepath.Join(home, "hindsight", "config.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{"memory_mode":"context"}`, string(hindsight))
	info, err := os.Stat(filepath.Join(home, "config.yaml"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestGatewayEnvironmentAllowlist(t *testing.T) {
	env := gatewayEnvironment([]string{
		"PATH=/usr/bin", "SSL_CERT_FILE=/ca.pem", "OTEL_SERVICE_NAME=x", "OPENAI_API_KEY=kagent-credential-injected",
		"DISCORD_BOT_TOKEN=secret", "HERMES_YOLO_MODE=1", "API_SERVER_KEY=attacker", "HERMES_HOME=/elsewhere",
	}, "/data/hermes", 8642, "k")
	require.ElementsMatch(t, []string{
		"PATH=/usr/bin", "SSL_CERT_FILE=/ca.pem", "OTEL_SERVICE_NAME=x", "HERMES_HOME=/data/hermes",
		"API_SERVER_ENABLED=true", "API_SERVER_HOST=127.0.0.1", "API_SERVER_PORT=8642", "API_SERVER_KEY=k",
	}, env)
}

func TestClientRespond(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		body       string
		wantText   string
		wantStatus string
		wantPublic string
		wantErr    string
	}{
		{name: "completed skips commentary", code: 200, body: `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"note"}]},{"type":"reasoning"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`, wantText: "ab"},
		{name: "incomplete keeps the wrap-up", code: 200, body: `{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}`, wantText: "partial"},
		{name: "failed", code: 200, body: `{"status":"failed","output":[],"error":{"message":"provider said no"}}`, wantStatus: "failed", wantPublic: "Hermes turn failed"},
		{name: "cancelled", code: 200, body: `{"status":"cancelled","output":[]}`, wantStatus: "cancelled", wantPublic: "Hermes turn was cancelled"},
		{name: "unsettled is never polled", code: 200, body: `{"status":"in_progress","output":[]}`, wantStatus: "in_progress", wantPublic: "Hermes turn did not settle"},
		{name: "completed without an answer", code: 200, body: `{"status":"completed","output":[]}`, wantStatus: "completed", wantPublic: "Hermes turn finished without an answer"},
		{name: "HTTP error", code: 429, body: `{"error":{"message":"busy"}}`, wantErr: "Hermes HTTP 429"},
		{name: "malformed", code: 200, body: `{`, wantErr: "decode Hermes response"},
		{name: "oversized", code: 200, body: `{"status":"completed","pad":"` + strings.Repeat("x", 2048) + `"}`, wantErr: "larger than"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var seen *http.Request
			var seenBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Clone(context.Background())
				_ = json.NewDecoder(r.Body).Decode(&seenBody)
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			c := &client{baseURL: server.URL + "/", key: "loopback-key", http: server.Client(), maxBytes: 1024}
			result, err := c.respond(t.Context(), request{SessionKey: "kagent-s", IdempotencyKey: "kagent-task-t", Instructions: "be brief", Input: "hi"})
			require.Equal(t, "/v1/responses", seen.URL.Path)
			require.Equal(t, "Bearer loopback-key", seen.Header.Get("Authorization"))
			require.Equal(t, "kagent-s", seen.Header.Get(SessionKeyHeader))
			require.Equal(t, "kagent-task-t", seen.Header.Get("Idempotency-Key"))
			require.Equal(t, map[string]any{"input": "hi", "instructions": "be brief", "store": true, "conversation": "kagent-s"}, seenBody)
			switch {
			case test.wantErr != "":
				require.ErrorContains(t, err, test.wantErr)
				require.NotContains(t, err.Error(), "busy")
			case test.wantPublic != "":
				var turnErr *TurnError
				require.ErrorAs(t, err, &turnErr)
				require.Equal(t, test.wantStatus, turnErr.Status)
				require.Equal(t, test.wantPublic, turnErr.Public())
				require.NotContains(t, turnErr.Public(), "provider said no")
			default:
				require.NoError(t, err)
				require.Equal(t, test.wantText, result.Text)
			}
		})
	}
}

func TestClientRejectsInvalidSessionKey(t *testing.T) {
	c := &client{baseURL: "http://127.0.0.1:1", http: http.DefaultClient, maxBytes: 1}
	for _, key := range []string{"", "has space", "line\nbreak", strings.Repeat("k", 257)} {
		_, err := c.respond(t.Context(), request{SessionKey: key, Input: "hi"})
		require.ErrorContains(t, err, "invalid Hermes session key")
	}
}

type recordingSink struct {
	mu       sync.Mutex
	sessions []string
	text     strings.Builder
	calls    []runtime.ModelCall
	bindErr  error
}

func (s *recordingSink) SessionStarted(e runtime.SessionStarted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, e.ContinuationID)
	return s.bindErr
}
func (s *recordingSink) TextDelta(e runtime.TextDelta) error { s.text.WriteString(e.Text); return nil }
func (*recordingSink) ToolCall(runtime.ToolCall) error       { return nil }
func (*recordingSink) ToolResult(runtime.ToolResult) error   { return nil }
func (s *recordingSink) ModelCall(e runtime.ModelCall)       { s.calls = append(s.calls, e) }

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func newFakeDriver(t *testing.T, environment []string) (*Driver, string) {
	t.Helper()
	home := t.TempDir()
	d, err := New(Config{
		GatewayCommand: fakehermes.Command(), Home: home, Port: freePort(t), Instructions: "be brief",
		Settings: Settings{Model: "deepseek/deepseek-v4.1-flash", BaseURL: "https://openrouter.ai/api/v1"},
		Model:    "deepseek/deepseek-v4.1-flash", Provider: "openai", Environment: environment,
		StartupTimeout: 20 * time.Second, TurnTimeout: 20 * time.Second, StopGrace: 5 * time.Second,
		MaxBytes: 1 << 20, Output: &bytes.Buffer{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d, home
}

func TestRunWithFakeGateway(t *testing.T) {
	d, home := newFakeDriver(t, append(os.Environ(), "DISCORD_BOT_TOKEN=leak", "HERMES_YOLO_MODE=1", "OPENAI_API_KEY=kagent-credential-injected"))
	require.NoError(t, d.Validate(t.Context()))
	require.NoError(t, d.Start(t.Context()))

	first := &recordingSink{}
	outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "task-1", ContextID: "ctx", Prompt: "hello"}, first)
	require.NoError(t, err)
	require.Equal(t, runtime.Outcome{}, outcome)
	require.Equal(t, "echo: hello", first.text.String())
	require.Len(t, first.sessions, 1)
	sessionKey := first.sessions[0]
	require.Regexp(t, regexp.MustCompile(`^kagent-[0-9a-f-]{36}$`), sessionKey)
	require.Len(t, first.calls, 1)
	call := first.calls[0]
	require.True(t, call.UsageOnly)
	require.Equal(t, int64(11), call.InputTokens)
	require.Equal(t, int64(7), call.OutputTokens)
	require.Equal(t, "openai", call.Provider)
	require.Equal(t, "deepseek/deepseek-v4.1-flash", call.RequestModel)
	require.Equal(t, "hermes-agent", call.ResponseModel)

	// A later turn continues the bound conversation and binds nothing new.
	second := &recordingSink{}
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "task-2", ContextID: "another-context", Prompt: "again", ContinuationID: sessionKey}, second)
	require.NoError(t, err)
	require.Empty(t, second.sessions)
	require.Equal(t, "echo: again", second.text.String())

	requests, err := fakehermes.Requests(home)
	require.NoError(t, err)
	require.Len(t, requests, 2)
	for i, request := range requests {
		require.Equal(t, sessionKey, request.SessionKey)
		require.Equal(t, sessionKey, request.Conversation)
		require.True(t, request.Store)
		require.Equal(t, "be brief", request.Instructions)
		require.Regexp(t, `^Bearer [0-9a-f]{64}$`, request.Authorization)
		require.Equal(t, []string{"kagent-task-task-1", "kagent-task-task-2"}[i], request.IdempotencyKey)
	}

	env, err := fakehermes.Env(home)
	require.NoError(t, err)
	for _, name := range []string{"DISCORD_BOT_TOKEN", "HERMES_YOLO_MODE", "OPENAI_API_KEY"} {
		require.False(t, slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, name+"=") }), name)
	}
	require.Contains(t, env, "API_SERVER_HOST=127.0.0.1")
	require.Contains(t, env, "HERMES_HOME="+home)
}

func TestRunFailuresAreSafeOutcomes(t *testing.T) {
	d, _ := newFakeDriver(t, os.Environ())
	for prompt, want := range map[string]string{"fail": "Hermes turn failed", "pending": "Hermes turn did not settle", " ": "Hermes harness requires a text prompt"} {
		sink := &recordingSink{}
		outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t", Prompt: prompt, ContinuationID: "kagent-fixed"}, sink)
		require.NoError(t, err, prompt)
		require.NotNil(t, outcome.Failure, prompt)
		require.Equal(t, want, outcome.Failure.Message)
		require.Nil(t, outcome.Pending)
		require.Empty(t, sink.text.String())
	}
}

func TestRunReportsCancellation(t *testing.T) {
	d, _ := newFakeDriver(t, os.Environ())
	require.NoError(t, d.Start(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := d.Run(ctx, runtime.Turn{TaskID: "t", Prompt: "slow", ContinuationID: "kagent-fixed"}, &recordingSink{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunRestartsAnExitedGateway(t *testing.T) {
	d, _ := newFakeDriver(t, os.Environ())
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "t1", Prompt: "exit", ContinuationID: "kagent-fixed"}, &recordingSink{})
	require.Error(t, err)
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.gateway.running()
	}, 10*time.Second, 50*time.Millisecond)
	sink := &recordingSink{}
	outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t2", Prompt: "back", ContinuationID: "kagent-fixed"}, sink)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Equal(t, "echo: back", sink.text.String())
}

func TestRunStopsWhenTheSessionCannotBind(t *testing.T) {
	d, home := newFakeDriver(t, os.Environ())
	sink := &recordingSink{bindErr: errors.New("disk full")}
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "t", Prompt: "hello"}, sink)
	require.ErrorContains(t, err, "disk full")
	requests, err := fakehermes.Requests(home)
	require.NoError(t, err)
	require.Empty(t, requests, "an unbound turn must not reach Hermes")
}

func TestRunRejectsInputResponse(t *testing.T) {
	d, _ := newFakeDriver(t, os.Environ())
	_, err := d.Run(t.Context(), runtime.Turn{InputResponse: &runtime.ApprovalDecision{ID: "1"}}, &recordingSink{})
	require.ErrorContains(t, err, "no parked turn")
}

// A snapshot restore gives /data new backing files. The next turn finds the
// gateway unhealthy, rewrites HERMES_HOME, and starts a new gateway.
func TestRunRestartsAfterDataReplacement(t *testing.T) {
	d, home := newFakeDriver(t, os.Environ())
	_, err := os.Stat(filepath.Join(home, "config.yaml"))
	require.True(t, os.IsNotExist(err), "the gateway and its config wait for the first turn")
	sink := &recordingSink{}
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "t1", Prompt: "hello", ContinuationID: "kagent-fixed"}, sink)
	require.NoError(t, err)
	require.Equal(t, "echo: hello", sink.text.String())

	// Replace /data: the old directory moves away and a restored one, with an
	// older config and a planted .env, takes its path.
	require.NoError(t, os.Rename(home, home+".old"))
	t.Cleanup(func() { _ = os.RemoveAll(home + ".old") })
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte("toolsets: [terminal]\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".env"), []byte("HERMES_YOLO_MODE=1\n"), 0o600))

	sink = &recordingSink{}
	outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t2", Prompt: "again", ContinuationID: "kagent-fixed"}, sink)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Equal(t, "echo: again", sink.text.String())
	_, err = os.Stat(filepath.Join(home, ".env"))
	require.True(t, os.IsNotExist(err), ".env must be removed before the new gateway starts")
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "toolsets: []\n")
	require.NotContains(t, string(raw), "terminal]")
	requests, err := fakehermes.Requests(home)
	require.NoError(t, err)
	require.Len(t, requests, 1, "the second turn reaches the new gateway")
}

func TestValidateRequiresGatewayExecutable(t *testing.T) {
	d, err := New(Config{GatewayCommand: []string{"/nonexistent/hermes", "gateway"}, Settings: Settings{Model: "m", BaseURL: "https://example.com/v1"}})
	require.NoError(t, err)
	require.ErrorContains(t, d.Validate(t.Context()), "find Hermes gateway executable")
}
