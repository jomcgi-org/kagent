package driver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// passEnv names the variables the gateway inherits. Everything else is
// dropped, so a messaging token or HERMES_YOLO_MODE in the Actor environment
// cannot enable a platform or skip approvals.
var passEnv = map[string]bool{
	"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true,
	// The Substrate egress gateway trust bundle.
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true, "AWS_CA_BUNDLE": true,
	"NODE_EXTRA_CA_CERTS": true, "CURL_CA_BUNDLE": true, "GIT_SSL_CAINFO": true,
	// The Hermes image's own runtime settings.
	"PYTHONPATH": true, "PYTHONUNBUFFERED": true, "PYTHONDONTWRITEBYTECODE": true, "PYTHONPYCACHEPREFIX": true,
	"PLAYWRIGHT_BROWSERS_PATH": true, "XDG_RUNTIME_DIR": true,
	"HERMES_PYTHON": true, "HERMES_RUNTIME_DIR": true, "HERMES_SITE": true, "HERMES_WEB_DIST": true,
	"HERMES_TUI_DIR": true, "HERMES_WRITE_SAFE_ROOT": true,
}

// gateway is one `hermes gateway` process with only the API server enabled,
// on loopback, behind a random key.
type gateway struct {
	command []string
	home    string
	port    int
	key     string
	output  io.Writer
	// marker is config.yaml as written for this start. A restore that
	// replaces /data replaces it too.
	marker  os.FileInfo
	cmd     *exec.Cmd
	exited  chan struct{}
	exitErr error
}

// newKey returns a random API server key. Hermes refuses keys shorter than 16
// characters.
func newKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// gatewayEnvironment returns the gateway's environment from the adapter's own.
func gatewayEnvironment(parent []string, home string, port int, key string) []string {
	env := make([]string, 0, len(parent)+5)
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		if passEnv[name] || strings.HasPrefix(name, "OTEL_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"HERMES_HOME="+home,
		"API_SERVER_ENABLED=true",
		"API_SERVER_HOST=127.0.0.1",
		"API_SERVER_PORT="+strconv.Itoa(port),
		"API_SERVER_KEY="+key,
	)
}

func (g *gateway) markerPath() string {
	return filepath.Join(g.home, "config.yaml")
}

// homeUnchanged reports whether HERMES_HOME still holds the config.yaml this
// gateway started with.
func (g *gateway) homeUnchanged() bool {
	current, err := os.Stat(g.markerPath())
	return err == nil && g.marker != nil && os.SameFile(g.marker, current)
}

func (g *gateway) url() string {
	return "http://127.0.0.1:" + strconv.Itoa(g.port)
}

func (g *gateway) start(parent []string) error {
	cmd := exec.Command(g.command[0], g.command[1:]...)
	cmd.Env = gatewayEnvironment(parent, g.home, g.port, g.key)
	cmd.Dir = g.home
	cmd.Stdout, cmd.Stderr = g.output, g.output
	utils.ConfigureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Hermes gateway: %w", err)
	}
	g.cmd = cmd
	g.exited = make(chan struct{})
	go func() {
		g.exitErr = cmd.Wait()
		close(g.exited)
	}()
	return nil
}

func (g *gateway) running() bool {
	if g == nil || g.exited == nil {
		return false
	}
	select {
	case <-g.exited:
		return false
	default:
		return true
	}
}

// stop sends SIGTERM to the gateway's process group, then SIGKILL after grace.
func (g *gateway) stop(grace time.Duration) {
	if g == nil || g.cmd == nil || g.cmd.Process == nil {
		return
	}
	_ = utils.TerminateProcessGroup(g.cmd.Process)
	select {
	case <-g.exited:
	case <-time.After(grace):
		_ = utils.KillProcessGroup(g.cmd.Process)
		<-g.exited
	}
}

func (g *gateway) healthy(ctx context.Context, client *http.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.url()+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitHealthy polls /health until it passes, the gateway exits or ctx ends.
func (g *gateway) waitHealthy(ctx context.Context, client *http.Client) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if g.healthy(ctx, client) {
			return nil
		}
		select {
		case <-g.exited:
			return fmt.Errorf("Hermes gateway exited before it was healthy: %v", g.exitErr)
		case <-ctx.Done():
			return fmt.Errorf("Hermes gateway not healthy: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
