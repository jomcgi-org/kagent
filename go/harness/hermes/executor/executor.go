// Package executor builds the Hermes Harness A2A executor for kagent-hermes
// and for binaries that embed the harness elsewhere.
package executor

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/harness/hermes/internal/adapter"
	"github.com/kagent-dev/kagent/go/harness/hermes/internal/driver"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
)

// Config is the input to New.
type Config struct {
	// ConfigJSON is the compiler-owned bootstrap configuration document.
	ConfigJSON []byte
	// DataDir is the absolute durable directory; it is created if missing.
	DataDir string
	// Environment is the Actor environment. The gateway inherits only an
	// allowlisted part of it.
	Environment []string
	// Output receives the gateway's logs. Nil means os.Stderr.
	Output io.Writer
}

// New validates the configuration and Hermes installation, then returns the
// executor and the resource to close on shutdown.
func New(ctx context.Context, cfg Config) (a2asrv.AgentExecutor, io.Closer, error) {
	runner, err := adapter.New(ctx, adapter.Input{
		ConfigJSON: cfg.ConfigJSON, DurableDir: cfg.DataDir, Environment: cfg.Environment, Output: cfg.Output,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("configure Hermes Harness: %w", err)
	}
	validateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := runner.Validate(validateCtx); err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	// The continuation is the Hermes session key: one per Actor, created on
	// its first turn and reused for every later turn.
	store, err := continuation.New(cfg.DataDir+"/adapter", "hermes", driver.ValidSessionKey)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	parsed, err := config.Parse(cfg.ConfigJSON)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	executor, err := runtimea2a.New(runner, store, parsed.RuntimeTelemetry)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	// The first turn starts the gateway. Starting it here would put it in the
	// golden snapshot, whose restore replaces the /data files it holds open.
	return executor, runner, nil
}
