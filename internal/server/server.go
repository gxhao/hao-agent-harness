// Package server 负责构造配置快照、原子切换运行依赖并管理 HTTP listener 生命周期。
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
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

type runtimeGeneration struct {
	mu      sync.Mutex
	active  int
	retired bool
	closed  bool
	cancel  context.CancelFunc
	client  *http.Client
	handler http.Handler
	drain   func()
}

func (g *runtimeGeneration) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.retired && g.active == 0 {
		g.closeLocked()
	}
}
func (g *runtimeGeneration) retire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retired = true
	if g.active == 0 {
		g.closeLocked()
	}
}
func (g *runtimeGeneration) closeLocked() {
	if g.closed {
		return
	}
	g.closed = true
	g.cancel()
	g.client.CloseIdleConnections()
}

type runtimeRouter struct {
	mu      sync.Mutex
	current *runtimeGeneration
}

func (r *runtimeRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	g := r.current
	if g != nil {
		g.mu.Lock()
		g.active++
		g.mu.Unlock()
	}
	r.mu.Unlock()
	if g == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer g.release()
	g.handler.ServeHTTP(w, req)
}

func (r *runtimeRouter) swap(g *runtimeGeneration) {
	r.mu.Lock()
	old := r.current
	r.current = g
	r.mu.Unlock()
	if old != nil {
		old.retire()
	}
}

func (r *runtimeRouter) retire() { r.swap(nil) }

func (r *runtimeRouter) drain() {
	r.mu.Lock()
	g := r.current
	r.mu.Unlock()
	if g != nil && g.drain != nil {
		g.drain()
	}
}

type serverSet struct {
	config          config.Config
	servers         []*http.Server
	listeners       []net.Listener
	startServers    []*http.Server
	startListeners  []net.Listener
	retireServers   []*http.Server
	retireListeners []net.Listener
}

type serveResult struct {
	set *serverSet
	err error
}

type App struct {
	HTTP            *http.Server
	Metrics         *http.Server
	logger          *slog.Logger
	metrics         *observability.Metrics
	router          *runtimeRouter
	providedClient  *http.Client
	config          config.Config
	mu              sync.Mutex
	currentSet      *serverSet
	runResults      chan serveResult
	running         bool
	rootCtx         context.Context
	shutdownTimeout time.Duration
	onConfigApplied func(config.Config)
}

// New 使用默认的安全 HTTP transport 组装配置快照和可热切换的请求路由。
func New(c config.Config, logger *slog.Logger) (*App, error) {
	return NewWithHTTPClient(c, logger, nil)
}

// NewWithHTTPClient 接受可注入测试的 HTTP client；重定向仍被禁用以防上游认证头泄露。
func NewWithHTTPClient(c config.Config, logger *slog.Logger, provided *http.Client) (*App, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	metrics := observability.New()
	app := &App{logger: logger, metrics: metrics, router: &runtimeRouter{}, providedClient: provided,
		config: c, runResults: make(chan serveResult, 16), rootCtx: context.Background(), shutdownTimeout: c.ShutdownTimeout}
	g, err := app.buildGeneration(c)
	if err != nil {
		return nil, err
	}
	app.router.current = g
	app.HTTP, app.Metrics = app.makeServers(c)
	return app, nil
}

