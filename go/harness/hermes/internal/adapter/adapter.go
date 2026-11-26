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
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
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

// New validates the configuration, writes HERMES_HOME, and returns the driver.
func New(_ context.Context, input Input) (*driver.Driver, error) {
	cfg, err := config.Parse(input.ConfigJSON)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(input.DurableDir) {
		return nil, fmt.Errorf("durable directory must be an absolute path")
	}
	home := config.HermesHome(input.DurableDir)
	if err := utils.EnsurePrivateDir(home); err != nil {
		return nil, fmt.Errorf("prepare Hermes home: %w", err)
	}
	if err := driver.WriteHome(home, driver.Settings{Model: cfg.Model, BaseURL: cfg.BaseURL, APIKey: lookup(input.Environment, config.APIKeyEnvName)}); err != nil {
		return nil, err
	}
	return driver.New(driver.Config{
		GatewayCommand: cfg.GatewayCommand, Home: home, Port: cfg.APIServerPort,
		Instructions: cfg.Instructions, Model: cfg.Model, Provider: cfg.RuntimeTelemetry.Provider,
		Environment: input.Environment, StartupTimeout: cfg.StartupTimeout(), TurnTimeout: cfg.TurnTimeout(),
		StopGrace: cfg.StopGrace(), MaxBytes: cfg.MaxResponseBytes, Output: input.Output,
	})
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
