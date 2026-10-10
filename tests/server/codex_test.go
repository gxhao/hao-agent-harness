package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/server"
)

type codexTransport func(*http.Request) (*http.Response, error)

func (f codexTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestServerUsesPersistedCodexCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.json")
	b, _ := json.Marshal(map[string]any{"client_id": "app_EMoamEEZ73f0CkXaXp7hrann", "access_token": "saved-access", "refresh_token": "saved-refresh", "account_id": "saved-account", "expires_at": time.Now().Add(time.Hour)})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{
		HTTPAddr: "127.0.0.1:0", ClientKeys: []string{"client-secret"}, OpenAICodexTokenFile: path,
		Models: config.Models{
			Providers: map[string]config.Provider{"openai": {AuthType: "openai_codex"}},
			Models:    map[string]config.Model{"default": {Provider: "openai", UpstreamModel: "codex-model", SupportsStream: true, MaxOutputTokens: 4096}},
		},
		MaxRequestBytes: 4096, MaxConcurrent: 4, RequestTimeout: time.Second, HeaderTimeout: time.Second,
		FirstEventTimeout: time.Second, StreamIdleTimeout: time.Second, WriteTimeout: time.Second,
		ShutdownTimeout: time.Second, LogLevel: "info",
	}
	called := false
	client := &http.Client{Transport: codexTransport(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.Host != "chatgpt.com" || r.Header.Get("Authorization") != "Bearer saved-access" || r.Header.Get("ChatGPT-Account-Id") != "saved-account" {
			t.Error("persisted credentials were not used")
		}
		events := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"codex-model\",\"created_at\":1791590400}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
			"data: {\"type\":\"response.done\",\"response\":{\"status\":\"completed\"}}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: r}, nil
	})}
	app, err := server.NewWithHTTPClient(c, slog.New(slog.NewTextHandler(io.Discard, nil)), client)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.HTTP.Handler.ServeHTTP(w, r)
	if w.Code != 200 || !called || !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Fatalf("Codex request: status %d, %s", w.Code, w.Body.String())
	}
	status := httptest.NewRequest(http.MethodGet, "/auth/openai/status", nil)
	status.RemoteAddr = "127.0.0.1:1234"
	status.Header.Set("Authorization", "Bearer client-secret")
	w = httptest.NewRecorder()
	app.HTTP.Handler.ServeHTTP(w, status)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cli_login":true`) || strings.Contains(w.Body.String(), "saved-access") {
		t.Fatalf("Codex status: %d %s", w.Code, w.Body.String())
	}
}
