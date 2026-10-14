# hao-agent-harness

使用 Go 实现的大模型请求 HTTP 服务端。向调用方提供统一的聊天接口，支持模型别名路由、普通 JSON 响应和 SSE 流式响应。客户端 API 不校验 API Key；服务默认仅监听本机。

详细需求与边界见 [设计文档](docs/llm-http-server-design.md)。实现支持 Chat Completions 风格接口的一个明确子集；供应商接入前需要验证参数和流式协议兼容性。

代码改动遵循先设计后执行、补全中文注释并同步文档的规范，详见 [AGENTS.md](AGENTS.md)。

OpenAI Codex 的登录、凭证刷新和故障排查见 [完整认证流程](docs/openai-codex-auth-flow.md)。

前端 Markdown 解析、安全过滤和标签处理约定见 [前端消息渲染说明](docs/frontend-rendering.md)。

## 项目结构

```text
cmd/server/            程序入口、信号处理
internal/chat/         请求、响应、Provider 接口和公共错误类型
internal/config/       YAML 配置加载、校验与热加载
internal/httpapi/      HTTP 协议、SSE 输出和访问日志
internal/service/      模型路由、并发准入和请求生命周期
internal/provider/     上游 HTTP 调用、响应校验和 SSE 解析
internal/observability/ 有限标签的 Prometheus 文本指标
internal/server/       依赖组装、监听器管理和优雅退出
internal/webui/        本地工作台和内嵌静态资源
tests/                 按配置、接口、供应商和生命周期分组的独立测试
configs/models.json    唯一模型配置文件
docs/                  设计文档
```

HTTP 层依赖业务服务，业务服务依赖 `chat.Provider` 接口，供应商适配器实现该接口。配置和进程生命周期由入口组装，测试可以使用本地假上游而无需真实模型密钥。

## 本地运行

项目工具链固定为 Go 1.26.5。运行配置统一在 `configs/models.json`，默认使用 OpenAI Codex。按账号可用模型编辑 `upstream_model`，首次使用先登录；已登录可直接启动。

YAML 解析依赖 `gopkg.in/yaml.v3` v3.0.1（MIT/Apache-2.0）；Go 模块会自动下载依赖。

PowerShell：

```powershell
go run ./cmd/server --login
go run ./cmd/server
```

若要使用网页 OpenAI OAuth，将配置中心中的 `auth_type` 改为 `openai_oauth` 并设置 `oauth_token_file` 后启动服务，再打开 `http://127.0.0.1:8080/`，在“模型与账号”点击 **Continue with ChatGPT**。授权回调要求通过 `127.0.0.1` 打开本地页面，不要使用 `localhost`。OAuth access/refresh token 保存在 `oauth_token_file` 指定的独立凭证文件。

该模式使用 OpenAI Docs 的 Sign in with ChatGPT 授权流程和 Responses API；上游请求始终启用流式响应并设置 `store:false`，即使本地客户端请求普通 JSON，服务也会在服务器内部读取并聚合流。授权范围和模型可用性由 OpenAI 账号决定，配置中的 `upstream_model` 必须是该账号可用的模型。API Key provider 的密钥直接配置在本地 YAML 中，务必避免提交真实密钥。

Linux/macOS：

```sh
go run ./cmd/server --login
go run ./cmd/server
```

服务和 `--login` 均从 `configs/models.json` 读取配置，不读取 `.env` 或进程环境变量。配置文件内容为 YAML，尽管保留 `.json` 文件名。修改文件后约 1 秒内会校验并热加载；无效配置会保留上一份有效配置。默认 API 监听 `127.0.0.1:8080`。取消客户端密钥校验后，能够连到监听地址的客户端都可调用模型；绑定非 loopback 地址前，需用防火墙或受控网络限制访问。

## 命令行 OAuth 登录（参考 pi）

