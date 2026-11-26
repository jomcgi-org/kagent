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
}

type modelConfig struct {
	Default  string `yaml:"default"`
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
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

type auxiliaryConfig struct {
	TitleGeneration titleConfig `yaml:"title_generation"`
}

type hermesConfig struct {
	Model            modelConfig         `yaml:"model"`
	Toolsets         []string            `yaml:"toolsets"`
	PlatformToolsets map[string][]string `yaml:"platform_toolsets"`
	Agent            agentConfig         `yaml:"agent"`
	Memory           memoryConfig        `yaml:"memory"`
	Approvals        approvalsConfig     `yaml:"approvals"`
	Security         securityConfig      `yaml:"security"`
	Auxiliary        auxiliaryConfig     `yaml:"auxiliary"`
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
	return yaml.Marshal(hermesConfig{
		Model: modelConfig{Default: s.Model, Provider: "custom", BaseURL: s.BaseURL, APIKey: s.APIKey},
		// An explicit empty list enables no toolset and merges no MCP server
		// (hermes_cli/tools_config.py _get_platform_tools at v0.23.4).
		Toolsets:         []string{},
		PlatformToolsets: map[string][]string{"api_server": {}},
		Agent:            agentConfig{DisabledToolsets: DisabledToolsets},
		Memory:           memoryConfig{},
		// api_server sessions are unattended: unattended_mode deny refuses a
		// flagged action at once instead of parking the turn, so a turn never
		// waits on a person.
		Approvals: approvalsConfig{Mode: "manual", Timeout: 10, CronMode: "deny", SingleQueryMode: "deny", UnattendedMode: "deny"},
		Security:  securityConfig{AllowLazyInstalls: false},
		Auxiliary: auxiliaryConfig{TitleGeneration: titleConfig{Enabled: false}},
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
