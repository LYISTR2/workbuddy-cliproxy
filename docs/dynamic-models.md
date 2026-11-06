# 动态模型目录

插件使用已登录账号的 access token、用户、企业和域信息请求
`https://www.workbuddy.cn/v3/config`，再按 `agents` 中所选渠道的模型 ID 筛选。
默认目录渠道为 `cli`；请求使用带数字版本的 WorkBuddy 客户端标识，并与登录、推理共用同一份 IDE 名称、类型、版本和国内站域信息。登录平台仍固定为 `workbuddy`。

只发布渠道允许且未被禁用的模型。渠道缺失、鉴权失败或无法获得可靠目录时，
不会回退到旧的固定模型表，也不会把整个产品模型池当作账号权限。
上下文窗口等数值仅在上游提供时填入，不再沿用固定表中的估计值。

## 模型列表字段

标准 OpenAI `/v1/models` 的 `id`、`object`、`owned_by` 保持不变。
仅为 WorkBuddy 模型补充以下可得字段：

| 字段 | 含义 |
| --- | --- |
| `name` / `display_name` | 上游显示名称 |
| `credits` | 积分倍率原文 |
| `reasoning` | 经过字段筛选的原生思考配置 |
| `supported_reasoning_efforts` | 仅在上游明确枚举时出现 |
| `default_reasoning_effort` | 默认档位，不代表只支持这一档 |
| `reasoning_effort_policy` | 当前采用 `compatible` 或 `strict` 策略 |
| `reasoning_effort_mapping` | 兼容模式下，通用输入档位到原生档位的实际映射 |
| `pricing` | 倍率显示值、来源，以及可得的优惠和档位规则 |
| `catalog` | 渠道、来源、拉取时间、新鲜度和目录状态 |
| `supports_images` / `supports_reasoning` | 上游声明的能力 |
| `context_length` / `max_completion_tokens` | 上游提供时才出现 |
| `catalog_variants` | 多账号存在对应元数据时分别列出，不合并成一个价格 |

结构示例（数值仅作说明，不代表实时价格）：

```json
{
  "id": "example-model",
  "object": "model",
  "owned_by": "workbuddy",
  "display_name": "Example Model",
  "credits": "x0.06 credits",
  "supported_reasoning_efforts": ["low", "high", "max"],
  "default_reasoning_effort": "high",
  "pricing": {
    "display": "x0.06 credits",
    "unit": "credit_multiplier",
    "source": "account_channel_catalog"
  },
  "catalog": {
    "provider": "workbuddy",
    "channel": "cli",
    "source": "https://www.workbuddy.cn/v3/config",
    "fetched_at": "2026-09-22T00:00:00Z",
    "stale": false,
    "availability": "listed_by_upstream"
  }
}
```

倍率不是美元单价，也不是实测账单。优惠条件保留供客户端判断，不自行套用。
模型被列出只表示上游配置包含该 ID，推理能否成功仍需实际请求验证。
桌面 IDE 和 CLI 的模型集合、思考档位或活动规则可能不同。

上游 PR #9 曾在 CPA v7.3.10 验证模型列表扩展；本 fork 验证 CPA v8.0.56 的加载和注册，使用宿主的响应拦截接口，
无需修改 CPA 主程序。插件 SDK 仍依赖 v7.2.30 的 C ABI。
Codex 特有的 `client_version` 目录格式不在此扩展范围内。

## 缓存与思考档位

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
      priority: 100
      catalog:
        channel: cli
        ttl-seconds: 300
        max-stale-seconds: 86400
        reasoning-effort-policy: compatible
