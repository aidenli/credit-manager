# API Key 与 OAuth 账户绑定设计分析

## 1. 结论

该功能可以在现有插件架构内完成，不需要修改 CLI Proxy API 宿主协议。

推荐一期实现：

- 一个自定义 Key 可绑定多个 OAuth 账户；一个 OAuth 账户也可被多个 Key 绑定。
- 未配置绑定的 Key 保持现状，继续由宿主/插件当前调度逻辑选择账户。
- 配置绑定的 Key 只允许从其绑定账户与宿主候选账户的交集中选择。
- 交集内按轮询选择，并继续应用已有并发限制和 warmup 占用判断。
- 绑定存在但没有可用账户时必须 fail-closed，返回“绑定账户当前均不可用”，不能悄悄回退到未绑定账户。
- 二期再增加 `round_robin` / `fill_first` 策略。

核心原因：项目已经注册 `Scheduler` 能力，`internal/plugin/scheduler.go:12` 的 `pickAuth` 可以读取请求头和宿主提供的候选账户，并通过 `SchedulerPickResponse{AuthID, Handled:true}` 指定账户。现有 warmup 功能已经使用同一机制定向选择 OAuth 账户。

## 2. 当前关键链路

### Key 管理

- Key 数据模型：`internal/store/keys.go` 的 `PluginKey`、`PluginKeySpec`、`PluginKeyPolicyUpdate`
- Key 创建/轮换：`internal/service/keys.go`
- 管理接口：`internal/management/keys.go`
- 管理响应：`internal/management/views.go:keyView`
- SQLite 迁移：`internal/store/migrations.go`，当前最高版本为 25
- 控制台：`internal/management/web/console.html`、`console.js`

### 鉴权与执行

1. `internal/plugin/auth.go:authenticate`
2. `internal/service/auth.go:Authenticate`
3. 校验 `tk-...`，加载 `PluginKey`，返回 `cmk:<kid>` principal
4. `internal/plugin/execute.go` 调用宿主执行模型请求
5. 宿主回调 `internal/plugin/scheduler.go:pickAuth`
6. 当前由 `Service.PickAuth` 结合候选账户、并发上限和 warmup 状态进行选择

### OAuth 标识

项目已经统一使用 `(provider, auth_id)` 标识账户：

- `internal/store/auth_quota.go:AuthIdentity`
- `internal/service/auth_concurrency.go:AuthPickCandidate`
- `auth_concurrency_limits` 也以 provider + auth_id 为逻辑主键

因此绑定表也应使用 `provider + auth_id`，不要只保存 `auth_id`，避免不同 provider 下 ID 冲突。

## 3. 数据库设计

不要把 OAuth ID 数组塞进 `plugin_keys` JSON 字段。该关系需要查询、校验、去重和后续扩展策略，应该使用关联表。

建议 migration 26：

```sql
CREATE TABLE key_auth_bindings (
    plugin_key_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    auth_id TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    created_at_unix_ms INTEGER NOT NULL,
    PRIMARY KEY (plugin_key_id, provider, auth_id),
    FOREIGN KEY (plugin_key_id) REFERENCES plugin_keys(id)
);

CREATE INDEX key_auth_bindings_auth_idx
    ON key_auth_bindings(provider, auth_id);
```

说明：

- `priority` 一期可以统一为 0，但现在预留，二期填充优先可直接使用。
- 不建议对 OAuth 账户建立外键，因为账户来自 CLI Proxy API 的动态 auth 文件，不是本插件稳定持有的主表实体。
- Key 被永久软删时应同步清理绑定，或者所有查询通过有效 Key 过滤；推荐在 `DeletePluginKey` 事务中删除关联行。

建议新增 Store 类型和方法：

```go
type KeyAuthBinding struct {
    PluginKeyID string
    Provider    string
    AuthID      string
    Priority    int
}

ReplaceKeyAuthBindings(ctx, keyID string, bindings []KeyAuthBinding) error
ListKeyAuthBindings(ctx, keyID string) ([]KeyAuthBinding, error)
ListKeyAuthBindingsByKid(ctx, kid string) ([]KeyAuthBinding, error)
```

`Replace` 应在事务中执行“删除旧绑定 + 批量插入新绑定”，并对 provider 做与 `authLimitProvider` 一致的规范化，对 auth_id 去空格和去重。

## 4. 一期路由算法

建议新增服务方法，而不是把数据库逻辑塞入插件层：

```go
PickAuthForKey(ctx, key, candidates) (authID string, handled bool, err error)
```

算法：

1. 查询该 Key 的绑定列表。
2. 无绑定：调用现有 `PickAuth`，完整保持旧行为。
3. 有绑定：将宿主 `candidates` 与绑定集合按 `(normalized provider, auth_id)` 求交集。
4. 对交集继续执行当前可用性过滤：
   - 账户必须存在于宿主候选列表；
   - 不处于 warmup hold；
   - 未达到 `auth_concurrency_limits`。
5. 对剩余账户轮询。
6. 没有剩余账户：返回专用错误，如 `ErrNoBoundAuthAvailable`，并由 scheduler 返回明确错误码 `bound_auth_unavailable`。

轮询游标不能只按 provider 保存。当前 `nextAuthPickLocked` 使用 provider 作为 key；新增绑定后应至少按 `plugin_key_id + provider` 保存，否则多个 Key 会共享游标、互相干扰。

