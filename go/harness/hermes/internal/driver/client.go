package driver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// SessionKeyHeader names the Hermes conversation for session and memory scoping.
const SessionKeyHeader = "X-Hermes-Session-Key"

// ReasonContentPolicyBlocked is the Hermes failure reason for a turn the
// model provider's safety filter refused. Hermes 0.23.4 starts the turn error
// with it (agent/conversation_loop.py: "content_policy_blocked: <detail>").
const ReasonContentPolicyBlocked = "content_policy_blocked"

// ReasonInsufficientCredits is the failure reason for a turn the provider
// refused for an empty prepaid balance: HTTP 402. Hermes 0.23.4 classifies a
// 402 as billing and fails the turn with its error summary, which starts
// with the status (agent/api_error_summary.py: "HTTP 402: <message>"). A 429
// is a rate limit that Hermes retries; it is not this reason.
const ReasonInsufficientCredits = "insufficient_credits"

// publicReasons are the Hermes failure reasons a TurnError may publish. Each
// is a fixed code; the provider text after it is never published.
var publicReasons = []string{ReasonContentPolicyBlocked}

// statusReasons map the HTTP status that starts a Hermes error summary to
// the reason a TurnError publishes for it.
var statusReasons = map[string]string{"HTTP 402:": ReasonInsufficientCredits}

// TurnError is a Hermes turn that ended without a usable answer. Its status
// and an allowlisted reason code are safe to publish; Detail comes from
// Hermes and is only logged.
type TurnError struct {
	Status string
	Detail string
}

// Reason returns the allowlisted failure reason that starts Detail, or "".
func (e *TurnError) Reason() string {
	for _, reason := range publicReasons {
		if rest, ok := strings.CutPrefix(e.Detail, reason); ok && (rest == "" || rest[0] == ':') {
			return reason
		}
	}
	for prefix, reason := range statusReasons {
		if strings.HasPrefix(e.Detail, prefix) {
			return reason
		}
	}
	return ""
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
		if reason := e.Reason(); reason != "" {
			return "Hermes turn failed: " + reason
		}
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
	Images         []runtime.Image
	// PreviousResponseID chains the turn to that stored response. Empty
	// chains it to the latest response of the session key's conversation.
	PreviousResponseID string
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
	// Input is the prompt text, or one user message item when the turn
	// carries images.
	Input        any    `json:"input"`
	Instructions string `json:"instructions,omitempty"`
	Store        bool   `json:"store"`
	// Hermes refuses a request with both fields.
	Conversation       string `json:"conversation,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

type inputItem struct {
	Role    string         `json:"role"`
	Content []inputContent `json:"content"`
}

type inputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// responsesInput is r's Responses input: the plain text, or a user message
// with the text and each image as a data URL, which Hermes passes to the
// model as image_url parts.
func responsesInput(r request) any {
	if len(r.Images) == 0 {
		return r.Input
	}
	content := []inputContent{{Type: "input_text", Text: r.Input}}
	for _, image := range r.Images {
		content = append(content, inputContent{Type: "input_image", ImageURL: "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)})
	}
	return []inputItem{{Role: "user", Content: content}}
}

// errPreviousNotFound is a Hermes 404 for a request that named a previous
// response: the response store no longer holds it.
var errPreviousNotFound = errors.New("Hermes has no stored previous response")

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
// The session key goes in SessionKeyHeader. The turn chains to
// r.PreviousResponseID, or else the session key names the `conversation`:
// at v0.23.4 the header alone scopes the session but does not replay its
// transcript. The idempotency key makes a retry of the same task return the
// first result. A TurnError returns with the usage Hermes reported.
func (c *client) respond(ctx context.Context, r request) (reply, error) {
	if !validSessionKey(r.SessionKey) {
		return reply{}, fmt.Errorf("invalid Hermes session key")
	}
	payload := responsesRequest{Input: responsesInput(r), Instructions: r.Instructions, Store: true, PreviousResponseID: r.PreviousResponseID}
	if r.PreviousResponseID == "" {
		payload.Conversation = r.SessionKey
	}
	body, err := json.Marshal(payload)
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
		// Hermes' message can carry provider text, so only the log has it.
		if decodeErr == nil && out.Error != nil && out.Error.Message != "" {
			logging.FromContext(ctx).WarnContext(ctx, "Hermes API server refused the turn", "status", resp.StatusCode, "error", fmt.Sprintf("%.300s", out.Error.Message))
		}
		if resp.StatusCode == http.StatusNotFound && r.PreviousResponseID != "" {
			return reply{}, fmt.Errorf("Hermes HTTP %d: %w", resp.StatusCode, errPreviousNotFound)
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

// forget deletes a stored response. Hermes also drops a conversation name
// that points at it, and the response's idempotency record.
func (c *client) forget(ctx context.Context, responseID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(c.baseURL, "/")+"/v1/responses/"+url.PathEscape(responseID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call Hermes: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, int64(c.maxBytes)))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("Hermes HTTP %d", resp.StatusCode)
	}
	return nil
}
