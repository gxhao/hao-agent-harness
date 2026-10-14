# 本地 YAML 配置中心重构提案

状态：已按用户确认完成实施  
日期：2026-10-15  
范围：移除客户端 API Key 鉴权和环境变量配置，将运行配置集中到 `configs/models.json`（YAML 内容）

## 1. 目标与当前行为

当前服务通过 `config.Load()` 混合读取环境变量、`.env` 和 `configs/models.json`：监听地址、超时、日志、客户端密钥及 OAuth 凭证路径来自环境变量；模型与 Provider 定义来自 JSON；API 和 OAuth 控制接口要求客户端 Bearer token。

目标行为：

- 不再校验客户端 Bearer token；聊天 API、模型列表及 OAuth 控制端点不再要求客户端密钥。
- 不再从进程环境或 `.env` 读取应用配置，删除 dotenv 加载路径及其说明和示例。
- 将所有普通运行配置集中到仓库现有的 `configs/models.json` 路径，文件内容改用 YAML。为遵循保留该文件的要求，暂按原路径和文件名保留；虽然扩展名为 `.json`，解析器仍只按 YAML 解析。若希望使用标准 `.yaml` 扩展名，需用户确认后再调整。
- 配置进程启动时完整读取、校验；运行期间定时检查文件变更。新内容只有在完整解析、校验并成功构造服务依赖后才原子切换；无效配置保留上一份有效配置并记录脱敏错误。已开始的请求继续由旧配置快照完成，新请求使用新快照。
- 热加载包含监听地址：先绑定新地址并启动新 listener，确认可用后再停止并排空旧 listener。若新 listener 绑定失败，保留旧 listener 和旧配置。
- 不提供管理 API 或远程配置服务；配置源仍是本地固定 YAML 文件。

## 2. 配置内容与凭证边界

YAML 统一承载：HTTP/metrics 监听地址、请求与并发限制、所有超时、日志级别和会话日志目录、Provider 类型及上游地址/密钥、OAuth 凭证文件路径、模型别名和能力声明。

例如：

```yaml
server:
  http_addr: 127.0.0.1:8080
  metrics_addr: 127.0.0.1:9090
  max_request_bytes: 1048576
  max_concurrent_requests: 64
  request_timeout: 120s
logging:
  level: info
  chat_log_dir: logs/requests
providers:
  openai:
    auth_type: openai_codex
    codex_token_file: openai-codex.json
models:
  default:
    provider: openai
    upstream_model: gpt-6-luna
```

API Key Provider 若继续支持，其 `api_key` 将直接成为 YAML 字段，不再使用 `api_key_env`。密钥属于敏感配置：实际值不得写进提交的示例或日志；本地生产配置应限制文件访问权限。仓库中只提供占位示例。OAuth access/refresh token 仍由现有 OAuth 管理器写入独立凭证文件，因为其会轮换并原子更新；YAML 保存凭证文件路径，而不是 token 内容。Codex 登录入口也需读取 YAML 中的凭证路径。

## 3. 请求与启动流程

```text
进程启动
  └─ 读取固定路径 configs/models.json（YAML）
      ├─ 限制文件大小、拒绝未知字段、校验配置和模型引用
      ├─ 创建上游 Provider、日志、HTTP 与 metrics 服务
      └─ 启动监听

运行期间
  └─ 定时读取并比较配置文件
      ├─ 未变化：继续运行
      ├─ 解析/校验/初始化失败：记录脱敏错误，保留旧快照
      └─ 全部成功：原子切换请求快照；新旧请求按各自快照完成

业务请求
  └─ 健康/就绪检查或业务路由
      ├─ 不读取、不比较客户端 Authorization
      └─ 沿用请求校验、并发限制、超时和上游密钥保护
```

启动时配置不合法或无法读取时，进程在监听前退出；运行期间变更不合法或新地址无法绑定时，服务继续使用上一份有效快照。错误只指出配置类别或字段，不输出密钥值。默认 API 地址仍是 loopback，生产部署需由网络边界保护服务。

## 4. 修改范围

