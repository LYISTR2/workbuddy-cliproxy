# 更新记录

## 0.3.2 — 2026-11-06

- 修复 v0.3.1 的 `WorkBuddy/CLIProxyAPI` 无数字版本标识：模型网关不能据此解析客户端版本，会以 `12403` 拒绝目录请求。
- 登录、目录和推理共用带数字版本的 WorkBuddy 客户端配置，统一 `X-IDE-Name`、`X-IDE-Type`、`X-IDE-Version` 和国内站 `X-Domain`，防止不同请求路径再次漂移。登录仍为 WorkBuddy 国内版 `platform=workbuddy`。
- 为旧凭据缺失用户 ID 的情况读取账号资料，再继续发现模型；已有有效凭据无需因为升级而直接删除。
- HTTP 错误及业务错误保留上游数字状态码，为 `12403` 提供版本校验提示；上游未返回模型定义时明确报错，不伪装成成功的空目录。
- 增加版本校验网关、错误码保留、账号资料恢复及发现后对话的回归测试。

验证：30 项测试含竞态检查通过。使用 CPA v8.0.56 和只处理合成凭据的本地 TLS 模拟网关，旧 v0.3.1 构建复现 `12403` 与零模型；新构建在正常凭据及缺失 UID 的旧凭据两种情况下，均通过 `/v1/models`、`/v0/management/auth-files/models?name=workbuddy.json` 和一次非流式对话验证。没有连接用户实际的 CPA 账号，不能据此确认真实账号当前权限、额度或上游目录。

协议问题参考：[WorkBuddy 模型发现客户端版本校验](https://github.com/cheaper-inference/OmniRoute/issues/17991)；客户端标识格式参考 [WorkBuddy 桌面端请求取证](https://github.com/dingminhua/dsh-connect-workbuddy/blob/main/docs/PROMO_BADGE.md)。

## 0.3.1 — 2026-11-06

- 默认授权服务改为 WorkBuddy 国内站 `www.workbuddy.cn`，使用 `platform=workbuddy`，直接打开官方返回的 WorkBuddy 网页；不再使用 CodeBuddy CLI 登录入口。
- 同步切换模型目录、聊天、令牌轮询和刷新服务到 WorkBuddy 国内站，使用 WorkBuddy 产品请求头。
- 通过 `/v2/plugin/account` 读取登录账号，确认有效 UID 后再保存凭据；保留一次性 token 以便账号资料暂未就绪时继续轮询。
- 刷新使用 JSON `{}` 正文和 `X-Auth-Refresh-Source: plugin`，刷新后尽力更新账号资料，资料接口临时失败时保留已轮换的令牌。
- 为官方授权 URL 添加独立 `loginSessionId`，校验 WorkBuddy 域名、平台和 state；补充登录、账号等待和刷新回归测试。HTTP 客户端遵循环境代理配置。
- 升级后需在 CPA 中重新进行 WorkBuddy 授权。真实扫码登录和账号模型推理仍需用户登录后确认。

验证：25 项测试（含竞态检查）、Linux amd64 共享库编译和 CPA v8.0.56 加载通过；通过已编译插件调用真实授权服务，确认返回 `https://www.workbuddy.cn/login`、`platform=workbuddy` 和独立登录会话。没有执行用户扫码登录。

## 0.3.0 — 2026-11-06

- 采用账号、企业、域、渠道及令牌隔离的动态模型目录，替代固定的十个模型；目录来自 CodeBuddy `/v3/config`，默认只注册 CLI 渠道允许的模型。
- 使用五分钟缓存、三十秒失败退避和有时限的旧目录回退；HTTP 401/403 及同值业务错误会作废缓存，不再沿用失效凭据的目录。
- 保留明确指定的思考档位，省略时使用上游默认值；不再将 Hy3 的所有请求强制改为 `high`。
- 按工具调用索引合并非流式响应中的名称和参数片段；具名强制工具选择会限制到目标工具再发送上游。
- 动态目录过滤明确禁用、图片生成和嵌入等条目；模型上下文及输出上限只使用上游公布的数值。
- 非流式响应遇到错误帧或没有任何 completion choice 时返回错误，不再合成空的成功响应。
- 模型列表扩展使用 SDK 原生响应类型，修改正文时同步移除旧 `Content-Length` / `ETag`。
- CLI 请求版本标识更新至 npm 官方包 `@tencent-ai/codebuddy-code` 2.180.0；实际账号调用尚未验证。
- 增加 Linux amd64 / arm64 的 GitHub Actions 测试、共享库构建与 SHA256 文件，更新 fork 的安装路径和动态刷新说明。

动态目录和基础工具适配来自 [nanguoopxiao/workbuddy-cliproxy](https://github.com/nanguoopxiao/workbuddy-cliproxy/tree/ed1433f4968ee8d560cb9c423ea72380aae21967)（上游 PR #9）；本 fork 继续补充错误处理、过滤、构建流程和回归测试。原项目 MIT 许可证保留。

验证范围：Linux amd64，Go 1.26.4，CPA v8.0.56；模拟目录测试覆盖解析、隔离、刷新、鉴权失效、思考档位与工具调用。未使用真实 CodeBuddy 账号验证推理、计费或额度，其他操作系统未做端到端检查。
