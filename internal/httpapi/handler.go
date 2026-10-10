// Package httpapi implements the public HTTP and SSE contracts.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/observability"
	"hao-agent-harness/internal/service"
)

type Options struct {
	ClientKeys        []string
	MaxRequestBytes   int64
	FirstEventTimeout time.Duration
	StreamIdleTimeout time.Duration
	WriteTimeout      time.Duration
}

type Handler struct {
	service *service.Service
	options Options
	keys    [][32]byte
	logger  *slog.Logger
	metrics *observability.Metrics
}

// New 注入业务服务、日志和指标依赖，并预先哈希客户端密钥，避免请求处理时保存明文副本。
func New(s *service.Service, opts Options, logger *slog.Logger, metrics *observability.Metrics) *Handler {
	h := &Handler{service: s, options: opts, logger: logger, metrics: metrics}
	for _, key := range opts.ClientKeys {
		h.keys = append(h.keys, sha256.Sum256([]byte(key)))
	}
	return h
}

type trace struct {
	id          string
	labels      observability.Labels
	usage       *chat.Usage
	first       time.Duration
	chatRequest bool
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// requestID 复用格式安全的调用方追踪 ID；输入不可信或格式不符时生成随机 ID。
func requestID(incoming string) string {
	if requestIDPattern.MatchString(incoming) {
		return incoming
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("request ID entropy unavailable")
	}
	return "req-" + hex.EncodeToString(b[:])
}

// ServeHTTP 实现 net/http 的 Handler 接口，完成路由、认证和统一错误处理。
// defer 会在正常返回、提前 return 或 panic 时收尾指标与日志，这也是 Go 常见的资源清理模式。
func (h *Handler) ServeHTTP(raw http.ResponseWriter, r *http.Request) {
	started := time.Now()
	w := &responseWriter{ResponseWriter: raw}
	t := &trace{labels: observability.Labels{Route: "unknown", Model: "none", Provider: "none", Outcome: "success"}}
	h.metrics.Begin()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.labels.Outcome = "internal_error"
			// The recovered value may contain credentials or user text.
			h.logger.Error("HTTP handler panic", "request_id", t.id)
			if w.status == 0 {
				writeError(w, chat.NewError(500, "internal_error", "服务内部错误"), t.id)
			} else if t.labels.Stream && w.status == 200 {
				payload, _ := json.Marshal(errorEnvelope(chat.NewError(500, "internal_error", "服务内部错误"), t.id))
				_ = h.writeSSE(w, payload, nil)
			}
		}
		t.labels.Status = w.status
		if t.labels.Status == 0 {
			t.labels.Status = 200
		}
		h.metrics.End()
		h.metrics.Observe(t.labels, time.Since(started), t.first, t.usage, t.chatRequest)
		h.logger.Info("http_request", "request_id", t.id, "route", t.labels.Route, "model", t.labels.Model, "provider", t.labels.Provider, "status", t.labels.Status, "outcome", t.labels.Outcome, "stream", t.labels.Stream, "duration_ms", time.Since(started).Milliseconds())
	}()
	t.id = requestID(r.Header.Get("X-Request-ID"))
	w.Header().Set("X-Request-ID", t.id)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	method := http.MethodGet
	switch r.URL.Path {
	case "/healthz", "/readyz", "/v1/models":
		t.labels.Route = r.URL.Path
	case "/v1/chat/completions":
		t.labels.Route = r.URL.Path
		method = http.MethodPost
		t.chatRequest = true
	default:
		h.fail(w, t, chat.NewError(404, "not_found", "接口不存在"))
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		h.fail(w, t, chat.NewError(405, "method_not_allowed", "请求方法不支持"))
		return
	}
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/readyz" {
		if !h.service.Ready() {
			h.fail(w, t, chat.NewError(503, "service_unavailable", "服务未就绪"))
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
		return
	}
	if !h.authorized(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		h.fail(w, t, chat.NewError(401, "unauthorized", "客户端凭证无效"))
		return
	}
	if !h.service.Ready() {
		h.fail(w, t, chat.NewError(503, "service_unavailable", "服务正在停止"))
		return
	}
	if r.URL.Path == "/v1/models" {
		h.models(w)
		return
	}
	h.complete(w, r, t)
}

// authorized 校验 Bearer 凭证；固定时间比较减少比较过程泄露密钥信息的风险。
func (h *Handler) authorized(header string) bool {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	matched := 0
	for _, key := range h.keys {
		matched |= subtle.ConstantTimeCompare(hash[:], key[:])
	}
	return matched == 1
}

// models 将服务配置中的模型别名转换成兼容 OpenAI 风格的模型列表响应。
func (h *Handler) models(w http.ResponseWriter) {
	type model struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	list := make([]model, 0)
	for _, name := range h.service.Models() {
		list = append(list, model{ID: name, Object: "model"})
	}
	writeJSON(w, 200, struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: list})
}

