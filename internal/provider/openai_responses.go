package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"hao-agent-harness/internal/chat"
)

const openAIResponsesURL = "https://api.openai.com/v1/responses"

// OAuthTokenSource 定义访问令牌提供者；实现方负责在令牌快过期时刷新。
type OAuthTokenSource interface {
	AccessToken(context.Context) (string, error)
}

// OpenAIResponses 将 OAuth 授权的 Responses API 事件流转换为本服务的聊天接口契约。
type OpenAIResponses struct {
	client *http.Client
	tokens OAuthTokenSource
	codex  interface{ AccountID() string }
}

// NewOpenAICodex sends subscription credentials to the Codex backend used by pi.
func NewOpenAICodex(client *http.Client, tokens interface {
	OAuthTokenSource
	AccountID() string
}) *OpenAIResponses {
	return &OpenAIResponses{client: client, tokens: tokens, codex: tokens}
}

// NewOpenAIResponses 创建固定请求 OpenAI 官方 Responses API 地址的 provider。
func NewOpenAIResponses(client *http.Client, tokens OAuthTokenSource) *OpenAIResponses {
	return &OpenAIResponses{client: client, tokens: tokens}
}

type responsesRequest struct {
	Instructions    *string        `json:"instructions,omitempty"`
	Model           string         `json:"model"`
	Input           []chat.Message `json:"input"`
	Stream          bool           `json:"stream"`
	Store           bool           `json:"store"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
}

type responseEnvelope struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	Usage     *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

// send 获取当前账号的访问令牌，并创建不存储内容的 Responses API 流式请求。
func (p *OpenAIResponses) send(ctx context.Context, request chat.Request) (*http.Response, error) {
	token, err := p.tokens.AccessToken(ctx)
	if err != nil {
		return nil, chat.NewError(503, "oauth_login_required", "请先完成 OpenAI 账号授权")
	}
	payload := responsesRequest{
		Model: request.Model, Input: request.Messages, Stream: true, Store: false,
		Temperature: request.Temperature, MaxOutputTokens: request.MaxTokens,
	}
	endpoint := openAIResponsesURL
	if p.codex != nil {
		if request.MaxTokens != nil {
			return nil, chat.NewError(400, "unsupported_parameter", "Codex 不支持 max_tokens")
		}
		endpoint = "https://chatgpt.com/backend-api/codex/responses"
		instructions := []string{}
		payload.Input = make([]chat.Message, 0, len(request.Messages))
		for _, message := range request.Messages {
			if message.Role == "system" {
				instructions = append(instructions, message.Content)
			} else {
				payload.Input = append(payload.Input, message)
			}
		}
		text := strings.Join(instructions, "\n\n")
		if text == "" {
			text = "You are a helpful assistant."
		}
		payload.Instructions = &text
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, chat.NewError(500, "internal_error", "服务内部错误")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, chat.NewError(500, "internal_error", "服务内部错误")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.codex != nil {
		account := p.codex.AccountID()
		if account == "" || strings.ContainsAny(account, "\r\n") {
			return nil, chat.NewError(503, "oauth_login_required", "请使用 --login 重新登录 OpenAI")
		}
		req.Header.Set("ChatGPT-Account-Id", account)
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("Originator", "hao-agent-harness")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, chat.NewError(429, "upstream_rate_limited", "大模型服务请求受限")
		}
		return nil, chat.NewError(502, "upstream_error", "大模型服务调用失败")
	}
	// Codex can return a valid SSE body without Content-Type. pi consumes that
	// stream directly; still validate every event and require a completion event.
	mediaType := resp.Header.Get("Content-Type")
	if !contentType(mediaType, "text/event-stream") && !(p.codex != nil && mediaType == "") {
		resp.Body.Close()
		return nil, protocolError()
	}
	return resp, nil
}

// Complete 读取 Responses 流中的文本增量并聚合成一个普通聊天响应。
func (p *OpenAIResponses) Complete(ctx context.Context, request chat.Request) (chat.Response, error) {
	stream, err := p.Stream(ctx, request)
	if err != nil {
		return chat.Response{}, err
	}
	defer stream.Close()
	var content strings.Builder
	var final chat.Event
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return chat.Response{}, err
		}
		if len(event.Choices) > 0 && event.Choices[0].Delta.Content != nil {
			content.WriteString(*event.Choices[0].Delta.Content)
		}
		if event.Usage != nil || len(event.Choices) > 0 && event.Choices[0].FinishReason != nil {
			final = event
		}
	}
	if final.ID == "" || final.Created <= 0 {
		return chat.Response{}, protocolError()
	}
	finish := "stop"
	if len(final.Choices) > 0 && final.Choices[0].FinishReason != nil {
		finish = *final.Choices[0].FinishReason
	}
	return chat.Response{ID: final.ID, Object: "chat.completion", Created: final.Created, Model: request.Model,
		Choices: []chat.Choice{{Index: 0, Message: chat.Message{Role: "assistant", Content: content.String()}, FinishReason: &finish}}, Usage: final.Usage}, nil
}

// Stream 打开 Responses 事件流，并通过通用 chat.Stream 接口逐条返回事件。
func (p *OpenAIResponses) Stream(ctx context.Context, request chat.Request) (chat.Stream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := p.send(streamCtx, request)
	if err != nil {
		cancel()
		return nil, err
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	return &responsesStream{body: resp.Body, scanner: scanner, ctx: streamCtx, cancel: cancel}, nil
}

type responsesStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	done    bool
	id      string
	model   string
	created int64
}

// Close 取消上游请求并关闭响应体；sync.Once 确保重复调用也只释放一次。
func (s *responsesStream) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.body.Close() })
	return err
}

type responseStreamEvent struct {
	Type     string           `json:"type"`
	Delta    string           `json:"delta"`
	Response responseEnvelope `json:"response"`
	Error    *struct {
		Code string `json:"code"`
	} `json:"error"`
}

// Next 解析并校验 SSE 事件，返回文本增量或结束统计；上游必须发送 completed 才算正常完成。
func (s *responsesStream) Next(ctx context.Context) (chat.Event, error) {
	if s.done {
		return chat.Event{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		s.Close()
		return chat.Event{}, err
	}
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
			data = nil
			size = 0
			var event responseStreamEvent
			if json.Unmarshal([]byte(payload), &event) != nil {
				return chat.Event{}, protocolError()
			}
			switch event.Type {
			case "response.created", "response.in_progress":
				s.setResponse(event.Response)
			case "response.output_text.delta":
				s.setResponse(event.Response)
				if s.id == "" || event.Delta == "" {
					if event.Delta == "" {
						continue
					}
					return chat.Event{}, protocolError()
				}
				return textEvent(s.id, s.model, s.created, event.Delta, nil, nil), nil
			case "response.completed", "response.incomplete", "response.done":
				s.setResponse(event.Response)
				if s.id == "" || s.created <= 0 {
					return chat.Event{}, protocolError()
				}
				finish := "stop"
				if event.Response.Status == "failed" || event.Response.Status == "cancelled" {
					return chat.Event{}, chat.NewError(502, "upstream_error", "大模型服务流式响应失败")
				}
				if event.Type == "response.incomplete" || event.Response.Status == "incomplete" {
					finish = "length"
				}
				s.done = true
				usage := responseUsage(event.Response.Usage)
				return textEvent(s.id, s.model, s.created, "", &finish, usage), nil
			case "response.failed", "error":
				return chat.Event{}, chat.NewError(502, "upstream_error", "大模型服务流式响应失败")
			default:
				// Refusal, reasoning and lifecycle events do not carry user-visible output text.
			}
			continue
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
			continue
		}
		if field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := ctx.Err(); err != nil {
		return chat.Event{}, err
	}
	if err := s.ctx.Err(); err != nil {
		return chat.Event{}, err
	}
	return chat.Event{}, protocolError()
}

// setResponse 从 Responses 生命周期事件中更新响应 ID、模型名和创建时间。
func (s *responsesStream) setResponse(response responseEnvelope) {
	if response.ID != "" {
		s.id = response.ID
	}
	if response.Model != "" {
		s.model = response.Model
	}
	if response.CreatedAt > 0 {
		s.created = response.CreatedAt
	}
}

// responseUsage 将 Responses API 的 token 字段名映射成内部统一的用量结构。
func responseUsage(usage *struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}) *chat.Usage {
	if usage == nil {
		return nil
	}
	return &chat.Usage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
}

// textEvent 将一个文本增量或流结束统计封装成单选项的兼容聊天 chunk。
func textEvent(id, model string, created int64, delta string, finish *string, usage *chat.Usage) chat.Event {
	choices := []chat.ChunkChoice{}
	if delta != "" || finish != nil {
		var content *string
		if delta != "" {
			content = &delta
		}
		choices = append(choices, chat.ChunkChoice{Index: 0, Delta: chat.Delta{Content: content}, FinishReason: finish})
	}
	return chat.Event{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: choices, Usage: usage}
}