func (a *App) buildGeneration(c config.Config) (*runtimeGeneration, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.ChatLogDir != "" {
		if err := os.MkdirAll(c.ChatLogDir, 0700); err != nil {
			return nil, errors.New("cannot create configured chat log directory")
		}
	}
	root, cancel := context.WithCancel(context.Background())
	client := provider.NewHTTPClient(c.HeaderTimeout, c.MaxConcurrent)
	if a.providedClient != nil {
		*client = *a.providedClient
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	providers := make(map[string]chat.Provider, len(c.Models.Providers))
	var oauthManager, codexManager *openaioauth.Manager
	var oauthHandler http.Handler
	for name, p := range c.Models.Providers {
		switch p.AuthMethod() {
		case "openai_oauth":
			if oauthManager == nil {
				var err error
				oauthManager, err = openaioauth.New(p.OAuthTokenFile, "hao-agent-harness", client)
				if err != nil {
					cancel()
					client.CloseIdleConnections()
					return nil, err
				}
				oauthHandler = oauthManager.Handler()
			}
			providers[name] = provider.NewOpenAIResponses(client, oauthManager)
		case "openai_codex":
			if codexManager == nil {
				var err error
				codexManager, err = openaioauth.NewCodex(p.CodexTokenFile, client)
				if err != nil {
					cancel()
					client.CloseIdleConnections()
					return nil, err
				}
				oauthHandler = codexManager.Handler()
			}
			providers[name] = provider.NewOpenAICodex(client, codexManager)
		default:
			providers[name] = provider.NewCompatible(client, p.ChatURL, p.APIKey)
		}
	}
	svc := service.New(root, c.Models.Models, providers, c.MaxConcurrent, c.RequestTimeout)
	h := httpapi.New(svc, httpapi.Options{MaxRequestBytes: c.MaxRequestBytes, FirstEventTimeout: c.FirstEventTimeout,
		StreamIdleTimeout: c.StreamIdleTimeout, WriteTimeout: c.WriteTimeout, ChatLogDir: c.ChatLogDir}, a.logger, a.metrics)
	return &runtimeGeneration{cancel: cancel, client: client, handler: webui.New(h, oauthHandler), drain: svc.Drain}, nil
}

func (a *App) makeServers(c config.Config) (*http.Server, *http.Server) {
	api := &http.Server{Addr: c.HTTPAddr, Handler: a.router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return a.rootCtx },
		ErrorLog: slog.NewLogLogger(a.logger.Handler(), slog.LevelError)}
	var metrics *http.Server
	if c.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", a.metrics)
		metrics = &http.Server{Addr: c.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
			WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
			ErrorLog: slog.NewLogLogger(a.logger.Handler(), slog.LevelError)}
	}
	return api, metrics
}

func (a *App) makeServerSet(c config.Config) (*serverSet, error) {
	api, metrics := a.makeServers(c)
	servers := []*http.Server{api}
	if metrics != nil {
		servers = append(servers, metrics)
	}
	set := &serverSet{config: c, servers: servers, startServers: servers}
	for _, srv := range servers {
		listener, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, opened := range set.listeners {
				_ = opened.Close()
			}
			return nil, err
		}
		set.listeners = append(set.listeners, listener)
	}
	set.startListeners = set.listeners
	return set, nil
}

// makeReplacementSet 仅重新绑定地址发生变化的 listener，避免修改 metrics 时抢占已有 API 端口。
func (a *App) makeReplacementSet(c config.Config, old *serverSet) (*serverSet, error) {
	api, metrics := a.makeServers(c)
	set := &serverSet{config: c, startServers: make([]*http.Server, 0), startListeners: make([]net.Listener, 0)}
	oldAPI, oldMetrics := old.servers[0], (*http.Server)(nil)
	oldAPIListener := old.listeners[0]
	if len(old.servers) > 1 {
		oldMetrics = old.servers[1]
	}
	var oldMetricsListener net.Listener
	if len(old.listeners) > 1 {
		oldMetricsListener = old.listeners[1]
	}
	bind := func(srv *http.Server, target *[]*http.Server, listeners *[]net.Listener) error {
		l, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		*target = append(*target, srv)
		*listeners = append(*listeners, l)
		return nil
	}
	if c.HTTPAddr == a.config.HTTPAddr {
		set.servers = append(set.servers, oldAPI)
		set.listeners = append(set.listeners, oldAPIListener)
	} else {
		if err := bind(api, &set.startServers, &set.startListeners); err != nil {
			return nil, err
		}
		set.servers = append(set.servers, api)
		set.listeners = append(set.listeners, set.startListeners[len(set.startListeners)-1])
		set.retireServers = append(set.retireServers, oldAPI)
		set.retireListeners = append(set.retireListeners, oldAPIListener)
	}
	if c.MetricsAddr == a.config.MetricsAddr {
		if oldMetrics != nil {
			set.servers = append(set.servers, oldMetrics)
			set.listeners = append(set.listeners, oldMetricsListener)
		}
	} else {
		if metrics != nil {
			if err := bind(metrics, &set.startServers, &set.startListeners); err != nil {
				for _, l := range set.startListeners {
					_ = l.Close()
				}
				return nil, err
			}
			set.servers = append(set.servers, metrics)
			set.listeners = append(set.listeners, set.startListeners[len(set.startListeners)-1])
		}
		if oldMetrics != nil {
			set.retireServers = append(set.retireServers, oldMetrics)
			set.retireListeners = append(set.retireListeners, oldMetricsListener)
		}
	}
	return set, nil
}

