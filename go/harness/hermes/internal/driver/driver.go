// Package driver runs Hermes Agent's API server and maps one runtime turn to
// one non-streaming /v1/responses call.
package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// SessionKeyPrefix starts every session key the driver creates.
const SessionKeyPrefix = "kagent-"

// Config contains the compiled and Actor-owned inputs for the driver.
type Config struct {
	GatewayCommand []string
	// Home is HERMES_HOME. It is durable: Hermes keeps its sessions there.
	Home string
	// Settings are written into Home before each gateway start.
	Settings       Settings
	Port           int
	Instructions   string
	Model          string
	Provider       string
	Environment    []string
	StartupTimeout time.Duration
	TurnTimeout    time.Duration
	StopGrace      time.Duration
	MaxBytes       int
	// Memory, when set, syncs Hermes's memory files with an MCP server
	// before and after each turn. Settings.Memory must be on.
	Memory *MemorySync
	// Output receives the gateway's stdout and stderr. Nil means os.Stderr.
	Output io.Writer
}

// Driver owns one Hermes gateway and runs the Actor's turns through it.
type Driver struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu      sync.Mutex
	key     string
	gateway *gateway
	memory  *memorySyncer
}

// New returns a driver. The gateway starts on Start or on the first turn.
func New(cfg Config) (*Driver, error) {
	if len(cfg.GatewayCommand) == 0 {
		return nil, fmt.Errorf("Hermes gateway command is required")
	}
	if _, err := RenderConfig(cfg.Settings); err != nil {
		return nil, err
	}
	if cfg.Output == nil {
		cfg.Output = os.Stderr
	}
	key, err := newKey()
	if err != nil {
		return nil, fmt.Errorf("create Hermes API server key: %w", err)
	}
	d := &Driver{cfg: cfg, http: &http.Client{Timeout: cfg.TurnTimeout}, now: time.Now, key: key}
	if (cfg.Memory != nil) != cfg.Settings.Memory {
		return nil, fmt.Errorf("hermes memory needs both its settings and its sync server")
	}
	if cfg.Memory != nil {
		d.memory = newMemorySyncer(*cfg.Memory, cfg.Home)
	}
	return d, nil
}

// Validate checks that the gateway executable exists.
func (d *Driver) Validate(context.Context) error {
	if _, err := exec.LookPath(d.cfg.GatewayCommand[0]); err != nil {
		return fmt.Errorf("find Hermes gateway executable: %w", err)
	}
	return nil
}

// Start runs the gateway and waits until its API server is healthy.
func (d *Driver) Start(ctx context.Context) error {
	_, err := d.ensureGateway(ctx)
	return err
}

// ensureGateway returns a client for a healthy gateway. It starts one when
// none runs, and replaces one that exited or fails its health check.
//
// The gateway starts lazily, never before a golden snapshot: a restore gives
// /data new backing files, which would break the SQLite files a running
// gateway holds open. Hermes' /health does not read that state, so the driver
// also compares the config.yaml it wrote at start with the one HERMES_HOME
// holds now; a different file means /data was replaced. Each start first
// rewrites HERMES_HOME, so a restored /data cannot carry an older config.yaml
// or a .env.
func (d *Driver) ensureGateway(ctx context.Context) (*client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gateway.running() && !d.gateway.homeUnchanged() {
		logging.FromContext(ctx).WarnContext(ctx, "HERMES_HOME was replaced under the Hermes gateway; starting it again")
		d.gateway.stop(d.cfg.StopGrace)
	} else if d.gateway.running() && !d.gateway.healthy(ctx, d.http) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		logging.FromContext(ctx).WarnContext(ctx, "Hermes gateway failed its health check; starting it again")
		d.gateway.stop(d.cfg.StopGrace)
	} else if d.gateway != nil && !d.gateway.running() {
		logging.FromContext(ctx).WarnContext(ctx, "Hermes gateway exited; starting it again", "error", d.gateway.exitErr)
	}
	if !d.gateway.running() {
		d.gateway = nil
		if err := WriteHome(d.cfg.Home, d.cfg.Settings); err != nil {
			return nil, err
		}
		g := &gateway{command: d.cfg.GatewayCommand, home: d.cfg.Home, port: d.cfg.Port, key: d.key, output: d.cfg.Output}
		marker, err := os.Stat(g.markerPath())
		if err != nil {
			return nil, fmt.Errorf("stat Hermes config: %w", err)
		}
		g.marker = marker
		if err := g.start(d.cfg.Environment); err != nil {
			return nil, err
		}
		startCtx, cancel := context.WithTimeout(ctx, d.cfg.StartupTimeout)
		err = g.waitHealthy(startCtx, d.http)
		cancel()
		if err != nil {
			g.stop(d.cfg.StopGrace)
			return nil, err
		}
		d.gateway = g
	}
	return &client{baseURL: d.gateway.url(), key: d.key, http: d.http, maxBytes: d.cfg.MaxBytes}, nil
}