- `internal/config/config.go`：改为从固定 YAML 文件解码所有普通配置；移除环境变量解析和客户端密钥字段；保留结构校验并扩展到完整配置。新增轮询变更检测，校验后再通知服务层应用配置。
- `internal/config/dotenv.go`、`cmd/server/main.go`：删除 dotenv 加载实现与启动调用。
- `internal/httpapi/handler.go`、`internal/server/server.go`、`internal/openaioauth/oauth.go`、`internal/openaioauth/codex.go`：删除客户端 Bearer 校验和密钥注入；保留 OAuth 回调的 state/nonce/PKCE 校验、loopback 限制和同源检查。
- `internal/server/server.go`：增加运行时配置快照和 listener 更新流程；旧快照仅在使用它的请求结束后释放，地址变更先启动新 listener，再优雅关闭旧 listener。
- `internal/provider/compatible.go`：上游 API Key 从 YAML Provider 配置注入；不得记录密钥。
- `cmd/server/login.go`、`internal/openaioauth/codex.go`：登录和服务统一从 YAML 配置读取 Codex 凭证文件路径。
- `configs/models.json`：保留路径，改成 YAML 文档并补全所有默认运行配置。
- `Dockerfile`、README、`.env.example`、`docs/dotenv-loading-design.md`、`docs/llm-http-server-design.md`、`docs/openai-codex-auth-flow.md`：移除 env/dotenv 部署指导，更新无客户端鉴权、配置格式、运行和安全边界。dotenv 设计文档应删除或转为简短迁移说明，避免保留过期规范。
- `tests/` 下现有配置、HTTP、服务端、OAuth 和命令行用例需改为验证 YAML 加载、无 Bearer 请求可访问，以及 OAuth 自身保护仍有效；测试仍统一放在根目录 `tests/`。
- `go.mod`：增加 YAML 解析依赖，并记录依赖版本及许可证信息。

实际改动前会再次检查调用链，以上文件是当前追踪得到的候选范围。

## 5. 兼容性、安全与边界

- HTTP API 的访问控制发生破坏性变化：原有 401 会消失，任何可访问监听地址的客户端都可调用模型，可能产生上游费用或滥用。默认只绑定 `127.0.0.1`；设置为非 loopback 地址时必须由防火墙、VPN 或受控反向代理保护。
- OAuth 控制端点不再使用本服务 Bearer token，但仍保留现有回环来源及 Origin/Host 校验；需复核状态、启动、退出路由，确保没有扩大为跨站远程操作。
- API Key 明文写入 YAML 是移除 env 读取后的直接结果，配置文件权限和版本控制必须谨慎。若不接受将上游密钥保存于配置文件，应另行确定密钥管理边界；此提案不引入新的 secret store。
- 不再接受旧 JSON 内容或任何环境变量覆盖。升级前需将 JSON 内容转换为 YAML，并把运行所需设置迁入该文件；不做自动迁移、不读取 `.env`。
- 保留文件名 `models.json` 与 YAML 内容不匹配，可能影响编辑器和工具识别。这是为保留用户指定路径的暂定选择。
- OAuth token 文件仍独立持久化，避免把可轮换令牌及其写入生命周期混入普通 YAML 配置。

## 6. 文档与验证计划

实现后以 README 作为用户运行说明，以 `docs/llm-http-server-design.md` 更新配置和鉴权架构，以 `docs/openai-codex-auth-flow.md` 更新 OAuth 控制面边界；移除 dotenv 专题文档或保留为迁移记录。配置示例、Docker 挂载路径和登录命令必须与实现一致。

按仓库规范补充/更新 `tests/config/`、`tests/httpapi/`、`tests/server/`、`tests/openaioauth/`、`tests/cmd/server/` 用例，覆盖 YAML 解码、未知字段/非法配置、配置中的密钥不泄漏、无 Authorization 的业务请求和 OAuth 控制端点边界。完成后计划运行 `gofmt`、`go vet ./...`、`go test ./...`、`go build ./cmd/server`，核对 `git diff`、示例和文档一致性。

## 7. 确认与实施记录

用户确认保留提案其余范围，并要求加入运行时热加载。实现按固定本地 YAML 文件监视、完整校验后构造新运行快照、请求边界原子切换；监听地址变化先绑定新 listener，再排空旧 listener。无效配置和绑定失败均保留旧运行配置。

实际更新了 README、服务端架构设计、OAuth 流程和会话日志文档；删除 dotenv 加载实现、模板和专门设计文档。验证执行了 `gofmt`、`go test -timeout 60s ./...`、`go vet ./...`、`go build -trimpath` 和 Web UI 测试。
