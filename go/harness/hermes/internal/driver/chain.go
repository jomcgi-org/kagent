package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// chainDir holds, under HERMES_HOME, the latest answered response of each
// session key. It sits beside Hermes's response store, so a restore of /data
// replaces both together.
const chainDir = "kagent-chain"

// chainHead is the latest answered turn of one Hermes conversation.
//
// Hermes's API server stores every turn it ran, a refused one too, and
// moves the `conversation` name to it. A refused message in the replayed
// transcript makes the provider's filter refuse every later turn. So the
// driver chains each turn to the last answered response with
// previous_response_id, and a turn without an answer never joins the chain.
type chainHead struct {
	// ResponseID is the stored Hermes response of the answered turn.
	ResponseID string `json:"responseID"`
	// TaskID is the turn's task. A retry of that task sends Base again, so
	// Hermes sees the same request and replays its first result.
	TaskID string `json:"taskID,omitempty"`
	// Base is the response the turn chained to; empty means the
	// conversation name.
	Base string `json:"base,omitempty"`
}

func chainPath(home, sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return filepath.Join(home, chainDir, hex.EncodeToString(sum[:])+".json")
}

// loadChainHead returns nil when the session has no recorded head, such as
// a session from an adapter before this file existed.
func loadChainHead(home, sessionKey string) (*chainHead, error) {
	raw, err := os.ReadFile(chainPath(home, sessionKey))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Hermes chain head: %w", err)
	}
	var head chainHead
	if err := json.Unmarshal(raw, &head); err != nil || head.ResponseID == "" {
		return nil, fmt.Errorf("decode Hermes chain head: invalid content")
	}
	return &head, nil
}

func saveChainHead(home, sessionKey string, head chainHead) error {
	raw, err := json.Marshal(head)
	if err != nil {
		return err
	}
	if err := utils.EnsurePrivateDir(filepath.Join(home, chainDir)); err != nil {
		return fmt.Errorf("prepare Hermes chain head: %w", err)
	}
	if err := utils.ReplacePrivateFile(chainPath(home, sessionKey), raw); err != nil {
		return fmt.Errorf("write Hermes chain head: %w", err)
	}
	return nil
}

// base returns the response a turn of taskID chains to, or "" for the
// conversation name.
func (h *chainHead) base(taskID string) string {
	if h == nil {
		return ""
	}
	if taskID != "" && h.TaskID == taskID {
		return h.Base
	}
	return h.ResponseID
}
