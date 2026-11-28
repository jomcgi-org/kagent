package driver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/kagent-dev/kagent/go/harness/hermes/internal/fakehermes"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

const jwtPlaceholder = "Bearer " + CredentialPlaceholder

// fakeVault is a memory MCP server like bosun-artifacts /notes/mcp: a
// version is the hex SHA-256 of a file, "" for a missing one, and a write
// from any other version changes nothing and returns the current copy.
type fakeVault struct {
	t     *testing.T
	mu    sync.Mutex
	files map[string]string
	// down answers every call with 503.
	down bool
	// beforeWrite runs before each memory_write is checked, to model another
	// session writing first.
	beforeWrite func(file string)
	writes      []map[string]string
	// handoff is the conversation's handoff note; handoffWrites counts
	// handoff_write calls.
	handoff       string
	handoffWrites int
}

func newFakeVault(t *testing.T, files map[string]string) (*fakeVault, *httptest.Server) {
	v := &fakeVault{t: t, files: files}
	server := httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(server.Close)
	return v, server
}

func version(content string, exists bool) string {
	if !exists {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (v *fakeVault) set(file, content string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.files[file] = content
}

func (v *fakeVault) setDown(down bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.down = down
}

func (v *fakeVault) get(file string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	content, ok := v.files[file]
	return content, ok
}

func (v *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	require.Equal(v.t, jwtPlaceholder, r.Header.Get("Authorization"))
	require.Equal(v.t, http.MethodPost, r.Method)
	v.mu.Lock()
	down := v.down
	v.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Method string `json:"method"`
		ID     int    `json:"id"`
		Params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		} `json:"params"`
	}
	require.NoError(v.t, json.NewDecoder(r.Body).Decode(&req))
	require.Equal(v.t, "tools/call", req.Method)
	file := req.Params.Arguments["file"]
	var out map[string]any
	switch req.Params.Name {
	case "handoff_read":
		require.Empty(v.t, req.Params.Arguments)
		v.mu.Lock()
		out = map[string]any{"channel_id": "1", "content": v.handoff}
		v.mu.Unlock()
	case "handoff_write":
		v.mu.Lock()
		v.handoff = req.Params.Arguments["content"]
		v.handoffWrites++
		out = map[string]any{"channel_id": "1", "content": v.handoff}
		v.mu.Unlock()
	}
	if strings.HasPrefix(req.Params.Name, "handoff_") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "{}"}}, "structuredContent": out, "isError": false,
		}})
		return
	}
	require.Contains(v.t, []string{"MEMORY.md", "USER.md"}, file)
	switch req.Params.Name {
	case "memory_read":
		content, ok := v.get(file)
		out = map[string]any{"file": file, "content": content, "version": version(content, ok)}
	case "memory_write":
		if v.beforeWrite != nil {
			v.beforeWrite(file)
		}
		v.mu.Lock()
		v.writes = append(v.writes, req.Params.Arguments)
		content, ok := v.files[file]
		if current := version(content, ok); current != req.Params.Arguments["base_version"] {
			out = map[string]any{"file": file, "content": content, "version": current, "conflict": true}
		} else {
			v.files[file] = req.Params.Arguments["content"]
			out = map[string]any{"file": file, "content": req.Params.Arguments["content"], "version": version(req.Params.Arguments["content"], true)}
		}
		v.mu.Unlock()
	default:
		v.t.Fatalf("unexpected tool %q", req.Params.Name)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
		"content": []map[string]any{{"type": "text", "text": "{}"}}, "structuredContent": out, "isError": false,
	}})
}

func (v *fakeVault) writeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.writes)
}

func entries(e ...string) string { return strings.Join(e, EntryDelimiter) }

func newTestSyncer(t *testing.T, url string) (*memorySyncer, string) {
	home := t.TempDir()
	return newMemorySyncer(MemorySync{URL: url, Headers: map[string]string{"Authorization": jwtPlaceholder}}, home), home
}

func readLocal(t *testing.T, home, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "memories", file))
	require.NoError(t, err)
	return string(raw)
}

func writeLocal(t *testing.T, home, file, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "memories", file), []byte(content), 0o600))
}

