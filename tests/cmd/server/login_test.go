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
			writeCLIEnv(t, dir, "INVALID\n")
			output := runCLI(t, binary, dir, tc.want, tc.args...)
			// 合法参数进入 dotenv 校验；帮助和非法参数必须在读取文件之前返回。
			if strings.Contains(output, ".env: line 1:") != (tc.want == 1) {
				t.Fatal("unexpected argument parsing and dotenv loading order")
			}
		})
	}

	t.Run("login-credential-path", func(t *testing.T) {
		dir := t.TempDir()
		// 目录不是凭证文件；若 .env 路径生效，登录将在任何网络操作之前退出。
		writeCLIEnv(t, dir, "OPENAI_CODEX_TOKEN_FILE=.\n")
		output := runCLI(t, binary, dir, 1, "--login", "--no-browser")
		if !strings.Contains(output, "cannot read OpenAI Codex credential file") {
			t.Fatal("login did not use credential path loaded from dotenv")
		}
	})
}

func writeCLIEnv(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0600); err != nil {
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
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, "OPENAI_CODEX_TOKEN_FILE") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
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
