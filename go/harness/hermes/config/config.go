// Package config defines the versioned, non-secret Hermes Harness runtime
// configuration shared by its compiler and Actor entrypoint.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
	// APIKeyEnvName carries the model credential placeholder. The compiler
	// binds the real key to the egress gateway, so the Actor receives only
	// translator.CredentialPlaceholder or nothing.
	APIKeyEnvName = "OPENAI_API_KEY"
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
	return name == APIKeyEnvName
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
	// BaseURL is the OpenAI-compatible endpoint Hermes calls as a custom provider.
	BaseURL string `json:"base_url"`
	// Instructions is the compiled system prompt, sent with every turn.
	Instructions string `json:"instructions,omitempty"`
	// RuntimeTelemetry carries the compiler-owned span identity and content
	// capture policy. It is absent for standalone runs.
	RuntimeTelemetry     tracing.RuntimeTelemetry `json:"runtime_telemetry,omitzero"`
	APIServerPort        int                      `json:"api_server_port"`
	StartupTimeoutMillis int                      `json:"startup_timeout_millis"`
	TurnTimeoutMillis    int                      `json:"turn_timeout_millis"`
	StopGraceMillis      int                      `json:"stop_grace_millis"`
	MaxResponseBytes     int                      `json:"max_response_bytes"`
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

func (c Config) StartupTimeout() time.Duration {
	return time.Duration(c.StartupTimeoutMillis) * time.Millisecond
}

func (c Config) TurnTimeout() time.Duration {
	return time.Duration(c.TurnTimeoutMillis) * time.Millisecond
}

func (c Config) StopGrace() time.Duration {
	return time.Duration(c.StopGraceMillis) * time.Millisecond
}
