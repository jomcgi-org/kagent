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
	require.Equal(t, map[string]any{"default": "deepseek/deepseek-v4.1-flash", "provider": ProviderName}, cfg["model"])
	require.Equal(t, map[string]any{ProviderName: map[string]any{
		"name": ProviderName, "base_url": "https://openrouter.ai/api/v1",
		"api_key": CredentialPlaceholder, "api_mode": "chat_completions",
	}}, cfg["providers"])
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

func TestRenderConfigOmitsCompressionByDefault(t *testing.T) {
	raw, err := RenderConfig(Settings{Model: "m", BaseURL: "https://openrouter.ai/api/v1"})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.NotContains(t, cfg, "compression")
	require.Equal(t, map[string]any{"title_generation": map[string]any{"enabled": false}}, cfg["auxiliary"])
}

func TestRenderConfigRoutesCompressionThroughNamedProvider(t *testing.T) {
	raw, err := RenderConfig(Settings{
		Model: "google/gemini-4-neon", BaseURL: "https://openrouter.ai/api/v1",
		CompressionThresholdTokens: 128000, CompressionMinTailUserMessages: 3, CompressionModel: "deepseek/deepseek-v4.1-flash-1017",
	})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{"threshold_tokens": 128000, "min_tail_user_messages": 3}, cfg["compression"])
	require.Equal(t, map[string]any{
		"title_generation": map[string]any{"enabled": false},
		"compression":      map[string]any{"provider": ProviderName, "model": "deepseek/deepseek-v4.1-flash-1017"},
	}, cfg["auxiliary"])
	// One providers entry: the summary uses the main model's endpoint and key.
	require.Len(t, cfg["providers"], 1)

	raw, err = RenderConfig(Settings{Model: "m", BaseURL: "https://openrouter.ai/api/v1", CompressionThresholdTokens: 128000})
	require.NoError(t, err)
	cfg = nil
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{"threshold_tokens": 128000}, cfg["compression"])
	require.NotContains(t, cfg["auxiliary"], "compression")

	_, err = RenderConfig(Settings{Model: "m", BaseURL: "https://openrouter.ai/api/v1", CompressionThresholdTokens: -1})
	require.ErrorContains(t, err, "nonnegative")
}

func TestRenderConfigSpeaksAnthropicMessagesWithItsCacheTTL(t *testing.T) {
	raw, err := RenderConfig(Settings{
		Model: "claude-opus-5-5", BaseURL: "https://api.anthropic.com", APIMode: "anthropic_messages", CacheTTL: "1h",
		CompressionModel: "claude-sonnet-5-5",
	})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{ProviderName: map[string]any{
		"name": ProviderName, "base_url": "https://api.anthropic.com",
		"api_key": CredentialPlaceholder, "api_mode": "anthropic_messages",
	}}, cfg["providers"])
	require.Equal(t, map[string]any{"cache_ttl": "1h"}, cfg["prompt_caching"])
	require.Equal(t, map[string]any{"provider": ProviderName, "model": "claude-sonnet-5-5"}, cfg["auxiliary"].(map[string]any)["compression"])

	raw, err = RenderConfig(Settings{Model: "m", BaseURL: "https://openrouter.ai/api/v1"})
	require.NoError(t, err)
	cfg = nil
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.NotContains(t, cfg, "prompt_caching")
}

