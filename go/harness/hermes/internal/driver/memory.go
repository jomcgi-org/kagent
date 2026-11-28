package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// EntryDelimiter separates the entries of a Hermes memory file
// (tools/memory_tool_store.py ENTRY_DELIMITER at v0.23.4).
const EntryDelimiter = "\n§\n"

// MemorySync is the MCP server that holds Hermes's memory files.
type MemorySync struct {
	URL string
	// Headers hold literals or the credential placeholder, as for the
	// binding's own MCP entry. The egress gateway replaces the placeholder.
	Headers map[string]string
}

// memoryFiles are the files Hermes's built-in memory reads, with their limits.
var memoryFiles = []struct {
	name  string
	limit int
}{{"MEMORY.md", MemoryCharLimit}, {"USER.md", UserCharLimit}}

const (
	// memoryCASAttempts bounds the merge and write-back retries of one file.
	memoryCASAttempts = 3
	// memorySyncTimeout bounds one sync of both files, so an outage delays a
	// turn by at most this much.
	memorySyncTimeout = 10 * time.Second
	// memoryStateFile records, per file, the vault copy the local file was
	// last synced with. It lives outside memories/, which Hermes reads.
	memoryStateFile = "kagent-memory-sync.json"
	// unknownVersion never matches a vault version, so a write from it always
	// conflicts and merges into the vault's copy.
	unknownVersion = "unknown"
)

// memoryBase is the vault copy a local file was last synced with.
type memoryBase struct {
	Content string `json:"content"`
	Version string `json:"version"`
}

// memorySyncer keeps $HERMES_HOME/memories in step with the MCP server. A
// failure never fails a turn: it logs, keeps the local copy, and the next sync
// writes it back.
type memorySyncer struct {
	server MemorySync
	home   string
	http   *http.Client
	mu     sync.Mutex
	nextID int
}

func newMemorySyncer(server MemorySync, home string) *memorySyncer {
	return &memorySyncer{server: server, home: home, http: &http.Client{Timeout: memorySyncTimeout}}
}

func (m *memorySyncer) dir() string { return filepath.Join(m.home, "memories") }

// Sync writes back each local file changed since its last sync, merging into a
// newer vault copy, and otherwise loads the vault copy into the local file.
// The gateway must be idle: Hermes writes the files only during a turn.
func (m *memorySyncer) Sync(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memorySyncTimeout)
	defer cancel()
	log := logging.FromContext(ctx)
	if err := utils.EnsurePrivateDir(m.dir()); err != nil {
		log.WarnContext(ctx, "hermes memory sync skipped", "error", err)
		return
	}
	state := m.loadState()
	for _, file := range memoryFiles {
		base, err := m.syncFile(ctx, file.name, file.limit, state[file.name])
		if err != nil {
			// The local copy stays; the next sync writes it back.
			log.WarnContext(ctx, "hermes memory sync failed; keeping the local copy", "file", file.name, "error", err)
			continue
		}
		state[file.name] = base
	}
	if err := m.saveState(state); err != nil {
		log.WarnContext(ctx, "hermes memory sync state not saved", "error", err)
	}
}

func (m *memorySyncer) syncFile(ctx context.Context, name string, limit int, base *memoryBase) (*memoryBase, error) {
	unlock, err := lockMemoryFile(filepath.Join(m.dir(), name))
	if err != nil {
		return base, err
	}
	defer unlock()
	path := filepath.Join(m.dir(), name)
	raw, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return base, fmt.Errorf("read %s: %w", name, err)
	}
	local := string(raw)
	if base != nil && local == base.Content || base == nil && !exists {
		// Unchanged since the last sync, or never synced and empty: load.
		current, err := m.read(ctx, name)
		if err != nil {
			return base, err
		}
		if base == nil || current.Version != base.Version || !exists {
			if err := m.writeLocal(path, current.Content); err != nil {
				return base, err
			}
		}
		return &memoryBase{Content: current.Content, Version: current.Version}, nil
	}
	// Changed locally, or local content of unknown origin: write it back.
	from := memoryBase{Version: unknownVersion}
	if base != nil {
		from = *base
	}
	content, version := local, from.Version
	for range memoryCASAttempts {
		result, err := m.write(ctx, name, content, version)
		if err != nil {
			return base, err
		}
		if !result.Conflict {
			if content != local {
				if err := m.writeLocal(path, content); err != nil {
					return base, err
				}
			}
			return &memoryBase{Content: content, Version: result.Version}, nil
		}
		// Another session wrote first: apply this session's entry changes to
		// the newer copy and try again from its version.
		merged := MergeEntries(from.Content, local, result.Content)
		if utf8.RuneCountInString(merged) > limit {
			logging.FromContext(ctx).WarnContext(ctx, "merged hermes memory exceeds its limit; keeping the newer copy",
				"file", name, "chars", utf8.RuneCountInString(merged), "limit", limit)
			merged = result.Content
		}
		if merged == result.Content {
			if err := m.writeLocal(path, merged); err != nil {
				return base, err
			}
			return &memoryBase{Content: merged, Version: result.Version}, nil
		}
		content, version = merged, result.Version
	}
	return base, fmt.Errorf("write %s: the vault copy kept changing", name)
}

