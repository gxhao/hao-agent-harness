package server_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/server"
)

func appFixture(t *testing.T, upstream http.HandlerFunc, grace time.Duration) (string, string, context.CancelFunc, <-chan error) {
	t.Helper()
	u := httptest.NewTLSServer(upstream)
	t.Cleanup(u.Close)
	c := config.Config{HTTPAddr: "127.0.0.1:0", MetricsAddr: "", Models: config.Models{Providers: map[string]config.Provider{"primary": {ChatURL: u.URL + "/v1/chat/completions", APIKey: "upstream-secret"}}, Models: map[string]config.Model{"default": {Provider: "primary", UpstreamModel: "real-model", SupportsStream: true, SupportsTemperature: true, SupportsMaxTokens: true, MaxOutputTokens: 4096}}}, MaxRequestBytes: 1024, MaxConcurrent: 8, RequestTimeout: 5 * time.Second, HeaderTimeout: time.Second, FirstEventTimeout: time.Second, StreamIdleTimeout: time.Second, WriteTimeout: time.Second, ShutdownTimeout: grace, LogLevel: "info"}
	// Distinct listener addresses are validated before binding. Tests supply two
	// already-bound ephemeral listeners to avoid port reservation races.
	c.MetricsAddr = "localhost:0"
	a, err := server.NewWithHTTPClient(c, slog.New(slog.NewTextHandler(io.Discard, nil)), u.Client())
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- a.ServeListeners(ctx, l, m) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("server cleanup timed out")
		}
	})
	return "http://" + l.Addr().String(), "http://" + m.Addr().String(), cancel, done
}

func serverReady(url string) bool {
	response, err := http.Get(url + "/readyz")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	return response.StatusCode == http.StatusOK
}

func TestMetricsAreSeparateFromPublicAPI(t *testing.T) {
	url, metricsURL, cancel, done := appFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("health called upstream") }, time.Second)
	client := &http.Client{Timeout: time.Second}
	for _, tc := range []struct {
		url    string
		status int
	}{
		{url + "/", 200}, {url + "/assets/app.css", 200}, {url + "/assets/app.js", 200},
		{url + "/assets/markdown.js", 200}, {url + "/assets/vendor/marked.js", 200}, {url + "/assets/vendor/purify.js", 200},
		{url + "/assets/vendor/README.md", 404},
		{url + "/assets/missing.js", 404}, {url + "/assets/app.js/../app.css", 404},
		{url + "/healthz", 200}, {url + "/metrics", 404}, {metricsURL + "/metrics", 200},
	} {
		resp, err := client.Get(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("got %d want %d", resp.StatusCode, tc.status)
		}
		if strings.HasSuffix(tc.url, "/") && tc.status == 200 {
			if !strings.Contains(string(body), "hao harness") || !strings.Contains(string(body), "name=\"viewport\"") || resp.Header.Get("Content-Security-Policy") == "" {
				t.Fatal("workspace shell or browser security policy missing")
			}
		}
		if strings.HasSuffix(tc.url, "/assets/app.js") && strings.Contains(string(body), "fonts.googleapis.com") {
			t.Fatal("browser UI unexpectedly depends on external assets")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestGracefulAndForcedStreamingShutdown(t *testing.T) {
	for _, forced := range []bool{false, true} {
		name := "graceful"
		if forced {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			finish, canceled := make(chan struct{}), make(chan struct{})
			grace := time.Second
			if forced {
				grace = 80 * time.Millisecond
			}
			url, _, cancel, done := appFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"real-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-finish:
					io.WriteString(w, "data: [DONE]\n\n")
				case <-r.Context().Done():
					close(canceled)
				}
			}, grace)
			client := &http.Client{Timeout: 3 * time.Second}
			r, err := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hello"}],"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer client-secret")
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("got %d", resp.StatusCode)
			}
			cancel()
			deadline := time.Now().Add(time.Second)
			for serverReady(url) {
				if time.Now().After(deadline) {
					t.Fatal("did not enter drain")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-done:
				t.Fatalf("shutdown abandoned active stream: %v", err)
			default:
			}
			if !forced {
				close(finish)
			}
			body, readErr := io.ReadAll(resp.Body)
			if !forced && (readErr != nil || !strings.Contains(string(body), "[DONE]")) {
				t.Fatalf("graceful stream incomplete: %s %v", body, readErr)
			}
			if forced {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("forced shutdown did not cancel upstream")
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown leaked server")
			}
		})
	}
}

func TestHotReloadRebindsListenerAndChangesModelSnapshot(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()
	firstAddr, secondAddr := freeTCPAddress(t), freeTCPAddress(t)
	c := config.Config{HTTPAddr: firstAddr, MetricsAddr: "", Models: config.Models{
		Providers: map[string]config.Provider{"primary": {ChatURL: upstream.URL + "/v1/chat/completions", APIKey: "provider-secret"}},
		Models:    map[string]config.Model{"before": {Provider: "primary", UpstreamModel: "model-a", SupportsStream: true, SupportsTemperature: true, SupportsMaxTokens: true, MaxOutputTokens: 4096}},
	}, MaxRequestBytes: 1024, MaxConcurrent: 8, RequestTimeout: 5 * time.Second, HeaderTimeout: time.Second,
		FirstEventTimeout: time.Second, StreamIdleTimeout: time.Second, WriteTimeout: time.Second, ShutdownTimeout: time.Second, LogLevel: "info"}
	app, err := server.NewWithHTTPClient(c, slog.New(slog.NewTextHandler(io.Discard, nil)), upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("hot reload server did not stop")
		}
	}()
	waitReady(t, "http://"+firstAddr)

	updated := c
	updated.HTTPAddr = secondAddr
	updated.Models = config.Models{Providers: c.Models.Providers, Models: map[string]config.Model{
		"after": {Provider: "primary", UpstreamModel: "model-b", SupportsStream: true, SupportsTemperature: true, SupportsMaxTokens: true, MaxOutputTokens: 4096},
	}}
	if err := app.ApplyConfig(updated); err != nil {
		t.Fatal(err)
	}
	waitReady(t, "http://"+secondAddr)
	response, err := http.Get("http://" + secondAddr + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"after"`) || strings.Contains(string(body), `"id":"before"`) {
		t.Fatalf("new listener did not use updated model snapshot: %d %s", response.StatusCode, body)
	}
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitReady(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(baseURL + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready", baseURL)
}
