package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func TestProductionRoundTrips(t *testing.T) {
	cfg := Production("deepseek/deepseek-v4.1-flash", "", "be brief")
	cfg.ControlPlaneURL = "http://kagent-controller.kagent:8083"
	cfg.RuntimeTelemetry = tracing.RuntimeTelemetry{Runtime: tracing.RuntimeHermes, AgentName: "a", AgentNamespace: "team"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.BaseURL != DefaultBaseURL || parsed.Instructions != "be brief" || parsed.GatewayCommand[0] != DefaultGatewayCommand[0] {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	valid := Production("m", "https://openrouter.ai/api/v1", "")
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"version", func(c *Config) { c.Version = 99 }, "unsupported config version"},
		{"command", func(c *Config) { c.GatewayCommand = nil }, "gateway_command"},
		{"model", func(c *Config) { c.Model = " " }, "model is required"},
		{"base URL", func(c *Config) { c.BaseURL = "openrouter.ai" }, "base_url"},
		{"base URL credentials", func(c *Config) { c.BaseURL = "https://user:pw@openrouter.ai" }, "base_url"},
		{"port", func(c *Config) { c.APIServerPort = 0 }, "api_server_port"},
		{"limits", func(c *Config) { c.TurnTimeoutMillis = 0 }, "must be positive"},
		{"log level", func(c *Config) { c.LogLevel = "trace" }, "log_level"},
		{"runtime", func(c *Config) {
			c.RuntimeTelemetry = tracing.RuntimeTelemetry{Runtime: tracing.RuntimeCodex, AgentName: "a", AgentNamespace: "n"}
		}, "names runtime"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			cfg.GatewayCommand = append([]string(nil), valid.GatewayCommand...)
			test.mutate(&cfg)
			raw, _ := json.Marshal(cfg)
			if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := Parse([]byte(`{"version":1,"unknown":true}`)); err == nil || !strings.Contains(err.Error(), "decode config") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestOwnsEnvironment(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "HERMES_HOME", "HERMES_YOLO_MODE", "API_SERVER_KEY"} {
		if !OwnsEnvironment(name) {
			t.Fatalf("%s must be reserved", name)
		}
	}
	if OwnsEnvironment("TZ") {
		t.Fatal("TZ must not be reserved")
	}
}
