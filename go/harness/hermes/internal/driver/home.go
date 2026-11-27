package driver

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// CredentialPlaceholder is the model key Hermes sends when the Actor has no
// key. The kagent compiler gives the Actor only this inert value, and the
// egress gateway replaces the Authorization header with the bound credential.
const CredentialPlaceholder = "kagent-credential-injected"

// DisabledToolsets names every built-in Hermes v0.23.4 toolset. The empty
// api_server platform list already enables none; this list also strips them
// if a later config adds a platform list.
var DisabledToolsets = config.BuiltinToolsets

// Settings are the values the adapter renders into Hermes' configuration.
type Settings struct {
	Model   string
	BaseURL string
	// APIKey is the key Hermes sends. Empty means CredentialPlaceholder.
	APIKey string
	// APIMode is the provider's api_mode. Empty means chat_completions.
	APIMode string
	// CacheTTL is prompt_caching.cache_ttl. Empty keeps Hermes's default.
	CacheTTL string
	// CompressionThresholdTokens is compression.threshold_tokens. Zero keeps
	// Hermes's ratio-only trigger.
	CompressionThresholdTokens int
	// CompressionMinTailUserMessages is compression.min_tail_user_messages.
	// Zero keeps Hermes's default.
	CompressionMinTailUserMessages int
	// CompressionModel is the summary model. Empty means Model.
	CompressionModel string
	// MCPServers are the remote MCP servers, by Hermes server name. Hermes
	// registers only each server's listed tools.
	MCPServers map[string]MCPServer
	// ServerTools are the Anthropic server tools. Nil adds none.
	ServerTools *ServerTools
}

// MCPServer is one Streamable HTTP MCP server.
type MCPServer struct {
	URL string
	// Headers hold literals or the credential placeholder.
	Headers map[string]string
	Tools   []string
}

// ServerTools selects Anthropic server tools. A zero MaxUses leaves a web
// tool out.
type ServerTools struct {
	WebSearchMaxUses int
	WebFetchMaxUses  int
	CodeExecution    bool
}

// ServerToolsPlugin names the Hermes plugin that adds the server tools to
// each Messages API request (plugin/__init__.py).
const ServerToolsPlugin = "kagent-server-tools"

//go:embed plugin/__init__.py plugin/plugin.yaml
var serverToolsPlugin embed.FS

// ProviderName names the providers: entry that holds the compiled endpoint.
const ProviderName = "kagent"

type modelConfig struct {
	Default  string `yaml:"default"`
	Provider string `yaml:"provider"`
}

// providerConfig is one named endpoint: OpenAI-compatible chat completions,
// or Anthropic Messages, where Hermes sends a non-OAuth key as x-api-key
// (agent/anthropic_adapter.py) and marks cache_control on api.anthropic.com
// (agent/agent_runtime_helpers.py anthropic_prompt_cache_policy). Hermes 0.23.4
// takes a named entry's own api_key before any environment key, for every
// host. A bare `provider: custom` with model.base_url resolves only for a
// loopback host: for a remote host, such as openrouter.ai, it ignores
// model.api_key and fails with "resolved without credentials".
type providerConfig struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	APIMode string `yaml:"api_mode"`
}

type agentConfig struct {
	DisabledToolsets []string `yaml:"disabled_toolsets"`
}

type memoryConfig struct {
	MemoryEnabled      bool   `yaml:"memory_enabled"`
	UserProfileEnabled bool   `yaml:"user_profile_enabled"`
	Provider           string `yaml:"provider"`
}

type approvalsConfig struct {
	Mode            string `yaml:"mode"`
	Timeout         int    `yaml:"timeout"`
	CronMode        string `yaml:"cron_mode"`
	SingleQueryMode string `yaml:"single_query_mode"`
	UnattendedMode  string `yaml:"unattended_mode"`
}

type securityConfig struct {
	AllowLazyInstalls bool `yaml:"allow_lazy_installs"`
}

type titleConfig struct {
	Enabled bool `yaml:"enabled"`
}

