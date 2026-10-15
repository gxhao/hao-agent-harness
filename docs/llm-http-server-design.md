# Go 大模型请求 HTTP 服务端设计

状态：已实现；真实供应商契约仍需按接入账号验证
更新：2026-10-15
项目：hao-agent-harness

## 1. 目标和边界

服务提供统一的 Chat Completions 风格 HTTP 接口，负责请求校验、模型别名路由、上游协议转换、SSE 流式转发、运行指标和优雅退出。支持兼容 Chat Completions 的 API Key Provider、OpenAI Responses API OAuth Provider，以及 OpenAI Codex Provider。

服务不提供用户系统、客户端密钥鉴权、计费、工具执行、Agent 编排、远程配置后台或数据库。客户端 API 不校验 Bearer token；默认只绑定 loopback。任何非 loopback 部署都必须由防火墙、VPN 或受控网关限制访问。API 调用可能产生上游费用。

## 2. 架构与请求路径

```text
客户端 / 本地工作台
  │ HTTP JSON / SSE（无客户端 Bearer 校验）
  ▼
HTTP Server
  ├─ request_id、访问日志
  ├─ 请求大小限制、参数校验、并发准入
  ▼
Chat Service
  ├─ 模型别名解析
  ├─ 超时与取消传播
  └─ 上游请求与响应转换
  ▼
Provider Adapter ── HTTP Client ── 大模型上游
```

HTTP 层处理接口契约，Service 层管理请求生命周期和路由，Provider 层适配上游。请求和 Provider 只使用内存中的当前配置快照。配置切换不修改已开始请求的快照。

## 3. HTTP 契约

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/healthz` | 进程存活；不调用上游 |
| GET | `/readyz` | 当前配置有效且服务未停机时返回 200 |
| GET | `/v1/models` | 返回公开模型别名；无客户端鉴权 |
| POST | `/v1/chat/completions` | 执行聊天请求；无客户端鉴权 |
| GET | `/metrics` | 独立 metrics listener；无客户端鉴权 |

请求示例：

```http
POST /v1/chat/completions
Content-Type: application/json
```

```json
{
  "model": "default",
  "messages": [{"role": "user", "content": "解释 Go 的 context。"}],
  "stream": false,
  "temperature": 0.7,
  "max_tokens": 1024
}
```

接口接受 `model`、`messages`、`stream`、`temperature`、`max_tokens`。未知字段返回 400；只支持文本消息及 `system`、`user`、`assistant` 角色。JSON 只能包含一个完整对象；请求默认上限 1 MiB，最大可配置到 64 MiB。`model` 命中配置别名时使用其 Provider 与上游模型名。未命中时，若配置了 `models.default` 且其 provider 为 `openai`，服务将请求中的模型名作为上游模型名，沿用 `default` 的参数能力，并通过已配置的 `openai` Provider 调用；否则返回 `unknown_model`。响应回显请求模型名，指标以固定的 `default` 别名计数。此回退只允许选择配置中的 Provider，不接受客户端 URL 或凭证。

正常非流式响应返回兼容 Chat Completions 的 JSON；`stream=true` 返回 SSE。流内错误会以 `data: {"error":...}` 事件发送并关闭连接，不发送 `[DONE]`。客户端需要同时检查 HTTP 状态和流内错误。上游缺少 usage 时不估算 token 数。

## 4. 配置中心与热加载

固定配置源是 `configs/models.json`，文件内容为 YAML。保留 `.json` 文件名是为了兼容用户指定的现有路径；解析器只接受 YAML 语法，拒绝旧 JSON 文档。配置包含监听地址、所有请求/流式/停机限制、日志级别和目录、Provider URL/类型/API Key/OAuth 文件路径、模型别名和能力声明。进程环境变量和 `.env` 均不参与配置。

最小配置结构示例：

```yaml
server:
  http_addr: 127.0.0.1:8080
  metrics_addr: 127.0.0.1:9090
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
  chat_log_dir: logs/requests
providers:
  openai:
    auth_type: openai_codex
    codex_token_file: credentials/openai-codex.json
models:
  default:
    provider: openai
    upstream_model: gpt-6-luna
    supports_stream: true
    supports_temperature: false
    supports_max_tokens: false
