package provider_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/provider"
)

const completionJSON = `{"id":"chat-1","object":"chat.completion","created":1,"model":"real-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
const chunkJSON = `{"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"real-model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`

func fakeProvider(t *testing.T, handler http.HandlerFunc) *provider.Compatible {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	client := provider.NewHTTPClient(time.Second, 8)
	t.Cleanup(client.CloseIdleConnections)
	return provider.NewCompatible(client, upstream.URL, "upstream-secret")
}

func TestCompletionValidationAndErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name, body, media, code string
		status                  int
	}{
		{name: "valid without usage", body: completionJSON, media: "application/json", status: 200},
		{name: "missing fields", body: `{}`, media: "application/json", status: 200, code: "upstream_protocol_error"},
		{name: "invalid JSON", body: `{`, media: "application/json", status: 200, code: "upstream_protocol_error"},
		{name: "trailing JSON", body: completionJSON + `{}`, media: "application/json", status: 200, code: "upstream_protocol_error"},
		{name: "wrong media", body: completionJSON, media: "text/plain", status: 200, code: "upstream_protocol_error"},
		{name: "oversize", body: strings.Repeat(" ", (8<<20)+1), media: "application/json", status: 200, code: "upstream_protocol_error"},
		{name: "provider auth", body: `upstream-secret`, status: 401, code: "upstream_error"},
		{name: "rate limited", status: 429, code: "upstream_rate_limited"},
		{name: "server failure", status: 503, code: "upstream_error"},
		{name: "unknown bad request", body: `upstream-secret`, status: 400, code: "upstream_error"},
		{name: "context limit", body: `{"error":{"code":"context_length_exceeded"}}`, status: 400, code: "context_length_exceeded"},
		{name: "tool calls unsupported", body: strings.ReplaceAll(completionJSON, `"stop"`, `"tool_calls"`), media: "application/json", status: 200, code: "upstream_protocol_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			result, err := p.Complete(context.Background(), chat.Request{Model: "real-model"})
			if tc.code == "" {
				if err != nil || result.Usage != nil || result.Choices[0].Message.Content != "hello" {
					t.Fatalf("unexpected result: %v", err)
				}
				return
			}
			if err == nil || chat.PublicError(err).Code != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
			if strings.Contains(err.Error(), "upstream-secret") {
				t.Fatal("leaked provider secret")
			}
		})
	}
}

func TestSSENetworkFragmentsAndMultiline(t *testing.T) {
	p := fakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		// Split JSON across data lines at a legal whitespace boundary and write
		// individual bytes to exercise framing independently from read boundaries.
		input := ": heartbeat\r\n\r\ndata: " + strings.Replace(chunkJSON, `,"choices"`, ",\r\ndata: \"choices\"", 1) + "\r\n\r\ndata: [DONE]\r\n\r\n"
		for _, b := range []byte(input) {
			w.Write([]byte{b})
			w.(http.Flusher).Flush()
		}
	})
	s, err := p.Stream(context.Background(), chat.Request{Model: "real-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event, err := s.Next(context.Background())
	if err != nil || event.Choices[0].Delta.Content == nil || *event.Choices[0].Delta.Content != "hello" {
		t.Fatalf("unexpected event: %v", err)
	}
	if _, err := s.Next(context.Background()); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestSSERejectsIncompleteAndOversizeEvents(t *testing.T) {
	for _, input := range []string{
		"data: " + chunkJSON + "\n",   // no empty line and no DONE
		"data: " + chunkJSON + "\n\n", // valid event, premature EOF
		"data: {invalid}\n\n",
		"data: " + strings.Repeat("x", (1<<20)+1) + "\n\n",
		"data: {\"error\":{\"message\":\"upstream-secret\"}}\n\n",
	} {
		t.Run(strings.ReplaceAll(input[:min(len(input), 30)], "\n", "_"), func(t *testing.T) {
			p := fakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, input)
			})
			s, err := p.Stream(context.Background(), chat.Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = s.Next(context.Background())
			if err == nil {
				_, err = s.Next(context.Background())
			}
			if err == nil || err == io.EOF {
				t.Fatalf("expected stream failure, got %v", err)
			}
			if strings.Contains(err.Error(), "upstream-secret") {
				t.Fatal("leaked upstream error")
			}
		})
	}
}

func TestSSEDeadlineCancelsBlockedUpstream(t *testing.T) {
	canceled := make(chan struct{})
	p := fakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	})
	s, err := p.Stream(context.Background(), chat.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want timeout, got %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not canceled")
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	called := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called <- struct{}{} }))
	defer target.Close()
	p := fakeProvider(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) })
	_, err := p.Complete(context.Background(), chat.Request{})
	if err == nil || chat.PublicError(err).Status != 502 {
		t.Fatalf("unexpected redirect result: %v", err)
	}
	select {
	case <-called:
		t.Fatal("followed credential-bearing redirect")
	default:
	}
}

func TestHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer upstream.Close()
	client := provider.NewHTTPClient(50*time.Millisecond, 8)
	defer client.CloseIdleConnections()
	p := provider.NewCompatible(client, upstream.URL, "upstream-secret")
	_, err := p.Complete(context.Background(), chat.Request{})
	if err == nil || chat.PublicError(err).Status != 504 {
		t.Fatalf("want timeout, got %v", err)
	}
}
