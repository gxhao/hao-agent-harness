package main

import (
	"io"
	"testing"
)

func TestLoginArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--login"}, {"--login", "openai"}, {"--login=openai-codex"},
		{"--login", "openai", "--no-browser", "--login-timeout", "1m"},
	} {
		options, err := parseOptions(args, io.Discard)
		if err != nil || options.login == "" {
			t.Fatalf("%v: %#v %v", args, options, err)
		}
	}
	for _, args := range [][]string{
		{"--login", "anthropic"}, {"--login", "openai", "extra"}, {"--no-browser"},
		{"--login-timeout", "1m"}, {"--login", "--login-timeout", "0s"}, {"--unknown"},
	} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if options, err := parseOptions(nil, io.Discard); err != nil || options.login != "" {
		t.Fatalf("default: %#v %v", options, err)
	}
}
