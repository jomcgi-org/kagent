package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
)

// SessionKeyHeader names the Hermes conversation for session and memory scoping.
const SessionKeyHeader = "X-Hermes-Session-Key"

// TurnError is a Hermes turn that ended without a usable answer. Its status
// is safe to publish; Detail comes from Hermes and is only logged.
type TurnError struct {
	Status string
	Detail string
}

func (e *TurnError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("Hermes turn %s: %.300s", e.Status, e.Detail)
	}
	return "Hermes turn " + e.Status
}

// Public is the failure message the A2A task carries.
func (e *TurnError) Public() string {
	switch e.Status {
	case "failed":
		return "Hermes turn failed"
	case "cancelled":
		return "Hermes turn was cancelled"
	case "completed", "incomplete":
		return "Hermes turn finished without an answer"
	default:
		// A non-terminal status, such as a turn waiting on an approval. The
		// adapter never polls, so a turn never waits on a person.
		return "Hermes turn did not settle"
	}
}

// client calls the Hermes API server on loopback.
type client struct {
	baseURL  string
	key      string
	http     *http.Client
	maxBytes int
}

type request struct {
	SessionKey     string
	IdempotencyKey string
	Instructions   string
	Input          string
}

type usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type reply struct {
	ID     string
	Model  string
	Status string
	Text   string
	Usage  *usage
}

type responsesRequest struct {
	Input        string `json:"input"`
	Instructions string `json:"instructions,omitempty"`
	Store        bool   `json:"store"`
	Conversation string `json:"conversation"`
}

type responsesBody struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Phase   string `json:"phase"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// validSessionKey reports whether Hermes accepts key in SessionKeyHeader:
// at most 256 characters and no control characters.
func validSessionKey(key string) bool {
	if key == "" || len(key) > 256 {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// respond runs one turn with one non-streaming /v1/responses call.
//
// The session key goes in SessionKeyHeader and also names the
// `conversation`, so the next turn chains to this one: at v0.23.4 the header
// alone scopes the session but does not replay its transcript. The
// idempotency key makes a retry of the same task return the first result.
// A TurnError returns with the usage Hermes reported.
func (c *client) respond(ctx context.Context, r request) (reply, error) {
	if !validSessionKey(r.SessionKey) {
		return reply{}, fmt.Errorf("invalid Hermes session key")
	}
	body, err := json.Marshal(responsesRequest{Input: r.Input, Instructions: r.Instructions, Store: true, Conversation: r.SessionKey})
	if err != nil {
		return reply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.baseURL, "/")+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set(SessionKeyHeader, r.SessionKey)
	if r.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", r.IdempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("call Hermes: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBytes)+1))
	if err != nil {
		return reply{}, fmt.Errorf("read Hermes response: %w", err)
	}
	if len(data) > c.maxBytes {
		return reply{}, fmt.Errorf("Hermes response larger than %d bytes", c.maxBytes)
	}
	var out responsesBody
	decodeErr := json.Unmarshal(data, &out)
	if resp.StatusCode != http.StatusOK {
		if decodeErr == nil && out.Error != nil && out.Error.Message != "" {
			return reply{}, fmt.Errorf("Hermes HTTP %d: %.300s", resp.StatusCode, out.Error.Message)
		}
		return reply{}, fmt.Errorf("Hermes HTTP %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return reply{}, fmt.Errorf("decode Hermes response: %w", decodeErr)
	}
	result := reply{ID: out.ID, Model: out.Model, Status: out.Status, Usage: out.Usage}
	switch out.Status {
	case "completed", "incomplete":
		// An incomplete turn ran out of its iteration budget and still holds
		// the agent's wrap-up, which is a usable answer.
	case "failed", "cancelled":
		turnErr := &TurnError{Status: out.Status}
		if out.Error != nil {
			turnErr.Detail = out.Error.Message
		}
		return result, turnErr
	default:
		return result, &TurnError{Status: out.Status}
	}
	var text strings.Builder
	for _, item := range out.Output {
		// Commentary items are progress notes, never the answer.
		if item.Type != "message" || item.Role != "assistant" || item.Phase == "commentary" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	if text.Len() == 0 {
		return result, &TurnError{Status: out.Status, Detail: "no answer"}
	}
	result.Text = text.String()
	return result, nil
}
