// Package config 负责从本地 YAML 配置中心加载、校验并热监视服务配置。
package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultPath 是应用唯一的运行配置入口；环境变量不会覆盖此路径或其中的值。
	DefaultPath    = "configs/models.json"
	maxConfigBytes = 1 << 20
)

type Provider struct {
	AuthType       string `yaml:"auth_type,omitempty"`
	ChatURL        string `yaml:"chat_url,omitempty"`
	APIKey         string `yaml:"api_key,omitempty"`
	OAuthTokenFile string `yaml:"oauth_token_file,omitempty"`
	CodexTokenFile string `yaml:"codex_token_file,omitempty"`
}

// AuthMethod 让省略 auth_type 的兼容 Provider 保持 API Key 模式。
func (p Provider) AuthMethod() string {
	if p.AuthType == "" {
		return "api_key"
	}
	return p.AuthType
}

type modelDefinition struct {
	Provider            string `yaml:"provider"`
	UpstreamModel       string `yaml:"upstream_model"`
	SupportsStream      *bool  `yaml:"supports_stream,omitempty"`
	SupportsTemperature *bool  `yaml:"supports_temperature,omitempty"`
	SupportsMaxTokens   *bool  `yaml:"supports_max_tokens,omitempty"`
	MaxOutputTokens     *int   `yaml:"max_output_tokens,omitempty"`
}

func (d modelDefinition) model(p Provider) Model {
	m := Model{Provider: d.Provider, UpstreamModel: d.UpstreamModel, SupportsStream: true,
		SupportsTemperature: p.AuthMethod() != "openai_codex", SupportsMaxTokens: p.AuthMethod() != "openai_codex"}
	if d.SupportsStream != nil {
		m.SupportsStream = *d.SupportsStream
	}
	if d.SupportsTemperature != nil {
		m.SupportsTemperature = *d.SupportsTemperature
	}
	if d.SupportsMaxTokens != nil {
		m.SupportsMaxTokens = *d.SupportsMaxTokens
	}
	if m.SupportsMaxTokens {
		m.MaxOutputTokens = 4096
	}
	if d.MaxOutputTokens != nil {
		m.MaxOutputTokens = *d.MaxOutputTokens
	}
	return m
}

type Model struct {
	Provider            string `yaml:"provider"`
	UpstreamModel       string `yaml:"upstream_model"`
	SupportsStream      bool   `yaml:"supports_stream"`
	SupportsTemperature bool   `yaml:"supports_temperature"`
	SupportsMaxTokens   bool   `yaml:"supports_max_tokens"`
	MaxOutputTokens     int    `yaml:"max_output_tokens"`
}

type Models struct {
	Providers map[string]Provider `yaml:"providers"`
	Models    map[string]Model    `yaml:"models"`
}

// Config 是运行中的不可变配置快照；热加载会整体替换快照，避免字段逐项更新时出现混合状态。
type Config struct {
	HTTPAddr          string
	MetricsAddr       string
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
	ChatLogDir        string
}

