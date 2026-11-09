# 会话分配监控

用于回答：新绑定是否按权重分配？同一个客户端会话是否持续命中？不同 Harness 聊天是否被识别成同一会话？监控仅观察，不改变权限、权重、选择算法或黏性 TTL。

## 配置与输出

在 **credit-manager 插件配置**（不是宿主的 routing.session-affinity）添加：

```yaml
session_affinity:
  log_decisions: true  # 默认 false；enabled/ttl 原有运行时设置保持不变
```

按现有宿主方式重新加载插件配置后生效。不提供新的管理写接口；此文档不代表已经部署或打开线上开关。当前实现的重新配置会重建调度状态，因此分析时必须按 `routing_epoch` 分段，不能把重新配置后的 cache miss 当作新建客户端聊天。

开启后，每次成功的插件 Key 调度输出一条 info 日志：

```text
credit-manager session-routing {"routing_epoch":"...","sequence":1,"at":"...","reason":"new_binding",...}
```

日志走已有 `host.log` 回调，落到宿主日志，不另建文件/数据库/异步队列。回调在调度锁释放后执行，失败不会让请求失败；它仍有同步日志 IO 和序列化开销，高流量时建议只在诊断窗口开启。宿主过滤 info 或日志写入失败时可能缺记录，不能承诺无损审计。预热专用请求、非插件 Key、鉴权/无候选失败不会生成这里的成功选择日志。

## 原因和统计口径

| reason | 意义 | 用途 |
|---|---|---|
| `new_binding` | 本调度状态中没有该会话+模型绑定 | 新绑定分布；不等于新建 Harness 聊天 |
| `affinity_hit` | 现有绑定仍可用，刷新 TTL | 同一会话的重复请求，不能计作新分配 |
| `expired_rebind` | 查到绑定但 TTL 已过期，再次加权选择 | 单独统计重绑定 |
| `unavailable_rebind` | 原绑定不在最终可选池内，重新选择 | 单独分析候选池变化/重试 |
| `no_session` | 黏性开启但没有可识别会话标识 | 按 Key 游标加权，不是会话创建 |
| `affinity_disabled` | 黏性未启用 | 按 Key 游标加权，不是会话创建 |

过期绑定可能被定期清理；清理后再来的旧聊天也会记成 `new_binding`。进程重启、重新配置、同一聊天换模型也可能产生新绑定。不能回溯记录功能启用前的会话。这里记录选择而非最终请求结果，后续原生重试可能再产生选择/重绑定事件。

**检查权重比例：**

1. 按 UTC 的 `at` 过滤窗口；按 `routing_epoch`、`selection_scope`、`model` 和候选池（每项 `auth_hash/priority/weight` 排序）分组。
2. 新绑定用 `reason=new_binding`；`expired_rebind` 和 `unavailable_rebind` 分开统计。不要把 `affinity_hit` 计入分配次数。
3. 只比较完整的相同候选池；`candidate_count=1` 是无竞争分配，不适合检验两账号的 1:5。`candidates_truncated=true` 也应排除。
4. 在同池同级账号同时可用时，权重 1:5 目标份额为 1/6 和 5/6。算法是平滑加权轮询，不是独立随机抽签。**`new_binding` 子序列只描述新绑定分布，不能单独验证共享游标是否正确**：`expired_rebind`、`unavailable_rebind`、其他 Key/模型/候选池也会推进同一个游标。如果权重 1 账号轮到时恰好都是重绑，新绑定子序列甚至可长期偏向权重 5，算法仍可能正确。
5. 验证算法时，要把同 epoch 的 `shared_session` 下全部加权重选事件（new + expired + unavailable）按 `sequence` 观察，并考虑跨池干扰；日志缺失/序列有缺口时不能完整重放。更简单的是用没有其他流量/重绑、固定双账号池的隔离测试窗口。单模型/单原因的切片即使很长也不保证精确 1:5。
6. 比较 `chosen_auth_hash` 的次数。`candidates` 是已经经过权限、并发/warmup、最高优先级、正权重、健康偏好过滤的**最终选择池**，不是所有已配置账号；不能从缺席推断确切排除原因。

## 字段与隐私

