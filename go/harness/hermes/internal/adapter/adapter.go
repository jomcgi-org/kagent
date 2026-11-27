// Package adapter constructs the Hermes runtime from compiler-owned
// configuration and Actor-owned paths.
package adapter

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/harness/hermes/internal/driver"
)

// Input contains compiler output and Actor-owned locations used to construct
// the Hermes driver.
type Input struct {
	ConfigJSON  []byte
	DurableDir  string
	Environment []string
	// Output receives the gateway's logs. Nil means os.Stderr.
	Output io.Writer
}

// New validates the configuration and returns the driver.
func New(_ context.Context, input Input) (*driver.Driver, error) {
	cfg, err := config.Parse(input.ConfigJSON)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(input.DurableDir) {
		return nil, fmt.Errorf("durable directory must be an absolute path")
	}
	// The driver writes HERMES_HOME before each gateway start, not here: a
	// snapshot restore can replace /data after this runs.
	return driver.New(driver.Config{
		GatewayCommand: cfg.GatewayCommand, Home: config.HermesHome(input.DurableDir), Port: cfg.APIServerPort,
		Settings: driver.Settings{
			Model: cfg.Model, BaseURL: cfg.BaseURL, APIKey: lookup(input.Environment, cfg.APIKeyEnv()),
			APIMode: cfg.APIMode, CacheTTL: cfg.CacheTTL,
			CompressionThresholdTokens: cfg.CompressionThresholdTokens, CompressionMinTailUserMessages: cfg.CompressionMinTailUserMessages,
			CompressionModel: cfg.CompressionModel,
			MCPServers:       mcpServers(cfg.MCPServers), ServerTools: serverTools(cfg.ServerTools),
		},
		Instructions: cfg.Instructions, Model: cfg.Model, Provider: cfg.RuntimeTelemetry.Provider,
		Environment: input.Environment, StartupTimeout: cfg.StartupTimeout(), TurnTimeout: cfg.TurnTimeout(),
		StopGrace: cfg.StopGrace(), MaxBytes: cfg.MaxResponseBytes, Output: input.Output,
	})
}

func mcpServers(servers map[string]config.MCPServer) map[string]driver.MCPServer {
	if len(servers) == 0 {
		return nil
	}
	result := make(map[string]driver.MCPServer, len(servers))
	for name, server := range servers {
		result[name] = driver.MCPServer{URL: server.URL, Headers: server.Headers, Tools: server.Tools}
	}
	return result
}

func serverTools(tools *config.ServerTools) *driver.ServerTools {
	if tools == nil {
		return nil
	}
	return &driver.ServerTools{WebSearchMaxUses: tools.WebSearchMaxUses, WebFetchMaxUses: tools.WebFetchMaxUses, CodeExecution: tools.CodeExecution}
}

func lookup(environment []string, name string) string {
	value := ""
	for _, entry := range environment {
		if v, ok := strings.CutPrefix(entry, name+"="); ok {
			value = v
		}
	}
	return value
}