// auxiliaryTaskConfig routes one auxiliary task. Hermes 0.23.4 resolves a
// provider that names a providers: entry to that entry's base_url and
// api_key (agent/auxiliary_client.py _resolve_named_custom_branch), so the
// summary goes to the same endpoint and egress credential as the main model.
type auxiliaryTaskConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type auxiliaryConfig struct {
	TitleGeneration titleConfig          `yaml:"title_generation"`
	Compression     *auxiliaryTaskConfig `yaml:"compression,omitempty"`
}

// compressionConfig is the compression: block. Hermes compresses at the
// lower of its ratio threshold and threshold_tokens, and keeps the newest
// min_tail_user_messages user messages verbatim.
type compressionConfig struct {
	ThresholdTokens     int `yaml:"threshold_tokens,omitempty"`
	MinTailUserMessages int `yaml:"min_tail_user_messages,omitempty"`
}

// promptCachingConfig is the prompt_caching: block. Hermes 0.23.4 defaults
// api_server sessions to a 5m TTL (hermes_cli/config_defaults.py).
type promptCachingConfig struct {
	CacheTTL string `yaml:"cache_ttl"`
}

// mcpServerConfig is one mcp_servers entry. Sampling and elicitation are
// off: a server must not start model calls or ask a person for input.
type mcpServerConfig struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Enabled bool              `yaml:"enabled"`
	// Timeout and ConnectTimeout are kagent's RemoteMCPServer default.
	Timeout        int `yaml:"timeout"`
	ConnectTimeout int `yaml:"connect_timeout"`
	// StrictRedirectHeaders keeps configured headers on the server's origin
	// and turns off the SSE fallback.
	StrictRedirectHeaders bool           `yaml:"strict_redirect_headers"`
	Trust                 string         `yaml:"trust"`
	Tools                 mcpToolsConfig `yaml:"tools"`
	Sampling              switchConfig   `yaml:"sampling"`
	Elicitation           switchConfig   `yaml:"elicitation"`
}

// mcpToolsConfig registers only Include, and none of Hermes' resource and
// prompt utility tools.
type mcpToolsConfig struct {
	Include   []string `yaml:"include"`
	Resources bool     `yaml:"resources"`
	Prompts   bool     `yaml:"prompts"`
}

type switchConfig struct {
	Enabled bool `yaml:"enabled"`
}

// toolsConfig turns off Tool Search, so each MCP tool is in the request
// itself rather than behind tool_search and tool_call, and Nous's remote
// connectors.
type toolsConfig struct {
	ToolSearch toolSearchConfig `yaml:"tool_search"`
	Connectors switchConfig     `yaml:"connectors"`
}

type toolSearchConfig struct {
	Enabled string `yaml:"enabled"`
}

type pluginsConfig struct {
	Enabled []string `yaml:"enabled"`
}

type hermesConfig struct {
	Model            modelConfig                `yaml:"model"`
	Providers        map[string]providerConfig  `yaml:"providers"`
	Toolsets         []string                   `yaml:"toolsets"`
	PlatformToolsets map[string][]string        `yaml:"platform_toolsets"`
	Agent            agentConfig                `yaml:"agent"`
	Memory           memoryConfig               `yaml:"memory"`
	Approvals        approvalsConfig            `yaml:"approvals"`
	Security         securityConfig             `yaml:"security"`
	Auxiliary        auxiliaryConfig            `yaml:"auxiliary"`
	Compression      *compressionConfig         `yaml:"compression,omitempty"`
	PromptCaching    *promptCachingConfig       `yaml:"prompt_caching,omitempty"`
	MCPServers       map[string]mcpServerConfig `yaml:"mcp_servers,omitempty"`
	Plugins          pluginsConfig              `yaml:"plugins"`
	Tools            toolsConfig                `yaml:"tools"`
}

// hindsightConfig is $HERMES_HOME/hindsight/config.json. It stays inert
// until memory.provider names hindsight.
type hindsightConfig struct {
	MemoryMode string `json:"memory_mode"`
}