- `routing_epoch`：本 Service 监控状态的标识。重启/重新配置后变化。`sequence` 单调递增，文件内顺序可能因并发不同，结合 epoch 去重。
- `session_hash` / `session_source`：实际用于黏性的身份及来源（`host_canonical`、`host_derived` 或具体头名）。即使黏性关闭也可记录识别到的身份供诊断。
- `header_session_hash` / `header_session_source`：按插件头优先序读到的身份。用于与 host 形成的有效身份**跨请求对比变化**。
- `binding_hash`：provider namespace + 有效会话 + 完整模型的绑定身份，仅在黏性实际启用且有会话标识时存在。不同 Key 可以观察到相同会话绑定，权限检查仍然先执行。
- `key_hash`：插件 Key ID 的脱敏指纹，不是 API Key 文本。
- `affinity_enabled` / `ttl_seconds`：当前实际运行设置；TTL 为滑动续期。
- `selection_scope`：`shared_session` 使用跨 Key/模型共享的加权游标；`per_key` 使用原按 Key 游标。
- `chosen_auth_hash` / `chosen_provider` / `chosen_weight`：最终选择；每项 `candidates` 包含账号指纹、provider、priority、weight。
- `candidate_count` 是最终池完整数量；最多输出 64 项，超出时 `candidates_truncated=true`。模型和 provider 文本最多 128 字节并去控制字符。

会话、绑定、Key 指纹使用本 epoch 的随机私钥做 HMAC-SHA256；跨 epoch 不可直接比同一会话。私钥不落盘、不打印。账号指纹采用稳定 SHA256，便于与管理员已知的账号对应；它是伪名，不是防字典猜测的加密。

可用以下本地表达式计算已知账号的日志指纹（provider 小写，auth ID 去首尾空格）：

```python
import hashlib
provider = "codex"
auth_id = "在此填管理接口中的账号 ID"
print(hashlib.sha256((provider.lower().strip() + "\0" + auth_id.strip()).encode()).hexdigest())
```

不记录原始会话 ID、账号文件名/邮箱、Authorization、API Key、cookie、请求或响应正文，也不遍历 metadata/headers 整体。相同会话的哈希关联仍属诊断数据，应限制日志访问并沿用宿主轮转/保留策略。

## Harness 观察方法

在一个不重新配置的监控窗口里，用相同 Key/模型创建多个真正的新聊天，观察各自首次请求及第二次请求：

- 有效 `session_hash` 各不相同，后续各自 `affinity_hit`：身份隔离正常。多个会话分到同一账号本身不代表身份复用，应看完整分配序列和候选池。
- 不同聊天 `header_session_hash` 不同、有效 `session_hash` 却相同：检查 host identity 来源、转发过程和自定义头；但仅两字段彼此不相等不代表异常，host 正常会添加身份命名空间。
- 无头（`header_session_source=none`），有效来源 `host_derived`：客户端可能没传显式会话身份，须核查 host 的派生身份输入。
- 两种 hash 跨聊天都不变：检查客户端/profile/plugin 是否配置固定会话头、是否真的创建了新聊天而非恢复旧聊天。

本地 Harness 审计显示普通新聊天和 fork 都生成新 UUID，agent loop 将它传作 `sessionId`。Responses/Codex 通常会发会话头和 `prompt_cache_key`；普通外部 OpenAI-compatible Chat Completions 默认可能不发显式会话字段。最终取决于请求路由、缓存设置、catalog compat 和自定义头，不能只从 GUI 聊天数量反推线上字段。

本地 CPA v8.0.60 标准执行入口会先做 session.Enrich：有效的显式会话头优先，并清除旧 derived 身份，不是正常地用 derived 覆盖有效的 Harness 头。缺少显式身份等更高优先来源时，derived 的根包括协议格式、调用方命名空间、首个 user 前的 system/developer 指令（每块前 50 字符）和完整规范化首个 user 内容，再做哈希。因此同一调用方、相同开头且没有显式 ID 的独立聊天可能被视为同一会话；不同首个 user 通常会改变根。既有 canonical/LCP 或 execution-session 元数据也可能先被采用。此为本地源码行为，不能代替对线上实际请求的日志验证。

监控观察的是 CPA 选择的凭据/账号，不是上游供应商实际处理请求的物理机器；后者无法由此日志证明。
