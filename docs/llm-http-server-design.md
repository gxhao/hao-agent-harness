# Go 大模型请求 HTTP 服务端设计

状态：初版已实现；供应商契约验证待接入真实上游后完成  
日期：2026-10-10  
项目：hao-agent-harness

## 1. 目标与范围

使用 Go 构建一个 HTTP 服务端，为调用方提供统一的大模型聊天接口。服务负责请求校验、客户端鉴权、模型路由、上游调用、流式转发和运行指标，避免客户端直接持有大模型供应商密钥。

初版假设：

- 调用方为内部应用或 Agent，通过 Bearer Token 访问服务。
- 上游提供兼容 OpenAI Chat Completions 风格的 HTTP 接口；具体兼容性在接入时通过契约测试验证。
- 支持多个上游配置，通过公开模型别名路由到指定上游模型。
- 支持非流式响应和 SSE 流式响应。
- 支持标准 API Key provider，以及使用 Sign in with ChatGPT OAuth 的 OpenAI Responses API provider。
- 服务无状态，不保存聊天历史；调用方每次提交所需上下文。

初版不包含用户系统、数据库、计费、工具执行、Agent 编排、文件上传、图片/音频输入、动态配置后台。工具调用、结构化输出及其他供应商原生接口留待后续扩展。

## 2. 架构

```text
调用方
  │ HTTPS / JSON / SSE
  ▼
HTTP Server
  ├─ request_id、鉴权、访问日志
  ├─ 请求大小限制、参数校验、并发准入
  ▼
Chat Service
  ├─ 模型别名解析
  ├─ 超时与取消传播
  └─ 上游请求与响应转换
  ▼
Provider Adapter
  └─ HTTP Client（连接复用）
  ▼
大模型上游
```

职责划分：HTTP 层处理协议，Service 层处理请求生命周期和路由，Provider 层处理供应商协议。OAuth provider 通过本地回调完成 PKCE 授权、校验 ID Token、轮换并保护凭证文件，再将内部聊天请求映射为 Responses API 输入。配置与日志、指标独立于业务逻辑。

部署默认由反向代理终止 TLS，Go 服务绑定内网地址；直连公网时必须提供 HTTPS。网关的响应缓冲和空闲超时需要与 SSE 配置一致。

## 3. HTTP 接口

