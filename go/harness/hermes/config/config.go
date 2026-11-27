// Package config defines the versioned, non-secret Hermes Harness runtime
// configuration shared by its compiler and Actor entrypoint.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const (
	Version = 1
	// PinnedHermesVersion is the Hermes Agent release the adapter was written
	// against. The adapter reads only the /v1/responses and /health routes.
	PinnedHermesVersion = "0.23.4"
	// DefaultBaseURL is OpenAI's API, the ModelConfig OpenAI provider default.
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultAnthropicBaseURL is the Anthropic provider default. Hermes
	// adds /v1/messages itself.
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	// APIKeyEnvName carries the model credential placeholder. The compiler
	// binds the real key to the egress gateway, so the Actor receives only
	// translator.CredentialPlaceholder or nothing.
	APIKeyEnvName = "OPENAI_API_KEY"
	// AnthropicAPIKeyEnvName carries the placeholder for an Anthropic
	// ModelConfig, which kagent binds to the gateway's x-api-key header.
	AnthropicAPIKeyEnvName = "ANTHROPIC_API_KEY"
	// APIModeChatCompletions and APIModeAnthropicMessages are the Hermes
	// provider api_mode values the adapter writes.
	APIModeChatCompletions   = "chat_completions"
	APIModeAnthropicMessages = "anthropic_messages"
	// HermesHomeEnvName, and the API_SERVER_* names, belong to the adapter.
	HermesHomeEnvName = "HERMES_HOME"
)

// DefaultGatewayCommand starts the gateway from the Hermes Agent image.
var DefaultGatewayCommand = []string{"/opt/hermes/.venv/bin/hermes", "gateway"}

// OwnsEnvironment reports whether the compiler or adapter reserves name for
// Hermes runtime configuration. Harness authors cannot override these values.
func OwnsEnvironment(name string) bool {
	if strings.HasPrefix(name, "API_SERVER_") || strings.HasPrefix(name, "HERMES_") {
		return true
	}
	return name == APIKeyEnvName || name == AnthropicAPIKeyEnvName
}

// Config is the compiler-owned Hermes runtime configuration.
type Config struct {
	MaxContentLength *int64 `json:"max_content_length,omitempty"`
	ControlPlaneURL  string `json:"control_plane_url"`
	LogLevel         string `json:"log_level,omitempty"`
	Version          int    `json:"version"`
	// GatewayCommand starts `hermes gateway`. The adapter enables only the API
	// server, on loopback.
	GatewayCommand []string `json:"gateway_command"`
	// Model is the provider model ID, such as deepseek/deepseek-v4.1-flash.
	Model string `json:"model"`
	// BaseURL is the endpoint Hermes calls as its named provider.
	BaseURL string `json:"base_url"`
	// APIMode is the provider's wire protocol: empty or chat_completions for
	// an OpenAI-compatible endpoint, anthropic_messages for Anthropic.
	APIMode string `json:"api_mode,omitempty"`
	// CacheTTL is Hermes prompt_caching.cache_ttl: 5m, 1h or off. Empty
	// keeps Hermes's default.
	CacheTTL string `json:"cache_ttl,omitempty"`
	// Instructions is the compiled system prompt, sent with every turn.
	Instructions string `json:"instructions,omitempty"`
	// CompressionThresholdTokens is Hermes's compression.threshold_tokens.
	// Zero keeps Hermes's ratio-only trigger.
	CompressionThresholdTokens int `json:"compression_threshold_tokens,omitempty"`
	// CompressionMinTailUserMessages is compression.min_tail_user_messages.
	// Zero keeps Hermes's default.
	CompressionMinTailUserMessages int `json:"compression_min_tail_user_messages,omitempty"`
	// CompressionModel is the summary model, called through the same endpoint
	// as Model. Empty means Model.
	CompressionModel string `json:"compression_model,omitempty"`
	// MCPServers are the Streamable HTTP MCP servers Hermes connects to, by
	// Hermes server name. Each lists the only tools Hermes registers.
	MCPServers map[string]MCPServer `json:"mcp_servers,omitempty"`
	// ServerTools are the Anthropic server tools added to each Messages API
	// request of a turn. Nil adds none.
	ServerTools *ServerTools `json:"server_tools,omitempty"`
	// RuntimeTelemetry carries the compiler-owned span identity and content
	// capture policy. It is absent for standalone runs.
	RuntimeTelemetry     tracing.RuntimeTelemetry `json:"runtime_telemetry,omitzero"`
	APIServerPort        int                      `json:"api_server_port"`
	StartupTimeoutMillis int                      `json:"startup_timeout_millis"`
	TurnTimeoutMillis    int                      `json:"turn_timeout_millis"`
	StopGraceMillis      int                      `json:"stop_grace_millis"`
	MaxResponseBytes     int                      `json:"max_response_bytes"`
}