func (a *App) startSet(set *serverSet) {
	servers, listeners := set.startServers, set.startListeners
	if servers == nil {
		servers, listeners = set.servers, set.listeners
	}
	for i, srv := range servers {
		listener := listeners[i]
		go func() {
			err := srv.Serve(listener)
			if !errors.Is(err, http.ErrServerClosed) {
				a.runResults <- serveResult{set: set, err: err}
			}
		}()
		a.logger.Info("listener_started", "address", listener.Addr().String())
	}
}

// ApplyConfig 在旧配置继续服务的同时构建新快照；监听器全部成功绑定后才切换路由。
func (a *App) ApplyConfig(c config.Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := c.Validate(); err != nil {
		return err
	}
	g, err := a.buildGeneration(c)
	if err != nil {
		return err
	}
	var replacement *serverSet
	listenersChanged := c.HTTPAddr != a.config.HTTPAddr || c.MetricsAddr != a.config.MetricsAddr
	if a.running && listenersChanged {
		replacement, err = a.makeReplacementSet(c, a.currentSet)
		if err != nil {
			g.retire()
			return errors.New("cannot bind updated listener configuration")
		}
	}
	oldSet := a.currentSet
	a.config = c
	a.shutdownTimeout = c.ShutdownTimeout
	if replacement != nil {
		retiredSet := &serverSet{servers: replacement.retireServers, listeners: replacement.retireListeners}
		replacement.retireServers = nil
		replacement.retireListeners = nil
		a.currentSet = replacement
		a.startSet(replacement)
		if oldSet != nil {
			a.shutdownSet(retiredSet, c.ShutdownTimeout)
		}
	}
	a.router.swap(g)
	a.HTTP, a.Metrics = a.makeServers(c)
	if a.onConfigApplied != nil {
		a.onConfigApplied(c)
	}
	a.logger.Info("configuration_reloaded")
	return nil
}

// SetConfigApplied 注册配置成功切换后的回调，例如同步更新结构化日志级别。
func (a *App) SetConfigApplied(fn func(config.Config)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onConfigApplied = fn
}

// Run 启动服务并轮询本地 YAML；文件变更只有在完整校验、依赖构造及监听器绑定成功后才生效。
func (a *App) Run(stop context.Context) error {
	a.mu.Lock()
	set, err := a.makeServerSet(a.config)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	a.currentSet, a.running = set, true
	a.startSet(set)
	a.mu.Unlock()
	watchCtx, cancelWatch := context.WithCancel(stop)
	defer cancelWatch()
	a.mu.Lock()
	initialConfig := a.config
	a.mu.Unlock()
	go config.WatchFile(watchCtx, config.DefaultPath, initialConfig, time.Second, a.ApplyConfig,
		func(err error) { a.logger.Error("configuration_reload_failed", "error", err.Error()) })
	var runErr error
	for {
		select {
		case <-stop.Done():
			goto shutdown
		case result := <-a.runResults:
			a.mu.Lock()
			current := result.set == a.currentSet
			a.mu.Unlock()
			if current {
				runErr = result.err
				goto shutdown
			}
		}
	}

shutdown:
	a.router.drain()
	a.mu.Lock()
	current := a.currentSet
	a.running = false
	timeout := a.shutdownTimeout
	a.mu.Unlock()
	if current != nil {
		a.shutdownSet(current, timeout)
	}
	a.router.retire()
	return runErr
}

// ServeListeners 使用调用方监听器；嵌入式调用保持明确 listener 生命周期，不启动文件监视器。
func (a *App) ServeListeners(stop context.Context, listeners ...net.Listener) error {
	servers := []*http.Server{a.HTTP}
	if a.Metrics != nil {
		servers = append(servers, a.Metrics)
	}
	if len(listeners) != len(servers) {
		return errors.New("the number of listeners must match the configured servers")
	}
	for _, listener := range listeners {
		if listener == nil {
			return errors.New("listeners cannot be nil")
		}
	}
	set := &serverSet{servers: servers, listeners: listeners}
	a.startSet(set)
	select {
	case <-stop.Done():
	case result := <-a.runResults:
		if result.set == set {
			return result.err
		}
	}
	a.router.drain()
	a.shutdownSet(set, a.shutdownTimeout)
	a.router.retire()
	return nil
}

func (a *App) shutdownSet(set *serverSet, timeout time.Duration) {
	servers := set.servers
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) { defer wg.Done(); _ = s.Shutdown(ctx) }(srv)
	}
	wg.Wait()
	for _, srv := range servers {
		_ = srv.Close()
	}
}
