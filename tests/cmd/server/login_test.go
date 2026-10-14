package servercmd_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestServerCLI 从独立测试目录编译并运行命令，验证真实入口和退出码。
// 所有运行都使用临时目录和明确的失败条件，不打开浏览器、不监听服务或访问上游。
func TestServerCLI(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "server")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/server")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cannot build CLI test executable: %v\n%s", err, output)
	}

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"default", nil, 1},
		{"login", []string{"--login"}, 1},
		{"login-provider", []string{"--login", "openai"}, 1},
		{"login-equals", []string{"--login=openai-codex"}, 1},
		{"login-options", []string{"--login", "openai", "--no-browser", "--login-timeout", "1m"}, 1},
		{"unsupported-provider", []string{"--login", "anthropic"}, 2},
		{"extra-argument", []string{"--login", "openai", "extra"}, 2},
		{"browser-without-login", []string{"--no-browser"}, 2},
		{"timeout-without-login", []string{"--login-timeout", "1m"}, 2},
		{"invalid-timeout", []string{"--login", "--login-timeout", "0s"}, 2},
		{"unknown-option", []string{"--unknown"}, 2},
		{"help", []string{"--help"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.want == 1 {
				writeCLIConfig(t, dir, "server: [")
			}
			output := runCLI(t, binary, dir, tc.want, tc.args...)
			// 合法参数进入配置加载；帮助和非法参数在读取配置文件之前返回。
			if strings.Contains(output, "invalid YAML configuration") != (tc.want == 1) {
				t.Fatal("unexpected argument parsing and configuration loading order")
			}
		})
	}

	t.Run("login-credential-path", func(t *testing.T) {
		dir := t.TempDir()
		// 目录不是凭证文件；YAML 路径生效时登录会在任何网络操作之前退出。
		writeCLIConfig(t, dir, `server:
  http_addr: 127.0.0.1:8080
  metrics_addr: ""
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
  chat_log_dir: ""
providers:
  openai:
    auth_type: openai_codex
    codex_token_file: .
models:
  default:
    provider: openai
    upstream_model: codex-model
    supports_stream: true
    supports_temperature: false
    supports_max_tokens: false
`)
		output := runCLI(t, binary, dir, 1, "--login", "--no-browser")
		if !strings.Contains(output, "cannot read OpenAI Codex credential file") {
			t.Fatal("login did not use credential path loaded from YAML")
		}
	})
}

func writeCLIConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "models.json"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// runCLI 限定运行时间并清除继承的凭证路径，防止本机登录状态干扰测试。
// 执行策略阻止进程启动与应用返回非零退出码分别报告，不能混为测试通过。
func runCLI(t *testing.T, binary, dir string, want int, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = []string{}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("CLI test timed out")
	}
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("CLI executable could not start (check OS execution policy or antivirus): %v", err)
		}
		code = exitError.ExitCode()
	}
	if code != want {
		t.Fatalf("CLI exit code %d; want %d\n%s", code, want, output)
	}
	return string(output)
}