// Run executes one turn. Hermes never parks a turn: approvals are denied in
// its configuration and a turn that does not settle fails.
func (d *Driver) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	if turn.InputResponse != nil {
		return runtime.Outcome{}, fmt.Errorf("Hermes harness has no parked turn to resume")
	}
	if strings.TrimSpace(turn.Prompt) == "" {
		return runtime.Outcome{Failure: &runtime.Failure{Message: "Hermes harness requires a text prompt"}}, nil
	}
	// One Actor owns one Hermes conversation. Its key is bound before the
	// call, so a turn interrupted mid-call continues the same conversation.
	sessionKey := turn.ContinuationID
	if sessionKey == "" {
		sessionKey = NewSessionKey()
		if err := sink.SessionStarted(runtime.SessionStarted{ContinuationID: sessionKey}); err != nil {
			return runtime.Outcome{}, err
		}
	}
	c, err := d.ensureGateway(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return runtime.Outcome{}, ctx.Err()
		}
		return runtime.Outcome{}, err
	}
	var idempotencyKey string
	if turn.TaskID != "" {
		idempotencyKey = "kagent-task-" + turn.TaskID
	}
	if d.memory != nil {
		// Load the vault's copy, which a new Hermes session puts in its
		// prompt, or write back a change a failed sync left behind. Then
		// write back what this turn changed.
		d.memory.Sync(ctx, false)
		defer d.memory.Sync(ctx, true)
	}
	start := d.now()
	result, err := c.respond(ctx, request{SessionKey: sessionKey, IdempotencyKey: idempotencyKey, Instructions: d.cfg.Instructions, Input: turn.Prompt, Images: turn.Images})
	if result.Usage != nil {
		// Hermes reports one total for the turn, which can span several model
		// requests, so the total counts tokens and opens no model span.
		sink.ModelCall(runtime.ModelCall{
			Provider: d.cfg.Provider, RequestModel: d.cfg.Model, ResponseModel: result.Model, ResponseID: result.ID,
			InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
			Start: start, End: d.now(), UsageOnly: true,
		})
	}
	if err != nil {
		if ctx.Err() != nil {
			return runtime.Outcome{}, ctx.Err()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			// A gateway whose /health answers while its responses hang would
			// pass the next health check, so replace it on the next turn.
			logging.FromContext(ctx).WarnContext(ctx, "Hermes turn timed out; stopping the gateway")
			d.stopGateway()
		}
		var turnErr *TurnError
		if errors.As(err, &turnErr) {
			logging.FromContext(ctx).WarnContext(ctx, "Hermes turn did not answer", "status", turnErr.Status, "error", turnErr)
			return runtime.Outcome{Failure: &runtime.Failure{Message: turnErr.Public()}}, nil
		}
		return runtime.Outcome{}, err
	}
	if err := sink.TextDelta(runtime.TextDelta{Text: result.Text}); err != nil {
		return runtime.Outcome{}, err
	}
	return runtime.Outcome{}, nil
}

// Close stops the gateway.
func (d *Driver) Close() error {
	d.stopGateway()
	return nil
}

func (d *Driver) stopGateway() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gateway.stop(d.cfg.StopGrace)
	d.gateway = nil
}

// NewSessionKey returns a new random Hermes session key.
func NewSessionKey() string {
	return SessionKeyPrefix + uuid.NewString()
}

// ValidSessionKey reports whether key is one the driver can send to Hermes.
func ValidSessionKey(key string) error {
	if !validSessionKey(key) {
		return fmt.Errorf("invalid Hermes session key")
	}
	return nil
}