func TestMemorySyncLoadsTheVaultAndWritesBackAChange(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": entries("Joe runs the homelab")})
	m, home := newTestSyncer(t, server.URL)

	m.Sync(t.Context(), true)
	require.Equal(t, entries("Joe runs the homelab"), readLocal(t, home, "MEMORY.md"))
	require.Equal(t, "", readLocal(t, home, "USER.md"), "a missing file starts empty")
	require.Zero(t, vault.writeCount(), "an unchanged file is not written")

	// Hermes's memory tool adds an entry during the turn.
	writeLocal(t, home, "MEMORY.md", entries("Joe runs the homelab", "The factory runs on node-1"))
	writeLocal(t, home, "USER.md", "Joe prefers short answers")
	m.Sync(t.Context(), true)
	got, _ := vault.get("MEMORY.md")
	require.Equal(t, entries("Joe runs the homelab", "The factory runs on node-1"), got)
	got, _ = vault.get("USER.md")
	require.Equal(t, "Joe prefers short answers", got)
	require.Equal(t, version(entries("Joe runs the homelab"), true), vault.writes[0]["base_version"])
	require.Equal(t, "", vault.writes[1]["base_version"], "USER.md was missing")

	// Another channel's session changes the vault: the next sync loads it.
	vault.set("MEMORY.md", entries("Joe runs the homelab", "The factory runs on node-1", "Garage holds the notes"))
	m.Sync(t.Context(), true)
	require.Equal(t, entries("Joe runs the homelab", "The factory runs on node-1", "Garage holds the notes"), readLocal(t, home, "MEMORY.md"))
	require.Equal(t, 2, vault.writeCount())
}

func TestMemorySyncMergesIntoANewerCopy(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": entries("a", "x", "gone")})
	m, home := newTestSyncer(t, server.URL)
	m.Sync(t.Context(), true)

	// This session replaces x, removes gone and adds c; another session
	// meanwhile adds d.
	writeLocal(t, home, "MEMORY.md", entries("a", "x2", "c"))
	vault.set("MEMORY.md", entries("a", "x", "gone", "d"))
	m.Sync(t.Context(), true)

	want := entries("a", "x2", "c", "d")
	got, _ := vault.get("MEMORY.md")
	require.Equal(t, want, got)
	require.Equal(t, want, readLocal(t, home, "MEMORY.md"))
	require.Len(t, vault.writes, 2, "one conflict, one write from the newer version")
	require.Equal(t, version(entries("a", "x", "gone", "d"), true), vault.writes[1]["base_version"])

	// The next sync sees no local change and writes nothing.
	m.Sync(t.Context(), true)
	require.Len(t, vault.writes, 2)
}

func TestMemorySyncRetriesABoundedNumberOfTimes(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": "a"})
	m, home := newTestSyncer(t, server.URL)
	m.Sync(t.Context(), true)
	writeLocal(t, home, "MEMORY.md", entries("a", "mine"))
	n := 0
	vault.beforeWrite = func(file string) {
		n++
		vault.set(file, entries("a", "theirs", string(rune('0'+n))))
	}
	m.Sync(t.Context(), true)
	require.Equal(t, memoryCASAttempts, vault.writeCount())
	require.Equal(t, entries("a", "mine"), readLocal(t, home, "MEMORY.md"), "the local change stays for the next sync")

	vault.beforeWrite = nil
	m.Sync(t.Context(), true)
	got, _ := vault.get("MEMORY.md")
	require.Equal(t, entries("a", "mine", "theirs", "3"), got)
}

func TestMemorySyncKeepsTheNewerCopyOverTheLimit(t *testing.T) {
	long := strings.Repeat("é", UserCharLimit/2)
	vault, server := newFakeVault(t, map[string]string{"USER.md": entries("Joe", "old")})
	m, home := newTestSyncer(t, server.URL)
	m.Sync(t.Context(), true)
	writeLocal(t, home, "USER.md", entries("Joe", long))
	vault.set("USER.md", entries("Joe", "old", long+"!"))
	m.Sync(t.Context(), true)

	// The addition would exceed the limit, so only the removal of "old" applies.
	got, _ := vault.get("USER.md")
	require.Equal(t, entries("Joe", long+"!"), got)
	require.Equal(t, entries("Joe", long+"!"), readLocal(t, home, "USER.md"))
	require.Len(t, vault.writes, 2)

	// With no removal, nothing is written.
	writeLocal(t, home, "USER.md", entries("Joe", long+"!", long))
	vault.set("USER.md", entries("Joe", long+"!", "new"))
	m.Sync(t.Context(), true)
	got, _ = vault.get("USER.md")
	require.Equal(t, entries("Joe", long+"!", "new"), got)
	require.Equal(t, got, readLocal(t, home, "USER.md"))
	require.Len(t, vault.writes, 3)
}

