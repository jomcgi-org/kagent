package driver

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/kagent-dev/kagent/go/harness/hermes/internal/fakehermes"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func newMemoryDriver(t *testing.T, url string) (*Driver, string) {
	t.Helper()
	home := t.TempDir()
	d, err := New(Config{
		GatewayCommand: fakehermes.Command(), Home: home, Port: freePort(t), Instructions: "be brief",
		Settings: Settings{Model: "m", BaseURL: "https://api.anthropic.com", Memory: true},
		Memory:   &MemorySync{URL: url, Headers: map[string]string{"Authorization": jwtPlaceholder}},
		Model:    "m", Provider: "anthropic", Environment: os.Environ(),
		StartupTimeout: 20 * time.Second, TurnTimeout: 20 * time.Second, StopGrace: 5 * time.Second,
		MaxBytes: 1 << 20, Output: &bytes.Buffer{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d, home
}

func lastInput(t *testing.T, home string) string {
	t.Helper()
	requests, err := fakehermes.Requests(home)
	require.NoError(t, err)
	require.NotEmpty(t, requests)
	return requests[len(requests)-1].Input
}

// A compaction summary becomes the conversation's handoff note after the
// turn, once per change. A new session's first turn starts from the note,
// and a retry of that turn sends the same input.
func TestHandoffNoteCarriesACompactionIntoTheNextSession(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{})
	d, home := newMemoryDriver(t, server.URL)

	sink := &recordingSink{}
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "1", Prompt: "hi"}, sink)
	require.NoError(t, err)
	require.Equal(t, "hi", lastInput(t, home), "no note yet: the prompt alone")
	key := sink.sessions[0]

	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "2", Prompt: "compact Joe is planning Skye", ContinuationID: key}, &recordingSink{})
	require.NoError(t, err)
	vault.mu.Lock()
	require.Equal(t, "Joe is planning Skye", vault.handoff)
	vault.mu.Unlock()
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "3", Prompt: "history", ContinuationID: key}, &recordingSink{})
	require.NoError(t, err)
	vault.mu.Lock()
	require.Equal(t, 1, vault.handoffWrites, "an unchanged summary is not written again")
	vault.mu.Unlock()

	// The next session runs in a new Actor with its own HERMES_HOME.
	next, nextHome := newMemoryDriver(t, server.URL)
	first := &recordingSink{}
	_, err = next.Run(t.Context(), runtime.Turn{TaskID: "10", Prompt: "where were we"}, first)
	require.NoError(t, err)
	want := HandoffPreamble + "Joe is planning Skye\n\nwhere were we"
	require.Equal(t, want, lastInput(t, nextHome))

	vault.mu.Lock()
	vault.handoff = "a newer note"
	vault.mu.Unlock()
	_, err = next.Run(t.Context(), runtime.Turn{TaskID: "10", Prompt: "where were we", ContinuationID: first.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	require.Equal(t, want, lastInput(t, nextHome), "a retry sends the first input again")
	_, err = next.Run(t.Context(), runtime.Turn{TaskID: "11", Prompt: "and then", ContinuationID: first.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	require.Equal(t, "and then", lastInput(t, nextHome), "only the first turn gets the note")
}

// The vault being down never fails a turn: the first turn starts without the
// note, and an unsent note goes on a later turn.
func TestHandoffSurvivesAnOutage(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{})
	vault.handoff = "older note"
	vault.setDown(true)
	d, home := newMemoryDriver(t, server.URL)
	sink := &recordingSink{}
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "1", Prompt: "compact the summary"}, sink)
	require.NoError(t, err)
	require.Equal(t, "compact the summary", lastInput(t, home))
	vault.setDown(false)
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "2", Prompt: "history", ContinuationID: sink.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	vault.mu.Lock()
	defer vault.mu.Unlock()
	require.Equal(t, "the summary", vault.handoff)
}

// A refused turn never updates the note, even if the summary file changed.
func TestARefusedTurnLeavesTheNote(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{})
	vault.handoff = "good note"
	d, home := newMemoryDriver(t, server.URL)
	sink := &recordingSink{}
	_, err := d.Run(t.Context(), runtime.Turn{TaskID: "1", Prompt: "hi"}, sink)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, HandoffFile), []byte("refused content"), 0o600))
	out, err := d.Run(t.Context(), runtime.Turn{TaskID: "2", Prompt: "blocked", ContinuationID: sink.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	require.NotNil(t, out.Failure)
	// A later answered turn in the same session does not send it either.
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "3", Prompt: "history", ContinuationID: sink.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	vault.mu.Lock()
	defer vault.mu.Unlock()
	require.Equal(t, "good note", vault.handoff)
	require.Zero(t, vault.handoffWrites)
}

// The handoff plugin loads with Hermes's memory only.
func TestHandoffPluginRunsWithMemory(t *testing.T) {
	home := t.TempDir()
	settings := Settings{Model: "m", BaseURL: "https://api.anthropic.com", Memory: true}
	require.NoError(t, WriteHome(home, settings))
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, []any{HandoffPlugin}, cfg["plugins"].(map[string]any)["enabled"])
	code, err := os.ReadFile(filepath.Join(home, "plugins", HandoffPlugin, "__init__.py"))
	require.NoError(t, err)
	require.True(t, strings.Contains(string(code), `register_hook("post_llm_call"`))

	settings.Memory = false
	require.NoError(t, WriteHome(home, settings))
	_, err = os.Stat(filepath.Join(home, "plugins", HandoffPlugin))
	require.True(t, os.IsNotExist(err), "the plugin directory is removed without memory")
}

func TestClipUTF8KeepsWholeRunes(t *testing.T) {
	require.Equal(t, "ab", clipUTF8("ab", 4))
	require.Equal(t, "a", clipUTF8("aé", 2))
	require.Equal(t, "aé", clipUTF8("aéb", 3))
}
