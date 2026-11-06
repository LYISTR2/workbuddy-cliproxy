# CPA 登录、额度与升级

v0.4.0 保持 `workbuddy` provider 和原有嵌套 `auth` / `account` 凭据格式。原有 `workbuddy.json` 按原路径加载和刷新；新登录使用账号、企业和域的摘要生成文件名，不把 UID 直接放进路径。同一身份再次授权会更新对应文件，不同身份各自保存。

## 登录

在支持插件 OAuth 的 CPA 管理面板中打开「OAuth 登录」，选择 **WorkBuddy**。插件提供名称和内置 W 标识，使用面板现有的打开链接、复制链接、轮询和保存流程。浏览器打开 `www.workbuddy.cn` 的官方 `platform=workbuddy` 授权页面；在该页完成授权后，面板自动读取资料并保存凭据，不需要手工下载 token 文件。

等待授权、读取账号资料、授权失效分别返回相应状态。登录请求沿用 CPA 提供的传输策略（包括登录代理）；每次轮询使用当前 callback 并沿用该 state 独立的 Cookie Jar。单次授权有效期五分钟；并发轮询会立即返回等待，放弃或过期的 state 会清理。一次性 token 在资料接口暂不可用时保留。关闭面板的登录框会停止宿主轮询，插件中的 state 最迟在五分钟后清理。

账号标签带昵称；旧文件名不改动。宿主通过顶层过期元数据、提前五分钟的 `NextRefreshAfter` 和刷新间隔判断何时刷新；缺少 refresh token 时明确提示重新授权。上游未给出有效 `expiresIn` 时按一小时安排，并提前刷新。

## 剩余额度

需要支持 **QuotaProvider** 的 CPA 和支持插件额度的管理面板；已在 CPA v8.0.56 验证。升级插件并重启后，在「额度」页面或凭据的额度区域点击刷新，WorkBuddy 会跟随其他账号显示原生额度卡片。

插件通过宿主的 `quota.identifier` / `quota.describe` / `quota.fetch` 注册能力，面板使用 `POST /v0/management/plugins/workbuddy/quota`（v8 也有对应管理路径），提交宿主分配的 `auth_index`。管理鉴权仍由 CPA 处理。

- 个人账号：查询 WorkBuddy `/v2/billing/meter/get-user-resource`，分别显示有效资源包的剩余量、总量、百分比和周期重置或到期时间。
- 企业账号：查询 `/v2/billing/meter/get-enterprise-user-usage`，显示个人企业额度和已用量计算出的剩余量，保留上游不限额标记。
- 资源包分别展示，避免把不同周期、不同有效期的套餐重复相加。未识别的响应结构会报错，不会把查询失败当成零余额。明确空列表表示没有有效资源包。
- 查询只读取额度，不支持重置、购买或修改套餐。最多处理 100 条资源；上游明确返回更多分页数据时提示当前限制。

支持单层、双层 `data`，以及 `resources` / `Response.Data.Accounts` 两种已知结构；数字和数字字符串均可处理。普通接口响应限制为 1 MiB，额度错误不回显上游正文。

旧版 CPA 没有额度插件接口时，聊天和原来的授权能力仍可使用，额度页面需升级宿主/面板；仅替换 `.so` 不能给旧面板增加页面。

## 请求与生命周期

有宿主 callback 时，登录、轮询、刷新、聊天和额度通过 CPA 的 HTTP stream 通道，遵循宿主传输策略；聊天因此能感知客户端取消。先同步读取响应头，HTTP 401 / 403 / 429 等错误以原状态交回 CPA，供宿主判断换号、失效和冷却。流内错误同时携带结构化状态；旧宿主忽略附加字段时仍能获得错误文本。

聊天默认最长 30 分钟，读空闲仍为三分钟。配置示例：

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true
      chat:
        max-duration-seconds: 1800
      catalog:
        channel: cli
        ttl-seconds: 300
        max-stale-seconds: 86400
```

总时长允许 1–7200 秒，0 或未设置使用 1800 秒。插件 quiesce / shutdown 会取消上游并等待活动操作，最长等待五秒；安装或升级二进制仍建议停止 CPA、替换对应架构文件、再启动。

共享 HTTP client 不保存 Cookie；登录各自使用独立 Cookie Jar。refresh token 只发给刷新接口，聊天、账号和额度请求不会携带。诊断只记录事件、状态码、数字错误码、账号摘要和耗时，不记录 token、请求体或响应正文。

## 验证范围

回归测试覆盖审查中的多账号、刷新调度、HTTP 状态、取消、Cookie、请求精度和错误边界；执行 `GO_BIN=/path/to/go sh scripts/build-linux.sh` 进行格式、vet、race 单测、Python 维护脚本测试和 c-shared 构建。CI 另运行 Staticcheck。

集成检查使用真实 CPA v8.0.56 和本地 TLS 模拟上游，验证原生 OAuth 两账号保存、旧凭据自动刷新、原生额度查询、严格 OpenAI SDK 的文本/推理/工具调用。没有使用真实 WorkBuddy 账号；实际套餐字段及余额仍以上游账号实测为准。