// MCPServer is one remote MCP server. Header values are literals or the
// credential placeholder that the egress gateway replaces.
type MCPServer struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Tools is the explicit allowlist, rendered as tools.include.
	Tools []string `json:"tools"`
}

// ServerTools selects Anthropic server tools. A zero MaxUses leaves a web
// tool out.
type ServerTools struct {
	WebSearchMaxUses int  `json:"web_search_max_uses,omitempty"`
	WebFetchMaxUses  int  `json:"web_fetch_max_uses,omitempty"`
	CodeExecution    bool `json:"code_execution,omitempty"`
}

// mcpServerName matches the names Hermes prefixes onto tool names. Kagent's
// runtime names use only these characters.
var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// BuiltinToolsets names every built-in Hermes v0.23.4 toolset. The runner
// disables all of them.
var BuiltinToolsets = []string{
	"a2a", "browser", "catalog", "clarify", "code_execution", "computer_use",
	"connections", "cronjob", "delegation", "desktop_ui", "discord",
	"discord_admin", "feishu_doc", "feishu_drive", "file", "image_gen",
	"kanban", "memory", "project", "search", "session_search", "skills",
	"terminal", "todo", "tts", "video", "video_gen", "vision", "web", "x_search",
}

// toolsetSelectors are the other names Hermes v0.23.4 resolves in
// platform_toolsets before an MCP server (toolsets.py TOOLSETS keys and
// selectors). A server so named would merge into that toolset.
var toolsetSelectors = []string{
	"all", "bot_room", "coding", "context_engine", "debugging", "no_mcp", "onboarding", "safe",
}

// ReservedMCPServerName reports whether Hermes would read name in
// platform_toolsets as a built-in or composite toolset, not an MCP server.
func ReservedMCPServerName(name string) bool {
	// Hermes names each server's toolset mcp-<name>, and resolves a listed
	// mcp-<name> to server <name>.
	return slices.Contains(BuiltinToolsets, name) || slices.Contains(toolsetSelectors, name) ||
		strings.HasPrefix(name, "hermes") || strings.HasPrefix(name, "mcp-")
}

// Production returns the configuration the compiler emits for model.
func Production(model, baseURL, instructions string) Config {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return Config{
		Version: Version, GatewayCommand: append([]string(nil), DefaultGatewayCommand...),
		Model: model, BaseURL: baseURL, Instructions: instructions,
		APIServerPort: 8642, StartupTimeoutMillis: 120_000, TurnTimeoutMillis: 600_000,
		StopGraceMillis: 20_000, MaxResponseBytes: 8 << 20,
	}
}