type fileConfig struct {
	Server struct {
		HTTPAddr          string `yaml:"http_addr"`
		MetricsAddr       string `yaml:"metrics_addr"`
		MaxRequestBytes   int64  `yaml:"max_request_bytes"`
		MaxConcurrent     int    `yaml:"max_concurrent_requests"`
		RequestTimeout    string `yaml:"request_timeout"`
		HeaderTimeout     string `yaml:"upstream_header_timeout"`
		FirstEventTimeout string `yaml:"stream_first_event_timeout"`
		StreamIdleTimeout string `yaml:"stream_idle_timeout"`
		WriteTimeout      string `yaml:"downstream_write_timeout"`
		ShutdownTimeout   string `yaml:"shutdown_timeout"`
	} `yaml:"server"`
	Logging struct {
		Level      string `yaml:"level"`
		ChatLogDir string `yaml:"chat_log_dir"`
	} `yaml:"logging"`
	Providers map[string]Provider        `yaml:"providers"`
	Models    map[string]modelDefinition `yaml:"models"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Load 从固定配置中心读取当前服务配置，不访问进程环境变量或 .env。
func Load() (Config, error) { return LoadFile(DefaultPath) }

// LoadFile 读取单个受限大小的 YAML 文件；严格拒绝 JSON 旧格式、未知字段和尾随文档。
func LoadFile(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("cannot read configuration file %s", path)
	}
	return decodeConfig(path, b)
}

func decodeConfig(path string, b []byte) (Config, error) {
	var c Config
	if len(b) == 0 || len(b) > maxConfigBytes {
		return c, errors.New("configuration file is empty or exceeds 1 MiB")
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return c, errors.New("configuration must use YAML syntax")
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var raw fileConfig
	if err := dec.Decode(&raw); err != nil {
		return c, errors.New("invalid YAML configuration")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain one YAML document")
	}
	c = Config{HTTPAddr: raw.Server.HTTPAddr, MetricsAddr: raw.Server.MetricsAddr,
		MaxRequestBytes: raw.Server.MaxRequestBytes, MaxConcurrent: raw.Server.MaxConcurrent,
		LogLevel: raw.Logging.Level, ChatLogDir: raw.Logging.ChatLogDir}
	for _, item := range []struct {
		text   string
		target *time.Duration
		field  string
	}{
		{raw.Server.RequestTimeout, &c.RequestTimeout, "request_timeout"},
		{raw.Server.HeaderTimeout, &c.HeaderTimeout, "upstream_header_timeout"},
		{raw.Server.FirstEventTimeout, &c.FirstEventTimeout, "stream_first_event_timeout"},
		{raw.Server.StreamIdleTimeout, &c.StreamIdleTimeout, "stream_idle_timeout"},
		{raw.Server.WriteTimeout, &c.WriteTimeout, "downstream_write_timeout"},
		{raw.Server.ShutdownTimeout, &c.ShutdownTimeout, "shutdown_timeout"},
	} {
		d, err := time.ParseDuration(item.text)
		if err != nil {
			return c, fmt.Errorf("invalid duration for %s", item.field)
		}
		*item.target = d
	}
	c.Models = Models{Providers: raw.Providers, Models: make(map[string]Model, len(raw.Models))}
	for name, definition := range raw.Models {
		c.Models.Models[name] = definition.model(c.Models.Providers[definition.Provider])
	}
	return c, c.Validate()
}

// Validate 检查配置之间的约束，并确保密钥只出现在内存配置中，不进入错误文本。
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return errors.New("invalid server.http_addr")
	}
	if c.MetricsAddr != "" {
		if _, _, err := net.SplitHostPort(c.MetricsAddr); err != nil {
			return errors.New("invalid server.metrics_addr")
		}
		if c.MetricsAddr == c.HTTPAddr {
			return errors.New("metrics and API listeners must differ")
		}
	}
	if c.MaxRequestBytes <= 0 || c.MaxRequestBytes > 64<<20 || c.MaxConcurrent <= 0 || c.MaxConcurrent > 100000 {
		return errors.New("invalid request size or concurrency limit")
	}
	for _, d := range []time.Duration{c.RequestTimeout, c.HeaderTimeout, c.FirstEventTimeout, c.StreamIdleTimeout, c.WriteTimeout, c.ShutdownTimeout} {
		if d <= 0 {
			return errors.New("timeouts must be positive")
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("invalid logging.level")
	}
	if len(c.Models.Models) == 0 || len(c.Models.Providers) == 0 {
		return errors.New("at least one provider and model are required")
	}
	for name, p := range c.Models.Providers {
		if !namePattern.MatchString(name) {
			return errors.New("invalid provider name")
		}
		switch p.AuthMethod() {
		case "api_key":
			u, err := url.Parse(p.ChatURL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
				return fmt.Errorf("provider %s requires a fixed HTTPS chat_url", name)
			}
			if strings.TrimSpace(p.APIKey) == "" || strings.ContainsAny(p.APIKey, "\r\n") {
				return fmt.Errorf("provider %s requires api_key", name)
			}
			if p.OAuthTokenFile != "" || p.CodexTokenFile != "" {
				return fmt.Errorf("provider %s has incompatible credential file fields", name)
			}
		case "openai_oauth":
			if p.APIKey != "" || p.ChatURL != "" || p.CodexTokenFile != "" || p.OAuthTokenFile == "" {
				return fmt.Errorf("provider %s openai_oauth requires oauth_token_file and uses a fixed API", name)
			}
		case "openai_codex":
			if p.APIKey != "" || p.ChatURL != "" || p.OAuthTokenFile != "" || p.CodexTokenFile == "" {
				return fmt.Errorf("provider %s openai_codex requires codex_token_file and uses a fixed API", name)
			}
		default:
			return fmt.Errorf("provider %s has an unsupported auth_type", name)
		}
	}
	for name, m := range c.Models.Models {
		if !namePattern.MatchString(name) || strings.TrimSpace(m.UpstreamModel) == "" {
			return errors.New("invalid model alias or upstream_model")
		}
		p, ok := c.Models.Providers[m.Provider]
		if !ok {
			return fmt.Errorf("model %s references unknown provider", name)
		}
		if p.AuthMethod() == "openai_codex" && m.SupportsMaxTokens {
			return fmt.Errorf("model %s openai_codex does not support max_tokens; set supports_max_tokens to false", name)
		}
		if m.MaxOutputTokens < 0 || m.SupportsMaxTokens && m.MaxOutputTokens == 0 {
			return fmt.Errorf("model %s requires positive max_output_tokens when max_tokens is supported", name)
		}
	}
	return nil
}

// WatchFile 轮询配置文件。只有内容变化且解析成功、应用回调成功时才推进快照；回调错误保留旧配置。
func WatchFile(ctx context.Context, path string, initialConfig Config, interval time.Duration, onApply func(Config) error, onError func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	initial, readErr := os.ReadFile(path)
	var last [32]byte
	var lastRejected [32]byte
	var hasRejected bool
	var lastApplyFailure [32]byte
	var hasApplyFailure bool
	if readErr != nil {
		onError(errors.New("cannot monitor configuration file"))
	} else {
		last = sha256.Sum256(initial)
		if current, err := decodeConfig(path, initial); err != nil {
			lastRejected, hasRejected = last, true
			onError(err)
		} else if !reflect.DeepEqual(current, initialConfig) {
			if err := onApply(current); err != nil {
				last = [32]byte{}
				lastApplyFailure, hasApplyFailure = sha256.Sum256(initial), true
				onError(err)
			}
		}
	}
	var lastReadError string
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b, err := os.ReadFile(path)
			if err != nil {
				if lastReadError == "" {
					onError(errors.New("cannot read changed configuration"))
					lastReadError = "read-error"
				}
				continue
			}
			lastReadError = ""
			digest := sha256.Sum256(b)
			if digest == last || hasRejected && digest == lastRejected {
				continue
			}
			c, err := decodeConfig(path, b)
			if err != nil {
				lastRejected, hasRejected = digest, true
				onError(err)
				continue
			}
			if err := onApply(c); err != nil {
				if !hasApplyFailure || digest != lastApplyFailure {
					onError(err)
					lastApplyFailure, hasApplyFailure = digest, true
				}
				continue
			}
			last = digest
			hasRejected = false
			hasApplyFailure = false
		}
	}
}
