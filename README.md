# hao-agent-harness

使用 Go 标准库实现的大模型请求 HTTP 服务端。向调用方提供统一的聊天接口，支持模型别名路由、Bearer 鉴权、普通 JSON 响应和 SSE 流式响应。没有第三方 Go 依赖。

详细需求与边界见 [设计文档](docs/llm-http-server-design.md)。实现支持 Chat Completions 风格接口的一个明确子集；供应商接入前需要验证参数和流式协议兼容性。

## 项目结构

```text
cmd/server/            程序入口、信号处理
internal/chat/         请求、响应、Provider 接口和公共错误类型
internal/config/       配置加载与启动校验
internal/httpapi/      HTTP 协议、鉴权、SSE 输出和访问日志
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

项目工具链固定为 Go 1.26.5。项目只保留 `configs/models.json` 一个模型配置文件，默认使用 OpenAI Codex。按账号可用模型编辑 `upstream_model`，首次使用先登录；已登录可直接启动。

PowerShell：

```powershell
go run ./cmd/server --login
$env:CLIENT_API_KEYS = 'your-client-token'
go run ./cmd/server
```

若要使用网页 OpenAI OAuth，将同一文件的 `auth_type` 改为 `openai_oauth` 后启动服务，再打开 `http://127.0.0.1:8080/`，在“连接设置”点击 **Continue with ChatGPT**。授权回调要求通过 `127.0.0.1` 打开本地页面，不要使用 `localhost`。该模式的凭证保存在用户配置目录下的 `hao-agent-harness/openai-oauth.json`，可通过 `OPENAI_OAUTH_TOKEN_FILE` 指定位置。

该模式使用 OpenAI Docs 的 Sign in with ChatGPT 授权流程和 Responses API；上游请求始终启用流式响应并设置 `store:false`，即使本地客户端请求普通 JSON，服务也会在服务器内部读取并聚合流。授权范围和模型可用性由 OpenAI 账号决定，配置中的 `upstream_model` 必须是该账号可用的模型。当前工作台保存一个活动 OpenAI 账号，不使用 `UPSTREAM_API_KEY_PRIMARY`。API Key provider 配置仍可照常使用。

Linux/macOS：

```sh
go run ./cmd/server --login
export CLIENT_API_KEYS='your-client-token'
go run ./cmd/server
```

`.env.example` 只用于展示环境变量；程序不会自动读取 `.env`。密钥在启动时从环境读取，不写入模型配置文件。供应商地址必须为固定 HTTPS 地址，禁止 URL 内嵌凭证、查询参数和重定向。

## 命令行 OAuth 登录（参考 pi）

