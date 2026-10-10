package observability_test

import (
	"hao-agent-harness/internal/observability"
	"net/http/httptest"
	"testing"
	"time"
)

type blockedWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	first   bool
}

func (w *blockedWriter) Write(b []byte) (int, error) {
	if !w.first {
		w.first = true
		close(w.started)
		<-w.release
	}
	return w.ResponseRecorder.Write(b)
}

func TestSlowScrapeDoesNotBlockRequests(t *testing.T) {
	m := observability.New()
	w := &blockedWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	scraped := make(chan struct{})
	go func() { m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil)); close(scraped) }()
	<-w.started
	observed := make(chan struct{})
	go func() {
		m.Begin()
		m.Observe(observability.Labels{Route: "/healthz", Outcome: "success", Status: 200}, time.Millisecond, 0, nil, false)
		m.End()
		close(observed)
	}()
	select {
	case <-observed:
	case <-time.After(time.Second):
		close(w.release)
		t.Fatal("slow scrape blocked request metrics")
	}
	close(w.release)
	<-scraped
}
