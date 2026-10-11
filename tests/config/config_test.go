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

// 模拟入口先加载 .env 再加载配置，确认客户端令牌和供应商密钥进入各自字段。
func TestLoadWithDotEnvCredentials(t *testing.T) {
	prepare(t, validModels)
	// prepare 已通过 t.Setenv 注册环境恢复，清除后可验证文件补充的行为。
	for _, name := range []string{"CLIENT_API_KEYS", "UPSTREAM_API_KEY_PRIMARY"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("CLIENT_API_KEYS=file-client\nUPSTREAM_API_KEY_PRIMARY=file-provider\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ClientKeys) != 1 || c.ClientKeys[0] != "file-client" || c.Models.Providers["primary"].APIKey != "file-provider" {
		t.Fatal("dotenv credentials were not loaded into their separate configuration fields")
	}
}

func TestChatLogDirectory(t *testing.T) {
	prepare(t, validModels)
	dir := t.TempDir()
	t.Setenv("CHAT_LOG_DIR", dir)
	c, err := config.Load()
	if err != nil || c.ChatLogDir != dir {
		t.Fatalf("custom log directory not loaded: %v", err)
	}
	t.Setenv("CHAT_LOG_DIR", "")
	c, err = config.Load()
	if err != nil || c.ChatLogDir != "" {
		t.Fatalf("empty log directory must disable logging: %v", err)
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

func TestLoadOpenAICodexProvider(t *testing.T) {
	models := `{"providers":{"openai":{"auth_type":"openai_codex"}},"models":{"default":{"provider":"openai","upstream_model":"codex-model","supports_stream":true,"supports_max_tokens":false,"max_output_tokens":4096}}}`
	prepare(t, models)
	t.Setenv("UPSTREAM_API_KEY_PRIMARY", "")
	t.Setenv("OPENAI_CODEX_TOKEN_FILE", "custom-codex.json")
	c, err := config.Load()
	if err != nil || c.OpenAICodexTokenFile != "custom-codex.json" {
		t.Fatalf("Codex config: %#v, %v", c, err)
	}
	for _, invalid := range []string{
		strings.Replace(models, `"auth_type":"openai_codex"`, `"auth_type":"openai_codex","chat_url":"https://example.com"`, 1),
		strings.Replace(models, `"supports_max_tokens":false`, `"supports_max_tokens":true`, 1),
	} {
		prepare(t, invalid)
		if _, err := config.Load(); err == nil {
			t.Fatal("accepted unsupported Codex configuration")
		}
	}
}

func TestLoadDefaultsToLocalModelsAndHonorsExplicitPath(t *testing.T) {
	models := `{"providers":{"openai":{"auth_type":"openai_codex"}},"models":{"default":{"provider":"openai","upstream_model":"test-codex","supports_stream":true,"max_output_tokens":4096}}}`
	prepare(t, models)
	explicitPath := os.Getenv("MODEL_CONFIG_PATH")
	t.Chdir(t.TempDir())
	if err := os.Mkdir("configs", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("configs/models.json", []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("MODEL_CONFIG_PATH"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load()
	if err != nil || c.Models.Providers["openai"].AuthType != "openai_codex" {
		t.Fatalf("default local config: %v", err)
	}
	t.Setenv("MODEL_CONFIG_PATH", explicitPath)
	if _, err := config.Load(); err != nil {
		t.Fatalf("explicit config: %v", err)
	}
	t.Setenv("MODEL_CONFIG_PATH", "missing.json")
	if _, err := config.Load(); err == nil {
		t.Fatal("silently ignored explicit missing configuration")
	}
	t.Setenv("MODEL_CONFIG_PATH", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("accepted explicitly empty configuration path")
	}
	os.Unsetenv("MODEL_CONFIG_PATH")
	t.Setenv("CLIENT_API_KEYS", "")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "CLIENT_API_KEYS") {
		t.Fatalf("missing client authentication: %v", err)
	}
	t.Setenv("CLIENT_API_KEYS", "client-secret")
	if err := os.Remove("configs/models.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "create configs/models.json") {
		t.Fatalf("missing default config: %v", err)
	}
}

func TestMinimalModelDefaultsAndExplicitOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, provider, fields         string
		stream, temperature, maxTokens bool
		limit                          int
	}{
		{"codex", `{"auth_type":"openai_codex"}`, "", true, false, false, 0},
		{"oauth", `{"auth_type":"openai_oauth"}`, "", true, true, true, 4096},
		{"api-key", `{"chat_url":"https://llm.example.com/v1/chat/completions","api_key_env":"UPSTREAM_API_KEY_PRIMARY"}`, "", true, true, true, 4096},
		{"explicit-false", `{"auth_type":"openai_oauth"}`, `,"supports_stream":false,"supports_temperature":false,"supports_max_tokens":false`, false, false, false, 0},
		{"explicit-limit", `{"auth_type":"openai_oauth"}`, `,"max_output_tokens":1024`, true, true, true, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepare(t, `{"providers":{"p":`+tc.provider+`},"models":{"default":{"provider":"p","upstream_model":"test-model"`+tc.fields+`}}}`)
			c, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			m := c.Models.Models["default"]
			if m.SupportsStream != tc.stream || m.SupportsTemperature != tc.temperature || m.SupportsMaxTokens != tc.maxTokens || m.MaxOutputTokens != tc.limit {
				t.Fatalf("incorrect defaults: %#v", m)
			}
		})
	}
}

func TestCanonicalConfigurationLoadsWithoutProviderAPIKey(t *testing.T) {
	b, err := os.ReadFile("../../configs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	prepare(t, string(b))
	t.Setenv("UPSTREAM_API_KEY_PRIMARY", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Models.Providers["openai"].AuthMethod() != "openai_codex" || c.Models.Models["default"].SupportsMaxTokens {
		t.Fatal("canonical config must use Codex defaults")
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
		{name: "zero max token limit", json: strings.ReplaceAll(validModels, `"max_output_tokens":4096`, `"max_output_tokens":0`)},
		{name: "negative max token limit", json: strings.ReplaceAll(validModels, `"max_output_tokens":4096`, `"max_output_tokens":-1`)},
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
