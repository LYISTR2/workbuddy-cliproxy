# workbuddy-cliproxy

本 fork 的 **v0.3.2** 使用 WorkBuddy 国内版官方网页授权（`platform=workbuddy`）和账号的实时模型目录，适配 CPA v8.0.56。更新说明见 [CHANGELOG](CHANGELOG.md)。模型由上游返回，后续新增模型无需逐个修改源码；模型重新注册的刷新方式见 [动态模型目录](docs/dynamic-models.md#定期刷新)。

把**腾讯 WorkBuddy 国内版**（`www.workbuddy.cn`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA)插件,任何支持 OpenAI / Anthropic 协议的客户端(Claude Code、Cursor、Cline、SDK……)都能直接调用 WorkBuddy 背后的模型。

对 [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) 公开 `workbuddy.so` 的 clean-room 逆向重写,补齐了源码与 x86_64 支持;workbuddy 的原始设计归属 Sliverkiss。

## 工作原理

在 CPA 里注册为 `workbuddy` provider：通过 `https://www.workbuddy.cn/v2/plugin/auth/state?platform=workbuddy` 获取官方授权链接，打开 WorkBuddy 登录网页，轮询 token，再通过 `/v2/plugin/account` 读取账号资料；聊天请求转发到 `www.workbuddy.cn/v2/chat/completions`。

升级后请在 CPA 中重新添加一次 WorkBuddy 授权，并停用不再使用的旧入口凭据。现有凭据不会自动改写，`workbuddy` provider 名和账号文件格式保持兼容。

## 模型

登录后，插件使用当前账号请求 WorkBuddy `GET /v3/config`，按 `agents.cli.models` 动态注册模型，不再维护固定列表。目录渠道 `cli` 与登录平台是两个参数；登录平台固定为 `workbuddy`。通过 `catalog.channel` 可以选择其他上游公布的渠道；渠道缺失时不会回退到整个产品模型池。

模型 ID、默认思考档位和积分倍率来自该账号、渠道的配置，可能与 WorkBuddy 桌面客户端不同。目录列出模型不代表已经验证推理可用性，实际调用仍受账号权限和额度约束。旧 ID 如果已不在上游目录中，将不再注册；客户端需要刷新模型列表，不会自动重写成另一模型。

标准 `GET /v1/models` 保留 `id`、`object`、`owned_by`，并补充上游公布的显示名称、思考档位、积分倍率和目录更新时间。字段与缓存行为见 [动态模型目录](docs/dynamic-models.md)。

## 模型列表为空的修复

v0.3.1 的客户端标识缺少可解析的版本号，会导致模型目录被网关以 `12403` 拒绝。v0.3.2 使用统一的 WorkBuddy 版本标识、IDE 名称/类型/版本和域信息，并兼容旧凭据缺少用户 ID 的情况。

升级时先保留现有账号文件，替换对应架构的 `workbuddy.so` 并重启 CPA；确认插件版本显示 `0.3.2`，再查看该凭据支持的模型。目录按账号重新发现，不用手写模型名单。若凭据本身已经失效，仍需要重新进行 WorkBuddy 授权。

目录失败时，CPA 日志会保留上游数字错误码；`12403` 指向客户端版本校验，HTTP `401/403` 指向账号鉴权。

## 安装

**前置**:带 CGO / 动态插件支持的 CLIProxyAPI、WorkBuddy 账号、Go 1.26+ 与 gcc；编译系统与架构需与 CPA 实例一致。本版本在 Linux amd64、CPA v8.0.56 上验证加载和注册。CPA 的 `no-plugin` 构建无法使用此插件。