```

解析器限制配置文件为 1 MiB，拒绝未知字段和尾随 YAML 文档。加载会校验 duration、监听地址、Provider 字段、HTTPS URL、模型引用、能力和必需凭证路径；错误不包含密钥值。API Key Provider 的 `api_key` 直接位于 YAML，因此实际配置文件应限制文件权限并避免提交真实密钥。OAuth token 会轮换，仍单独保存在 `oauth_token_file` 或 `codex_token_file` 指定的文件中。

配置加载及应用流程：

1. 进程启动时读取、解析并校验 YAML；失败则不启动 listener。
2. 运行时每秒计算文件内容摘要。内容变化后重新读取并完整校验。
3. 先构建新的 Provider、Service、HTTP Handler 和 OAuth manager。若监听地址改变，先绑定并启动新 listener。
4. 所有步骤成功后，原子切换新配置快照。新请求使用新配置；已开始请求保持旧快照直到完成。
5. 切换后排空被替换的 listener，并在旧快照不再被请求引用后释放其 context 和 HTTP 连接。
6. 解析、校验、依赖构建或 listener 绑定失败时，记录脱敏错误并继续用旧快照服务。文件内容修正后 watcher 会再次尝试。

监听地址更新时，旧、新 listener 短暂并存；无法绑定新地址时不会关闭旧 listener。热加载是本地文件能力，不提供配置管理 API、远程存储或分布式一致性。

## 5. Provider 与 OAuth

API Key Provider 使用固定 HTTPS `chat_url` 和 YAML 中的 `api_key`。禁止 URL credentials、query、fragment 及自动重定向。Codex 与 OpenAI OAuth 使用固定 Responses API endpoint，凭证分别保存在独立文件中。OAuth callback 使用 loopback、state、nonce 和 PKCE 保护；本服务的 OAuth start/status/logout 控制接口不要求客户端 Bearer，但只允许 loopback，start/logout 另校验同源 Origin。

上游认证头和客户端输入不会写入日志。Provider 密钥错误转换为通用上游错误，不把上游响应正文、URL 或堆栈发给客户端。

## 6. 错误与限制

| HTTP | code 示例 | 含义 |
| --- | --- | --- |
| 400 | `invalid_request`、`unknown_model` | 请求无效或模型别名不存在 |
| 413 / 415 | `request_too_large`、`unsupported_media_type` | 请求体过大或媒体类型不支持 |
| 429 | `capacity_exceeded`、`upstream_rate_limited` | 本地容量已满或上游限流 |
| 502 | `upstream_error`、`upstream_protocol_error` | 上游失败或响应不符合协议 |
| 503 | `service_unavailable`、`oauth_login_required` | 服务停机、未登录或 OAuth 凭证不可用 |
| 504 | `upstream_timeout` | 上游等待超过截止时间 |

并发达到上限立即返回 429，不排队。服务不自动重试或切换 Provider，以免重复执行产生费用。流式响应开始后发生错误时，HTTP 状态不能修改，因此以 SSE 错误事件结束。

## 7. 生命周期与观测

请求总超时默认 120 秒；上游响应头、首事件、事件空闲、下游写入各有单独限制。客户端取消、超时和停机信号向上游传播。Shutdown 默认等待 30 秒，之后取消上游请求并强制关闭连接。

访问日志记录 request ID、路由、公开模型别名、provider、状态、业务结果和耗时，不记录提示词、回复、Authorization 或供应商凭证。会话日志是独立功能，默认按会话保存完整请求与合并回复；设置 `logging.chat_log_dir: ""` 可关闭。

Prometheus 指标包括请求量、错误、在途请求、容量拒绝、耗时和 token 用量。标签只使用有限集合，不包含 request ID、用户内容或上游原始错误。metrics listener 无鉴权，必须限制在运维网络内。

## 8. 测试与代码结构

生产代码位于 `cmd/` 和 `internal/`；测试及夹具统一放在根目录 `tests/`。配置加载、无鉴权 HTTP 契约、OAuth 控制边界、Provider 协议和 listener 生命周期均用本地测试验证，不要求真实密钥或上游调用。

```text
cmd/server/                  配置加载、CLI 登录、信号处理
internal/config/              YAML 解析、校验、文件热监视
internal/httpapi/             Chat Completions / SSE 契约
internal/service/             模型路由、并发和请求生命周期
internal/provider/            上游协议和 HTTP Client
internal/openaioauth/         OAuth 凭证管理和刷新
internal/server/               快照切换、listener 更新、优雅退出
internal/webui/                内嵌工作台及静态资源
tests/{config,httpapi,server,provider,openaioauth}/
```

实现后运行 `gofmt`、`go vet ./...`、`go test -timeout 60s ./...` 和 `go build ./cmd/server`。详细调用示例和运行说明见 [README](../README.md)。
