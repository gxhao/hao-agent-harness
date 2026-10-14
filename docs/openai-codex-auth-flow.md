# OpenAI Codex 登录与凭证流程

本文说明 `openai_codex` provider 的登录、凭证保存、自动刷新和本机控制端点。应用配置统一从 [`configs/models.json`](../configs/models.json) 读取；该文件使用 YAML 语法，虽然扩展名仍是 `.json`。

## 本地服务访问边界

本服务不验证客户端 Bearer token，浏览器和 API 调用均不需要 `Authorization`。默认 API 地址为 `127.0.0.1:8080`，metrics 默认在 `127.0.0.1:9090`。若把监听地址改为非 loopback 地址，必须使用防火墙、VPN 或受控反向代理限制访问；否则可访问者能够调用模型并产生上游费用。

OpenAI Codex access token 是上游凭证，仅由服务端发送给 OpenAI；它与客户端访问服务的凭证无关。OAuth 控制接口不检查 Bearer token，但仅允许 loopback 来源；启动和退出操作还要求 `Origin` 与请求 Host 同源。回调仍验证 OAuth state、PKCE 等协议字段。

## 登录和启动

先在配置中心确认有 `openai_codex` Provider，且配置了 `codex_token_file`：

```yaml
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

在项目根目录执行：

```powershell
go run ./cmd/server --login
go run ./cmd/server
```

`--login` 默认登录 Codex；也支持 `--login openai` 和 `--login=openai-codex`。登录命令与服务启动都读取同一份 YAML，因此使用同一 `codex_token_file`。登录完成后保存凭证并退出，不会启动 API 服务。登录使用 PKCE 和随机 state，回调监听 `127.0.0.1:1455`；浏览器回调失败时，可将本次回调 URL 粘贴到终端。

访问 `http://127.0.0.1:8080/` 可打开工作台。工作台无需粘贴客户端密钥，会自动读取模型列表。对于 `openai_oauth` provider，可在“模型与账号”中发起 ChatGPT 授权；该 provider 必须配置独立的 `oauth_token_file`。

## 凭证生命周期

凭证文件由 OAuth 管理器以仅所有者可读写的权限保存，包含 access token、轮换 refresh token、账号 ID 和到期时间。不要提交或分享此文件。相对路径相对于启动服务时的当前工作目录解析。

Codex provider 会在请求时获取访问令牌：令牌仍有效时直接使用；距离到期不足 90 秒时串行刷新；刷新成功后校验账号 ID 并原子保存新的 token。刷新请求失败、凭证不完整或保存失败会返回可识别的 OAuth 错误，不会把凭证写入日志。

OAuth provider 和 Codex provider 使用不同的上游协议和凭证格式，凭证文件不可互换。`openai_codex` 固定调用 `https://chatgpt.com/backend-api/codex/responses`，发送 `Authorization: Bearer <OpenAI access token>` 与 `ChatGPT-Account-Id`，并要求 `stream:true`、`store:false`。普通 API 请求仍使用 Chat Completions 风格的本地接口。

## 本地 OAuth 控制接口

| 接口 | 方法 | 行为 | 边界 |
| --- | --- | --- | --- |
| `/auth/openai/status` | GET | 返回连接状态，不返回令牌 | 仅 loopback |
| `/auth/openai/start` | POST | 创建 OAuth 授权流程 | 仅 loopback 且同源；Codex 模式返回提示使用 CLI 登录 |
| `/auth/openai/callback` | GET | 完成网页 OAuth 回调 | 仅 loopback，并验证 state/nonce |
| `/auth/openai/logout` | POST | 清除本地凭证 | 仅 loopback 且同源；不代表远端 token 已撤销 |

所有控制接口都不要求本服务 Bearer token。浏览器同源检查用于阻止跨站请求，不能替代网络隔离；不要把无鉴权的 API 或 metrics 监听端口暴露到不可信网络。

## 配置热加载

进程每秒检查 `configs/models.json`。变更后会先完整解析、校验并构造新的 Provider、模型路由和 OAuth 管理器，再原子切换。正在处理的请求继续使用旧快照；新请求使用新快照。配置无效或新监听地址无法绑定时，保留上一份有效配置并写入脱敏错误日志。编辑完成后不需要重启服务。

## 常见故障

| 现象 | 处理 |
| --- | --- |
| `cannot read configuration file` | 从项目根目录启动，并确认 `configs/models.json` 存在且可读 |
| `configuration must use YAML syntax` | 将旧 JSON 转为 YAML；文件路径仍为 `configs/models.json` |
| `configuration_reload_failed` | 检查字段名、duration、模型引用和监听端口；旧配置仍继续服务 |
| `Use server --login openai` | 当前 provider 是 Codex 模式，应运行 `go run ./cmd/server --login` |
| `oauth_login_required` / HTTP 503 | 检查 `codex_token_file` 路径和可写权限，然后重新登录 |
| 本地 API 返回 4xx | 本服务没有客户端鉴权；检查请求字段、模型别名和内容类型 |
| 上游 HTTP 429/502 | 检查账号额度、模型可用性或上游协议；本服务不会输出令牌 |

测试不需要真实账号：

```powershell
go test -timeout 60s ./tests/config ./tests/openaioauth ./tests/provider ./tests/server ./tests/cmd/server
go vet ./...
```
