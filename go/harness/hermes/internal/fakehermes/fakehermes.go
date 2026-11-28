// Package fakehermes is a stand-in for `hermes gateway` in tests. A test
// binary calls RunIfRequested from TestMain and uses Command as the gateway
// command, so the driver starts a real child process.
package fakehermes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const modeEnv = "kagent-hermes-fake-gateway"

// Command returns the gateway command that runs the fake in this test binary.
// Command[0] is the test binary, which the driver resolves with LookPath.
func Command() []string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return []string{exe, "-test.run=^$", "--", modeEnv}
}

// Request is one /v1/responses call the fake recorded.
type Request struct {
	Authorization  string `json:"authorization"`
	SessionKey     string `json:"session_key"`
	IdempotencyKey string `json:"idempotency_key"`
	Input          string `json:"input"`
	Instructions   string `json:"instructions"`
	Conversation   string `json:"conversation"`
	Previous       string `json:"previous_response_id"`
	Store          bool   `json:"store"`
}

// Requests reads the calls the fake recorded under home.
func Requests(home string) ([]Request, error) {
	data, err := os.ReadFile(filepath.Join(home, "fake-requests.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var requests []Request
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var request Request
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

// Env reads the environment the fake gateway started with under home.
func Env(home string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(home, "fake-env.txt"))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n"), nil
}

// RunIfRequested serves the fake and exits when the binary is the gateway
// child. Otherwise it returns at once.
func RunIfRequested() {
	// The marker travels in argv: the driver drops unknown environment names.
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != modeEnv {
		return
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func serve() error {
	home, key := os.Getenv("HERMES_HOME"), os.Getenv("API_SERVER_KEY")
	if home == "" || key == "" || os.Getenv("API_SERVER_HOST") != "127.0.0.1" || os.Getenv("API_SERVER_ENABLED") != "true" {
		return fmt.Errorf("fake Hermes gateway: API server environment is missing")
	}
	if err := os.WriteFile(filepath.Join(home, "fake-env.txt"), []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
		return err
	}
	// Like Hermes' open SQLite response store, the fake holds a file in
	// HERMES_HOME. When a restore replaces the directory, the held file no
	// longer backs HERMES_HOME and turns fail, while /health, like Hermes'
	// own, still answers ok.
	held, err := os.Open(filepath.Join(home, "fake-env.txt"))
	if err != nil {
		return err
	}
	defer held.Close()
	heldInfo, err := held.Stat()
	if err != nil {
		return err
	}
	backed := func() bool {
		current, err := os.Stat(filepath.Join(home, "fake-env.txt"))
		return err == nil && os.SameFile(heldInfo, current)
	}
	var mu sync.Mutex
	storePath := filepath.Join(home, "fake-store.json")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("DELETE /v1/responses/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		st := loadStore(storePath)
		id := r.PathValue("id")
		if _, ok := st.Responses[id]; !ok {
			http.Error(w, `{"error":{"message":"Response not found"}}`, http.StatusNotFound)
			return
		}
		st.forget(id)
		if err := st.save(storePath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"object":"response","deleted":true}`, id)
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input        string `json:"input"`
			Instructions string `json:"instructions"`
			Conversation string `json:"conversation"`
			Previous     string `json:"previous_response_id"`
			Store        bool   `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		recorded := Request{
			Authorization: r.Header.Get("Authorization"), SessionKey: r.Header.Get("X-Hermes-Session-Key"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Input: body.Input, Instructions: body.Instructions,
			Conversation: body.Conversation, Previous: body.Previous, Store: body.Store,
		}
		line, _ := json.Marshal(recorded)
		mu.Lock()
		f, err := os.OpenFile(filepath.Join(home, "fake-requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
		mu.Unlock()
		if body.Input == "slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !backed() {
			http.Error(w, `{"error":{"message":"attempt to write a readonly database"}}`, http.StatusInternalServerError)
			return
		}
		if recorded.Authorization != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid API key"}}`))
			return
		}
		if body.Conversation != "" && body.Previous != "" {
			http.Error(w, `{"error":{"message":"Cannot use both 'conversation' and 'previous_response_id'"}}`, http.StatusBadRequest)
			return
		}
		// Like Hermes's response store: an exact retry of an idempotency key
		// replays its first result, and another body under it conflicts.
		st := loadStore(storePath)
		fingerprint := body.Input + "\x00" + body.Conversation + "\x00" + body.Previous
		if seen, ok := st.Keys[recorded.IdempotencyKey]; ok && recorded.IdempotencyKey != "" {
			if seen.Fingerprint != fingerprint {
				http.Error(w, `{"error":{"message":"admission_conflict"}}`, http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(seen.Body))
			return
		}
		var history []string
		switch {
		case body.Previous != "":
			stored, ok := st.Responses[body.Previous]
			if !ok {
				http.Error(w, `{"error":{"message":"Previous response not found"}}`, http.StatusNotFound)
				return
			}
			history = stored
		case body.Conversation != "":
			history = st.Responses[st.Conversations[body.Conversation]]
		}
		status, text, extra := answer(home, history, body.Input)
		if status == "exit" {
			os.Exit(3)
		}
		// Like Hermes, store the turn whatever its outcome, and move the
		// conversation name to it.
		st.Next++
		id := fmt.Sprintf("resp_%d", st.Next)
		st.Responses[id] = append(append([]string{}, history...), body.Input)
		if body.Conversation != "" {
			st.Conversations[body.Conversation] = id
		}
		out := responseBody(id, status, text, extra)
		if recorded.IdempotencyKey != "" {
			st.Keys[recorded.IdempotencyKey] = replay{Fingerprint: fingerprint, Body: out, Response: id}
		}
		if err := st.save(storePath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(out))
	})
	return http.ListenAndServe("127.0.0.1:"+os.Getenv("API_SERVER_PORT"), mux)
}

// answer runs the fake agent. An empty status writes nothing.
func answer(home string, history []string, input string) (status, text, extra string) {
	// Like a provider safety filter, which judges the whole transcript: a
	// blocked message anywhere in it refuses the turn.
	for _, message := range append(append([]string{}, history...), input) {
		if strings.Contains(message, "blocked") {
			return "failed", "", `,"error":{"message":"content_policy_blocked: Cyber safeguards flagged this message"}`
		}
	}
	// Like Hermes's memory tool, "remember <entry>" adds an entry to
	// MEMORY.md; "recall" answers with what MEMORY.md holds.
	memory := filepath.Join(home, "memories", "MEMORY.md")
	if entry, ok := strings.CutPrefix(input, "remember "); ok {
		raw, _ := os.ReadFile(memory)
		text := entry
		if len(raw) > 0 {
			text = string(raw) + "\n§\n" + entry
		}
		if err := os.WriteFile(memory, []byte(text), 0o600); err != nil {
			return "failed", "", `,"error":{"message":"write memory"}`
		}
		return "completed", "saved", ""
	}
	switch input {
	case "recall":
		raw, _ := os.ReadFile(memory)
		return "completed", "memory: " + string(raw), ""
	case "history":
		// The transcript the turn continued, so a test can see the chain.
		return "completed", "history: " + strings.Join(history, "|"), ""
	case "exit":
		return "exit", "", ""
	case "fail":
		return "failed", "", `,"error":{"message":"provider said no"}`
	case "pending":
		return "in_progress", "", ""
	}
	return "completed", "echo: " + input, ""
}

type replay struct {
	Fingerprint string
	Body        string
	Response    string
}

// store is the fake's response store. It lives in a file, so a new gateway
// process on the same HERMES_HOME keeps it.
type store struct {
	Next          int
	Responses     map[string][]string
	Conversations map[string]string
	Keys          map[string]replay
}

func loadStore(path string) *store {
	st := &store{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, st)
	}
	if st.Responses == nil {
		st.Responses = map[string][]string{}
	}
	if st.Conversations == nil {
		st.Conversations = map[string]string{}
	}
	if st.Keys == nil {
		st.Keys = map[string]replay{}
	}
	return st
}

func (st *store) save(path string) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// forget deletes a response like Hermes's DELETE: the conversation names
// that point at it and its idempotency record go too.
func (st *store) forget(id string) {
	delete(st.Responses, id)
	for name, target := range st.Conversations {
		if target == id {
			delete(st.Conversations, name)
		}
	}
	for k, seen := range st.Keys {
		if seen.Response == id {
			delete(st.Keys, k)
		}
	}
}

// StoredResponses reads the response transcripts the fake stored under home.
func StoredResponses(home string) map[string][]string {
	return loadStore(filepath.Join(home, "fake-store.json")).Responses
}

// SeedConversation stores a response holding history and points the
// conversation name at it, like a session from an earlier adapter.
func SeedConversation(home, conversation string, history []string) error {
	path := filepath.Join(home, "fake-store.json")
	st := loadStore(path)
	st.Next++
	id := fmt.Sprintf("resp_%d", st.Next)
	st.Responses[id] = history
	st.Conversations[conversation] = id
	return st.save(path)
}

func responseBody(id, status, text, extra string) string {
	output := "[]"
	if text != "" {
		encoded, _ := json.Marshal(text)
		output = `[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"thinking"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]`
	}
	return fmt.Sprintf(`{"id":%q,"object":"response","status":%q,"model":"hermes-agent","output":%s,"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}%s}`, id, status, output, extra)
}