建议：

```go
authPickCursor map[string]int
cursorKey := pluginKeyID + "\x00" + normalizedProvider
```

该游标是进程内状态，重启后从头开始，作为一期实现是合理的；无需为了公平性将游标落库。

## 5. 如何识别当前 Key

`pickAuth` 已经能访问 `req.Options.Headers`，warmup 逻辑也依赖该字段。可以直接调用已有：

```go
svc.LookupPluginKeyFromHeaders(ctx, req.Options.Headers)
```

但应区分三种情况：

- 不是插件 Key：忽略绑定逻辑，保持当前行为；
- 是合法插件 Key且无绑定：保持当前行为；
- 是合法插件 Key且有绑定：强制在绑定集合中选择。

如果实测某类宿主请求不会把 Authorization 传到 scheduler，则应在 executor 调用宿主前注入一个仅内部使用的 Key 身份头（只传 kid 或内部随机请求 token，不传明文 key），并在 scheduler 中读取；不要把明文 key 复制到额外 header。

## 6. “账号可用”的一期定义

一期应以宿主传入的 scheduler candidates 作为“账号已启用且适配当前 provider/model”的权威集合，再叠加插件本地状态：

- CLI Proxy API 候选集存在；
- 未达到本插件配置的账号并发上限；
- 不处于 warmup 独占状态。

不建议一期直接用 `AuthQuotaOverviewItem.Status` 判定可用性。`fresh/stale/idle/unavailable` 描述的是额度快照获取状态，不完全等价于 OAuth 凭证能否执行请求。额度耗尽的精确判断可在二期单独定义，避免误杀。

## 7. 管理 API 与控制台

### API

在 Key 创建和更新请求中增加：

```json
{
  "auth_bindings": [
    {"provider": "codex", "auth_id": "account-a"},
    {"provider": "codex", "auth_id": "account-b"}
  ]
}
```

建议约定：

- 字段缺失：不修改现有绑定（更新接口）。
- 传空数组：清空绑定，恢复默认调度。
- 创建 Key 时缺失或空数组：不绑定。
- 重复项自动去重；空 provider/auth_id 返回 400。
- 返回的 `keyView` 增加 `auth_bindings`。

账户选择列表可复用 `GET /v0/management/credit-manager/auth-quotas`，其中已有 `provider`、`auth_id`、展示名和状态。

### UI

在 Key 新建/编辑弹窗增加：

- “限定 OAuth 账户”多选列表；
- 按 provider 分组；
- 支持搜索 auth_id、email、label；
- 明确提示：“未选择表示不限制；选择后若全部不可用，请求将失败，不会使用其他账户。”

## 8. 需要修改的文件

一期主要改动：

1. `internal/store/migrations.go`：migration 26，创建关联表和索引。
2. 新增 `internal/store/key_auth_bindings.go`：关联表 CRUD 与规范化。
3. `internal/service/auth_concurrency.go`：新增按 Key 过滤和轮询逻辑；保留原 `PickAuth` 兼容路径。
4. `internal/plugin/scheduler.go`：warmup 判断后、通用 `PickAuth` 前执行 Key 绑定路由。
5. `internal/management/keys.go`：创建/更新参数与事务调用。
6. `internal/management/views.go`：返回绑定列表。
7. `internal/management/web/console.html`、`console.js`：多选控件、加载和提交。
8. 对应 store/service/management/plugin 测试文件。

`PluginKey` 本体不必加入 OAuth 数组字段。绑定是独立聚合，只有管理响应需要组合加载。

## 9. 测试清单

必须覆盖：

- Key A 绑定账户 1、2；Key B 绑定账户 2、3，关联关系正确。
- 无绑定 Key 完全保持旧路由行为。
- 有绑定 Key 只能命中绑定账户。
- 绑定账户不在宿主 candidates 时被排除。
- 绑定账户达到并发上限或 warmup busy 时被排除。
- 多个可用绑定账户按 Key 独立轮询。
- 所有绑定账户不可用时 fail-closed，不回退到未绑定账户。
- provider 别名/大小写规范化后仍能匹配。
- 更新接口字段缺失、空数组、重复项的语义正确。
- Key 删除后关联绑定被清理。
- 并发 scheduler 调用通过 `go test -race`，游标无数据竞争。

## 10. 二期策略扩展

二期建议在 Key 级增加：

```text
auth_routing_strategy = round_robin | fill_first
```

- `round_robin`：一期算法。
- `fill_first`：按 `priority` 升序选择第一个可用账户，只有其不可用/满载时才选择下一个。

不要把该字段塞进每条绑定关系；策略属于 Key，优先级属于绑定项。可以在 `plugin_keys` 增加 `auth_routing_strategy`，绑定表保留 `priority`。

## 11. 风险与决策

实施前只需确认一个产品语义：配置了绑定但所有绑定账户不可用时，是否允许回退到任意账户。

推荐答案：不允许，必须 fail-closed。否则“绑定”只是偏好，不是访问隔离，可能导致 Key 使用了管理员未授权的 OAuth 账户。

整体上，这个功能与现有架构契合度很高。真正需要避免的错误有三个：把多对多关系塞入 JSON、仅用 auth_id 不带 provider、无可用绑定账户时静默回退。