func TestRenderConfigEnablesOnlyListedMCPTools(t *testing.T) {
	raw, err := RenderConfig(Settings{Model: "m", BaseURL: "https://api.anthropic.com", MCPServers: map[string]MCPServer{
		"scotland": {URL: "https://scotland.example.com/mcp", Tools: []string{"plan", "check", "plan"}},
		"recall":   {URL: "http://hindsight.test.svc:8888/mcp/", Headers: map[string]string{"Authorization": CredentialPlaceholder}, Tools: []string{"recall"}},
	}})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, []any{}, cfg["toolsets"])
	require.Equal(t, map[string]any{"api_server": []any{"recall", "scotland"}}, cfg["platform_toolsets"])
	require.Len(t, cfg["agent"].(map[string]any)["disabled_toolsets"], len(DisabledToolsets))
	servers := cfg["mcp_servers"].(map[string]any)
	require.Equal(t, map[string]any{
		"url": "https://scotland.example.com/mcp", "enabled": true, "trust": "full",
		"timeout": 30, "connect_timeout": 30, "strict_redirect_headers": true,
		"tools":    map[string]any{"include": []any{"check", "plan"}, "resources": false, "prompts": false},
		"sampling": map[string]any{"enabled": false}, "elicitation": map[string]any{"enabled": false},
	}, servers["scotland"])
	require.Equal(t, map[string]any{"Authorization": CredentialPlaceholder}, servers["recall"].(map[string]any)["headers"])
	require.Equal(t, map[string]any{"enabled": []any{}}, cfg["plugins"])
	require.Equal(t, map[string]any{"tool_search": map[string]any{"enabled": "off"}, "connectors": map[string]any{"enabled": false}}, cfg["tools"])

	_, err = RenderConfig(Settings{Model: "m", BaseURL: "https://api.anthropic.com", MCPServers: map[string]MCPServer{"memory": {URL: "https://x.example.com", Tools: []string{"t"}}}})
	require.ErrorContains(t, err, "built-in toolset")
	_, err = RenderConfig(Settings{Model: "m", BaseURL: "https://api.anthropic.com", MCPServers: map[string]MCPServer{"m": {URL: "https://x.example.com"}}})
	require.ErrorContains(t, err, "needs a URL and its tools")
}

func TestWriteHomeWritesAndRemovesServerToolsPlugin(t *testing.T) {
	home := t.TempDir()
	settings := Settings{Model: "claude-opus-5-5", BaseURL: "https://api.anthropic.com", ServerTools: &ServerTools{WebSearchMaxUses: 5, WebFetchMaxUses: 3, CodeExecution: true}}
	require.NoError(t, WriteHome(home, settings))
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{"enabled": []any{ServerToolsPlugin}}, cfg["plugins"])
	dir := filepath.Join(home, "plugins", ServerToolsPlugin)
	tools, err := os.ReadFile(filepath.Join(dir, "server_tools.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{"tools":[
		{"type":"web_search_20250305","name":"web_search","max_uses":5},
		{"type":"web_fetch_20250910","name":"web_fetch","max_uses":3},
		{"type":"code_execution_20260521","name":"code_execution"}]}`, string(tools))
	for _, name := range []string{"__init__.py", "plugin.yaml"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, name)
	}

	settings.ServerTools = nil
	require.NoError(t, WriteHome(home, settings))
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err), "the plugin must be removed when no server tool is selected")

	_, err = RenderConfig(Settings{Model: "m", BaseURL: "https://api.anthropic.com", ServerTools: &ServerTools{}})
	require.ErrorContains(t, err, "select no tool")

	// Without code execution, the basic web tools, which run no code.
	raw, err = serverToolsJSON(ServerTools{WebSearchMaxUses: 2, WebFetchMaxUses: 1})
	require.NoError(t, err)
	require.JSONEq(t, `{"tools":[
		{"type":"web_search_20250305","name":"web_search","max_uses":2},
		{"type":"web_fetch_20250910","name":"web_fetch","max_uses":1}]}`, string(raw))
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
		{name: "refused by the provider filter", code: 200, body: `{"status":"failed","output":[],"error":{"message":"content_policy_blocked: provider said no"}}`, wantStatus: "failed", wantPublic: "Hermes turn failed: content_policy_blocked"},
		{name: "unlisted reason", code: 200, body: `{"status":"failed","output":[],"error":{"message":"billing: provider said no"}}`, wantStatus: "failed", wantPublic: "Hermes turn failed"},
		{name: "reason must be the whole code", code: 200, body: `{"status":"failed","output":[],"error":{"message":"content_policy_blocked_x provider said no"}}`, wantStatus: "failed", wantPublic: "Hermes turn failed"},
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

func TestClientChainsToAPreviousResponse(t *testing.T) {
	for _, test := range []struct {
		name    string
		code    int
		wantErr error
	}{{name: "answered", code: 200}, {name: "evicted", code: 404, wantErr: errPreviousNotFound}} {
		t.Run(test.name, func(t *testing.T) {
			var seenBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&seenBody)
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(`{"id":"r2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}],"error":{"message":"Previous response not found"}}`))
			}))
			defer server.Close()
			c := &client{baseURL: server.URL, key: "k", http: server.Client(), maxBytes: 1024}
			_, err := c.respond(t.Context(), request{SessionKey: "kagent-s", Input: "hi", PreviousResponseID: "r1"})
			// Hermes refuses a request with both a conversation and a previous response.
			require.Equal(t, map[string]any{"input": "hi", "store": true, "previous_response_id": "r1"}, seenBody)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTurnErrorPublishesOnlyAllowlistedReasons(t *testing.T) {
	for detail, want := range map[string]string{
		"content_policy_blocked: Cyber safeguards flagged this message": ReasonContentPolicyBlocked,
		"content_policy_blocked":                       ReasonContentPolicyBlocked,
		"content_policy_blockedness: x":                "",
		"context_overflow: too long":                   "",
		"provider said content_policy_blocked: nested": "",
		"": "",
	} {
		turnErr := &TurnError{Status: "failed", Detail: detail}
		require.Equal(t, want, turnErr.Reason(), detail)
		require.NotContains(t, turnErr.Public(), "Cyber", detail)
	}
	// Only a failed turn carries a reason.
	require.Equal(t, "Hermes turn was cancelled", (&TurnError{Status: "cancelled", Detail: "content_policy_blocked: x"}).Public())
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
	// The first turn continues by conversation name; the next chains to the
	// answered response.
	require.Equal(t, sessionKey, requests[0].Conversation)
	require.Empty(t, requests[0].Previous)
	require.Empty(t, requests[1].Conversation)
	require.Equal(t, "resp_1", requests[1].Previous)
	for i, request := range requests {
		require.Equal(t, sessionKey, request.SessionKey)
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
		outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t-" + prompt, Prompt: prompt, ContinuationID: "kagent-fixed"}, sink)
		require.NoError(t, err, prompt)
		require.NotNil(t, outcome.Failure, prompt)
		require.Equal(t, want, outcome.Failure.Message)
		require.Nil(t, outcome.Pending)
		require.Empty(t, sink.text.String())
	}
}