// Parse decodes and validates a strict configuration document.
func Parse(b []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Config{}, fmt.Errorf("decode config: trailing JSON value")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects configurations the compiler could not have produced.
func (c Config) Validate() error {
	if c.MaxContentLength != nil && *c.MaxContentLength < 0 {
		return fmt.Errorf("max_content_length must be nonnegative")
	}
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn, or error")
	}
	if c.Version != Version {
		return fmt.Errorf("unsupported config version %d (want %d)", c.Version, Version)
	}
	if len(c.GatewayCommand) == 0 || strings.TrimSpace(c.GatewayCommand[0]) == "" {
		return fmt.Errorf("gateway_command is required")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("model is required")
	}
	switch c.APIMode {
	case "", APIModeChatCompletions, APIModeAnthropicMessages:
	default:
		return fmt.Errorf("api_mode must be chat_completions or anthropic_messages")
	}
	switch c.CacheTTL {
	case "", "5m", "1h", "off":
	default:
		return fmt.Errorf("cache_ttl must be 5m, 1h or off")
	}
	if c.CompressionThresholdTokens < 0 {
		return fmt.Errorf("compression_threshold_tokens must be nonnegative")
	}
	if c.CompressionMinTailUserMessages < 0 {
		return fmt.Errorf("compression_min_tail_user_messages must be nonnegative")
	}
	if c.CompressionModel != strings.TrimSpace(c.CompressionModel) || strings.ContainsAny(c.CompressionModel, " \t\r\n") {
		return fmt.Errorf("compression_model must not contain whitespace")
	}
	if err := c.validateTools(); err != nil {
		return err
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("base_url must be an absolute HTTP(S) URL without credentials or fragment")
	}
	if c.APIServerPort <= 0 || c.APIServerPort > 65535 {
		return fmt.Errorf("api_server_port must be a TCP port")
	}
	if c.StartupTimeoutMillis <= 0 || c.TurnTimeoutMillis <= 0 || c.StopGraceMillis <= 0 || c.MaxResponseBytes <= 0 {
		return fmt.Errorf("startup, turn, stop grace, and response limits must be positive")
	}
	if err := c.RuntimeTelemetry.Validate(); err != nil {
		return err
	}
	if runtime := c.RuntimeTelemetry.Runtime; runtime != "" && runtime != tracing.RuntimeHermes {
		return fmt.Errorf("hermes runtime telemetry names runtime %q", runtime)
	}
	return nil
}

// APIKeyEnv is the environment variable that holds the model credential
// placeholder for c's provider.
func (c Config) APIKeyEnv() string {
	if c.APIMode == APIModeAnthropicMessages {
		return AnthropicAPIKeyEnvName
	}
	return APIKeyEnvName
}

func (c Config) validateTools() error {
	for name, server := range c.MCPServers {
		if !mcpServerName.MatchString(name) {
			return fmt.Errorf("mcp_servers name %q must match %s", name, mcpServerName)
		}
		if ReservedMCPServerName(name) {
			return fmt.Errorf("mcp_servers name %q is a Hermes toolset name", name)
		}
		// Hermes expands ${VAR} in every string of a server entry.
		if strings.Contains(server.URL, "${") {
			return fmt.Errorf("mcp_servers %q url must not reference variables", name)
		}
		u, err := url.Parse(server.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("mcp_servers %q url must be an absolute HTTP(S) URL without credentials or fragment", name)
		}
		if len(server.Tools) == 0 {
			return fmt.Errorf("mcp_servers %q must list its tools", name)
		}
		for _, tool := range server.Tools {
			if strings.TrimSpace(tool) == "" || strings.ContainsAny(tool, "*?[") || strings.Contains(tool, "${") {
				return fmt.Errorf("mcp_servers %q tool %q must be an exact name", name, tool)
			}
		}
		for header := range server.Headers {
			if strings.TrimSpace(header) == "" || strings.Contains(header, "${") {
				return fmt.Errorf("mcp_servers %q has an invalid header name", name)
			}
		}
		for _, value := range server.Headers {
			if strings.Contains(value, "${") {
				return fmt.Errorf("mcp_servers %q header values must not reference variables", name)
			}
		}
	}
	if t := c.ServerTools; t != nil {
		if t.WebSearchMaxUses < 0 || t.WebSearchMaxUses > 20 || t.WebFetchMaxUses < 0 || t.WebFetchMaxUses > 20 {
			return fmt.Errorf("server_tools max uses must be between 0 and 20")
		}
		if t.WebSearchMaxUses == 0 && t.WebFetchMaxUses == 0 && !t.CodeExecution {
			return fmt.Errorf("server_tools must select a tool")
		}
	}
	return nil
}

func (c Config) StartupTimeout() time.Duration {
	return time.Duration(c.StartupTimeoutMillis) * time.Millisecond
}

func (c Config) TurnTimeout() time.Duration {
	return time.Duration(c.TurnTimeoutMillis) * time.Millisecond
}

func (c Config) StopGrace() time.Duration {
	return time.Duration(c.StopGraceMillis) * time.Millisecond
}
