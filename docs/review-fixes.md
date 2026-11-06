# fd76071 审查问题处理

对应用户提供的 `workbuddy-cliproxy-review.md`，修复版本 v0.4.0。保留 provider 名、旧文件路径及嵌套凭据格式。

| 编号 | 处理 | 主要验证 |
| --- | --- | --- |
| P0-1 | 按 UID / 企业 / 域生成摘要身份；登录 ID 与文件名一致，旧文件交由宿主按路径识别；刷新保留宿主身份 | `TestMultiAccountAuthIdentityIsUnique`；真实 CPA 模拟两次登录 |
| P0-2 | 顶层 `expires_at`、`refresh_interval_seconds`、AuthData / refresh `NextRefreshAfter` | `TestAuthExpiryAndRefreshScheduling`；真实 CPA 对一分钟后过期的凭据自动调用 refresh |
| P1-1 | 所有流式路径先同步检查响应头；HTTP 错误共享有状态类型，流内错误附带 `error_detail` | `TestStreamHTTPStatusBeforeSuccess` |
| P1-2 | 共享根 context、总时长、活动操作等待、shutdown 取消；有 callback 的聊天走宿主 HTTP 通道 | `TestShutdownAndTotalDeadlineStopHeartbeatOnlyStreams`；`TestHostHTTPClosesWhenCallerCancels` |
| P1-3 | 共享 client 无 Jar；仅登录 state 使用独立 Jar | `TestAccountRequestsDoNotShareCookiesOrRefreshTokens` |
| P1-4 | refresh token 只在刷新接口设置 | 上述测试、现有 WorkBuddy refresh 测试、额度请求测试 |
| P1-5 | `UseNumber`、禁用 HTML 转义、一次 map 改写，仅 system / developer 角色 | `TestPayloadPreservesNumbersHTMLAndUserMessages` |
| P1-6 | 维护脚本按能力检测，处理 variants / 不可用行与多账号计数；新鲜缓存不 PATCH；默认 timer 15 分钟 | `scripts/test_refresh_models.py` 五项测试 |
| P2-1 | 缓存 key 不含 token；同一条目内用 token 摘要检测轮换并重新发现；闲置清理与 256 条上限 | `TestTokenRotationDoesNotGrowCache`；`TestCacheExpiryEvictsAbandonedScopesAndBoundsAccounts` |
| P2-2 | 文本、推理、工具参数用 `strings.Builder` | `BenchmarkTextAggregation`；工具参数聚合测试 |
| P2-3 | 热路径读取不可变 snapshot、独立 freshness 包装和 ID 索引；需修改的 Get 调用保留手动深拷贝 | `TestUnrelatedReconfigurePreservesTTLAndDeepCopy`；`BenchmarkCatalogHotPath` |
| P2-4 | 用请求中已声明的工具名区分完整重复、累计名字、真实分片；没有 schema 时保留首个完整名字 | `TestToolNameFragmentsUseOfferedSchema` |
| P2-5 | 字节截断回退 UTF-8 边界，处理非正数长度 | `TestTruncateAlwaysReturnsValidUTF8` |
| P2-6 | 新登录清理过期 state，定时清理放弃的 state，成功时停止定时器；poll 使用 TryLock | `TestLoginCleanupAndConcurrentPoll` |
| P2-7 | 缺失 / 非正 lifetime 使用一小时；缺 refresh token 直接返回重新授权错误 | `TestAuthExpiryAndRefreshScheduling` |
| P2-8 | 普通 JSON / 额度响应上限 1 MiB，处理读错误；错误不回显上游 msg / body | `TestJSONResponseLimitAndErrorRedaction`；额度错误测试 |
| P2-9 | 刷新失败、资料失败、目录失败 / stale、聊天 / 流式失败、额度失败使用宿主结构化日志，回退 slog | `TestDiagnosticNeverLogsCredentialsOrBodies` |
| P2-10 | 未知 RPC 返回 501 error，C ABI 对错误返回非零 | `TestUnknownRPCAndProviderRegistration` |
| P2-11 | 单一版本变量，tag / VERSION 可通过 ldflags 注入；CI 增加 gofmt / vet / Staticcheck；Actions 固定 SHA，Dependabot 维护 | 构建脚本、工作流和 Staticcheck v0.8.2 |
| P2-12 | hostCallFn 可替换，覆盖真实异步 pump、取消、状态码及上述边界 | `go test -race -cover ./...`，主包覆盖率达到审查要求的 80% |

P0-2 的审查示例仅增加过期时间和 NextRefreshAfter，在没有注册 RefreshLead 的宿主上仍可能不足，因此本次同时提供刷新间隔，并用真实 CPA 的自动刷新检查验证。P2-4 的名字分片存在协议歧义，通过已声明函数名消除歧义；参数始终按增量追加。

单次本机基准（1 MB / 10k chunks，`-benchtime=1x`）中，原字符串拼接约 829 ms / 5.04 GB 累计分配，Builder 约 1.47 ms / 5.24 MB；目录旧 JSON 拷贝约 205 μs / 27.9 KB，View + 索引约 6.9 μs / 128 B。这是微基准，不代表真实网络请求提速比例。
