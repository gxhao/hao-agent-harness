package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hao-agent-harness/internal/httpapi"
)

func logEntries(t *testing.T, file string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Fatal("log must be indented")
	}
	if strings.Contains(string(data), "client-secret") || strings.Contains(string(data), "upstream-secret") {
		t.Fatal("credential leaked")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	var entries []map[string]any
	for {
		var entry map[string]any
		err := decoder.Decode(&entry)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestSessionChatLogs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			dir := t.TempDir()
			upstream := func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, streamEvent+streamEvent+"data: [DONE]\n\n")
				} else {
					success(w, r)
				}
			}
			body, answer := requestJSON, "private-answer"
			if stream {
				body, answer = streamRequestJSON, "hellohello"
			}
			// New handler instances simulate restarting the service.
			for range 2 {
				f := newFixture(t, upstream, 8, func(opts *httpapi.Options) { opts.ChatLogDir = dir })
				r := request("POST", "/v1/chat/completions", body)
				r.Header.Set("X-Session-ID", "conversation-1")
				r.Header.Set("X-Request-ID", "same-id")
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				if w.Code != 200 || w.Header().Get("X-Session-ID") != "conversation-1" {
					t.Fatalf("response %d", w.Code)
				}
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 || files[0].IsDir() {
				t.Fatalf("expected one session file: %v", err)
			}
			entries := logEntries(t, filepath.Join(dir, files[0].Name()))
			if len(entries) != 2 {
				t.Fatalf("expected two completed turns, got %d", len(entries))
			}
			for _, entry := range entries {
				if entry["outcome"] != "success" || entry["session_id"] != "conversation-1" {
					t.Fatalf("entry %v", entry)
				}
				encoded, _ := json.Marshal(entry["request"])
				if !strings.Contains(string(encoded), "private-prompt") {
					t.Fatal("full request missing")
				}
				response := entry["response"].(map[string]any)
				choice := response["choices"].([]any)[0].(map[string]any)
				if choice["message"].(map[string]any)["content"] != answer {
					t.Fatal("aggregated reply missing")
				}
				if response["object"] != "chat.completion" {
					t.Fatal("stream chunks must not be logged")
				}
			}
		})
	}
}

func TestStreamingWritesOnlyAfterCompletion(t *testing.T) {
	dir := t.TempDir()
	release, sent := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamEvent)
		w.(http.Flusher).Flush()
		close(sent)
		<-release
		io.WriteString(w, "data: [DONE]\n\n")
	}, 8, func(opts *httpapi.Options) { opts.ChatLogDir = dir })
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := request("POST", "/v1/chat/completions", streamRequestJSON)
		r.Header.Set("X-Session-ID", "paused")
		f.handler.ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("logs written before stream completion")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish")
	}
	if len(logEntries(t, filepath.Join(dir, "session-paused.log"))) != 1 {
		t.Fatal("must write one final entry")
	}
}

func TestChatLogRetainsPartialStreamAndFailure(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamEvent)
	}, 8, func(opts *httpapi.Options) { opts.ChatLogDir = dir })
	r := request("POST", "/v1/chat/completions", streamRequestJSON)
	r.Header.Set("X-Session-ID", "failed")
	f.handler.ServeHTTP(httptest.NewRecorder(), r)
	entries := logEntries(t, filepath.Join(dir, "session-failed.log"))
	if len(entries) != 1 || entries[0]["outcome"] != "upstream_protocol_error" || entries[0]["error"] == nil {
		t.Fatal("failure result missing")
	}
	encoded, _ := json.Marshal(entries[0]["response"])
	if !strings.Contains(string(encoded), "hello") {
		t.Fatal("partial reply missing")
	}
}

func TestConcurrentSessionLogAppends(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, success, 8, func(opts *httpapi.Options) { opts.ChatLogDir = dir })
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := request("POST", "/v1/chat/completions", requestJSON)
			r.Header.Set("X-Session-ID", "parallel")
			f.handler.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
	if len(logEntries(t, filepath.Join(dir, "session-parallel.log"))) != 6 {
		t.Fatal("concurrent turns lost or interleaved")
	}
}

func TestSessionIDsAndLogFailures(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, success, 8, func(opts *httpapi.Options) { opts.ChatLogDir = dir })
	for _, id := range []string{"../escape", "..", "a/b", "a\\b", strings.Repeat("a", 65)} {
		r := request("POST", "/v1/chat/completions", requestJSON)
		r.Header.Set("X-Session-ID", id)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("invalid session ID accepted")
		}
	}
	ids := map[string]bool{}
	for range 2 {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, request("POST", "/v1/chat/completions", requestJSON))
		id := w.Header().Get("X-Session-ID")
		if id == "" || ids[id] {
			t.Fatal("missing session IDs must be generated independently")
		}
		ids[id] = true
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 2 {
		t.Fatal("independent sessions must have separate files")
	}
	f = newFixture(t, success, 8, func(opts *httpapi.Options) { opts.ChatLogDir = filepath.Join(t.TempDir(), "missing", "directory") })
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, request("POST", "/v1/chat/completions", requestJSON))
	if w.Code != 200 || !strings.Contains(f.logs.String(), "chat_log_failed") {
		t.Fatal("log failure must be visible without breaking chat")
	}
}
