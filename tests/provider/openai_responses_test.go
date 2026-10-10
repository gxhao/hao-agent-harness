package provider_test

import (
	"context"
	"encoding/json"
	"errors"
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
