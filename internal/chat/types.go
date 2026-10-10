// Package chat defines the provider-independent chat contract.
package chat

import (
	"context"
	"errors"
	"net"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

type Response struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Delta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Event struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// Provider 是上游模型适配器的契约；Go 通过方法集隐式实现接口，不需要 implements 声明。
type Provider interface {
	Complete(context.Context, Request) (Response, error)
	Stream(context.Context, Request) (Stream, error)
}

// Stream 表示按顺序读取的模型事件流；Close 必须能解除正在等待的 Next。
// 只有确认收到正常结束标记时 Next 才返回 io.EOF，连接意外断开应返回错误。
type Stream interface {
	Next(context.Context) (Event, error)
	Close() error
}

// Error 只保存可以返回给客户端的净化信息，不能把上游响应正文或密钥放进来。
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error 让 *chat.Error 满足标准库 error 接口，错误字符串仅使用稳定的错误码。
func (e *Error) Error() string { return e.Code }

// NewError 创建可安全公开给调用方的领域错误。
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// PublicError 将 context、网络或内部错误归一化为不会泄漏上游细节的 HTTP 错误。
func PublicError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(504, "upstream_timeout", "大模型服务响应超时")
	}
	if errors.Is(err, context.Canceled) {
		return NewError(503, "request_canceled", "请求已取消")
	}
	var public *Error
	if errors.As(err, &public) {
		return public
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return NewError(504, "upstream_timeout", "大模型服务响应超时")
	}
	return NewError(500, "internal_error", "服务内部错误")
}
