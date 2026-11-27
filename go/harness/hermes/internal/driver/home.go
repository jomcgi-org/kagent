package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// CredentialPlaceholder is the model key Hermes sends when the Actor has no
// key. The kagent compiler gives the Actor only this inert value, and the
// egress gateway replaces the Authorization header with the bound credential.
const CredentialPlaceholder = "kagent-credential-injected"

// DisabledToolsets names every built-in Hermes v0.23.4 toolset. The empty
// api_server platform list already enables none; this list also strips them
// if a later config adds a platform list.
var DisabledToolsets = []string{
	"a2a", "browser", "catalog", "clarify", "code_execution", "computer_use",
	"connections", "cronjob", "delegation", "desktop_ui", "discord",
	"discord_admin", "feishu_doc", "feishu_drive", "file", "image_gen",
	"kanban", "memory", "project", "search", "session_search", "skills",
	"terminal", "todo", "tts", "video", "video_gen", "vision", "web", "x_search",
}

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
}

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

type hermesConfig struct {
	Model            modelConfig               `yaml:"model"`
	Providers        map[string]providerConfig `yaml:"providers"`
	Toolsets         []string                  `yaml:"toolsets"`
	PlatformToolsets map[string][]string       `yaml:"platform_toolsets"`
	Agent            agentConfig               `yaml:"agent"`
	Memory           memoryConfig              `yaml:"memory"`
	Approvals        approvalsConfig           `yaml:"approvals"`
	Security         securityConfig            `yaml:"security"`
	Auxiliary        auxiliaryConfig           `yaml:"auxiliary"`
	Compression      *compressionConfig        `yaml:"compression,omitempty"`
	PromptCaching    *promptCachingConfig      `yaml:"prompt_caching,omitempty"`
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
	return yaml.Marshal(hermesConfig{
		Model: modelConfig{Default: s.Model, Provider: ProviderName},
		Providers: map[string]providerConfig{ProviderName: {
			Name: ProviderName, BaseURL: s.BaseURL, APIKey: s.APIKey, APIMode: apiMode,
		}},
		// An explicit empty list enables no toolset and merges no MCP server
		// (hermes_cli/tools_config.py _get_platform_tools at v0.23.4).
		Toolsets:         []string{},
		PlatformToolsets: map[string][]string{"api_server": {}},
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
	})
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
	return nil
}
