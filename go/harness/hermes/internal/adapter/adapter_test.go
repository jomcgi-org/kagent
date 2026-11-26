package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/harness/hermes/config"
)

func TestNewWritesHermesHomeWithTheCompiledModel(t *testing.T) {
	dataDir := t.TempDir()
	raw, err := json.Marshal(config.Production("deepseek/deepseek-v4.1-flash", "https://openrouter.ai/api/v1", ""))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(config.HermesHome(dataDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(config.HermesHome(dataDir), ".env"), []byte("TELEGRAM_BOT_TOKEN=x\n"), 0o600))
	d, err := New(t.Context(), Input{ConfigJSON: raw, DurableDir: dataDir, Environment: []string{"OPENAI_API_KEY=kagent-credential-injected"}})
	require.NoError(t, err)
	require.NotNil(t, d)
	written, err := os.ReadFile(filepath.Join(config.HermesHome(dataDir), "config.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(written), "default: deepseek/deepseek-v4.1-flash")
	require.Contains(t, string(written), "base_url: https://openrouter.ai/api/v1")
	require.Contains(t, string(written), "api_key: kagent-credential-injected")
	_, err = os.Stat(filepath.Join(config.HermesHome(dataDir), ".env"))
	require.True(t, os.IsNotExist(err))
}
