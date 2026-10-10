// Package server assembles dependencies and owns both listener lifecycles.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hao-agent-harness/internal/chat"
	"hao-agent-harness/internal/config"
	"hao-agent-harness/internal/httpapi"
	"hao-agent-harness/internal/observability"
	"hao-agent-harness/internal/openaioauth"
	"hao-agent-harness/internal/provider"
	"hao-agent-harness/internal/service"
	"hao-agent-harness/internal/webui"
)

type App struct {
	HTTP            *http.Server
	Metrics         *http.Server
	service         *service.Service
	client          *http.Client
	cancel          context.CancelFunc
	shutdownTimeout time.Duration
	logger          *slog.Logger
}

// New 使用默认的安全 HTTP transport 组装服务依赖和监听器。
func New(c config.Config, logger *slog.Logger) (*App, error) {
	return NewWithHTTPClient(c, logger, nil)
}

// NewWithHTTPClient 用调用方提供的 HTTP client 组装服务，便于嵌入其他程序或注入测试传输层。
// client 复制到应用私有实例中，并强制关闭重定向，避免上游将带认证头的请求转发到其他地址。
func NewWithHTTPClient(c config.Config, logger *slog.Logger, provided *http.Client) (*App, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.ChatLogDir != "" {
		if err := os.MkdirAll(c.ChatLogDir, 0700); err != nil {
			return nil, errors.New("cannot create CHAT_LOG_DIR")
		}
		logger.Info("chat_logging_enabled", "directory", c.ChatLogDir)
	}
	root, cancel := context.WithCancel(context.Background())
	client := provider.NewHTTPClient(c.HeaderTimeout, c.MaxConcurrent)
	if provided != nil {
		*client = *provided
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	providers := make(map[string]chat.Provider, len(c.Models.Providers))
	var oauthManager *openaioauth.Manager
	var codexManager *openaioauth.Manager
	for name, p := range c.Models.Providers {
		authType := p.AuthMethod()
		if authType == "openai_oauth" {
			if oauthManager == nil {
				path := c.OpenAITokenFile
				if path == "" {
					base, err := os.UserConfigDir()
					if err != nil {
						cancel()
						return nil, errors.New("cannot find the user configuration directory for OpenAI OAuth credentials")
					}
					path = filepath.Join(base, "hao-agent-harness", "openai-oauth.json")
				}
				var err error
				oauthManager, err = openaioauth.New(path, "hao-agent-harness", client, c.ClientKeys)
				if err != nil {
					cancel()
					return nil, err
				}
			}
			providers[name] = provider.NewOpenAIResponses(client, oauthManager)
			continue
		}
		if authType == "openai_codex" {
			if codexManager == nil {
				path := c.OpenAICodexTokenFile
				var err error
				if path == "" {
					path, err = openaioauth.CodexTokenPath()
				}
				if err == nil {
					codexManager, err = openaioauth.NewCodex(path, client, c.ClientKeys)
				}
				if err != nil {
					cancel()
					return nil, err
				}
			}
			providers[name] = provider.NewOpenAICodex(client, codexManager)
			continue
		}
		providers[name] = provider.NewCompatible(client, p.ChatURL, p.APIKey)
	}
	s := service.New(root, c.Models.Models, providers, c.MaxConcurrent, c.RequestTimeout)
	metrics := observability.New()
	h := httpapi.New(s, httpapi.Options{ClientKeys: c.ClientKeys, MaxRequestBytes: c.MaxRequestBytes, FirstEventTimeout: c.FirstEventTimeout, StreamIdleTimeout: c.StreamIdleTimeout, WriteTimeout: c.WriteTimeout, ChatLogDir: c.ChatLogDir}, logger, metrics)
	app := &App{service: s, client: client, cancel: cancel, shutdownTimeout: c.ShutdownTimeout, logger: logger}
	var oauthHandler http.Handler
	if oauthManager != nil {
		oauthHandler = oauthManager.Handler()
	} else if codexManager != nil {
		oauthHandler = codexManager.Handler()
	}
	app.HTTP = &http.Server{Addr: c.HTTPAddr, Handler: webui.New(h, oauthHandler), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return root }, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
	if c.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics)
		app.Metrics = &http.Server{Addr: c.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
	}
	return app, nil
}

// Run 先绑定全部配置的端口，再启动服务；任一端口绑定失败时会关闭已绑定的 listener。
// 它阻塞到 stop 被取消或 listener 出错，然后进入优雅关闭流程。
func (a *App) Run(stop context.Context) error {
	defer a.cancel()
	defer a.client.CloseIdleConnections()
	servers := []*http.Server{a.HTTP}
	if a.Metrics != nil {
		servers = append(servers, a.Metrics)
	}
	listeners := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		listener, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return err
		}
		listeners = append(listeners, listener)
	}
	return a.serve(stop, servers, listeners)
}

// ServeListeners 在调用方提供的 listener 上提供 API 和指标服务，顺序为 API 后指标。
// 这适用于父进程管理 socket 或系统动态分配端口的场景；方法返回前会关闭这些 listener。
func (a *App) ServeListeners(stop context.Context, listeners ...net.Listener) error {
	servers := a.servers()
	if len(listeners) != len(servers) {
		return errors.New("the number of listeners must match the configured servers")
	}
	for _, listener := range listeners {
		if listener == nil {
			return errors.New("listeners cannot be nil")
		}
	}
	defer a.cancel()
	defer a.client.CloseIdleConnections()
	return a.serve(stop, servers, listeners)
}

// servers 按固定顺序收集已配置的 HTTP server，供启动和关闭逻辑共同使用。
func (a *App) servers() []*http.Server {
	servers := []*http.Server{a.HTTP}
	if a.Metrics != nil {
		servers = append(servers, a.Metrics)
	}
	return servers
}

// serve 并发运行各 listener；收到停止信号后先排空请求，再在宽限期结束时取消上游并强制关闭。
// goroutine 通过带缓冲 channel 报告结果，WaitGroup 确保退出前所有服务 goroutine 都已结束。
func (a *App) serve(stop context.Context, servers []*http.Server, listeners []net.Listener) error {
	results := make(chan error, len(servers))
	var serving sync.WaitGroup
	for i, srv := range servers {
		serving.Add(1)
		go func() { defer serving.Done(); results <- srv.Serve(listeners[i]) }()
		a.logger.Info("listener_started", "address", listeners[i].Addr().String())
	}
	var runErr error
	select {
	case <-stop.Done():
	case err := <-results:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	a.service.Drain()
	ctx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()
	var shutdown sync.WaitGroup
	shutdownErrors := make(chan error, len(servers))
	for _, srv := range servers {
		shutdown.Add(1)
		go func() { defer shutdown.Done(); shutdownErrors <- srv.Shutdown(ctx) }()
	}
	shutdown.Wait()
	close(shutdownErrors)
	for err := range shutdownErrors {
		if err != nil && runErr == nil && !errors.Is(err, context.DeadlineExceeded) {
			runErr = err
		}
	}
	// Shutdown does not forcibly close active connections. Cancel upstream work
	// and close listeners/connections once the grace period has elapsed.
	a.cancel()
	for _, srv := range servers {
		_ = srv.Close()
	}
	serving.Wait()
	a.logger.Info("server_stopped", "grace_period_expired", ctx.Err() != nil)
	return runErr
}