新增的 `openai_codex` 模式参考 [pi 的 OpenAI Codex OAuth 实现](https://github.com/earendil-works/pi/blob/main/packages/ai/src/auth/oauth/openai-codex.ts)。它与上面的 `openai_oauth` 网页授权是两个独立模式：登录端点为 `https://auth.openai.com/oauth/authorize`，回调为 `http://localhost:1455/auth/callback`，推理端点为 `https://chatgpt.com/backend-api/codex/responses`，请求携带 `ChatGPT-Account-Id`。

```powershell
# 登录路径从配置中心的 openai_codex.codex_token_file 读取。
go run ./cmd/server --login
# 也支持 --login openai 和 --login=openai-codex。

# 在无桌面环境中手动打开登录地址，并粘贴完整回调 URL。
go run ./cmd/server --login openai --no-browser --login-timeout 5m

# 登录完成后使用现有的 configs/models.json 启动服务。
# 编辑 configs/models.json，将 upstream_model 改为账号可用的 Codex 模型。
go run ./cmd/server
```

`--login` 默认登录 OpenAI Codex，成功后保存凭证并退出，不启动 API 或指标服务。自动打开浏览器；不能自动回调或端口 1455 被占用时，可以在终端粘贴本次登录的完整回调 URL。登录使用 PKCE 与随机 state 校验，默认 10 分钟超时，支持 Ctrl+C 取消。当前命令行 OAuth 仅支持 `openai` / `openai-codex`（二者指向同一提供商）；其他提供商继续使用 API Key 配置。

Codex 凭证路径由 `configs/models.json` 的 `providers.<名称>.codex_token_file` 指定，登录和服务启动共用该路径。服务会在访问令牌临近过期时刷新并保存轮换凭证。该文件包含账号访问令牌，不要提交到版本库。当前保存一个活动 Codex 账号；重新登录前停止服务，登录后再启动。

Codex 模式复用现有 Responses SSE 解析，支持普通 JSON 聚合响应与 SSE 输出，处理 `response.done`，将 system 消息转换为 `instructions`，设置 `stream:true` 和 `store:false`。Codex 默认关闭 temperature 和 max_tokens；传入不支持的参数会被拒绝。模型和账户权限仍需通过真实账号验证；[OpenAI Docs 的认证说明](https://learn.chatgpt.com/docs/auth)说明了账号登录与 API Key 的区别。

## 调用接口

所有业务接口均不要求客户端凭证：

```sh
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

查询模型别名：

```sh
curl http://127.0.0.1:8080/v1/models
```

普通聊天请求：

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"解释 Go 的 context"}],"max_tokens":1024}'
```

SSE 流式请求：

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

这些 curl 示例使用 POSIX shell 语法。Windows 下可使用 `Invoke-RestMethod`：

```powershell
$body = @{ model = 'default'; messages = @(@{ role = 'user'; content = '你好' }) } | ConvertTo-Json -Depth 5
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/v1/chat/completions' -ContentType 'application/json; charset=utf-8' -Body ([System.Text.Encoding]::UTF8.GetBytes($body))
```

接口接受 `model`、`messages`、`stream`、`temperature`、`max_tokens`；未知字段返回 400。只支持文本消息及 `system`、`user`、`assistant` 角色。`temperature=0` 会被完整保留；没有指定的可选参数交给供应商默认处理。`max_output_tokens` 限制显式传入的 `max_tokens`，未传入时不会覆盖供应商默认值。

返回的模型名称始终是公开别名。供应商不提供 usage 时省略该字段，不推算 token 用量。流式正常结束发送 `data: [DONE]`；流中失败发送 `data: {"error":...}` 后关闭，不发送 `[DONE]`。客户端需要同时识别 HTTP 错误和流内错误。

## 配置

所有应用配置都放在 `configs/models.json`。文件扩展名为 `.json` 仅为兼容现有路径，内容必须是 YAML；旧 JSON 内容会被拒绝。配置包含：

- `server`：监听地址、请求大小和并发上限、业务与流式超时、停机宽限期。
- `logging`：日志级别与会话日志目录；空目录关闭会话日志。
- `providers`：上游类型、固定 HTTPS 地址、API Key 或 OAuth 凭证文件路径。
- `models`：公开模型别名、上游模型名及可选参数能力。

API Key provider 的密钥用 `api_key` 字段直接存入本地 YAML。配置可能含敏感信息，应限制文件权限且不要提交真实密钥。OAuth 访问/刷新令牌仍存放在独立文件中；`oauth_token_file` 或 `codex_token_file` 只配置文件路径。

```yaml
server:
  http_addr: 127.0.0.1:8080
  metrics_addr: 127.0.0.1:9090 # 空字符串关闭独立指标监听
  max_request_bytes: 1048576
  max_concurrent_requests: 64
  request_timeout: 120s
  upstream_header_timeout: 30s
  stream_first_event_timeout: 30s
  stream_idle_timeout: 30s
  downstream_write_timeout: 15s
  shutdown_timeout: 30s
logging:
  level: info # debug、info、warn、error
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

API Key provider 示例：

```yaml
providers:
  primary:
    auth_type: api_key
    chat_url: https://api.openai.com/v1/chat/completions
    api_key: replace-with-your-provider-key
```

模型能力未显式填写时，Codex 默认开启 stream 并关闭 temperature/max_tokens；其他 provider 默认开启这三项。支持 max_tokens 时输出上限默认 4096。配置严格校验未知字段、Provider URL、密钥和模型引用。

服务每秒检查一次文件内容。配置先完整解析、校验并构造新服务快照，再整体切换；已经开始的请求继续使用旧快照，新请求使用新快照。无效 YAML、校验失败或新监听地址绑定失败会保留旧配置并记录脱敏错误。修改 HTTP 或 metrics 地址时，新地址先启动，随后旧 listener 才停止并排空请求。无需重启进程，也没有管理 API 或远程配置服务。相对日志和凭证路径按进程当前工作目录解析。

## 运行与观测

- API 默认仅监听本机。生产环境通过网关提供 HTTPS，并关闭 SSE 响应缓冲、设置适当的流式空闲超时。
- `http://127.0.0.1:9090/metrics` 输出请求量、错误分类、在途请求、耗时累计值和计数、首事件延迟及 token 用量；没有分位数或 histogram bucket。
- 浏览器打开 `http://127.0.0.1:8080/` 可使用内置工作台；它会直接连接服务并加载配置中心中的模型。对话自动保存在当前浏览器的 localStorage，刷新或重启服务后恢复；通过“导出对话”可下载全部会话的 JSON 备份。恢复时使用同一浏览器、地址和端口。
- 助手回复支持 Markdown 标题、粗体、列表、引用、代码块、表格和任务列表；流式输出与历史回看使用相同格式。解析与 HTML 安全过滤资源随服务打包，无需连接 CDN。用户输入仍按原文显示。
- 指标监听器独立于 API；因指标端点无鉴权，部署时限制在运维网络内，不要直接暴露公网。
- 控制台访问日志为 JSON 元数据；会话日志默认另存到 `logs/requests/session-<会话ID>.log`。一个会话一个文件，每轮回复结束后一次追加格式化的完整请求与合并后的回复，生成过程中不写文件。前端自动传递 `X-Session-ID`，其他客户端可复用同一请求头关联多轮对话。`logging.chat_log_dir` 可以指定目录，设置为空可关闭详细日志。凭证不写入日志。文件说明见 [对话保存与请求日志](docs/conversation-and-logs.md)。
- 达到容量上限立即返回 429，不排队；无自动重试或供应商自动切换。
- 收到退出信号后停止准入，允许在途请求在宽限期内完成；超时后取消上游请求并关闭仍在运行的连接。
- 普通响应上限为 8 MiB，SSE 单事件上限为 1 MiB；异常断流按协议错误处理。注释心跳不重置首事件或空闲计时。

## 验证与构建

测试文件统一放在根目录 `tests/`：`internal/<模块>/` 对应 `tests/<模块>/`，`cmd/<命令>/` 对应 `tests/cmd/<命令>/`；生产目录不放测试文件。配置加载测试位于 `tests/config/`，命令行入口测试位于 `tests/cmd/server/`，通过公开接口和实际程序行为验证，无需导出私有函数。

```sh
gofmt -w cmd internal tests
go vet ./...
go test -timeout 60s ./...
go build -trimpath -o bin/server ./cmd/server
```

Windows 可将输出路径改为 `bin/server.exe`。具备 CGO 和 C 编译器的环境执行：

```sh
go test -race -timeout 60s -coverprofile=coverage.out ./...
```

测试覆盖 YAML 配置校验与热加载、无客户端鉴权的接口行为、日志隐私、路由和可选参数、上游错误映射、响应大小限制、SSE 分片与多行数据、超时、取消、并发名额释放、指标隔离及优雅/强制停机。CI 执行格式检查、vet、race 测试和构建。

命令行测试会编译一次临时可执行文件，并在隔离工作目录中验证参数、退出码和 YAML 加载顺序，不需要真实账号或网络请求。若 Windows Defender 或执行策略阻止该程序启动，测试会明确报告启动失败；这属于环境阻断，不能视为测试通过。仅验证本次配置和命令行改动可执行 `go test -timeout 60s ./tests/config ./tests/cmd/server`。

前端 Markdown 回归测试使用 Node.js 26，覆盖普通/流式回复、历史回看、停止生成、错误保留和 HTML 安全过滤；Node.js 仅用于测试，运行服务不需要安装：

```powershell
npm ci --prefix tests/webui --ignore-scripts
npm test --prefix tests/webui
```

## Docker

镜像包含静态服务二进制和 HTTPS CA 证书，以非 root 用户运行：

```sh
docker build -t hao-agent-harness .
docker run --rm -p 127.0.0.1:8080:8080 \
  --mount type=bind,src="$(pwd)/configs/models.json",dst=/configs/models.json,readonly \
  --mount type=bind,src="/absolute/path/to/credential-directory",dst=/credentials \
  hao-agent-harness
```

配置中的 OAuth 凭证路径需指向容器内可写目录，例如把 `codex_token_file` 设为 `/credentials/openai-codex.json`；运行前可在宿主机用同一配置路径执行 `--login`，或确保挂载目录可写。YAML 配置对容器用户只需可读；若使用 API Key provider，密钥保存在该 YAML 文件中并应限制访问。客户端 API 无密钥校验，容器端口仅绑定宿主机回环地址；metrics 端口仍应限制在运维网络内。

实现参考 Go 官方 [net/http 文档](https://pkg.go.dev/net/http)，CI 配置参考官方 [checkout](https://github.com/actions/checkout) 和 [setup-go](https://github.com/actions/setup-go) 用法。
