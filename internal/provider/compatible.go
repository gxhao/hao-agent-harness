// Package provider adapts the supported subset of upstream chat protocols.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"hao-agent-harness/internal/chat"
)

const (
	maxResponseBytes = 8 << 20
	maxEventBytes    = 1 << 20
)

type Compatible struct {
	client *http.Client
	url    string
	key    string
}

// NewHTTPClient 创建复用连接的标准库客户端，并限制连接池、握手和响应头等待时间。
// 禁止自动重定向可以确保认证请求不会被转发到配置 URL 之外的主机。
func NewHTTPClient(headerTimeout time.Duration, capacity int) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = headerTimeout
	t.MaxIdleConns = capacity * 2
	t.MaxIdleConnsPerHost = capacity
	t.MaxConnsPerHost = capacity
	t.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// NewCompatible 创建兼容 OpenAI Chat Completions 子集的上游适配器。
func NewCompatible(client *http.Client, url, key string) *Compatible {
	return &Compatible{client: client, url: url, key: key}
}

// send 序列化请求并执行上游 POST；它统一设置认证和内容协商头，也统一净化上游状态错误。
func (p *Compatible) send(ctx context.Context, request chat.Request) (*http.Response, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, chat.NewError(500, "internal_error", "服务内部错误")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, chat.NewError(500, "internal_error", "服务内部错误")
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", "application/json")
	if request.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if resp.StatusCode == 429 {
			return nil, chat.NewError(429, "upstream_rate_limited", "大模型服务请求受限")
		}
		if resp.StatusCode == 400 || resp.StatusCode == 422 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(b, &envelope) == nil && envelope.Error.Code == "context_length_exceeded" {
				return nil, chat.NewError(400, "context_length_exceeded", "请求超出模型上下文限制")
			}
		}
		return nil, chat.NewError(502, "upstream_error", "大模型服务调用失败")
	}
	return resp, nil
}

// Complete 获取并校验完整 JSON 响应；大小上限避免上游异常响应占用过多内存。
func (p *Compatible) Complete(ctx context.Context, request chat.Request) (chat.Response, error) {
	request.Stream = false
	resp, err := p.send(ctx, request)
	if err != nil {
		return chat.Response{}, err
	}
	defer resp.Body.Close()
	if !contentType(resp.Header.Get("Content-Type"), "application/json") {
		return chat.Response{}, protocolError()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return chat.Response{}, transportError(ctx, err)
	}
	if len(body) > maxResponseBytes {
		return chat.Response{}, protocolError()
	}
	var result chat.Response
	if json.Unmarshal(body, &result) != nil || !validResponse(result) {
		return chat.Response{}, protocolError()
	}
	return result, nil
}

// Stream 建立 SSE 请求并返回惰性读取器；事件由后续 Next 调用逐条消费。
func (p *Compatible) Stream(ctx context.Context, request chat.Request) (chat.Stream, error) {
	request.Stream = true
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := p.send(streamCtx, request)
	if err != nil {
		cancel()
		return nil, err
	}
	if !contentType(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		resp.Body.Close()
		return nil, protocolError()
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	return &sseStream{body: resp.Body, scanner: scanner, ctx: streamCtx, cancel: cancel}, nil
}

type sseStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	done    bool
}

// Close 取消上游请求并关闭响应体；sync.Once 让并发或重复 Close 都安全。
func (s *sseStream) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.body.Close() })
	return err
}

// Next 读取并验证一个完整 SSE 事件；调用方 context 取消时会关闭网络体来打断阻塞读取。
// 这里没有额外 reader goroutine，避免每个事件流都产生后台 goroutine 和队列。
func (s *sseStream) Next(ctx context.Context) (chat.Event, error) {
	if s.done {
		return chat.Event{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		s.Close()
		return chat.Event{}, err
	}
	// Canceling the network request interrupts a blocked Scanner.Scan. No reader
	// goroutine or unbounded queue is created per event.
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	var data []string
	size := 0
	for s.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return chat.Event{}, err
		}
		line := s.scanner.Text()
		if line == "" {
			if len(data) == 0 {
				size = 0
				continue
			}
			payload := strings.Join(data, "\n")
			if payload == "[DONE]" {
				s.done = true
				return chat.Event{}, io.EOF
			}
			var raw struct {
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal([]byte(payload), &raw) != nil {
				return chat.Event{}, protocolError()
			}
			if len(raw.Error) > 0 && string(raw.Error) != "null" {
				return chat.Event{}, chat.NewError(502, "upstream_error", "大模型服务流式响应失败")
			}
			var event chat.Event
			if json.Unmarshal([]byte(payload), &event) != nil || !validEvent(event) {
				return chat.Event{}, protocolError()
			}
			return event, nil
		}
		size += len(line) + 1
		if size > maxEventBytes {
			return chat.Event{}, protocolError()
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		if field == "data" {
			data = append(data, value)
		}
	}
	if err := ctx.Err(); err != nil {
		return chat.Event{}, err
	}
	if err := s.ctx.Err(); err != nil {
		return chat.Event{}, err
	}
	// EOF without a complete [DONE] event is a protocol failure, including a
	// final data line with no terminating empty line.
	return chat.Event{}, protocolError()
}

// contentType 解析 MIME 类型后比较主类型，允许 Content-Type 附带 charset 等参数。
func contentType(value, want string) bool {
	media, _, err := mime.ParseMediaType(value)
	return err == nil && media == want
}

// validUsage 检查 token 统计非负且总数与输入、输出之和一致；nil 表示上游未提供统计。
func validUsage(u *chat.Usage) bool {
	return u == nil || (u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.TotalTokens >= 0 && u.TotalTokens >= u.PromptTokens && u.TotalTokens-u.PromptTokens == u.CompletionTokens)
}

// validResponse 校验完整响应的必需字段、choices 和结束原因，拒绝结构不完整的上游数据。
func validResponse(r chat.Response) bool {
	if r.ID == "" || r.Object != "chat.completion" || r.Model == "" || r.Created <= 0 || len(r.Choices) == 0 || !validUsage(r.Usage) {
		return false
	}
	for _, c := range r.Choices {
		if c.Index < 0 || c.Message.Role != "assistant" || c.FinishReason == nil || !supportedFinish(*c.FinishReason) {
			return false
		}
	}
	return true
}

// validEvent 校验单个流式事件；用量尾事件可不含 choices，但普通空事件无效。
func validEvent(e chat.Event) bool {
	if e.ID == "" || e.Object != "chat.completion.chunk" || e.Model == "" || e.Created <= 0 || !validUsage(e.Usage) {
		return false
	}
	if len(e.Choices) == 0 && e.Usage == nil {
		return false
	}
	for _, c := range e.Choices {
		if c.Index < 0 || (c.Delta.Role != "" && c.Delta.Role != "assistant") || (c.FinishReason != nil && !supportedFinish(*c.FinishReason)) {
			return false
		}
	}
	return true
}

// supportedFinish 判断结束原因是否属于本服务支持的协议取值。
func supportedFinish(reason string) bool {
	return reason == "stop" || reason == "length" || reason == "content_filter"
}

// protocolError 返回不包含上游正文的统一协议错误。
func protocolError() *chat.Error {
	return chat.NewError(502, "upstream_protocol_error", "大模型服务响应协议无效")
}

// transportError 优先保留 context 的取消原因，再把网络超时和其他连接错误映射为公开错误。
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return chat.NewError(504, "upstream_timeout", "大模型服务响应超时")
	}
	return chat.NewError(502, "upstream_error", "大模型服务连接失败")
}