// A turn the provider's filter refuses leaves the chain, so the next turn
// continues the transcript from before it and is answered.
func TestRunDropsARefusedTurnFromTheConversation(t *testing.T) {
	d, home := newFakeDriver(t, os.Environ())
	run := func(taskID, prompt string) (runtime.Outcome, string) {
		t.Helper()
		sink := &recordingSink{}
		outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: taskID, Prompt: prompt, ContinuationID: "kagent-fixed"}, sink)
		require.NoError(t, err, prompt)
		return outcome, sink.text.String()
	}
	_, text := run("t1", "hello")
	require.Equal(t, "echo: hello", text)

	outcome, text := run("t2", "a blocked joke")
	require.NotNil(t, outcome.Failure)
	require.Equal(t, "Hermes turn failed: content_policy_blocked", outcome.Failure.Message)
	require.NotContains(t, outcome.Failure.Message, "Cyber")
	require.Empty(t, text)

	outcome, text = run("t3", "history")
	require.Nil(t, outcome.Failure, "a later turn must not carry the refused message")
	require.Equal(t, "history: hello", text)
	_, text = run("t4", "history")
	require.Equal(t, "history: hello|history", text)

	// The refused turn is no longer stored.
	for id, transcript := range fakehermes.StoredResponses(home) {
		require.NotContains(t, strings.Join(transcript, "|"), "blocked", id)
	}
}

// A session from an adapter that chained by conversation name only: its
// first refused turn moves the name, and deleting that turn frees it.
func TestRunFreesAConversationNameAfterARefusal(t *testing.T) {
	d, home := newFakeDriver(t, os.Environ())
	require.NoError(t, fakehermes.SeedConversation(home, "kagent-legacy", []string{"older"}))
	sink := &recordingSink{}
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "t1", Prompt: "blocked", ContinuationID: "kagent-legacy"}, sink)
	require.NoError(t, err)
	sink = &recordingSink{}
	outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t2", Prompt: "history", ContinuationID: "kagent-legacy"}, sink)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Equal(t, "history: ", sink.text.String())
}