// RenderConfig returns config.yaml for Hermes.
func RenderConfig(s Settings) ([]byte, error) {
	if strings.TrimSpace(s.Model) == "" || strings.TrimSpace(s.BaseURL) == "" {
		return nil, fmt.Errorf("hermes model and base URL are required")
	}
	if s.APIKey == "" {
		s.APIKey = CredentialPlaceholder
	}
	if s.CompressionThresholdTokens < 0 || s.CompressionMinTailUserMessages < 0 {
		return nil, fmt.Errorf("hermes compression settings must be nonnegative")
	}
	auxiliary := auxiliaryConfig{TitleGeneration: titleConfig{Enabled: false}}
	if model := strings.TrimSpace(s.CompressionModel); model != "" {
		auxiliary.Compression = &auxiliaryTaskConfig{Provider: ProviderName, Model: model}
	}
	apiMode := s.APIMode
	if apiMode == "" {
		apiMode = "chat_completions"
	}
	var caching *promptCachingConfig
	if s.CacheTTL != "" {
		caching = &promptCachingConfig{CacheTTL: s.CacheTTL}
	}
	var compression *compressionConfig
	if s.CompressionThresholdTokens > 0 || s.CompressionMinTailUserMessages > 0 {
		compression = &compressionConfig{ThresholdTokens: s.CompressionThresholdTokens, MinTailUserMessages: s.CompressionMinTailUserMessages}
	}
	mcpServers, serverNames, err := renderMCPServers(s.MCPServers)
	if err != nil {
		return nil, err
	}
	plugins := pluginsConfig{Enabled: []string{}}
	if s.ServerTools != nil {
		if _, err := serverToolsJSON(*s.ServerTools); err != nil {
			return nil, err
		}
		plugins.Enabled = []string{ServerToolsPlugin}
	}
	return yaml.Marshal(hermesConfig{
		Model: modelConfig{Default: s.Model, Provider: ProviderName},
		Providers: map[string]providerConfig{ProviderName: {
			Name: ProviderName, BaseURL: s.BaseURL, APIKey: s.APIKey, APIMode: apiMode,
		}},
		// An explicit list that names only MCP servers enables no built-in
		// toolset and only those servers; an empty one enables nothing
		// (hermes_cli/tools_config.py _get_platform_tools at v0.23.4).
		Toolsets:         []string{},
		PlatformToolsets: map[string][]string{"api_server": serverNames},
		Agent:            agentConfig{DisabledToolsets: DisabledToolsets},
		Memory:           memoryConfig{},
		// api_server sessions are unattended: unattended_mode deny refuses a
		// flagged action at once instead of parking the turn, so a turn never
		// waits on a person.
		Approvals:     approvalsConfig{Mode: "manual", Timeout: 10, CronMode: "deny", SingleQueryMode: "deny", UnattendedMode: "deny"},
		Security:      securityConfig{AllowLazyInstalls: false},
		Auxiliary:     auxiliary,
		Compression:   compression,
		PromptCaching: caching,
		MCPServers:    mcpServers,
		Plugins:       plugins,
		Tools:         toolsConfig{ToolSearch: toolSearchConfig{Enabled: "off"}},
	})
}