// ParseEntries splits a memory file as Hermes does: on the full delimiter,
// stripped, without empty or repeated entries.
func ParseEntries(raw string) []string {
	var entries []string
	for part := range strings.SplitSeq(raw, EntryDelimiter) {
		if entry := strings.TrimSpace(part); entry != "" && !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	return entries
}

// MergeEntries applies the entry changes from base to local onto newer: it
// drops the entries local removed and inserts the ones it added, each after
// the nearest earlier local entry that newer still holds. A replaced entry is
// one removal and one addition, so it keeps its place.
func MergeEntries(base, local, newer string) string {
	baseEntries, localEntries := ParseEntries(base), ParseEntries(local)
	result := slices.DeleteFunc(ParseEntries(newer), func(entry string) bool {
		return slices.Contains(baseEntries, entry) && !slices.Contains(localEntries, entry)
	})
	for i, entry := range localEntries {
		if slices.Contains(baseEntries, entry) || slices.Contains(result, entry) {
			continue
		}
		at := len(result)
		for j := i - 1; j >= 0; j-- {
			if k := slices.Index(result, localEntries[j]); k >= 0 {
				at = k + 1
				break
			}
		}
		result = slices.Insert(result, at, entry)
	}
	return strings.Join(result, EntryDelimiter)
}

func (m *memorySyncer) writeLocal(path, content string) error {
	if err := utils.ReplacePrivateFile(path, []byte(content)); err != nil {
		return fmt.Errorf("write local %s: %w", filepath.Base(path), err)
	}
	return nil
}

// lockMemoryFile takes the lock Hermes takes before it changes path
// (MemoryStore._file_lock: an exclusive flock on <path>.lock).
func lockMemoryFile(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", filepath.Base(path), err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", filepath.Base(path), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (m *memorySyncer) loadState() map[string]*memoryBase {
	state := map[string]*memoryBase{}
	raw, err := os.ReadFile(filepath.Join(m.home, memoryStateFile))
	if err != nil {
		return state
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return map[string]*memoryBase{}
	}
	maps.DeleteFunc(state, func(_ string, base *memoryBase) bool { return base == nil })
	return state
}

func (m *memorySyncer) saveState(state map[string]*memoryBase) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return utils.ReplacePrivateFile(filepath.Join(m.home, memoryStateFile), raw)
}

// memoryResult is what memory_read and memory_write return.
type memoryResult struct {
	File     string `json:"file"`
	Content  string `json:"content"`
	Version  string `json:"version"`
	Conflict bool   `json:"conflict"`
}

func (m *memorySyncer) read(ctx context.Context, name string) (memoryResult, error) {
	return m.call(ctx, config.MemoryReadTool, map[string]string{"file": name})
}

func (m *memorySyncer) write(ctx context.Context, name, content, baseVersion string) (memoryResult, error) {
	return m.call(ctx, config.MemoryWriteTool, map[string]string{"file": name, "content": content, "base_version": baseVersion})
}

// maxMemoryResponse bounds a tool answer: a 64 KiB file in escaped JSON, twice.
const maxMemoryResponse = 1 << 20

// call sends one MCP tools/call over Streamable HTTP. The memory server is
// stateless and answers with one application/json object, so the call needs
// no initialize and no session.
func (m *memorySyncer) call(ctx context.Context, tool string, arguments any) (memoryResult, error) {
	m.nextID++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": m.nextID, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments},
	})
	if err != nil {
		return memoryResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.server.URL, bytes.NewReader(body))
	if err != nil {
		return memoryResult{}, err
	}
	for name, value := range m.server.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	resp, err := m.http.Do(req)
	if err != nil {
		return memoryResult{}, fmt.Errorf("%s: %w", tool, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMemoryResponse+1))
	if err != nil {
		return memoryResult{}, fmt.Errorf("%s: read answer: %w", tool, err)
	}
	if resp.StatusCode != http.StatusOK {
		return memoryResult{}, fmt.Errorf("%s: HTTP %d", tool, resp.StatusCode)
	}
	if len(raw) > maxMemoryResponse {
		return memoryResult{}, fmt.Errorf("%s: answer too large", tool)
	}
	var answer struct {
		Result *struct {
			StructuredContent *memoryResult `json:"structuredContent"`
			Content           []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return memoryResult{}, fmt.Errorf("%s: decode answer: %w", tool, err)
	}
	switch {
	case answer.Error != nil:
		return memoryResult{}, fmt.Errorf("%s: %s", tool, answer.Error.Message)
	case answer.Result == nil:
		return memoryResult{}, fmt.Errorf("%s: answer has no result", tool)
	case answer.Result.IsError:
		var text []string
		for _, c := range answer.Result.Content {
			text = append(text, c.Text)
		}
		return memoryResult{}, fmt.Errorf("%s: %s", tool, strings.Join(text, " "))
	case answer.Result.StructuredContent == nil:
		return memoryResult{}, fmt.Errorf("%s: answer has no structured content", tool)
	}
	return *answer.Result.StructuredContent, nil
}
