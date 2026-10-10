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
configs/               配置示例
docs/                  设计文档
```

HTTP 层依赖业务服务，业务服务依赖 `chat.Provider` 接口，供应商适配器实现该接口。配置和进程生命周期由入口组装，测试可以使用本地假上游而无需真实模型密钥。

## 本地运行

项目工具链固定为 Go 1.26.5。先复制配置示例，再编辑 `chat_url` 和 `upstream_model`，使其匹配实际供应商；示例域名不能用于真实调用。

PowerShell：

```powershell
Copy-Item configs/models.example.json configs/models.json
# 编辑 configs/models.json 后设置凭证。
$env:CLIENT_API_KEYS = 'your-client-token'
$env:UPSTREAM_API_KEY_PRIMARY = 'your-provider-key'
$env:MODEL_CONFIG_PATH = 'configs/models.json'
go run ./cmd/server
```

若要使用 OpenAI OAuth（ChatGPT 账号授权），复制 `configs/models.openai-oauth.example.json` 为 `configs/models.json`，设置 `CLIENT_API_KEYS` 和 `MODEL_CONFIG_PATH` 后启动服务，再打开 `http://127.0.0.1:8080/`，在“连接设置”点击 **Continue with ChatGPT**。授权回调要求通过 `127.0.0.1` 打开本地页面，不要使用 `localhost`。OAuth 凭证默认保存到当前用户配置目录下的 `hao-agent-harness/openai-oauth.json`，也可通过 `OPENAI_OAUTH_TOKEN_FILE` 指定位置。

该模式使用 OpenAI Docs 的 Sign in with ChatGPT 授权流程和 Responses API；上游请求始终启用流式响应并设置 `store:false`，即使本地客户端请求普通 JSON，服务也会在服务器内部读取并聚合流。授权范围和模型可用性由 OpenAI 账号决定，配置中的 `upstream_model` 必须是该账号可用的模型。当前工作台保存一个活动 OpenAI 账号，不使用 `UPSTREAM_API_KEY_PRIMARY`。API Key provider 配置仍可照常使用。

Linux/macOS：

```sh
cp configs/models.example.json configs/models.json
# 编辑 configs/models.json 后设置凭证。
export CLIENT_API_KEYS='your-client-token'
export UPSTREAM_API_KEY_PRIMARY='your-provider-key'
export MODEL_CONFIG_PATH='configs/models.json'
go run ./cmd/server
```

`.env.example` 只用于展示环境变量；程序不会自动读取 `.env`。密钥在启动时从环境读取，不写入模型配置文件。供应商地址必须为固定 HTTPS 地址，禁止 URL 内嵌凭证、查询参数和重定向。

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

| 环境变量 | 默认值 | 含义 |
| --- | --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080` | API 监听地址 |
| `METRICS_ADDR` | `127.0.0.1:9090` | 独立指标监听地址；空字符串关闭 |
| `CLIENT_API_KEYS` | 必填 | 逗号分隔的客户端 Token，可配置新旧 Token 进行轮换 |
| `MODEL_CONFIG_PATH` | 必填 | 模型配置 JSON 路径 |
| `UPSTREAM_API_KEY_PRIMARY` | 示例供应商必填 | 由配置中的 `api_key_env` 指定，支持多个供应商 |
| `OPENAI_OAUTH_TOKEN_FILE` | 用户配置目录默认路径 | OpenAI OAuth 轮换凭证的本地存储文件，仅 `auth_type: openai_oauth` 时使用 |
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
  -e CLIENT_API_KEYS -e UPSTREAM_API_KEY_PRIMARY \
  --mount type=bind,src="$(pwd)/configs/models.json",dst=/config/models.json,readonly \
  hao-agent-harness
```

运行前先在宿主机导出两个密钥环境变量，并保证挂载配置对容器用户可读。指标默认绑定容器内回环地址；如需收集，请显式配置运维网络地址并限制访问。

实现参考 Go 官方 [net/http 文档](https://pkg.go.dev/net/http)，CI 配置参考官方 [checkout](https://github.com/actions/checkout) 和 [setup-go](https://github.com/actions/setup-go) 用法。