新增的 `openai_codex` 模式参考 [pi 的 OpenAI Codex OAuth 实现](https://github.com/earendil-works/pi/blob/main/packages/ai/src/auth/oauth/openai-codex.ts)。它与上面的 `openai_oauth` 网页授权是两个独立模式：登录端点为 `https://auth.openai.com/oauth/authorize`，回调为 `http://localhost:1455/auth/callback`，推理端点为 `https://chatgpt.com/backend-api/codex/responses`，请求携带 `ChatGPT-Account-Id`。

```powershell
# 独立登录，无需先设置 CLIENT_API_KEYS 或 MODEL_CONFIG_PATH。
go run ./cmd/server --login
# 也支持 --login openai 和 --login=openai-codex。

# 在无桌面环境中手动打开登录地址，并粘贴完整回调 URL。
go run ./cmd/server --login openai --no-browser --login-timeout 5m

# 登录完成后使用现有的 configs/models.json 启动服务。
# 编辑 configs/models.json，将 upstream_model 改为账号可用的 Codex 模型。
$env:CLIENT_API_KEYS = 'your-client-token'
# 启动默认读取 configs/models.json，不必设置 MODEL_CONFIG_PATH。
go run ./cmd/server
```

`--login` 默认登录 OpenAI Codex，成功后保存凭证并退出，不启动 API 或指标服务。自动打开浏览器；不能自动回调或端口 1455 被占用时，可以在终端粘贴本次登录的完整回调 URL。登录使用 PKCE 与随机 state 校验，默认 10 分钟超时，支持 Ctrl+C 取消。当前命令行 OAuth 仅支持 `openai` / `openai-codex`（二者指向同一提供商）；其他提供商继续使用 API Key 配置。

凭证默认保存到用户配置目录的 `hao-agent-harness/openai-codex.json`；可通过 `OPENAI_CODEX_TOKEN_FILE` 指定路径，登录和启动服务必须使用同一路径。服务会在访问令牌临近过期时刷新并保存轮换凭证。该文件包含账号访问令牌，不要提交到版本库。当前保存一个活动 Codex 账号；重新登录前停止服务，登录后再启动。

Codex 模式复用现有 Responses SSE 解析，支持普通 JSON 聚合响应与 SSE 输出，处理 `response.done`，将 system 消息转换为 `instructions`，设置 `stream:true` 和 `store:false`。Codex 默认关闭 temperature 和 max_tokens；传入不支持的参数会被拒绝。模型和账户权限仍需通过真实账号验证；[OpenAI Docs 的认证说明](https://learn.chatgpt.com/docs/auth)说明了账号登录与 API Key 的区别。

## 调用接口

健康检查不需要客户端凭证：

```sh
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

查询模型别名：

```sh
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer your-client-token'
```

普通聊天请求：

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer your-client-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"解释 Go 的 context"}],"max_tokens":1024}'
```

SSE 流式请求：

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer your-client-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

这些 curl 示例使用 POSIX shell 语法。Windows 下可使用 `Invoke-RestMethod`：

```powershell
$headers = @{ Authorization = "Bearer $env:CLIENT_API_KEYS" }
$body = @{ model = 'default'; messages = @(@{ role = 'user'; content = '你好' }) } | ConvertTo-Json -Depth 5
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/v1/chat/completions' -Headers $headers -ContentType 'application/json; charset=utf-8' -Body ([System.Text.Encoding]::UTF8.GetBytes($body))
```

接口接受 `model`、`messages`、`stream`、`temperature`、`max_tokens`；未知字段返回 400。只支持文本消息及 `system`、`user`、`assistant` 角色。`temperature=0` 会被完整保留；没有指定的可选参数交给供应商默认处理。`max_output_tokens` 限制显式传入的 `max_tokens`，未传入时不会覆盖供应商默认值。

返回的模型名称始终是公开别名。供应商不提供 usage 时省略该字段，不推算 token 用量。流式正常结束发送 `data: [DONE]`；流中失败发送 `data: {"error":...}` 后关闭，不发送 `[DONE]`。客户端需要同时识别 HTTP 错误和流内错误。

## 配置

直接编辑 `configs/models.json`，不需要复制模板。最小模型项只需 `provider` 和 `upstream_model`。可在同一文件增加提供商和模型别名。

| 模型字段 | `openai_codex` 默认值 | API Key / `openai_oauth` 默认值 |
| --- | --- | --- |
| `supports_stream` | `true` | `true` |
| `supports_temperature` | `false` | `true` |
| `supports_max_tokens` | `false` | `true` |
| `max_output_tokens` | 不使用，可省略 | 支持 max_tokens 时为 `4096` |

显式设置的 `false` 会保留。能力声明须与实际模型匹配；Codex 不允许开启 max_tokens。API Key 提供商省略 `auth_type` 即为 `api_key`，在同一文件的 provider 中设置 `chat_url` 和 `api_key_env`，然后设置对应环境变量。例如：

```json
"openai": {
  "auth_type": "api_key",
  "chat_url": "https://api.openai.com/v1/chat/completions",
  "api_key_env": "UPSTREAM_API_KEY_PRIMARY"
}
```

配置文件可提交到版本库，密钥和 OAuth 凭证仍单独存储。

| 环境变量 | 默认值 | 含义 |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080` | API 监听地址 |
| `METRICS_ADDR` | `127.0.0.1:9090` | 独立指标监听地址；空字符串关闭 |
| `CLIENT_API_KEYS` | 必填 | 逗号分隔的客户端 Token，可配置新旧 Token 进行轮换 |
| `MODEL_CONFIG_PATH` | `configs/models.json` | 模型配置 JSON 路径；显式指定时优先使用该路径 |
| `UPSTREAM_API_KEY_PRIMARY` | API Key 模式按需设置 | 由配置中的 `api_key_env` 指定，支持多个供应商；默认 Codex 模式无需设置 |
| `OPENAI_OAUTH_TOKEN_FILE` | 用户配置目录默认路径 | OpenAI OAuth 轮换凭证的本地存储文件，仅 `auth_type: openai_oauth` 时使用 |
| `OPENAI_CODEX_TOKEN_FILE` | 用户配置目录默认路径 | `--login` 和 `auth_type: openai_codex` 共用的 Codex 凭证文件 |
| `MAX_REQUEST_BYTES` | `1048576` | 请求体上限，最大 64 MiB |
| `MAX_CONCURRENT_REQUESTS` | `64` | 单实例聊天并发上限，流式请求持续占用名额 |
| `REQUEST_TIMEOUT` | `120s` | 从业务准入开始计算的总时限，包含上游调用与流式过程 |
| `UPSTREAM_HEADER_TIMEOUT` | `30s` | 等待上游响应头的时限 |
| `STREAM_FIRST_EVENT_TIMEOUT` | `30s` | 确认上游 SSE 响应后等待首个有效 JSON 事件的时限 |
| `STREAM_IDLE_TIMEOUT` | `30s` | 两个有效 JSON 事件之间的上游读取时限 |
| `DOWNSTREAM_WRITE_TIMEOUT` | `15s` | 每次下游写入的时限 |
| `SHUTDOWN_TIMEOUT` | `30s` | 停机宽限期 |
| `LOG_LEVEL` | `info` | `debug`、`info`、`warn`、`error` |

支持的模型能力由模型配置声明。错误配置、缺失密钥、非法 URL 和非正超时会在启动时失败。配置变更通过重启生效。

## 运行与观测

- API 默认仅监听本机。生产环境通过网关提供 HTTPS，并关闭 SSE 响应缓冲、设置适当的流式空闲超时。
- `http://127.0.0.1:9090/metrics` 输出请求量、错误分类、在途请求、耗时累计值和计数、首事件延迟及 token 用量；没有分位数或 histogram bucket。
- 浏览器打开 `http://127.0.0.1:8080/` 可使用内置工作台；在“连接设置”输入 `CLIENT_API_KEYS` 中的令牌并加载模型。令牌放在标签页会话存储里，对话仅保留在当前页面。
- 指标监听器独立于公开 API，部署时限制在运维网络内。不要把未经鉴权的指标端口直接暴露公网。
- 日志为 JSON，仅记录受控元数据，不记录客户端密钥、供应商密钥、提示词或生成内容。
- 达到容量上限立即返回 429，不排队；无自动重试或供应商自动切换。
- 收到退出信号后停止准入，允许在途请求在宽限期内完成；超时后取消上游请求并关闭仍在运行的连接。
- 普通响应上限为 8 MiB，SSE 单事件上限为 1 MiB；异常断流按协议错误处理。注释心跳不重置首事件或空闲计时。

## 验证与构建

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

测试覆盖请求/配置校验、鉴权与日志隐私、路由和可选参数、上游错误映射、响应大小限制、SSE 分片与多行数据、超时、取消、并发名额释放、指标隔离及优雅/强制停机。CI 执行格式检查、vet、race 测试和构建。

## Docker

镜像包含静态服务二进制和 HTTPS CA 证书，以非 root 用户运行：

```sh
docker build -t hao-agent-harness .
docker run --rm -p 127.0.0.1:8080:8080 \
  -e CLIENT_API_KEYS -e OPENAI_CODEX_TOKEN_FILE=/credentials/openai-codex.json \
  --mount type=bind,src="$(pwd)/configs/models.json",dst=/config/models.json,readonly \
  --mount type=bind,src="/absolute/path/to/credential-directory",dst=/credentials \
  hao-agent-harness
```

运行前先在宿主机登录并设置 `CLIENT_API_KEYS`，将凭证目录路径替换为实际路径。配置需对容器用户可读，凭证目录需可读写，以便保存轮换令牌。若切换到 API Key 模式，则设置对应上游密钥并省略凭证目录挂载。指标默认绑定容器内回环地址；如需收集，请显式配置运维网络地址并限制访问。

实现参考 Go 官方 [net/http 文档](https://pkg.go.dev/net/http)，CI 配置参考官方 [checkout](https://github.com/actions/checkout) 和 [setup-go](https://github.com/actions/setup-go) 用法。
