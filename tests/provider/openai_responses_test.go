package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/provider"
)

type tokenSource struct {
	token string
	err   error
}

func (s tokenSource) AccessToken(context.Context) (string, error) { return s.token, s.err }

type codexTokenSource struct{ tokenSource }

func (codexTokenSource) AccountID() string { return "account-one" }

func TestOpenAICodexUsesSubscriptionEndpointAndInstructions(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("ChatGPT-Account-Id") != "account-one" || r.Header.Get("OpenAI-Beta") != "responses=experimental" {
			t.Fatal("incorrect Codex endpoint or headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["instructions"] != "system prompt" || body["store"] != false || body["stream"] != true || len(body["input"].([]any)) != 2 {
			t.Fatalf("incorrect Codex body: %v", body)
		}
		if _, exists := body["max_output_tokens"]; exists {
			t.Fatal("unsupported max_output_tokens sent")
		}
		if body["temperature"] != float64(0) {
			t.Fatal("zero temperature lost")
		}
		events := strings.ReplaceAll(responseEvents, "response.completed", "response.done")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: r}, nil
	})}
	p := provider.NewOpenAICodex(client, codexTokenSource{tokenSource{token: "access"}})
	zero := 0.0
	response, err := p.Complete(context.Background(), chat.Request{Model: "codex", Temperature: &zero, Messages: []chat.Message{{Role: "system", Content: "system prompt"}, {Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].Message.Content != "hello world" {
		t.Fatalf("Codex completion: %#v", response)
	}
	limit := 12
	if _, err := p.Complete(context.Background(), chat.Request{Model: "codex", MaxTokens: &limit}); err == nil || chat.PublicError(err).Code != "unsupported_parameter" {
		t.Fatalf("unsupported limit: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const responseEvents = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"created_at\":1791590400}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"created_at\":1791590400,\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n"

func TestOpenAIResponsesStreamUsesOAuthAndMapsEvents(t *testing.T) {
	var called bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.String() != "https://api.openai.com/v1/responses" || r.Header.Get("Authorization") != "Bearer oauth-access" {
			t.Fatalf("unexpected upstream request: %s, authorization=%q", r.URL, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true || body["store"] != false || body["model"] != "gpt-test" {
			t.Fatalf("unexpected Responses API body: %v", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(responseEvents)), Request: r}, nil
	})}
	p := provider.NewOpenAIResponses(client, tokenSource{token: "oauth-access"})
	stream, err := p.Stream(context.Background(), chat.Request{Model: "gpt-test", Messages: []chat.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || len(first.Choices) != 1 || first.Choices[0].Delta.Content == nil || *first.Choices[0].Delta.Content != "hello" {
		t.Fatalf("unexpected first chunk: %#v, err=%v", first, err)
	}
	second, err := stream.Next(context.Background())
	if err != nil || second.Choices[0].Delta.Content == nil || *second.Choices[0].Delta.Content != " world" {
		t.Fatalf("unexpected second chunk: %#v, err=%v", second, err)
	}
	final, err := stream.Next(context.Background())
	if err != nil || final.Usage == nil || final.Usage.TotalTokens != 5 || final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected completion chunk: %#v, err=%v", final, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal event should end the stream, got %v", err)
	}
	if !called {
		t.Fatal("expected upstream request")
	}
}

func TestOpenAIResponsesCompleteAggregatesStream(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(responseEvents)), Request: r}, nil
	})}
	p := provider.NewOpenAIResponses(client, tokenSource{token: "oauth-access"})
	response, err := p.Complete(context.Background(), chat.Request{Model: "public-alias", Messages: []chat.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Object != "chat.completion" || response.Model != "public-alias" || response.Choices[0].Message.Content != "hello world" || response.Usage == nil || response.Usage.PromptTokens != 2 {
		t.Fatalf("unexpected completion response: %#v", response)
	}
}

func TestOpenAIResponsesRequiresOAuthAndVerifiedCompletion(t *testing.T) {
	p := provider.NewOpenAIResponses(http.DefaultClient, tokenSource{err: errors.New("not signed in")})
	if _, err := p.Complete(context.Background(), chat.Request{Model: "gpt-test"}); err == nil || chat.PublicError(err).Code != "oauth_login_required" {
		t.Fatalf("expected login-required error, got %v", err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		incomplete := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"created_at\":1791590400}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(incomplete)), Request: r}, nil
	})}
	p = provider.NewOpenAIResponses(client, tokenSource{token: "oauth-access"})
	if _, err := p.Complete(context.Background(), chat.Request{Model: "gpt-test"}); err == nil || chat.PublicError(err).Code != "upstream_protocol_error" {
		t.Fatalf("expected protocol error for missing completion event, got %v", err)
	}
}

func TestCodexSSEWithoutContentType(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseEvents)), Request: r}, nil
			})}
			p := provider.NewOpenAICodex(client, codexTokenSource{tokenSource{token: "access"}})
			request := chat.Request{Model: "codex", Messages: []chat.Message{{Role: "user", Content: "hi"}}}
			if !streaming {
				response, err := p.Complete(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				if response.Choices[0].Message.Content != "hello world" {
					t.Fatal("text was not aggregated")
				}
				return
			}
			stream, err := p.Stream(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			completed := false
			for {
				event, err := stream.Next(context.Background())
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(event.Choices) > 0 && event.Choices[0].FinishReason != nil {
					completed = true
				}
			}
			if !completed {
				t.Fatal("completion event missing")
			}
		})
	}
}

func TestCodexMissingHeaderDoesNotAcceptInvalidStreams(t *testing.T) {
	for _, tc := range []struct {
		name, media, body string
		codex             bool
	}{
		{"ordinary-oauth-stays-strict", "", responseEvents, false},
		{"explicit-html-rejected", "text/html", responseEvents, true},
		{"missing-header-html-rejected", "", "<html>not an event stream</html>", true},
		{"missing-header-json-rejected", "", "data: invalid JSON\n\n", true},
		{"missing-completion-rejected", "", strings.ReplaceAll(responseEvents, "response.completed", "response.unknown"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.media}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}
			p := provider.NewOpenAIResponses(client, tokenSource{token: "access"})
			if tc.codex {
				p = provider.NewOpenAICodex(client, codexTokenSource{tokenSource{token: "access"}})
			}
			if _, err := p.Complete(context.Background(), chat.Request{Model: "test"}); err == nil || chat.PublicError(err).Code != "upstream_protocol_error" {
				t.Fatalf("expected protocol failure, got %v", err)
			}
		})
	}
}