func renderMCPServers(servers map[string]MCPServer) (map[string]mcpServerConfig, []string, error) {
	names := make([]string, 0, len(servers))
	if len(servers) == 0 {
		return nil, names, nil
	}
	rendered := make(map[string]mcpServerConfig, len(servers))
	for name, server := range servers {
		// platform_toolsets resolves a built-in or composite toolset name
		// before an MCP server of the same name.
		if config.ReservedMCPServerName(name) {
			return nil, nil, fmt.Errorf("hermes MCP server name %q is a built-in toolset", name)
		}
		if strings.TrimSpace(server.URL) == "" || len(server.Tools) == 0 {
			return nil, nil, fmt.Errorf("hermes MCP server %q needs a URL and its tools", name)
		}
		tools := slices.Clone(server.Tools)
		slices.Sort(tools)
		rendered[name] = mcpServerConfig{
			URL: server.URL, Headers: server.Headers, Enabled: true, Trust: "full",
			Timeout: 30, ConnectTimeout: 30, StrictRedirectHeaders: true,
			Tools: mcpToolsConfig{Include: slices.Compact(tools)},
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return rendered, names, nil
}

type serverToolsFile struct {
	Tools []map[string]any `json:"tools"`
}

// serverToolsJSON returns the plugin's server_tools.json: the Anthropic tool
// entries in a fixed order, so each request sends identical tools.
func serverToolsJSON(t ServerTools) ([]byte, error) {
	if t.WebSearchMaxUses < 0 || t.WebSearchMaxUses > 20 || t.WebFetchMaxUses < 0 || t.WebFetchMaxUses > 20 {
		return nil, fmt.Errorf("hermes server tool max uses must be between 0 and 20")
	}
	// The basic web tools call directly. The 20260209 versions call through
	// code execution, which disable_parallel_tool_use (plugin/__init__.py)
	// does not support.
	search, fetch := "web_search_20250305", "web_fetch_20250910"
	var tools []map[string]any
	if t.WebSearchMaxUses > 0 {
		tools = append(tools, map[string]any{"type": search, "name": "web_search", "max_uses": t.WebSearchMaxUses})
	}
	if t.WebFetchMaxUses > 0 {
		tools = append(tools, map[string]any{"type": fetch, "name": "web_fetch", "max_uses": t.WebFetchMaxUses})
	}
	if t.CodeExecution {
		tools = append(tools, map[string]any{"type": "code_execution_20260521", "name": "code_execution"})
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("hermes server tools select no tool")
	}
	return json.MarshalIndent(serverToolsFile{Tools: tools}, "", "  ")
}

// WriteHome writes the rendered configuration into home. It replaces files
// from an earlier start, so the compiled configuration owns them. It also
// removes $HERMES_HOME/.env: Hermes loads it over the process environment,
// so a durable file could re-enable what the environment allowlist drops.
func WriteHome(home string, s Settings) error {
	cfg, err := RenderConfig(s)
	if err != nil {
		return err
	}
	hindsight, err := json.MarshalIndent(hindsightConfig{MemoryMode: "context"}, "", "  ")
	if err != nil {
		return err
	}
	if err := utils.EnsurePrivateDir(filepath.Join(home, "hindsight")); err != nil {
		return fmt.Errorf("prepare Hermes home: %w", err)
	}
	if err := os.Remove(filepath.Join(home, ".env")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove Hermes .env: %w", err)
	}
	if err := utils.ReplacePrivateFile(filepath.Join(home, "config.yaml"), cfg); err != nil {
		return fmt.Errorf("write Hermes config: %w", err)
	}
	if err := utils.ReplacePrivateFile(filepath.Join(home, "hindsight", "config.json"), hindsight); err != nil {
		return fmt.Errorf("write Hermes hindsight config: %w", err)
	}
	return writeServerToolsPlugin(home, s.ServerTools)
}

// writeServerToolsPlugin replaces the plugin directory, or removes it when no
// server tool is selected, so a plugin from an earlier start never loads.
func writeServerToolsPlugin(home string, tools *ServerTools) error {
	dir := filepath.Join(home, "plugins", ServerToolsPlugin)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove Hermes server tools plugin: %w", err)
	}
	if tools == nil {
		return nil
	}
	config, err := serverToolsJSON(*tools)
	if err != nil {
		return err
	}
	if err := utils.EnsurePrivateDir(dir); err != nil {
		return fmt.Errorf("prepare Hermes server tools plugin: %w", err)
	}
	files := map[string][]byte{"server_tools.json": config}
	for _, name := range []string{"__init__.py", "plugin.yaml"} {
		content, err := serverToolsPlugin.ReadFile("plugin/" + name)
		if err != nil {
			return err
		}
		files[name] = content
	}
	for name, content := range files {
		if err := utils.ReplacePrivateFile(filepath.Join(dir, name), content); err != nil {
			return fmt.Errorf("write Hermes server tools plugin: %w", err)
		}
	}
	return nil
}
