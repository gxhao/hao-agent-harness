package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/service"
)

type waitingProvider struct{}

func (waitingProvider) Complete(ctx context.Context, _ chat.Request) (chat.Response, error) {
	<-ctx.Done()
	return chat.Response{}, ctx.Err()
}
func (waitingProvider) Stream(context.Context, chat.Request) (chat.Stream, error) { panic("not used") }

func serviceFixture(root context.Context, timeout time.Duration) *service.Service {
	return service.New(root, map[string]config.Model{"default": {Provider: "primary", UpstreamModel: "real", MaxOutputTokens: 100}}, map[string]chat.Provider{"primary": waitingProvider{}}, 1, timeout)
}

func TestModelCapabilities(t *testing.T) {
	s := serviceFixture(context.Background(), time.Second)
	zero := 0.0
	one := 1
	for _, req := range []chat.Request{
		{Model: "default", Messages: []chat.Message{{Role: "user", Content: "hello"}}, Stream: true},
		{Model: "default", Messages: []chat.Message{{Role: "user", Content: "hello"}}, Temperature: &zero},
		{Model: "default", Messages: []chat.Message{{Role: "user", Content: "hello"}}, MaxTokens: &one},
		{Model: "default", Messages: []chat.Message{{Role: "user", Content: "  "}}},
		{Model: "default", Messages: make([]chat.Message, 201)},
	} {
		if session, err := s.Open(context.Background(), req); err == nil {
			session.Close()
			t.Fatal("unsupported capability or invalid request accepted")
		}
	}
}

func TestSessionCancellationAndRelease(t *testing.T) {
	for _, tc := range []string{"deadline", "root cancellation", "client cancellation"} {
		t.Run(tc, func(t *testing.T) {
			root, rootCancel := context.WithCancel(context.Background())
			defer rootCancel()
			client, clientCancel := context.WithCancel(context.Background())
			defer clientCancel()
			timeout := time.Second
			if tc == "deadline" {
				timeout = 30 * time.Millisecond
			}
			s := serviceFixture(root, timeout)
			req := chat.Request{Model: "default", Messages: []chat.Message{{Role: "user", Content: "hello"}}}
			session, err := s.Open(client, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Open(client, req); err == nil || chat.PublicError(err).Status != 429 {
				t.Fatal("capacity not enforced")
			}
			done := make(chan error, 1)
			go func() { _, err := session.Complete(); done <- err }()
			if tc == "root cancellation" {
				rootCancel()
			}
			if tc == "client cancellation" {
				clientCancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("unexpected cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request did not cancel")
			}
			session.Close()
			session.Close()
			released, err := s.Open(context.Background(), req)
			if tc == "root cancellation" {
				if err == nil || chat.PublicError(err).Status != 503 {
					t.Fatal("root-canceled service should be unavailable after releasing capacity")
				}
			} else if err != nil {
				t.Fatal("permit not released or Close not idempotent")
			} else {
				released.Close()
			}
		})
	}
}
