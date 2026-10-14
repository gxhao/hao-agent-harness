package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hao-agent-harness/internal/config"
)

const validYAML = `server:
  http_addr: 127.0.0.1:8080
  metrics_addr: 127.0.0.1:9090
  max_request_bytes: 1048576
  max_concurrent_requests: 64
  request_timeout: 120s
  upstream_header_timeout: 30s
  stream_first_event_timeout: 30s
  stream_idle_timeout: 30s
  downstream_write_timeout: 15s
  shutdown_timeout: 30s
logging:
  level: info
  chat_log_dir: logs/requests
providers:
  openai:
    auth_type: openai_codex
    codex_token_file: credentials/codex.json
models:
  default:
    provider: openai
    upstream_model: gpt-6-luna
    supports_stream: true
    supports_temperature: false
    supports_max_tokens: false
`

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadYAMLConfigCenter(t *testing.T) {
	c, err := config.LoadFile(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "127.0.0.1:8080" || c.MaxConcurrent != 64 || c.Models.Models["default"].SupportsMaxTokens {
		t.Fatalf("unexpected loaded configuration: %#v", c)
	}
	if c.Models.Providers["openai"].CodexTokenFile != "credentials/codex.json" {
		t.Fatal("credential path was not loaded")
	}
}

func TestLoadAPIKeyFromYAML(t *testing.T) {
	data := strings.ReplaceAll(validYAML, "auth_type: openai_codex\n    codex_token_file: credentials/codex.json", "auth_type: api_key\n    chat_url: https://llm.example.com/v1/chat/completions\n    api_key: provider-secret")
	data = strings.ReplaceAll(data, "supports_temperature: false", "supports_temperature: true")
	data = strings.ReplaceAll(data, "supports_max_tokens: false", "supports_max_tokens: true\n    max_output_tokens: 4096")
	c, err := config.LoadFile(writeConfig(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if c.Models.Providers["openai"].APIKey != "provider-secret" {
		t.Fatal("provider API key was not loaded")
	}
}

func TestLoadRejectsBadConfigWithoutLeakingSecrets(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"json legacy format", `{"providers":{}}`},
		{"unknown field", strings.Replace(validYAML, "level: info", "level: info\n  unknown: true", 1)},
		{"invalid duration", strings.Replace(validYAML, "request_timeout: 120s", "request_timeout: soon", 1)},
		{"unknown provider", strings.Replace(validYAML, "provider: openai", "provider: absent", 1)},
		{"invalid URL", strings.Replace(validYAML, "auth_type: openai_codex\n    codex_token_file: credentials/codex.json", "auth_type: api_key\n    chat_url: http://example.com/v1/chat/completions\n    api_key: never-print-this-secret", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadFile(writeConfig(t, tc.data))
			if err == nil {
				t.Fatal("invalid configuration was accepted")
			}
			if strings.Contains(err.Error(), "never-print-this-secret") {
				t.Fatal("error leaked provider secret")
			}
		})
	}
}

func TestCanonicalConfigIsYAMLAndValid(t *testing.T) {
	c, err := config.LoadFile("../../configs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Models.Providers["openai"].AuthMethod() != "openai_codex" {
		t.Fatal("canonical provider is not Codex")
	}
}

func TestWatchFileAppliesOnlyValidChangedConfig(t *testing.T) {
	path := writeConfig(t, validYAML)
	initial, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan config.Config, 4)
	errors := make(chan error, 4)
	go config.WatchFile(ctx, path, initial, 20*time.Millisecond, func(c config.Config) error { updates <- c; return nil }, func(err error) { errors <- err })
	time.Sleep(40 * time.Millisecond) // 让 watcher 先记录原始内容，再模拟文件更新。
	updated := strings.Replace(validYAML, "gpt-6-luna", "updated-model", 1)
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-updates:
		if c.Models.Models["default"].UpstreamModel != "updated-model" {
			t.Fatal("watcher applied wrong snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not apply updated configuration")
	}
	if err := os.WriteFile(path, []byte("server: ["), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errors:
	case <-time.After(time.Second):
		t.Fatal("watcher did not report invalid config")
	}
	select {
	case <-updates:
		t.Fatal("watcher applied invalid configuration")
	case <-time.After(80 * time.Millisecond):
	}
}
