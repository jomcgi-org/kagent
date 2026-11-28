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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input        string `json:"input"`
			Instructions string `json:"instructions"`
			Conversation string `json:"conversation"`
			Store        bool   `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		recorded := Request{
			Authorization: r.Header.Get("Authorization"), SessionKey: r.Header.Get("X-Hermes-Session-Key"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Input: body.Input, Instructions: body.Instructions,
			Conversation: body.Conversation, Store: body.Store,
		}
		line, _ := json.Marshal(recorded)
		mu.Lock()
		f, err := os.OpenFile(filepath.Join(home, "fake-requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
		mu.Unlock()
		if !backed() {
			http.Error(w, `{"error":{"message":"attempt to write a readonly database"}}`, http.StatusInternalServerError)
			return
		}
		if recorded.Authorization != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid API key"}}`))
			return
		}
		// Like Hermes's memory tool, "remember <entry>" adds an entry to
		// MEMORY.md; "recall" answers with what MEMORY.md holds.
		memory := filepath.Join(home, "memories", "MEMORY.md")
		if entry, ok := strings.CutPrefix(body.Input, "remember "); ok {
			raw, _ := os.ReadFile(memory)
			text := entry
			if len(raw) > 0 {
				text = string(raw) + "\n§\n" + entry
			}
			if err := os.WriteFile(memory, []byte(text), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeResponse(w, "completed", "saved", "")
			return
		}
		switch body.Input {
		case "recall":
			raw, _ := os.ReadFile(memory)
			writeResponse(w, "completed", "memory: "+string(raw), "")
			return
		case "exit":
			os.Exit(3)
		case "slow":
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		case "fail":
			writeResponse(w, "failed", "", `,"error":{"message":"provider said no"}`)
			return
		case "pending":
			writeResponse(w, "in_progress", "", "")
			return
		}
		writeResponse(w, "completed", "echo: "+body.Input, "")
	})
	return http.ListenAndServe("127.0.0.1:"+os.Getenv("API_SERVER_PORT"), mux)
}

func writeResponse(w http.ResponseWriter, status, text, extra string) {
	output := "[]"
	if text != "" {
		encoded, _ := json.Marshal(text)
		output = `[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"thinking"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"resp_1","object":"response","status":%q,"model":"hermes-agent","output":%s,"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}%s}`, status, output, extra)
}
