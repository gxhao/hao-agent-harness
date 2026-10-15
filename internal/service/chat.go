// Package service owns routing, admission and request lifetimes.
package service

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/config"
)

type Service struct {
	root      context.Context
	models    map[string]config.Model
	providers map[string]chat.Provider
	permits   chan struct{}
	timeout   time.Duration
	mu        sync.Mutex
	draining  bool
}

// New 注入模型、provider 和根 context；带缓冲 channel 用作并发许可池，限制同时处理的请求数。
func New(root context.Context, models map[string]config.Model, providers map[string]chat.Provider, capacity int, timeout time.Duration) *Service {
	return &Service{root: root, models: models, providers: providers, permits: make(chan struct{}, capacity), timeout: timeout}
}

// Ready 在互斥锁保护下读取停止状态，并检查根 context 是否仍有效。
func (s *Service) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.draining && s.root.Err() == nil
}

// Drain 标记服务进入排空状态，让新请求失败而已有请求继续按关闭流程完成。
func (s *Service) Drain() { s.mu.Lock(); s.draining = true; s.mu.Unlock() }

// Models 返回排序后的公开模型别名，避免直接暴露 map 的随机遍历顺序。
func (s *Service) Models() []string {
	names := make([]string, 0, len(s.models))
	for name := range s.models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Open 校验请求、解析模型别名并申请并发许可；Session.Close 会负责归还许可。
// 派生 context 同时受调用方取消、服务退出和单请求超时约束。
func (s *Service) Open(ctx context.Context, req chat.Request) (*Session, error) {
	m, ok := s.models[req.Model]
	alias := req.Model
	metricModel := alias
	if !ok {
		// OpenAI 兼容 API 通常直接传上游模型名。仅在配置了 default 模型且
		// 它明确使用 openai provider 时，将未配置的名称作为上游模型透传。
		// provider 仍来自本地配置，客户端不能借此指定任意 URL 或凭证。
		defaultModel, hasDefault := s.models["default"]
		_, hasOpenAI := s.providers["openai"]
		if !hasDefault || defaultModel.Provider != "openai" || !hasOpenAI || req.Model == "" {
			return nil, chat.NewError(400, "unknown_model", "模型不存在")
		}
		m = defaultModel
		m.UpstreamModel = req.Model
		metricModel = "default"
	}
	if err := validate(req, m); err != nil {
		return nil, err
	}
	p, ok := s.providers[m.Provider]
	if !ok {
		return nil, chat.NewError(500, "internal_error", "服务内部错误")
	}
	s.mu.Lock()
	if s.draining || s.root.Err() != nil {
		s.mu.Unlock()
		return nil, chat.NewError(503, "service_unavailable", "服务正在停止")
	}
	select {
	case s.permits <- struct{}{}:
	default:
		s.mu.Unlock()
		return nil, chat.NewError(429, "capacity_exceeded", "服务并发容量已满")
	}
	s.mu.Unlock()
	requestCtx, cancel := context.WithTimeout(ctx, s.timeout)
	stop := context.AfterFunc(s.root, cancel)
	req.Model = m.UpstreamModel
	return &Session{ctx: requestCtx, cancel: cancel, stop: stop, release: func() { <-s.permits }, provider: p, request: req, Alias: alias, MetricModel: metricModel, ProviderName: m.Provider}, nil
}

// validate 检查消息格式、数值范围及所选模型声明支持的参数。
func validate(req chat.Request, m config.Model) error {
	bad := func() error {
		return chat.NewError(400, "invalid_request", "请求参数无效或模型不支持该参数")
	}
	if len(req.Messages) == 0 || len(req.Messages) > 200 {
		return bad()
	}
	for _, msg := range req.Messages {
		if msg.Role != "system" && msg.Role != "user" && msg.Role != "assistant" {
			return bad()
		}
		if strings.TrimSpace(msg.Content) == "" {
			return bad()
		}
	}
	if req.Stream && !m.SupportsStream {
		return bad()
	}
	if req.Temperature != nil {
		v := *req.Temperature
		if !m.SupportsTemperature || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 2 {
			return bad()
		}
	}
	if req.MaxTokens != nil && (!m.SupportsMaxTokens || *req.MaxTokens <= 0 || *req.MaxTokens > m.MaxOutputTokens) {
		return bad()
	}
	return nil
}

type Session struct {
	ctx          context.Context
	cancel       context.CancelFunc
	stop         func() bool
	release      func()
	once         sync.Once
	provider     chat.Provider
	request      chat.Request
	Alias        string
	MetricModel  string
	ProviderName string
}

// Context 返回本次调用的生命周期 context，供 HTTP 层限制写入和上游读取时长。
func (s *Session) Context() context.Context { return s.ctx }

// Complete 发起非流式上游请求，并把内部上游模型名改回客户端使用的别名。
func (s *Session) Complete() (chat.Response, error) {
	response, err := s.provider.Complete(s.ctx, s.request)
	if err == nil {
		response.Model = s.Alias
	}
	return response, err
}

// Stream 打开上游事件流；返回的 Stream 由调用方顺序读取并最终关闭。
func (s *Session) Stream() (chat.Stream, error) { return s.provider.Stream(s.ctx, s.request) }

// Close 幂等地取消 context 并归还并发许可；sync.Once 防止多个退出路径重复释放。
func (s *Session) Close() {
	s.once.Do(func() { s.stop(); s.cancel(); s.release() })
}