// A retry of the task that moved the chain sends that turn's request again,
// so Hermes replays its first answer instead of refusing a changed body.
func TestRunRetriesATaskWithItsFirstRequest(t *testing.T) {
	d, home := newFakeDriver(t, os.Environ())
	for _, taskID := range []string{"t1", "t2", "t2"} {
		sink := &recordingSink{}
		outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: taskID, Prompt: "hello " + taskID, ContinuationID: "kagent-fixed"}, sink)
		require.NoError(t, err)
		require.Nil(t, outcome.Failure)
		require.Equal(t, "echo: hello "+taskID, sink.text.String())
	}
	requests, err := fakehermes.Requests(home)
	require.NoError(t, err)
	require.Len(t, requests, 3)
	require.Equal(t, requests[1], requests[2])
	require.Equal(t, "resp_1", requests[2].Previous)
}

// When the response store no longer holds the head, the turn continues by
// conversation name under another idempotency key.
func TestRunContinuesByNameWhenTheHeadIsGone(t *testing.T) {
	d, home := newFakeDriver(t, os.Environ())
	require.NoError(t, saveChainHead(home, "kagent-fixed", chainHead{ResponseID: "resp_gone", TaskID: "t0"}))
	sink := &recordingSink{}
	outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t1", Prompt: "hello", ContinuationID: "kagent-fixed"}, sink)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Equal(t, "echo: hello", sink.text.String())
	requests, err := fakehermes.Requests(home)
	require.NoError(t, err)
	require.Len(t, requests, 2)
	require.Equal(t, "resp_gone", requests[0].Previous)
	require.Equal(t, "kagent-fixed", requests[1].Conversation)
	require.Equal(t, "kagent-task-t1-conversation", requests[1].IdempotencyKey)
	head, err := loadChainHead(home, "kagent-fixed")
	require.NoError(t, err)
	require.Equal(t, &chainHead{ResponseID: "resp_1", TaskID: "t1", IdempotencyKey: "kagent-task-t1-conversation"}, head)

	// A retry of that task resends the fallback request, so Hermes replays
	// it and does not run the turn twice.
	sink = &recordingSink{}
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "t1", Prompt: "hello", ContinuationID: "kagent-fixed"}, sink)
	require.NoError(t, err)
	require.Equal(t, "echo: hello", sink.text.String())
	requests, err = fakehermes.Requests(home)
	require.NoError(t, err)
	require.Len(t, requests, 3)
	require.Equal(t, requests[1], requests[2])
	require.Len(t, fakehermes.StoredResponses(home), 1, "the retry must not run the turn again")
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

// A snapshot restore gives /data new backing files while /health still
// answers ok. The next turn sees that config.yaml is not the file it wrote,
// rewrites HERMES_HOME, and starts a new gateway.
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

// A gateway whose /health answers while a response hangs is stopped at the
// client timeout and replaced on the next turn.
func TestRunReplacesAGatewayAfterATurnTimeout(t *testing.T) {
	d, _ := newFakeDriver(t, os.Environ())
	d.http.Timeout = time.Second
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "t1", Prompt: "slow", ContinuationID: "kagent-fixed"}, &recordingSink{})
	require.Error(t, err)
	d.mu.Lock()
	stopped := d.gateway == nil
	d.mu.Unlock()
	require.True(t, stopped, "a timed-out gateway must be stopped")
	sink := &recordingSink{}
	outcome, err := d.Run(t.Context(), runtime.Turn{TaskID: "t2", Prompt: "back", ContinuationID: "kagent-fixed"}, sink)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Equal(t, "echo: back", sink.text.String())
}

func TestValidateRequiresGatewayExecutable(t *testing.T) {
	d, err := New(Config{GatewayCommand: []string{"/nonexistent/hermes", "gateway"}, Settings: Settings{Model: "m", BaseURL: "https://example.com/v1"}})
	require.NoError(t, err)
	require.ErrorContains(t, d.Validate(t.Context()), "find Hermes gateway executable")
}

func TestClientRespondSendsImagesAsInputImageParts(t *testing.T) {
	var seenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a cat"}]}]}`))
	}))
	defer server.Close()
	c := &client{baseURL: server.URL, key: "k", http: server.Client(), maxBytes: 1024}
	images := []runtime.Image{{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}, {MediaType: "image/jpeg", Data: []byte{0xff, 0xd8}}}
	result, err := c.respond(t.Context(), request{SessionKey: "kagent-s", Input: "what is this?", Images: images})
	require.NoError(t, err)
	require.Equal(t, "a cat", result.Text)
	require.Equal(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "what is this?"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw=="},
		map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64,/9g="},
	}}}, seenBody["input"])
}