在本仓库的 [Actions](https://github.com/LYISTR2/workbuddy-cliproxy/actions) 中下载成功构建的 `workbuddy-linux-amd64` 或 `workbuddy-linux-arm64` 产物，解压得到 `workbuddy.so`。amd64 构建使用 Ubuntu 22.04，arm64 使用 Ubuntu 24.04；较旧发行版或其他系统请在目标环境编译。

自行编译：

```bash
git clone https://github.com/LYISTR2/workbuddy-cliproxy.git
cd workbuddy-cliproxy
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -buildmode=c-shared -o workbuddy.so .
```

产物:`.so`(Linux)/ `.dylib`(macOS)/ `.dll`(Windows)。放到 CPA 的 `plugins/` 目录,在 `config.yaml` 启用:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
      priority: 100
      catalog:
        channel: cli
        ttl-seconds: 300
        max-stale-seconds: 86400
```

重启 CPA,日志出现 `plugin loaded ... plugin_id=workbuddy` 即成功。然后到 CPA 面板添加 workbuddy 凭据,扫码登录 WorkBuddy；`GET /v1/models` 将显示该账号当前渠道的模型。未登录时不发布静态模型。

## 使用

CPA 默认端口 `8317`,API key 见 `config.yaml` 的 `api-keys`。
CPA v8 的新配置对应 `server.port` 和 `access.api-keys`，旧配置仍由 CPA 兼容。

先获取当前账号的模型列表，以下示例中的 `<model-id>` 均替换为返回的 WorkBuddy 模型 ID：

```bash
curl http://localhost:8317/v1/models -H "Authorization: Bearer <api-key>"
```

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`(不带 `/v1`,走 `x-api-key`) |

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8317
export ANTHROPIC_API_KEY=<api-key>
export ANTHROPIC_MODEL="<model-id>"
claude
```

```bash
# curl / OpenAI
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"<model-id>","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

流式 / 非流式都支持;非流式请求会被内部转成流式再聚合(WorkBuddy 上游 `code 11101` 拒绝非流式)。

## Claude Code 兼容性

腾讯 WorkBuddy 的内容审核把 Claude Code 的两句固定 system 模板逐字加进了黑名单,命中即回"敏感内容"拒答:

- `You are Claude Code, Anthropic's official CLI for Claude.`(身份句)
- `Main branch (you will usually use this for PRs)`(git 注入句)

任何一字改动都绕过(精确匹配,非语义审核)。workbuddy 转发前会自动把这两句做最小改写(`CLI`→`CLI tool`、`Main branch`→`Default branch`),语义不变,Claude Code 照常工作。

属于 cat-and-mouse:腾讯哪天多加模板句,得跟着改 `sanitizeBlockedTemplates`。

## 思考模式

客户端显式传入的 `reasoning_effort` 会保留；省略时使用上游模型配置的默认值，不再将所有 Hy3 请求强制改成 `high`。只有上游明确提供 `supportedEfforts` 时才按该列表校验，不能把默认档位推断成唯一可用档位。上游不允许的显式档位会返回 HTTP 400。

`model` 使用目录中的真实 ID，思考档位通过单独的 `reasoning_effort` 字段传入。思考内容仍走 SSE 的 `delta.reasoning_content`。

## 流式

真流式(async):转发上游时边读边通过 `host.stream.emit` 把每个 chunk 实时推给 CPA,客户端逐字收到(不是等收齐了一股脑)。hy3 几千字的思考过程也是实时流出的,不是憋半天再刷出来。

## 测试与定期刷新

Linux 下运行 `sh scripts/build-linux.sh`，脚本会执行 `go test -race ./...` 并构建共享库；可用 `GO_BIN` 指定 Go 编译器。Windows 可运行纯 Go 模块测试 `go test ./internal/catalog`。

测试使用模拟账号和目录，未使用你的 WorkBuddy 凭据。实际模型可用性、额度和计费需登录后确认。

目录按需缓存 5 分钟。要定期更新 CPA 的模型注册，可选用 `scripts/refresh_models.py` 和 `deploy/` 中的 systemd 模板；它们通过管理 API 触发配置热重载，不重启 CPA。安装步骤、凭据格式和限制见 [定期刷新](docs/dynamic-models.md#定期刷新)。

## License

MIT。
动态目录及工具兼容实现基于 nanguoopxiao 的 [上游 PR #9](https://github.com/lovingfish/workbuddy-cliproxy/pull/9)，保留原项目及 Sliverkiss 的贡献说明。