// complete 处理聊天请求：限制并解析 JSON、创建带生命周期的 Session，再选择普通响应或 SSE。
func (h *Handler) complete(w *responseWriter, r *http.Request, t *trace) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		h.fail(w, t, chat.NewError(415, "unsupported_media_type", "需要 application/json 请求体"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.options.MaxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			h.fail(w, t, chat.NewError(413, "request_too_large", "请求体超过限制"))
		} else {
			h.fail(w, t, chat.NewError(400, "invalid_request", "无法读取请求体"))
		}
		return
	}
	var request chat.Request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil {
		h.fail(w, t, chat.NewError(400, "invalid_request", "请求 JSON 无效"))
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		h.fail(w, t, chat.NewError(400, "invalid_request", "请求只能包含一个 JSON 对象"))
		return
	}
	t.labels.Stream = request.Stream
	session, err := h.service.Open(r.Context(), request)
	if err != nil {
		h.fail(w, t, err)
		return
	}
	defer session.Close()
	// Only configured aliases become metric labels; untrusted names never do.
	t.labels.Model = session.Alias
	t.labels.Provider = session.ProviderName
	if request.Stream {
		h.stream(w, r, t, session)
		return
	}
	response, err := session.Complete()
	if err != nil {
		h.failContext(w, r, t, err)
		return
	}
	t.usage = response.Usage
	if err := setWriteDeadline(w, boundedWriteTimeout(session.Context(), h.options.WriteTimeout)); err != nil {
		t.labels.Outcome = "downstream_error"
		return
	}
	if err := writeJSON(w, 200, response); err != nil {
		t.labels.Outcome = "downstream_error"
	}
}

// stream 顺序读取上游事件并立即转发给客户端；首事件和后续空闲分别使用独立超时。
// defer 负责关闭上游流，即使遇到客户端断开或中途错误也会执行。
func (h *Handler) stream(w *responseWriter, r *http.Request, t *trace, session *service.Session) {
	started := time.Now()
	stream, err := session.Stream()
	if err != nil {
		h.failContext(w, r, t, err)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := setWriteDeadline(w, boundedWriteTimeout(session.Context(), h.options.WriteTimeout)); err != nil {
		t.labels.Outcome = "downstream_error"
		return
	}
	w.WriteHeader(200)
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.labels.Outcome = "downstream_error"
		return
	}
	first := true
	for {
		timeout := h.options.StreamIdleTimeout
		if first {
			timeout = h.options.FirstEventTimeout
		}
		ctx, cancel := context.WithTimeout(session.Context(), timeout)
		event, err := stream.Next(ctx)
		cancel()
		if err == io.EOF {
			if err := h.writeSSE(w, []byte("[DONE]"), session.Context()); err != nil {
				t.labels.Outcome = "downstream_error"
			}
			return
		}
		if err != nil {
			public := chat.PublicError(err)
			t.labels.Outcome = public.Code
			if r.Context().Err() != nil {
				t.labels.Outcome = "client_canceled"
				return
			}
			payload, _ := json.Marshal(errorEnvelope(public, t.id))
			if err := h.writeSSE(w, payload, nil); err != nil {
				t.labels.Outcome = "downstream_error"
			}
			return
		}
		if first {
			first = false
			t.first = time.Since(started)
		}
		event.Model = session.Alias
		if event.Usage != nil {
			t.usage = event.Usage
		}
		payload, err := json.Marshal(event)
		if err != nil {
			t.labels.Outcome = "internal_error"
			return
		}
		if err := h.writeSSE(w, payload, session.Context()); err != nil {
			t.labels.Outcome = "downstream_error"
			return
		}
	}
}

// writeSSE 写入一个 SSE data 事件并 Flush，让已缓冲数据尽快到达客户端。
func (h *Handler) writeSSE(w http.ResponseWriter, payload []byte, ctx context.Context) error {
	if err := setWriteDeadline(w, boundedWriteTimeout(ctx, h.options.WriteTimeout)); err != nil {
		return err
	}
	if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

// boundedWriteTimeout 取配置上限与 context 剩余时间中的较小值，避免写入超过请求截止时间。
func boundedWriteTimeout(ctx context.Context, limit time.Duration) time.Duration {
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			return min(limit, time.Until(deadline))
		}
	}
	return limit
}

// failContext 区分客户端主动取消与服务错误；客户端已经离开时不再尝试写错误响应。
func (h *Handler) failContext(w *responseWriter, r *http.Request, t *trace, err error) {
	if r.Context().Err() != nil {
		t.labels.Outcome = "client_canceled"
		return
	}
	h.fail(w, t, err)
}

// fail 将内部错误映射成公开错误，并设置写入期限后发送统一 JSON 错误体。
func (h *Handler) fail(w http.ResponseWriter, t *trace, err error) {
	public := chat.PublicError(err)
	t.labels.Outcome = public.Code
	if err := setWriteDeadline(w, h.options.WriteTimeout); err != nil {
		t.labels.Outcome = "downstream_error"
		return
	}
	if err := writeError(w, public, t.id); err != nil {
		t.labels.Outcome = "downstream_error"
	}
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// errorEnvelope 组合客户端稳定使用的错误字段和请求追踪 ID。
func errorEnvelope(public *chat.Error, id string) any {
	return struct {
		Error errorDetail `json:"error"`
	}{Error: errorDetail{Code: public.Code, Message: public.Message, RequestID: id}}
}

// writeError 按领域错误携带的 HTTP 状态码写出统一错误结构。
func writeError(w http.ResponseWriter, err *chat.Error, id string) error {
	return writeJSON(w, err.Status, errorEnvelope(err, id))
}

// writeJSON 设置 JSON 响应头、状态码并编码 value；编码错误交还调用方记录处理结果。
func writeJSON(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(value)
}

// setWriteDeadline 通过 ResponseController 设置底层连接写期限；不支持该能力的测试 writer 可忽略。
func setWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	} // e.g. httptest.ResponseRecorder
	return err
}

// Unwrap keeps ResponseController capabilities available through middleware.
type responseWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap 让 http.ResponseController 穿过包装器访问底层 writer 的 Flush 和 deadline 能力。
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WriteHeader 只记录并转发第一次状态码；HTTP 响应提交后状态码不能再更改。
func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
// Write 在尚未写状态码时按 HTTP 约定先隐式发送 200，再转发响应内容。
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
