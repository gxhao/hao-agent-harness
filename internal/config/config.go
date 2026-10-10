// Package config loads and validates startup configuration without exposing secrets.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Provider struct {
	AuthType  string `json:"auth_type,omitempty"`
	ChatURL   string `json:"chat_url"`
	APIKeyEnv string `json:"api_key_env"`
	APIKey    string `json:"-"`
}

type Model struct {
	Provider            string `json:"provider"`
	UpstreamModel       string `json:"upstream_model"`
	SupportsStream      bool   `json:"supports_stream"`
	SupportsTemperature bool   `json:"supports_temperature"`
	SupportsMaxTokens   bool   `json:"supports_max_tokens"`
	MaxOutputTokens     int    `json:"max_output_tokens"`
}

type Models struct {
	Providers map[string]Provider `json:"providers"`
	Models    map[string]Model    `json:"models"`
}

type Config struct {
	HTTPAddr          string
	MetricsAddr       string
	ClientKeys        []string
	Models            Models
	MaxRequestBytes   int64
	MaxConcurrent     int
	RequestTimeout    time.Duration
	HeaderTimeout     time.Duration
	FirstEventTimeout time.Duration
	StreamIdleTimeout time.Duration
	WriteTimeout      time.Duration
	ShutdownTimeout   time.Duration
	LogLevel          string
	OpenAITokenFile   string
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Load 从环境变量和模型 JSON 文件构造配置；密钥只从指定环境变量读取，不写入日志或错误文本。
func Load() (Config, error) {
	c := Config{HTTPAddr: env("HTTP_ADDR", "127.0.0.1:8080"), MetricsAddr: env("METRICS_ADDR", "127.0.0.1:9090"), LogLevel: env("LOG_LEVEL", "info"), OpenAITokenFile: os.Getenv("OPENAI_OAUTH_TOKEN_FILE")}
	for _, key := range strings.Split(os.Getenv("CLIENT_API_KEYS"), ",") {
		key = strings.TrimSpace(key)
		if key != "" {
			c.ClientKeys = append(c.ClientKeys, key)
		}
	}
	var err error
	if c.MaxRequestBytes, err = integer("MAX_REQUEST_BYTES", 1<<20); err != nil {
		return c, err
	}
	maxConcurrent, err := integer("MAX_CONCURRENT_REQUESTS", 64)
	if err != nil {
		return c, err
	}
	if maxConcurrent > 100000 {
		return c, fmt.Errorf("MAX_CONCURRENT_REQUESTS must be <= 100000")
	}
	c.MaxConcurrent = int(maxConcurrent)
	for _, spec := range []struct {
		name     string
		fallback time.Duration
		dest     *time.Duration
	}{
		{"REQUEST_TIMEOUT", 120 * time.Second, &c.RequestTimeout},
		{"UPSTREAM_HEADER_TIMEOUT", 30 * time.Second, &c.HeaderTimeout},
		{"STREAM_FIRST_EVENT_TIMEOUT", 30 * time.Second, &c.FirstEventTimeout},
		{"STREAM_IDLE_TIMEOUT", 30 * time.Second, &c.StreamIdleTimeout},
		{"DOWNSTREAM_WRITE_TIMEOUT", 15 * time.Second, &c.WriteTimeout},
		{"SHUTDOWN_TIMEOUT", 30 * time.Second, &c.ShutdownTimeout},
	} {
		*spec.dest, err = time.ParseDuration(env(spec.name, spec.fallback.String()))
		if err != nil || *spec.dest <= 0 {
			return c, fmt.Errorf("%s must be a positive duration", spec.name)
		}
	}
	path := os.Getenv("MODEL_CONFIG_PATH")
	if path == "" {
		return c, fmt.Errorf("MODEL_CONFIG_PATH is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("cannot open model configuration")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return c, fmt.Errorf("model configuration exceeds limit or cannot be read")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c.Models); err != nil {
		return c, fmt.Errorf("invalid model configuration JSON")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("model configuration must contain one JSON object")
	}
	for name, p := range c.Models.Providers {
		if p.APIKeyEnv != "" {
			p.APIKey = os.Getenv(p.APIKeyEnv)
		}
		c.Models.Providers[name] = p
	}
	return c, c.Validate()
}

// Validate 检查配置之间的约束。这里用值接收者，因为校验只读取 Config，不需要修改它。
func (c Config) Validate() error {
	if len(c.ClientKeys) == 0 {
		return fmt.Errorf("CLIENT_API_KEYS is required")
	}
	for _, key := range c.ClientKeys {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, " \t\r\n") {
			return fmt.Errorf("client tokens must be nonempty and contain no whitespace")
		}
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return fmt.Errorf("invalid HTTP_ADDR")
	}
	if c.MetricsAddr != "" {
		if _, _, err := net.SplitHostPort(c.MetricsAddr); err != nil {
			return fmt.Errorf("invalid METRICS_ADDR")
		}
		if c.MetricsAddr == c.HTTPAddr {
			return fmt.Errorf("metrics and API listeners must differ")
		}
	}
	if c.MaxRequestBytes <= 0 || c.MaxRequestBytes > 64<<20 || c.MaxConcurrent <= 0 || c.MaxConcurrent > 100000 {
		return fmt.Errorf("invalid request size or concurrency limit")
	}
	for _, d := range []time.Duration{c.RequestTimeout, c.HeaderTimeout, c.FirstEventTimeout, c.StreamIdleTimeout, c.WriteTimeout, c.ShutdownTimeout} {
		if d <= 0 {
			return fmt.Errorf("timeouts must be positive")
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid LOG_LEVEL")
	}
	if len(c.Models.Models) == 0 || len(c.Models.Providers) == 0 {
		return fmt.Errorf("at least one provider and model are required")
	}
	for name, p := range c.Models.Providers {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid provider name or key environment variable")
		}
		if p.AuthType == "" {
			p.AuthType = "api_key"
		}
		switch p.AuthType {
		case "api_key":
			if !envPattern.MatchString(p.APIKeyEnv) {
				return fmt.Errorf("provider %s requires a valid api_key_env", name)
			}
		case "openai_oauth":
			if p.APIKeyEnv != "" || p.ChatURL != "" {
				return fmt.Errorf("provider %s openai_oauth uses the fixed Responses API and does not accept chat_url or api_key_env", name)
			}
		default:
			return fmt.Errorf("provider %s has an unsupported auth_type", name)
		}
		if p.AuthType == "openai_oauth" {
			continue
		}
		u, err := url.Parse(p.ChatURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
			return fmt.Errorf("provider %s requires a fixed HTTPS URL without credentials, query or fragment", name)
		}
		if strings.TrimSpace(p.APIKey) == "" || strings.ContainsAny(p.APIKey, "\r\n") {
			return fmt.Errorf("provider %s is missing a valid API key", name)
		}
	}
	for name, m := range c.Models.Models {
		if !namePattern.MatchString(name) || strings.TrimSpace(m.UpstreamModel) == "" {
			return fmt.Errorf("invalid model alias or upstream model")
		}
		if _, ok := c.Models.Providers[m.Provider]; !ok {
			return fmt.Errorf("model %s references unknown provider", name)
		}
		if m.MaxOutputTokens <= 0 {
			return fmt.Errorf("model %s requires positive max_output_tokens", name)
		}
	}
	return nil
}

// env 返回已设置的环境变量，即使它是空字符串；只有变量不存在时才采用默认值。
func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

// integer 解析正整数环境变量，并将格式错误或非正数转换成带变量名的配置错误。
func integer(name string, fallback int64) (int64, error) {
	v, err := strconv.ParseInt(env(name, strconv.FormatInt(fallback, 10)), 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return v, nil
}
