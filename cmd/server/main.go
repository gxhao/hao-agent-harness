package main

import (
	"context"
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
	bootstrap := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, err := config.Load()
	if err != nil {
		bootstrap.Error("configuration_failed", "error", err.Error())
		return 1
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		bootstrap.Error("invalid_log_level")
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	app, err := server.New(c, logger)
	if err != nil {
		logger.Error("initialization_failed", "error", err.Error())
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx); err != nil {
		logger.Error("server_failed", "error", err.Error())
		return 1
	}
	return 0
}
