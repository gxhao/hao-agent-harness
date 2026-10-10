package config_test

import (
	"hao-agent-harness/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validModels = `{"providers":{"primary":{"chat_url":"https://llm.example.com/v1/chat/completions","api_key_env":"UPSTREAM_API_KEY_PRIMARY"}},"models":{"default":{"provider":"primary","upstream_model":"test-model","supports_stream":true,"supports_temperature":true,"supports_max_tokens":true,"max_output_tokens":4096}}}`

func prepare(t *testing.T, modelJSON string) {
	t.Helper()
	for name, value := range map[string]string{
		"HTTP_ADDR": "127.0.0.1:8080", "METRICS_ADDR": "127.0.0.1:9090", "LOG_LEVEL": "info",
		"CLIENT_API_KEYS": "client-old,client-new", "UPSTREAM_API_KEY_PRIMARY": "provider-secret",
		"MAX_REQUEST_BYTES": "1048576", "MAX_CONCURRENT_REQUESTS": "64", "REQUEST_TIMEOUT": "120s",
		"UPSTREAM_HEADER_TIMEOUT": "30s", "STREAM_FIRST_EVENT_TIMEOUT": "30s", "STREAM_IDLE_TIMEOUT": "30s",
		"DOWNSTREAM_WRITE_TIMEOUT": "15s", "SHUTDOWN_TIMEOUT": "30s",
	} {
		t.Setenv(name, value)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(modelJSON), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_CONFIG_PATH", path)
}

func TestLoad(t *testing.T) {
	prepare(t, validModels)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ClientKeys) != 2 || c.Models.Providers["primary"].APIKey != "provider-secret" || c.MaxConcurrent != 64 {
		t.Fatalf("unexpected loaded configuration")
	}
	// Empty metrics address intentionally disables the private listener.
	t.Setenv("METRICS_ADDR", "")
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOpenAIOAuthProviderWithoutAPIKey(t *testing.T) {
	oauthModels := `{"providers":{"openai":{"auth_type":"openai_oauth"}},"models":{"default":{"provider":"openai","upstream_model":"account-model","supports_stream":true,"supports_temperature":true,"supports_max_tokens":true,"max_output_tokens":4096}}}`
	prepare(t, oauthModels)
	t.Setenv("UPSTREAM_API_KEY_PRIMARY", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Models.Providers["openai"].AuthType != "openai_oauth" {
		t.Fatalf("auth type = %q", c.Models.Providers["openai"].AuthType)
	}
}

func TestInvalidConfigurationDoesNotExposeSecrets(t *testing.T) {
	for _, tc := range []struct{ name, env, value, json string }{
		{name: "missing clients", env: "CLIENT_API_KEYS", value: ""},
		{name: "missing provider key", env: "UPSTREAM_API_KEY_PRIMARY", value: ""},
		{name: "negative timeout", env: "REQUEST_TIMEOUT", value: "-1s"},
		{name: "bad duration", env: "STREAM_IDLE_TIMEOUT", value: "invalid"},
		{name: "bad capacity", env: "MAX_CONCURRENT_REQUESTS", value: "0"},
		{name: "huge capacity", env: "MAX_CONCURRENT_REQUESTS", value: "100001"},
		{name: "bad size", env: "MAX_REQUEST_BYTES", value: "67108865"},
		{name: "bad log", env: "LOG_LEVEL", value: "verbose"},
		{name: "same listener", env: "METRICS_ADDR", value: "127.0.0.1:8080"},
		{name: "plain HTTP", json: strings.ReplaceAll(validModels, "https://", "http://")},
		{name: "URL credentials", json: strings.ReplaceAll(validModels, "https://llm", "https://provider-secret@llm")},
		{name: "URL query", json: strings.ReplaceAll(validModels, "/v1/chat/completions", "/v1/chat/completions?key=provider-secret")},
		{name: "unknown provider", json: strings.ReplaceAll(validModels, `"provider":"primary"`, `"provider":"missing"`)},
		{name: "unknown field", json: strings.ReplaceAll(validModels, `"max_output_tokens":4096`, `"max_output_tokens":4096,"secret":"provider-secret"`)},
		{name: "trailing object", json: validModels + `{}`},
		{name: "null", json: `null`},
		{name: "empty", json: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.json
			if input == "" {
				input = validModels
			}
			prepare(t, input)
			if tc.env != "" {
				t.Setenv(tc.env, tc.value)
			}
			_, err := config.Load()
			if err == nil {
				t.Fatal("expected invalid configuration")
			}
			if strings.Contains(err.Error(), "provider-secret") {
				t.Fatal("configuration error leaked a secret")
			}
		})
	}
}
