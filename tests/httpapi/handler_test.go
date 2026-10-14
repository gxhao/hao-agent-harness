package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/httpapi"
	"hao-agent-harness/internal/observability"
	"hao-agent-harness/internal/provider"
	"hao-agent-harness/internal/service"
)

const requestJSON = `{"model":"default","messages":[{"role":"user","content":"private-prompt"}]}`
const streamRequestJSON = `{"model":"default","messages":[{"role":"user","content":"private-prompt"}],"stream":true}`
const responseJSON = `{"id":"chat-1","object":"chat.completion","created":1,"model":"real-model","choices":[{"index":0,"message":{"role":"assistant","content":"private-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`
const streamEvent = `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"real-model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n"

type fixture struct {
	handler *httpapi.Handler
	service *service.Service
	metrics *observability.Metrics
	logs    *bytes.Buffer
}

func newFixture(t *testing.T, upstream http.HandlerFunc, capacity int, modify func(*httpapi.Options)) fixture {
	t.Helper()
	u := httptest.NewServer(upstream)
	t.Cleanup(u.Close)
	client := provider.NewHTTPClient(time.Second, 8)
	t.Cleanup(client.CloseIdleConnections)
	root, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := service.New(root, map[string]config.Model{"default": {Provider: "primary", UpstreamModel: "real-model", SupportsStream: true, SupportsTemperature: true, SupportsMaxTokens: true, MaxOutputTokens: 4096}}, map[string]chat.Provider{"primary": provider.NewCompatible(client, u.URL, "upstream-secret")}, capacity, 2*time.Second)
	metrics := observability.New()
	logs := &bytes.Buffer{}
	opts := httpapi.Options{MaxRequestBytes: 1024, FirstEventTimeout: time.Second, StreamIdleTimeout: time.Second, WriteTimeout: time.Second}
	if modify != nil {
		modify(&opts)
	}
	return fixture{handler: httpapi.New(s, opts, slog.New(slog.NewJSONHandler(logs, nil)), metrics), service: s, metrics: metrics, logs: logs}
}

func request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func success(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, responseJSON)
}

func TestRequestContract(t *testing.T) {
	var calls atomic.Int64
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, r) }, 8, nil)
	for _, tc := range []struct {
		name, body, method, path, auth, media, code string
		status                                      int
	}{
		{name: "wrong media", body: requestJSON, media: "text/plain", status: 415, code: "unsupported_media_type"},
		{name: "invalid JSON", body: `{`, status: 400, code: "invalid_request"},
		{name: "unknown field", body: strings.Replace(requestJSON, `"model"`, `"unsupported":true,"model"`, 1), status: 400, code: "invalid_request"},
		{name: "unknown message field", body: strings.Replace(requestJSON, `"role"`, `"extra":true,"role"`, 1), status: 400, code: "invalid_request"},
		{name: "trailing data", body: requestJSON + ` {}`, status: 400, code: "invalid_request"},
		{name: "too large including whitespace", body: requestJSON + strings.Repeat(" ", 1024), status: 413, code: "request_too_large"},
		{name: "unknown model", body: strings.Replace(requestJSON, "default", "untrusted-random-model", 1), status: 400, code: "unknown_model"},
		{name: "empty messages", body: `{"model":"default","messages":[]}`, status: 400, code: "invalid_request"},
		{name: "tool role", body: strings.Replace(requestJSON, `"user"`, `"tool"`, 1), status: 400, code: "invalid_request"},
		{name: "array content", body: `{"model":"default","messages":[{"role":"user","content":[]}]}`, status: 400, code: "invalid_request"},
		{name: "invalid temperature", body: strings.TrimSuffix(requestJSON, "}") + `,"temperature":3}`, status: 400, code: "invalid_request"},
		{name: "too many output tokens", body: strings.TrimSuffix(requestJSON, "}") + `,"max_tokens":4097}`, status: 400, code: "invalid_request"},
		{name: "wrong method", body: requestJSON, method: "GET", status: 405, code: "method_not_allowed"},
		{name: "unknown path", path: "/arbitrary-user-path", status: 404, code: "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, path := tc.method, tc.path
			if method == "" {
				method = "POST"
			}
			if path == "" {
				path = "/v1/chat/completions"
			}
			r := request(method, path, tc.body)
			if tc.auth == "missing" {
				r.Header.Del("Authorization")
			} else if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if tc.media != "" {
				r.Header.Set("Content-Type", tc.media)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			var envelope struct {
				Error struct {
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.RequestID == "" || envelope.Error.RequestID != w.Header().Get("X-Request-ID") {
				t.Fatal("inconsistent request ID")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid requests reached upstream")
	}
	w := httptest.NewRecorder()
	f.metrics.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "untrusted-random-model") || strings.Contains(w.Body.String(), "arbitrary-user-path") {
		t.Fatal("untrusted high cardinality metric label")
	}
}

func TestClientAuthorizationIsNotRequiredOrValidated(t *testing.T) {
	var calls atomic.Int64
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, r) }, 8, nil)
	for _, authorization := range []string{"", "Bearer invalid", "Basic anything"} {
		r := request(http.MethodPost, "/v1/chat/completions", requestJSON)
		if authorization == "" {
			r.Header.Del("Authorization")
		} else {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("authorization %q returned %d: %s", authorization, w.Code, w.Body.String())
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls=%d; want 3", calls.Load())
	}
}

func TestCompletionRoutingOptionalZeroAndLogPrivacy(t *testing.T) {
	var got chat.Request
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("wrong upstream credential")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		success(w, r)
	}, 8, nil)
	body := strings.TrimSuffix(requestJSON, "}") + `,"temperature":0}`
	r := request("POST", "/v1/chat/completions", body)
	r.Header.Set("X-Request-ID", "client-id-123")
	r.Header.Set("Authorization", "Bearer rotated-secret")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var response chat.Response
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got.Model != "real-model" || got.Temperature == nil || *got.Temperature != 0 || got.MaxTokens != nil || response.Model != "default" || w.Header().Get("X-Request-ID") != "client-id-123" {
		t.Fatal("routing or optional parameter contract violated")
	}
	for _, private := range []string{"client-secret", "rotated-secret", "upstream-secret", "private-prompt", "private-answer"} {
		if strings.Contains(f.logs.String(), private) {
			t.Fatalf("logs leaked %s", private)
		}
	}
	m := httptest.NewRecorder()
	f.metrics.ServeHTTP(m, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(m.Body.String(), "llm_prompt_tokens_total") || !strings.Contains(m.Body.String(), "llm_http_inflight 0") {
		t.Fatal("missing metrics or leaked in-flight count")
	}
}

func TestHealthModelsAndDrain(t *testing.T) {
	f := newFixture(t, success, 8, nil)
	for _, path := range []string{"/healthz", "/readyz", "/v1/models"} {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, request("GET", path, ""))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if path == "/v1/models" && (strings.Contains(w.Body.String(), "real-model") || !strings.Contains(w.Body.String(), "default")) {
			t.Fatal("model listing leaked upstream routing")
		}
	}
	f.service.Drain()
	for _, path := range []string{"/readyz", "/v1/models", "/v1/chat/completions"} {
		method := "GET"
		if path == "/v1/chat/completions" {
			method = "POST"
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, request(method, path, requestJSON))
		if w.Code != 503 {
			t.Fatalf("draining %s: %d", path, w.Code)
		}
	}
}

func TestStreamNormalAndMidstreamFailures(t *testing.T) {
	for _, tc := range []struct{ name, tail, code string }{
		{name: "normal", tail: "data: [DONE]\n\n"},
		{name: "disconnected", code: "upstream_protocol_error"},
		{name: "upstream error", tail: "data: {\"error\":{\"message\":\"upstream-secret\"}}\n\n", code: "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, streamEvent+tc.tail)
			}, 8, nil)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, request("POST", "/v1/chat/completions", streamRequestJSON))
			if w.Code != 200 || !w.Flushed || w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), `"model":"default"`) {
				t.Fatalf("invalid stream: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "real-model") || strings.Contains(w.Body.String(), "upstream-secret") {
				t.Fatal("stream leaked upstream details")
			}
			if tc.code == "" {
				if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("missing DONE")
				}
			} else if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatal("bad terminal stream error")
			}
		})
	}
}

func TestStreamFirstAndIdleTimeout(t *testing.T) {
	for _, initial := range []string{": heartbeat\n\n", streamEvent} {
		t.Run(initial[:4], func(t *testing.T) {
			canceled := make(chan struct{})
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, initial)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}, 8, func(o *httpapi.Options) {
				o.FirstEventTimeout = 50 * time.Millisecond
				o.StreamIdleTimeout = 50 * time.Millisecond
			})
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, request("POST", "/v1/chat/completions", streamRequestJSON))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "upstream_timeout") || strings.Contains(w.Body.String(), "[DONE]") {
				t.Fatalf("unexpected timeout: %s", w.Body.String())
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("timeout did not cancel upstream")
			}
		})
	}
}

func TestStreamCapacityAndClientCancellation(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamEvent)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}, 1, nil)
	api := httptest.NewServer(f.handler)
	defer api.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/chat/completions", strings.NewReader(streamRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("Content-Type", "application/json")
	resp, err := api.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, request("POST", "/v1/chat/completions", requestJSON))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "capacity_exceeded") {
		t.Fatalf("capacity not enforced: %d", w.Code)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not propagate")
	}
	// Admit a session without calling the fake upstream a second time. Waiting
	// here observes resource release, rather than assuming cancellation is synchronous.
	deadline := time.Now().Add(2 * time.Second)
	for {
		session, err := f.service.Open(context.Background(), chat.Request{Model: "default", Messages: []chat.Message{{Role: "user", Content: "check"}}})
		if err == nil {
			session.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream leaked capacity")
		}
		time.Sleep(time.Millisecond)
	}
}

// deadlineWriter models a client whose socket stays unwritable until its write
// deadline. This checks resource release without relying on OS buffer sizes.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *deadlineWriter) Write([]byte) (int, error) {
	time.Sleep(time.Until(w.deadline))
	return 0, os.ErrDeadlineExceeded
}

func TestSlowClientWriteDeadlineReleasesUpstream(t *testing.T) {
	canceled := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamEvent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}, 1, func(o *httpapi.Options) { o.WriteTimeout = 30 * time.Millisecond })
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	f.handler.ServeHTTP(w, request("POST", "/v1/chat/completions", streamRequestJSON))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("slow client left upstream running")
	}
	session, err := f.service.Open(context.Background(), chat.Request{Model: "default", Messages: []chat.Message{{Role: "user", Content: "check"}}})
	if err != nil {
		t.Fatal("slow client leaked capacity")
	}
	session.Close()
	if !strings.Contains(f.logs.String(), `"outcome":"downstream_error"`) {
		t.Fatal("missing downstream failure result")
	}
}