```

缓存按用户、企业、域、渠道和令牌隔离。同一缓存条目的并发请求合并拉取。
临时错误可使用距离成功拉取时间不超过 `max-stale-seconds` 的目录，
并标记 `stale: true`；失败后有 30 秒重试退避。HTTP 401/403 会清除对应缓存。
若宿主仍保留模型注册但插件已无可靠元数据，则返回
`catalog.availability: metadata_unavailable`，不附带旧价格。

目录支持的显式 `reasoning_effort` 保持不变，省略参数时使用上游默认值。仅提供默认档位不意味着只能使用该档位。

默认 `reasoning-effort-policy: compatible`：只对上游明确提供 `supportedEfforts` 的模型，将不在原生集合中的通用档位按 `minimal < low < medium < high < xhigh < max` 归一。优先选不低于请求的最近档位；若超过上游最高档，则采用最高可用档。候选值始终取自该模型真实公布的集合。

| 客户端输入 | 目录档位 | 上游接收 |
| --- | --- | --- |
| `medium` | `low/high/max` | `high` |
| `xhigh` | `low/high/max` | `max` |
| `max` | `low/high/xhigh` | `xhigh` |
| `low` | `low/high/max` | `low`（不改写） |

`strict` 策略保留原先的枚举校验，不在目录中的档位直接返回 HTTP 400，并列出可用值。无论选择哪种策略，未知字符串、错误类型和上游明确禁止的关闭思考请求都不会自动替换成开启思考。`none` 不参与强度排序，仍按 `canDisableThinking` 处理。

未声明 `supportedEfforts` 时不推断可用档位，继续保留客户端的显式值。兼容输入也不会被加入 `supported_reasoning_efforts` 或宿主 Thinking Levels；实际映射通过独立的 `reasoning_effort_mapping` 公布。来自嵌套 `reasoning.effort` 的输入归一后只保留平铺的 `reasoning_effort`，其余 reasoning 设置保留。

## 定期刷新

TTL 控制插件的按需发现缓存。CPA 的模型注册还需要宿主触发重新发现。
可选脚本 `scripts/refresh_models.py` 通过管理 API 更新插件配置中的
`_catalog_refresh` 标记，触发 CPA 的配置热重载与模型重新注册；随后检查
`/v1/models` 的拉取时间是否更新，不重启 CPA，也不修改账号令牌。

当前脚本面向单账号目录，要求已经登录、存在可用模型且插件版本为
`0.2.0-catalog`、`0.3.0`、`0.3.1`、`0.3.2` 或本 fork 的 `0.3.3`。多账号 `catalog_variants` 的刷新验证尚未支持。
Python 3 标准库即可运行：

```bash
python3 scripts/refresh_models.py \
  --base-url http://127.0.0.1:8317 \
  --credentials-file /etc/cliproxyapi/model-refresh.json
```

凭据文件使用如下结构，填写现有 CPA 管理密钥和 API key，并限制文件读取权限：

```json
{
  "management_key": "<CPA management key>",
  "api_key": "<CPA API key>"
}
```

`deploy/` 提供可选 systemd 模板，默认 CPA 服务和运行用户均为 `cliproxyapi`，
访问地址为 `http://127.0.0.1:8317`。安装前按实际环境修改服务名、用户、
`LoadCredential` 文件路径，以及 `ExecStart` 中的脚本路径和 `--base-url`。
默认安装路径如下：

```bash
install -d -m 0755 /opt/workbuddy-cliproxy
install -m 0755 scripts/refresh_models.py /opt/workbuddy-cliproxy/refresh_models.py
install -m 0644 deploy/codebuddy-model-refresh.service /etc/systemd/system/
install -m 0644 deploy/codebuddy-model-refresh.timer /etc/systemd/system/
systemctl daemon-reload
systemctl start codebuddy-model-refresh.service
systemctl enable --now codebuddy-model-refresh.timer
```

systemd 通过 `LoadCredential` 向服务提供密钥，不把密钥放进命令行或日志。
模板在启动两分钟后首次执行，之后每五分钟执行；CPA 已停止时跳过。
可通过 `journalctl -u codebuddy-model-refresh.service` 检查执行结果。
