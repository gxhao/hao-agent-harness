# OpenAI 兼容代理模块设计提案

状态：待用户确认
更新：2026-10-15

## 1. 需求与现状

需求是增加 `proxy/` 模块，供其他模块通过 OpenAI 兼容协议调用，并提供可访问的 HTTP 地址。

当前服务已经提供 `GET /v1/models` 和 `POST /v1/chat/completions`，默认监听 `127.0.0.1:8080`。将 `server.http_addr` 配置为可路由的网卡地址即可从其他主机访问，但当前 API 不校验客户端 Bearer token。`internal/` 中的 Go 包也不能被仓库外的 Go 模块直接导入。

因此需要确认“代理模块”面向调用方的形态，以及 HTTP 地址的访问范围。

## 2. 可选方向

### A. 可导入的 Go 客户端库

在仓库根目录新增公开 `proxy` Go 包，接收 HTTP Base URL，封装模型列表、普通聊天和 SSE 流式聊天请求。其他 Go 模块通过导入该包调用当前服务；服务端仍由现有 `cmd/server` 启动，地址仍使用 `server.http_addr`。

优点是职责清晰，复用现有服务协议；缺点是非 Go 调用方仍需自行使用 HTTP/OpenAI SDK。

### B. 独立代理服务

增加一个可单独启动的代理入口及 `proxy/` 包，复用现有配置、模型路由和 Provider，单独监听配置的 HTTP 地址并提供 OpenAI 兼容端点。

优点是服务进程边界清楚；缺点是需要设计与现有 `cmd/server` 的生命周期、配置和端口关系，容易重复现有 HTTP 服务职责。

### C. 开放现有 HTTP 服务

保持现有处理链路，只补充明确的监听配置说明、访问地址输出或启动选项，让其他模块直接访问现有 `/v1` API。该方向不新增独立代理逻辑。

优点是改动最小；缺点是无法满足“新增可导入的 `proxy/` 模块”这一字面需求。

## 3. 推荐方案

如果目标是让其他 Go 模块以库的方式复用，推荐 A：新增公开 `proxy/` 客户端包，使用标准 `net/http`，允许调用方配置服务 Base URL、可选客户端 Bearer token、请求超时和 HTTP client；支持模型列表、非流式聊天和逐事件读取 SSE。客户端 API Key 仅作为可选请求头发送，不写日志，也不要求现有服务端立即启用鉴权。

在不改变现有 API 契约的前提下，沿用 `server.http_addr` 作为服务监听配置，并在 README 提供本机示例和远程调用地址示例。远程监听需由部署者设置明确可达的 IP/主机名，并用网络边界限制未授权访问；不把无鉴权服务默认改为 `0.0.0.0`。

请求流为：调用方模块 → `proxy.Client` → 配置的 `http(s)://主机:端口/v1` → 现有 HTTP API → Service/Provider → 上游。流式模式不缓冲完整响应；请求取消会传递给 HTTP 请求并关闭连接。

## 4. 接口草案与边界

- 包路径：`hao-agent-harness/proxy`。
- 客户端构造：`proxy.NewClient(baseURL string, options ...Option)`。
- 地址使用 `/v1` 作为 Base URL 后缀，避免调用方重复拼接端点。
- 暴露 `ListModels(ctx)`、`CreateChatCompletion(ctx, request)` 和 `CreateChatCompletionStream(ctx, request)`；请求/响应结构覆盖当前服务支持的字段子集。
- 非 2xx 响应转换为不含响应正文、Authorization 或上游敏感信息的安全错误；SSE 流内错误返回稳定的错误类型。
- 约束仅接受 HTTP/HTTPS URL，不允许 URL 内嵌用户名密码；禁止自动跨主机重定向，避免 Bearer token 泄漏。
- 服务端接口仍不校验客户端 token。若未来启用认证，需单独设计配置、密钥存储与兼容迁移。

## 5. 文件与验证计划

若确认推荐方案，预计新增 `proxy/client.go`、`proxy/types.go` 和 `tests/proxy/client_test.go`；更新 `README.md` 的调用说明和 `docs/llm-http-server-design.md` 的包结构/接口章节。测试放在根目录 `tests/proxy/`，使用本地 HTTP 测试服务覆盖普通响应、模型列表、SSE、错误响应、取消和重定向安全；之后执行 gofmt、go vet、全量 Go 测试及构建，并检查差异。

若选择 B 或 C，则先调整本提案和涉及的设计文档，再开始实现。

## 6. 待确认

请确认采用推荐方案 A，或选择 B（独立代理服务）/C（开放现有服务）。若采用 A，还请指出远程 HTTP 地址是只供本机/同机模块使用，还是需要局域网主机访问；默认按现有配置由部署者自行绑定地址，并保持 loopback 默认值。
