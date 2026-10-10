// Package observability provides a bounded-label Prometheus text endpoint.
package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"hao-agent-harness/internal/chat"
)

type Labels struct {
	Route    string
	Model    string
	Provider string
	Outcome  string
	Status   int
	Stream   bool
}

type aggregate struct {
	count        uint64
	duration     float64
	firstCount   uint64
	firstSum     float64
	usageMissing uint64
	prompt       int64
	completion   int64
}

type Metrics struct {
	mu       sync.Mutex
	inflight int64
	rows     map[Labels]*aggregate
}

// New 创建指标集合，并初始化按有限标签组合聚合的 map。
func New() *Metrics { return &Metrics{rows: make(map[Labels]*aggregate)} }

// Begin 在请求开始时增加并发中的请求数；互斥锁保护多个 HTTP goroutine 的共享数据。
func (m *Metrics) Begin() { m.mu.Lock(); m.inflight++; m.mu.Unlock() }

// End 在请求结束时减少并发数，通常由 HTTP handler 的 defer 保证执行。
func (m *Metrics) End() { m.mu.Lock(); m.inflight--; m.mu.Unlock() }

// Observe 汇总请求耗时、首事件耗时和 token 用量；锁只覆盖内存更新，不覆盖网络写出。
func (m *Metrics) Observe(labels Labels, duration time.Duration, first time.Duration, usage *chat.Usage, chatRequest bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.rows[labels]
	if a == nil {
		a = &aggregate{}
		m.rows[labels] = a
	}
	a.count++
	a.duration += duration.Seconds()
	if first > 0 {
		a.firstCount++
		a.firstSum += first.Seconds()
	}
	if chatRequest {
		if usage == nil {
			a.usageMissing++
		} else {
			a.prompt += usage.PromptTokens
			a.completion += usage.CompletionTokens
		}
	}
}

// ServeHTTP 实现标准库 http.Handler 接口，快照指标后输出 Prometheus 文本格式。
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.mu.Lock()
	inflight := m.inflight
	rows := make(map[Labels]aggregate, len(m.rows))
	for labels, a := range m.rows {
		rows[labels] = *a
	}
	m.mu.Unlock()
	// A slow metrics reader must never hold the lock used by business requests.
	fmt.Fprintln(w, "# TYPE llm_http_inflight gauge")
	fmt.Fprintf(w, "llm_http_inflight %d\n", inflight)
	for _, spec := range []struct{ name, kind string }{
		{"llm_http_requests_total", "counter"}, {"llm_http_request_duration_seconds", "summary"},
		{"llm_upstream_first_event_seconds", "summary"}, {"llm_usage_missing_total", "counter"},
		{"llm_prompt_tokens_total", "counter"}, {"llm_completion_tokens_total", "counter"},
	} {
		fmt.Fprintf(w, "# TYPE %s %s\n", spec.name, spec.kind)
	}
	lines := make([]string, 0, len(rows))
	for l, a := range rows {
		labels := fmt.Sprintf("route=%s,model=%s,provider=%s,outcome=%s,status=%s,stream=%s", strconv.Quote(l.Route), strconv.Quote(l.Model), strconv.Quote(l.Provider), strconv.Quote(l.Outcome), strconv.Quote(strconv.Itoa(l.Status)), strconv.Quote(strconv.FormatBool(l.Stream)))
		lines = append(lines, fmt.Sprintf("llm_http_requests_total{%s} %d\nllm_http_request_duration_seconds_sum{%s} %g\nllm_http_request_duration_seconds_count{%s} %d\nllm_upstream_first_event_seconds_sum{%s} %g\nllm_upstream_first_event_seconds_count{%s} %d\nllm_usage_missing_total{%s} %d\nllm_prompt_tokens_total{%s} %d\nllm_completion_tokens_total{%s} %d\n", labels, a.count, labels, a.duration, labels, a.count, labels, a.firstSum, labels, a.firstCount, labels, a.usageMissing, labels, a.prompt, labels, a.completion))
	}
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Fprint(w, line)
	}
}