### 3.1 健康与就绪

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/healthz` | 进程存活返回 200；不调用上游 |
| GET | `/readyz` | 配置有效且未进入停机状态返回 200，否则 503 |
| GET | `/v1/models` | 鉴权后返回可用模型别名；不暴露上游密钥或地址 |
| POST | `/v1/chat/completions` | 鉴权后执行聊天请求 |

就绪接口不保证供应商可用，避免上游故障引起本服务反复重启。健康接口仅返回最少信息；指标端点仅向运维网络开放。

### 3.2 聊天请求

```http
POST /v1/chat/completions
Authorization: Bearer <client-token>
Content-Type: application/json
```

```json
{
  "model": "default",
  "messages": [
    {"role": "system", "content": "你是一个简洁的助手。"},
    {"role": "user", "content": "解释 Go 的 context。"}
  ],
  "stream": false,
  "temperature": 0.7,
  "max_tokens": 1024
}
```

初版接受字段：`model`、`messages`、`stream`、`temperature`、`max_tokens`。未知字段返回 400，避免调用方误以为某项能力已经生效。可选数值使用指针表达“未传入”，区分未指定参数与显式的零值。

校验规则：

- 只接受 `application/json`，其他类型返回 415；请求体默认上限 1 MiB，超限返回 413。
- JSON 必须只有一个完整对象，拒绝尾随数据与非法 JSON。
- `model` 必须命中已配置别名；未知模型返回 400，不允许任意上游 URL。
- `messages` 数量为 1～200；`role` 只支持 `system`、`user`、`assistant`，`content` 为非空字符串。
- 初版约定 `temperature` 范围为 0～2，`max_tokens` 为正整数；模型配置可以收紧范围或声明不支持某项参数，不支持时返回 400。
- 字节数和消息数限制不代表模型 token 限制。初版不声称准确计算输入 token；上下文超限由上游检测并转换为可识别错误。

### 3.3 非流式响应

以下为本服务拟采用的响应契约示例，具体值由上游返回结果生成：

```json
{
  "id": "chatcmpl-example",
  "object": "chat.completion",
  "created": 1791590400,
  "model": "default",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "context 用于传播取消信号和截止时间。"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 20, "completion_tokens": 15, "total_tokens": 35}
}
```

响应中的 `model` 使用公开别名。上游未提供 usage 时省略该字段，不估算或填零冒充实际用量。对上游响应设置独立大小上限，默认 8 MiB；无效或超限响应按上游协议错误处理。

### 3.4 流式响应

`stream=true` 时返回 `Content-Type: text/event-stream`、`Cache-Control: no-cache` 和 `X-Accel-Buffering: no`。确认上游状态成功且具备合法 SSE 类型后，再提交下游 200 响应；每个完整事件写出后立即 Flush。

```text
data: {"id":"chatcmpl-example","object":"chat.completion.chunk","model":"default","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}

data: [DONE]

```

Provider 解析完整 SSE 事件并规范化后输出，支持跨网络读取分片、多行 data 及 CRLF。注释心跳不计入首个有效内容时间。单事件默认上限 1 MiB，避免无界内存增长。使用逐事件读写和有界缓冲，慢客户端自然施加背压。

已经提交响应后，HTTP 状态码不可修改。中途错误输出一个 `data: {"error":{...}}` 事件，随后关闭连接，不发送表示正常完成的 `[DONE]`；调用方必须能够识别流内错误。上游未发送终止标记就断开也视为不完整响应。该错误扩展需要纳入客户端契约测试。

## 4. 错误契约

所有 JSON 错误响应具有相同结构，同时在响应头返回 `X-Request-ID`：

```json
{
  "error": {
    "code": "upstream_timeout",
    "message": "大模型服务响应超时",
    "request_id": "req-example"
  }
}
```

| HTTP 状态 | code 示例 | 含义 |
| --- | --- | --- |
| 400 | `invalid_request` / `unknown_model` / `context_length_exceeded` | 请求无效或超出模型上下文限制 |
| 401 | `unauthorized` | 本服务客户端密钥缺失或无效 |
| 413 / 415 | `request_too_large` / `unsupported_media_type` | 请求体过大或类型不支持 |
| 429 | `capacity_exceeded` / `upstream_rate_limited` | 本服务并发容量不足或上游限流 |
| 502 | `upstream_error` / `upstream_protocol_error` | 上游失败、供应商鉴权失败或响应协议不合法 |
| 503 | `service_unavailable` | 服务正在停机或不可就绪 |
| 504 | `upstream_timeout` | 上游超过截止时间 |
| 500 | `internal_error` | 服务内部错误 |

供应商密钥错误不能映射为本服务的 401，以免调用方误认为自己的凭证有误。只转换经过识别的供应商错误；其他错误统一处理，不向客户端原样泄露供应商响应、URL 或内部堆栈。客户端取消仅记录取消结果，不尝试向断开的连接写错误。

## 5. Go 模块与目录

优先使用标准库 `net/http`、`encoding/json`、`context`、`log/slog`，初版无需 Web 框架。Go 工具链版本在实现时固定到受支持版本，并由 CI 使用同一版本。

```text
cmd/server/main.go                 # 加载配置、组装依赖、启动与退出
internal/config/config.go          # 环境变量与启动校验
internal/httpapi/handler.go        # 请求与响应契约
internal/httpapi/middleware.go     # 鉴权、request_id、日志、恢复 panic
internal/service/chat.go           # 模型路由、容量准入、生命周期
internal/provider/provider.go      # Provider 接口和内部结果类型
internal/provider/compatible.go    # 兼容协议、错误转换、SSE 解析
internal/observability/metrics.go   # 指标适配
internal/webui/                    # 内嵌的本地聊天工作台
tests/{config,httpapi,provider,service,server,observability}/
                                   # 外部测试和本地假上游
docs/llm-http-server-design.md
```

Provider 的概念接口：

```go
type Provider interface {
    Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
    Stream(ctx context.Context, req ChatRequest) (ChatStream, error)
}

type ChatStream interface {
    Next(ctx context.Context) (ChatEvent, error)
    Close() error
}
```

`Next` 返回完整、规范化的事件，正常结束返回 `io.EOF`；供应商错误使用内部类型携带分类和可重试信息。HTTP Handler 负责最终 JSON/SSE 编码。`Close` 必须释放上游响应体；非流式路径也必须关闭响应体。中间件包装 ResponseWriter 时需保持流式 Flush 能力。

## 6. 配置与密钥

普通配置通过环境变量及部署配置文件提供；API Key 和客户端密钥通过环境变量、当前工作目录的 `.env` 或密钥管理系统注入。入口在参数解析后、登录与服务分流前加载 `.env`，只补充尚不存在的进程变量，已有变量（包括空值）优先。文件缺失时继续；读取失败、超过 1 MiB 或格式错误时退出，错误不包含配置值。完整解析后才注入变量，不执行 shell 或变量展开，不热加载，不搜索其他目录。具体格式与边界见 [.env 加载设计](dotenv-loading-design.md)。OAuth refresh token 单独存入用户配置目录或 `OPENAI_OAUTH_TOKEN_FILE` 指定的文件。下面列出配置项：

| 配置 | 默认值 / 必填 | 用途 |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080` | 监听地址；容器按部署需要覆盖 |
| `METRICS_ADDR` | `127.0.0.1:9090` | 独立指标监听地址；空字符串关闭 |
| `CLIENT_API_KEYS` | 必填 | 本服务允许的客户端 Token，支持轮换重叠期 |
| `MODEL_CONFIG_PATH` | `configs/models.json` | 唯一模型配置文件的默认路径；可显式覆盖 |
| `UPSTREAM_API_KEY_<NAME>` | API Key Provider 必填 | 各供应商密钥 |
| `OPENAI_OAUTH_TOKEN_FILE` | 用户配置目录默认路径 | OpenAI OAuth access/refresh token 文件 |
| `OPENAI_CODEX_TOKEN_FILE` | 用户配置目录默认路径 | 命令行 Codex 登录及服务共用的凭证文件 |
| `MAX_REQUEST_BYTES` | `1048576` | 请求体上限 |
| `MAX_CONCURRENT_REQUESTS` | `64` | 单实例并发上限，流式占用到连接结束 |
| `REQUEST_TIMEOUT` | `120s` | 普通及流式请求的总时限 |
| `UPSTREAM_HEADER_TIMEOUT` | `30s` | 等待上游响应头的时限 |
| `STREAM_FIRST_EVENT_TIMEOUT` | `30s` | 等待首个有效流事件的时限 |
| `STREAM_IDLE_TIMEOUT` | `30s` | 两次有效流事件之间的最长间隔 |
| `DOWNSTREAM_WRITE_TIMEOUT` | `15s` | 每次下游写入时限 |
| `SHUTDOWN_TIMEOUT` | `30s` | 停机等待正在处理的请求 |
| `LOG_LEVEL` | `info` | 日志级别 |
| `CHAT_LOG_DIR` | `logs/requests` | 会话日志目录，一会话一文件，空值关闭 |

模型配置统一存储在 `configs/models.json`，提供商与模型能力的默认值见 README。API Key 模式配置示意：

```json
{
  "providers": {
    "primary": {
      "chat_url": "https://llm.example.com/v1/chat/completions",
      "api_key_env": "UPSTREAM_API_KEY_PRIMARY"
    }
  },
  "models": {
    "default": {
      "provider": "primary",
      "upstream_model": "provider-model-name",
      "supports_stream": true,
      "supports_temperature": true,
      "supports_max_tokens": true,
      "max_output_tokens": 4096
    }
  }
}
```

OpenAI OAuth provider 使用固定配置，不接受自定义上游地址或 API Key 环境变量：

```json
{
  "providers": {"openai": {"auth_type": "openai_oauth"}},
  "models": {
    "default": {
      "provider": "openai",
      "upstream_model": "account-available-model",
      "supports_stream": true,
      "supports_temperature": true,
      "supports_max_tokens": true,
      "max_output_tokens": 4096
    }
  }
}
```

OAuth 仅用于 OpenAI 官方公开的 Responses API 流程：上游请求设置 `stream:true` 和 `store:false`，再将 `response.output_text.delta` 转为本服务的 Chat Completions 风格 SSE。普通下游请求由服务聚合内部流。首次授权通过 loopback callback 使用 PKCE，并验证 state、nonce、签名、issuer、audience、过期时间和 `chatgpt.tokens.use.direct` scope。凭证以限制权限的文件原子写入；刷新时整体替换 access token 与轮换后的 refresh token。授权回调和控制端点只允许本机访问，启动、状态和退出操作还需要本服务客户端令牌。

默认的 `openai_codex` provider 使用独立的命令行 OAuth 流程（`--login`），请求固定的 `https://chatgpt.com/backend-api/codex/responses` 并携带账号 ID。模型项仅需 `provider` 和 `upstream_model`；默认开启 stream，关闭 temperature 和 max_tokens，因此不要求输出上限。API Key 和网页 OAuth 模式默认开启这些能力，输出上限默认为 4096；显式配置可覆盖默认值。

启动时校验密钥、URL、模型引用和所有数值配置；无效则直接退出。上游地址由运维固定，默认要求 HTTPS，不接受用户输入地址。HTTP Client 禁止跟随重定向，防止携带凭证转发到其他主机。配置修改通过重启生效，初版不提供热加载。

## 7. 超时、取消与容量控制

- 复用 HTTP Client 和 Transport，设置连接、TLS 握手、响应头超时以及连接池容量。
- 每个请求派生带总截止时间的 context；客户端断开、超时或停机取消时，立即取消上游请求并释放并发名额。
- 流式首事件和空闲超时通过可取消的上游读取实现，超时后关闭响应体；避免启动无法退出的读取 goroutine。
- 下游写入设置每次写入的截止时间，避免慢客户端永久占用资源。Server 设置请求头读取超时和空闲连接超时；SSE 写入期限单独管理，避免固定 WriteTimeout 提前截断流。
- 并发达到上限时立即返回 429，不排队。该限制为单实例容量控制；跨副本配额及每用户速率限制后续由网关或共享组件实现。
- 初版不自动重试、不自动切换供应商。请求可能已经在上游执行并产生费用，HTTP POST 的重试无法保证幂等；后续需明确去重能力、预算和允许重试的阶段再引入策略。

## 8. 安全与可观测性

客户端 Token 使用恒定时间比较，支持同时配置新旧密钥进行轮换。不记录 Authorization 或供应商凭证。控制台访问日志仅记录元数据；会话日志按 `X-Session-ID` 汇总为一个文件，每轮结束后一次追加完整请求、合并回复和最终状态，不保存逐条流式事件。`CHAT_LOG_DIR` 空值关闭会话日志。默认关闭 CORS。

服务生成受控格式的 request_id；只有格式和长度校验通过的入站 ID 才能复用。响应头与错误体携带同一 ID。

结构化日志记录：request_id、路径、公开模型别名、Provider 名称、HTTP 状态、业务结果、耗时、是否流式和取消原因。流式 HTTP 200 可能随后失败，因此业务结果必须单独记录。

指标包括请求总数、错误数、在途请求数、容量拒绝数、总耗时、上游首内容耗时和上游错误分类。供应商返回 usage 时记录 token 用量；缺失 usage 的请求单独计数。指标标签只使用有限集合，禁止 request_id、用户文本和上游原始错误作为标签。

## 9. 启动与优雅退出

启动顺序：加载并校验配置 → 初始化日志及共享 HTTP Client → 注册路由 → 监听端口。

收到退出信号后标记未就绪并拒绝新的业务请求，调用 HTTP Server 的 Shutdown 等待在途请求。宽限期内允许正在返回的 SSE 完成；超过停机时限后取消应用根 context，关闭仍在运行的上游流及下游连接，最后清理空闲连接。平台信号处理按实际部署环境实现。

## 10. 实施顺序与验收

1. 建立 Go 模块、配置校验、健康接口和鉴权中间件。
2. 实现模型别名路由、非流式 Provider、请求校验及统一错误响应。
3. 实现 SSE 解析和转发、首事件/空闲/总超时、取消传播与并发准入。
4. 补充指标、优雅退出、部署示例及接口使用说明。

通过本地 `httptest` 假上游验证，无需真实密钥或产生模型调用费用：

- 普通响应正确转换，模型别名、可选字段和缺失 usage 按契约处理。
- 鉴权失败、非法 JSON、尾随 JSON、未知字段、超大请求和模型不存在均正确返回。
- 上游 401、429、5xx、非法响应及响应超限被正确分类且不泄露凭证。
- SSE 多行数据、网络分片、CRLF、心跳、终止标记及流内错误正确处理。
- 客户端取消、超时、慢客户端和上游异常断流能够停止工作并释放资源。
- 并发上限生效，流式结束才释放名额；Shutdown 宽限期及强制取消符合设计。

实现完成后运行格式检查、`go vet ./...`、`go test ./...`；支持 race detector 的 CI 环境运行 `go test -race ./...`。再对目标供应商做小规模契约验证，确认参数支持、错误映射和流式行为。压力验证重点观察连接数、内存和 goroutine 是否在请求结束后回落。

## 11. 后续扩展

根据实际需求添加供应商适配器、工具调用消息、结构化输出、按租户配额和计费记录。会话持久化与 Agent 编排应作为独立模块设计，避免与 HTTP 转发服务的请求生命周期耦合。新增能力需同时更新公开字段契约、模型能力配置和供应商契约测试。

## 12. 初版实现说明

代码结构和使用方法见项目 README。请求/响应及 Provider 接口放在 `internal/chat`，进程组装与停机放在 `internal/server`；HTTP 接口和中间件共同放在 `internal/httpapi`。六组外部测试独立放在项目根目录 `tests/`。本地工作台的 HTML/CSS/JS 通过 `internal/webui` 嵌入可执行文件，无需单独部署前端服务器。工具链固定为 Go 1.26.5，无第三方 Go 依赖。

总请求时限从业务准入开始计算；HTTP 请求头读取时限为 5 秒，请求体读取时限为 15 秒。上游首有效事件时限在收到合法 SSE 响应头后开始计算，SSE 注释心跳不会重置时限。空闲时限约束每次上游事件读取，下游背压期间另由写入及总请求时限约束。

初版记录首有效 JSON 事件延迟，不单独区分角色事件和文本内容事件。指标提供计数与耗时累计值，不提供分位数或直方图。模型配置中的 `max_output_tokens` 校验显式传入的参数；省略参数时保留供应商默认行为。

本地假上游测试可验证实现协议和资源释放行为；真实供应商契约验证与生产规模压力测试仍需在接入阶段进行。
