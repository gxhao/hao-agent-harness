package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/openaioauth"
)

type cliOptions struct {
	login     string
	noBrowser bool
	timeout   time.Duration
}

type loginFlag string

func (v *loginFlag) String() string   { return string(*v) }
func (v *loginFlag) IsBoolFlag() bool { return true }
func (v *loginFlag) Set(value string) error {
	if value == "true" {
		value = "openai"
	}
	*v = loginFlag(value)
	return nil
}

func parseOptions(args []string, output io.Writer) (cliOptions, error) {
	// Convert the optional provider argument before flag.Parse so flags can follow it.
	normalized := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if (args[i] == "--login" || args[i] == "-login") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			normalized = append(normalized, "--login="+args[i+1])
			i++
		} else {
			normalized = append(normalized, args[i])
		}
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(output)
	var login loginFlag
	var options cliOptions
	fs.Var(&login, "login", "登录 OpenAI Codex（--login 或 --login openai / openai-codex），保存后退出")
	fs.BoolVar(&options.noBrowser, "no-browser", false, "登录时不自动打开浏览器")
	fs.DurationVar(&options.timeout, "login-timeout", 10*time.Minute, "登录超时，例如 5m")
	if err := fs.Parse(normalized); err != nil {
		return options, err
	}
	remaining := fs.Args()
	if len(remaining) > 0 {
		return options, cliError(output, "unexpected positional arguments")
	}
	options.login = string(login)
	if options.login != "" && options.login != "openai" && options.login != "openai-codex" {
		return options, cliError(output, "unsupported login provider; supported: openai, openai-codex")
	}
	if options.timeout <= 0 {
		return options, cliError(output, "--login-timeout must be positive")
	}
	if options.login == "" {
		var loginOnly bool
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "no-browser" || f.Name == "login-timeout" {
				loginOnly = true
			}
		})
		if loginOnly {
			return options, cliError(output, "--no-browser and --login-timeout require --login")
		}
	}
	return options, nil
}

func cliError(output io.Writer, message string) error {
	fmt.Fprintln(output, message)
	return errors.New(message)
}

func runLogin(ctx context.Context, options cliOptions, c config.Config, input io.Reader, output io.Writer) int {
	var path string
	for _, provider := range c.Models.Providers {
		if provider.AuthMethod() == "openai_codex" {
			path = provider.CodexTokenFile
			break
		}
	}
	if path == "" {
		fmt.Fprintln(output, "配置中心中未设置 openai_codex 的 codex_token_file")
		return 1
	}
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	manager, err := openaioauth.NewCodex(path, client)
	if err != nil {
		fmt.Fprintln(output, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()
	manual := make(chan string)
	// Standard input may block until the process exits; it never holds up callback completion.
	go func() {
		defer close(manual)
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			if value := strings.TrimSpace(scanner.Text()); value != "" {
				select {
				case manual <- value:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	loginOptions := openaioauth.LoginOptions{Output: output, ManualInput: manual}
	if !options.noBrowser {
		loginOptions.OpenBrowser = openBrowser
	}
	if err := manager.Login(ctx, loginOptions); err != nil {
		fmt.Fprintln(output, "OpenAI 登录失败：", err)
		return 1
	}
	fmt.Fprintln(output, "OpenAI Codex 登录成功，凭证已保存：", path)
	fmt.Fprintln(output, "请确认 configs/models.json 中已配置 codex_token_file，然后运行 go run ./cmd/server。")
	return 0
}

func openBrowser(address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	case "darwin":
		command = exec.Command("open", address)
	default:
		command = exec.Command("xdg-open", address)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go command.Wait()
	return nil
}
