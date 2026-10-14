package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/server"
)

// main 是进程入口；把退出码交给 os.Exit，便于 run 在所有错误路径统一返回状态。
func main() { os.Exit(run()) }

// run 负责加载配置、初始化结构化日志，并把系统终止信号转换成可取消的 context。
func run() int {
	options, err := parseOptions(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	bootstrap := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, err := config.Load()
	if err != nil {
		bootstrap.Error("configuration_failed", "error", err.Error())
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if options.login != "" {
		return runLogin(ctx, options, c, os.Stdin, os.Stdout)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		bootstrap.Error("invalid_log_level")
		return 1
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar}))
	app, err := server.New(c, logger)
	if err != nil {
		logger.Error("initialization_failed", "error", err.Error())
		return 1
	}
	app.SetConfigApplied(func(updated config.Config) {
		var next slog.Level
		if next.UnmarshalText([]byte(updated.LogLevel)) == nil {
			levelVar.Set(next)
		}
	})
	if err := app.Run(ctx); err != nil {
		logger.Error("server_failed", "error", err.Error())
		return 1
	}
	return 0
}
