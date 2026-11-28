package executor

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/harness/hermes/internal/fakehermes"
)

func TestMain(m *testing.M) {
	fakehermes.RunIfRequested()
	os.Exit(m.Run())
}

func testConfig(t *testing.T) []byte {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	cfg := config.Production("deepseek/deepseek-v4.1-flash", "https://openrouter.ai/api/v1", "be brief")
	cfg.GatewayCommand = fakehermes.Command()
	cfg.APIServerPort = port
	cfg.StartupTimeoutMillis = 20_000
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	return raw
}

func request(taskID a2atype.TaskID, contextID, text string) *a2asrv.ExecutorContext {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
	message.TaskID, message.ContextID = taskID, contextID
	return &a2asrv.ExecutorContext{TaskID: taskID, ContextID: contextID, Message: message}
}

func collect(t *testing.T, executor a2asrv.AgentExecutor, reqCtx *a2asrv.ExecutorContext) (string, a2atype.TaskState) {
	t.Helper()
	var text string
	var state a2atype.TaskState
	for event, err := range executor.Execute(t.Context(), reqCtx) {
		require.NoError(t, err)
		switch e := event.(type) {
		case *a2atype.TaskArtifactUpdateEvent:
			for _, part := range e.Artifact.Parts {
				text += part.Text()
			}
		case *a2atype.TaskStatusUpdateEvent:
			state = e.Status.State
		}
	}
	return text, state
}

// The executor speaks kagent's private A2A contract through the shared
// runtime: one Hermes conversation per Actor across A2A contexts, and a
// session key that survives a new executor on the same durable directory.
func TestExecutorRunsTurnsThroughHermes(t *testing.T) {
	dataDir := t.TempDir()
	raw := testConfig(t)
	exec, closer, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: dataDir, Environment: os.Environ(), Output: &bytes.Buffer{}})
	require.NoError(t, err)
	text, state := collect(t, exec, request("task-1", "context-a", "hello"))
	require.Equal(t, a2atype.TaskStateCompleted, state)
	require.Equal(t, "echo: hello", text)
	text, state = collect(t, exec, request("task-2", "context-b", "fail"))
	require.Equal(t, a2atype.TaskStateFailed, state)
	require.Empty(t, text)
	require.NoError(t, closer.Close())

	// A replacement Actor process restores the same durable directory.
	exec, closer, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: dataDir, Environment: os.Environ(), Output: &bytes.Buffer{}})
	require.NoError(t, err)
	defer closer.Close()
	_, state = collect(t, exec, request("task-3", "context-c", "again"))
	require.Equal(t, a2atype.TaskStateCompleted, state)

	requests, err := fakehermes.Requests(config.HermesHome(dataDir))
	require.NoError(t, err)
	require.Len(t, requests, 3)
	for _, r := range requests {
		require.Equal(t, requests[0].SessionKey, r.SessionKey, "one Actor owns one Hermes conversation")
	}
	// The first turn continues by name. The failed turn never joins the
	// chain, and the replacement process continues from the answered turn.
	require.Equal(t, requests[0].SessionKey, requests[0].Conversation)
	require.Equal(t, "resp_1", requests[1].Previous)
	require.Equal(t, "resp_1", requests[2].Previous)
	stored, err := os.ReadFile(filepath.Join(dataDir, "adapter", "state.json"))
	require.NoError(t, err)
	require.Contains(t, string(stored), `"runtime": "hermes"`)
	require.Contains(t, string(stored), requests[0].SessionKey)
}

func TestNewRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "relative data dir", cfg: Config{ConfigJSON: testConfig(t), DataDir: "relative/dir"}, wantErr: "absolute path"},
		{name: "malformed config", cfg: Config{ConfigJSON: []byte(`{"version":`), DataDir: t.TempDir()}, wantErr: "decode config"},
		{name: "unknown config field", cfg: Config{ConfigJSON: []byte(`{"nope":1}`), DataDir: t.TempDir()}, wantErr: "decode config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := New(t.Context(), tt.cfg)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestNewRequiresHermesExecutable(t *testing.T) {
	cfg := config.Production("m", "", "")
	cfg.GatewayCommand = []string{"/nonexistent/hermes", "gateway"}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	_, _, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: t.TempDir()})
	require.ErrorContains(t, err, "find Hermes gateway executable")
}