func TestMemorySyncLoadsALostLocalFile(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": entries("a", "b")})
	m, home := newTestSyncer(t, server.URL)
	m.Sync(t.Context(), true)
	require.NoError(t, os.Remove(filepath.Join(home, "memories", "MEMORY.md")))
	m.Sync(t.Context(), true)
	got, _ := vault.get("MEMORY.md")
	require.Equal(t, entries("a", "b"), got, "a lost local file never empties the vault")
	require.Equal(t, entries("a", "b"), readLocal(t, home, "MEMORY.md"))
	require.Zero(t, vault.writeCount())
}

func TestMemorySyncBeforeATurnStopsWithTheTurn(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": "a"})
	m, home := newTestSyncer(t, server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.Sync(ctx, false)
	require.NoFileExists(t, filepath.Join(home, "memories", "MEMORY.md"))
	m.Sync(ctx, true)
	require.Equal(t, "a", readLocal(t, home, "MEMORY.md"))
	require.Zero(t, vault.writeCount())
}

func TestMemorySyncSurvivesAnOutage(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": "a"})
	m, home := newTestSyncer(t, server.URL)

	// The vault is down at the first turn: the files start empty and the
	// turn's change stays local.
	vault.setDown(true)
	m.Sync(t.Context(), true)
	require.NoFileExists(t, filepath.Join(home, "memories", "MEMORY.md"))
	writeLocal(t, home, "MEMORY.md", "b")
	m.Sync(t.Context(), true)
	require.Equal(t, "b", readLocal(t, home, "MEMORY.md"))

	// A new process on the same volume, after the vault is back: the
	// change merges into the vault's copy, which it never read.
	vault.setDown(false)
	m = newMemorySyncer(m.server, home)
	m.Sync(t.Context(), true)
	got, _ := vault.get("MEMORY.md")
	require.Equal(t, entries("a", "b"), got)
	require.Equal(t, entries("a", "b"), readLocal(t, home, "MEMORY.md"))

	// A later outage keeps the last synced base, so a removal still merges.
	vault.setDown(true)
	writeLocal(t, home, "MEMORY.md", "b")
	m.Sync(t.Context(), true)
	vault.setDown(false)
	vault.set("MEMORY.md", entries("a", "b", "c"))
	m = newMemorySyncer(m.server, home)
	m.Sync(t.Context(), true)
	got, _ = vault.get("MEMORY.md")
	require.Equal(t, entries("b", "c"), got)
}

func TestMemorySyncReportsToolErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"This session has no Discord server"}],"isError":true}}`))
	}))
	t.Cleanup(server.Close)
	m, home := newTestSyncer(t, server.URL)
	_, err := m.read(t.Context(), "MEMORY.md")
	require.ErrorContains(t, err, "no Discord server")
	m.Sync(t.Context(), true)
	require.NoFileExists(t, filepath.Join(home, "memories", "MEMORY.md"))
}

func TestMergeEntries(t *testing.T) {
	for name, tc := range map[string]struct{ base, local, newer, want []string }{
		"add to both":        {[]string{"a"}, []string{"a", "b"}, []string{"a", "c"}, []string{"a", "b", "c"}},
		"same add":           {[]string{"a"}, []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}},
		"remove":             {[]string{"a", "b"}, []string{"b"}, []string{"a", "b", "c"}, []string{"b", "c"}},
		"replace in place":   {[]string{"a", "b", "c"}, []string{"a", "B", "c"}, []string{"a", "b", "c", "d"}, []string{"a", "B", "c", "d"}},
		"they removed":       {[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"a"}, []string{"a", "c"}},
		"no base":            {nil, []string{"x"}, []string{"a"}, []string{"a", "x"}},
		"replace first":      {[]string{"a", "b"}, []string{"A", "b"}, []string{"a", "b", "z"}, []string{"b", "z", "A"}},
		"whitespace entries": {[]string{"a"}, []string{"a", " b ", ""}, []string{"a"}, []string{"a", "b"}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, entries(tc.want...), MergeEntries(entries(tc.base...), entries(tc.local...), entries(tc.newer...)))
		})
	}
	require.Equal(t, []string{"a", "b§c"}, ParseEntries("a\n§\n\n§\na\n§\nb§c\n"))
}

func TestRenderConfigEnablesOnlyTheMemoryToolset(t *testing.T) {
	raw, err := RenderConfig(Settings{
		Model: "m", BaseURL: "https://api.anthropic.com", Memory: true,
		MCPServers: map[string]MCPServer{"notes": {URL: "http://notes.test:8080/notes/mcp", Tools: []string{"notes_read"}}},
	})
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{"api_server": []any{"memory", "notes"}}, cfg["platform_toolsets"])
	disabled := cfg["agent"].(map[string]any)["disabled_toolsets"].([]any)
	require.Len(t, disabled, len(DisabledToolsets)-1)
	require.NotContains(t, disabled, "memory")
	require.Contains(t, DisabledToolsets, "memory", "the shared list keeps memory")
	require.Equal(t, map[string]any{
		"memory_enabled": true, "user_profile_enabled": true, "provider": "", "nudge_interval": 0,
		"write_approval": false, "memory_char_limit": MemoryCharLimit, "user_char_limit": UserCharLimit,
	}, cfg["memory"])

	raw, err = RenderConfig(Settings{Model: "m", BaseURL: "https://api.anthropic.com"})
	require.NoError(t, err)
	cfg = nil
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, map[string]any{"memory_enabled": false, "user_profile_enabled": false, "provider": ""}, cfg["memory"])
}

func TestNewRequiresMemorySettingsAndServerTogether(t *testing.T) {
	base := Config{GatewayCommand: fakehermes.Command(), Home: t.TempDir(), Settings: Settings{Model: "m", BaseURL: "https://api.anthropic.com"}}
	withServer := base
	withServer.Memory = &MemorySync{URL: "http://notes.test/mcp"}
	_, err := New(withServer)
	require.ErrorContains(t, err, "memory")
	withSettings := base
	withSettings.Settings.Memory = true
	_, err = New(withSettings)
	require.ErrorContains(t, err, "memory")
}

func TestRunSyncsMemoryAroundEachTurn(t *testing.T) {
	vault, server := newFakeVault(t, map[string]string{"MEMORY.md": "Joe runs the homelab"})
	home := t.TempDir()
	d, err := New(Config{
		GatewayCommand: fakehermes.Command(), Home: home, Port: freePort(t), Instructions: "be brief",
		Settings: Settings{Model: "m", BaseURL: "https://api.anthropic.com", Memory: true},
		Memory:   &MemorySync{URL: server.URL, Headers: map[string]string{"Authorization": jwtPlaceholder}},
		Model:    "m", Provider: "anthropic", Environment: os.Environ(),
		StartupTimeout: 20 * time.Second, TurnTimeout: 20 * time.Second, StopGrace: 5 * time.Second,
		MaxBytes: 1 << 20, Output: &bytes.Buffer{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	sink := &recordingSink{}
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "1", Prompt: "recall"}, sink)
	require.NoError(t, err)
	require.Equal(t, "memory: Joe runs the homelab", sink.text.String(), "the turn sees the vault's copy")

	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "2", Prompt: "remember Joe likes Skye", ContinuationID: sink.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	got, _ := vault.get("MEMORY.md")
	require.Equal(t, entries("Joe runs the homelab", "Joe likes Skye"), got, "the turn's change is written back")

	// The vault is down: the turn still answers, and the change waits.
	vault.setDown(true)
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "3", Prompt: "remember Joe walks the Cuillin", ContinuationID: sink.sessions[0]}, &recordingSink{})
	require.NoError(t, err)
	vault.setDown(false)
	third := &recordingSink{}
	_, err = d.Run(t.Context(), runtime.Turn{TaskID: "4", Prompt: "recall", ContinuationID: sink.sessions[0]}, third)
	require.NoError(t, err)
	require.Equal(t, "memory: "+entries("Joe runs the homelab", "Joe likes Skye", "Joe walks the Cuillin"), third.text.String())
	got, _ = vault.get("MEMORY.md")
	require.Equal(t, entries("Joe runs the homelab", "Joe likes Skye", "Joe walks the Cuillin"), got)
}
