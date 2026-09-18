# API Key ↔ OAuth 账户绑定：部署后验证测试用例与详细步骤

适用版本：**credit-manager v1.8.1**（commit `2760cbd`）
目标主机：`gdapi`（`biz-1`），宿主 `cliproxyapi.service`，API `:8317`
编写日期：2026-09-17

---

## 0. 开始前：本轮已完成的验证（L0 层，无需重跑）

| 编号 | 用例 | 方法 | 结果 |
| --- | --- | --- | --- |
| L0-1 | 产物包含绑定功能 | `strings` 比对新旧 .so | 新 .so 含 `bound_auth_unavailable`/`key_auth_bindings`/`PickAuthForKey`/`ErrNoBoundAuthAvailable`；旧 .so 全为 0 ✅ |
| L0-2 | 插件被宿主加载 | `journalctl` | `plugin loaded ... version=1.8.1`、`plugin registered` ✅ |
| L0-3 | 迁移 26 已应用 | 只读打开生产库 | `max migration version: 26`、表 + 索引存在、5 列、外键 1 ✅ |
| L0-4 | 存量数据未损坏 | 只读计数 | `plugin_keys` 4 行、`callers` 1 行、`key_auth_bindings` 0 行 ✅ |
| L0-5 | 回退可用 | 真实回退再前进 | 回退到 v1.8.0 加载成功，再部署回 v1.8.1 ✅ |
| L0-6 | 宿主透传 Key 的代码路径 | SDK 源码 | `schedulerOptions()` 原样复制 `opts.Headers`（含 `Authorization`）✅ |

**L0-6 的源码证据**（`CLIProxyAPI v7.2.128`）：

- `sdk/cliproxy/executor/types.go:147` — `// Headers are forwarded to the provider request builder.` `Headers http.Header`
- `sdk/cliproxy/auth/conductor_selection.go:489` — `func schedulerOptions(opts cliproxyexecutor.Options) ... { Headers: cloneHTTPHeader(opts.Headers) }`
- `sdk/cliproxy/auth/conductor_selection.go:563` — `Options: schedulerOptions(opts)` 填入 `SchedulerPickRequest`
- 宿主自身测试也用该字段传客户端 Key：`home_websocket_reuse_test.go:45` → `Headers: http.Header{"Authorization": {"Bearer client-key"}}`

结论：插件的读取路径成立。**但"运行时真的带上了"仍必须由 L1-1 实测确认**——这是唯一无法靠静态分析替代的一步。

---

## 1. 环境事实与限制（决定了哪些用例现在能跑）

| 事实 | 影响 |
| --- | --- |
| OAuth 账户只有 **1 个** codex（`gaoding_003@163.com`） | RR 轮询、账户 B 兜底等用例**需要 ≥2 个真实账户**，需先补一个 |
| 另有 3 个 OpenAI-compat 端点（`agnes` 2 个 key、`api.yoshub.com` 1 个 key） | 可充当"第二个候选账户"的低成本备选，见 §2.3 |
| `key_auth_bindings` 当前 0 行 | 生产库干净，测试数据需自行创建并清理 |
| 管理 API `secret-key` 为 bcrypt 哈希，明文不可恢复 | `credit-manager/*` 接口只能由**持有密钥的你**调用 |
| 宿主加载新插件后会**删除旧 .so** | 回退依赖 `/root/credit-manager-backups/`，不要依赖插件目录 |

**可用管理接口**（均在 `/v0/management/` 下，全部需要管理密钥）：
`credit-manager/health`、`overview`、`keys`(GET/POST)、`keys/update`、`keys/rotate`、`keys/revoke`、
`keys/reveal`、`keys/delete`、`keys/reset-spend`、`auth-quotas`、`auth-quotas/refresh`、
`auth-quotas/warmup`、`auth-quotas/warmup/settings`、`auth-quotas/concurrency`、
`auth-quotas/concurrency/batch`、`usage`、`usage/summary`、`audit`、`balance`、`pricing`、`callers`。

---

## 2. 前置准备

### 2.1 拿管理密钥

`credit-manager/*` 管理接口全部需要宿主的 management key（`/root/cliproxyapi/config.yaml` 的 `remote-management.secret-key`，bcrypt 存储、不可逆）。若已丢失，需重设：

```bash
# 在服务器上生成新哈希（宿主启动时会把明文转哈希，直接写明文即可）
# 备份原配置后，把 secret-key 换成一段新的长随机串，然后重启
cp /root/cliproxyapi/config.yaml /root/cliproxyapi/config.yaml.bak-$(date -u +%Y%m%dT%H%M%SZ)
# 编辑 secret-key: "你的新密钥"
systemctl restart cliproxyapi.service
```

> 注意：重设密钥会使**旧密钥立即失效**，管理面板需要同步更新。

### 2.2 确认账户清单（绑定选择列表的数据源）

```bash
KEY='<管理密钥>'
curl -s -H "Authorization: Bearer $KEY" \
  'http://127.0.0.1:8317/v0/management/credit-manager/auth-quotas?page=1&page_size=50' \
  | python3 -m json.tool | head -60
```

记录你要用于绑定的 `provider` + `auth_id`（无 `auth_id` 时用 `auth_index`，与控制台一致）。

**预期**：至少能看到 1 条 codex 账户，外加 compat 端点对应的条目。

### 2.3 （可选）补第二个账户——多数用例的前提

优先顺序：

1. **再加一个 Codex OAuth 账户**（最贴近真实语义）：控制台插件页或宿主 OAuth 流程添加第二个 codex 登录，auth 目录下会出现第二个 `codex-*.json`。
2. **用 compat key 充当第二候选**：`agnes` 已有 2 个 api-key-entry，天然可能形成 2 个候选。用 §2.2 的清单确认它是否被列为独立 `auth_id`（若两者同名同 provider，绑定无法区分，则只能选方案 1）。

### 2.4 签发测试 Key

控制台：**密钥 → 新建**，或直接调接口：

```bash
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"label":"bind-test-A","auth_bindings":[{"provider":"codex","auth_id":"<账户1>"}]}' \
  http://127.0.0.1:8317/v0/management/credit-manager/keys | python3 -m json.tool
```

**预期**：返回体含 `plaintext`（`tk-...`，**只显示这一次**）、`auth_bindings` 回显 1 条。

记下：`KEY_A=tk-...`、`KEY_A_ID=<id>`。

### 2.5 准备基线 Key（无绑定）

```bash
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"label":"bind-test-unbound"}' \
  http://127.0.0.1:8317/v0/management/credit-manager/keys | python3 -m json.tool
# 记下 KEY_U=tk-...
```

### 2.6 选定测试模型与调用方式

```bash
# 用你环境里真实可用的模型；compat 端点示例：
MODEL='agnes-3.0-flash'   # 或 gpt-5.6-sol 等
probe() {  # $1=key  $2=model
  curl -s -o /tmp/resp.json -w 'http=%{http_code}\n' \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":8}" \
    http://127.0.0.1:8317/v1/chat/completions
  head -c 300 /tmp/resp.json; echo
}
```

---

## 3. L1 层：关键验证（先做，决定后续用例是否有意义）

### L1-1 ★最高优先级：宿主是否把客户端 Key 透传给 scheduler

> 若此用例失败，**所有绑定都会静默失效**（插件读不到 Key → 当作非插件请求 → 走原调度，即 fail-open）。

**目的**：确认 `PickAuthForKey` 真的能由 `Authorization` 识别出当前 Key。

**步骤（用一个绑定到账户 A 的 Key 发一次真实请求，观测实际命中的账户）**：

1. 先看基线：用**无绑定** `KEY_U` 连发 5 次，观察命中的账户分布（用 §4.1 的观测手段）。
2. 用**已绑定账户 A** 的 `KEY_A` 连发 5 次。
3. 对比两者命中账户。

**预期**：

- `KEY_A` 的 5 次请求**全部**命中账户 A；
- `KEY_U` 按原有调度行为分布（无并发限制时会话由宿主决定）。

**判定与失败含义**：

| 现象 | 判定 | 含义 |
| --- | --- | --- |
| `KEY_A` 只命中 A | ✅ 通过 | 绑定生效，进入 L2 |
| `KEY_A` 命中了 A 之外的账户 | ❌ **严重** | 绑定未生效且 fail-open，属于访问隔离失效，**必须回退或热修** |
| `KEY_A` 返回 `bound_auth_unavailable` | ⚠️ | 插件识别到了 Key（说明透传 OK），但账户 A 当时不可用 → 查 §5.2 |

**如何观测"命中了哪个账户"**（三选一，按可行性排序）：

1. **看上游账号侧**：请求到 `gaoding_003@163.com` 的用量/日志（最直接）。
2. **看宿主日志**：`journalctl -u cliproxyapi -f` 观察 auth 选择与 provider 请求日志。
3. **看插件用量记录**：请求后查管理接口
   ```bash
   curl -s -H "Authorization: Bearer $KEY" \
     'http://127.0.0.1:8317/v0/management/credit-manager/usage?page=1&page_size=5' | python3 -m json.tool
   ```
   （若用量视图含 auth 维度，可直接看出账户）

**若 L1-1 失败（确认为未透传）**：不要继续跑 L2，直接按设计文档 §5 的兜底方案处理——在 executor 调宿主前注入一个仅内部使用的 Key 身份头（只传 kid 或内部随机 token），并让 scheduler 读取；**不要把明文 key 复制到额外 header**。

---

## 4. L2 层：核心路由语义

### 4.1 TC-01 有绑定 Key 只能命中绑定账户

- **前置**：账户 A 可用；`KEY_A` 绑定 A。
- **步骤**：`probe "$KEY_A" "$MODEL"` 连续 10 次。
- **预期**：10/10 命中 A；无一次落到其他账户。
- **失败含义**：交集过滤失效（`bound` map 或 provider 规范化有问题）。

### 4.2 TC-02 无绑定 Key 保持旧行为

- **前置**：`KEY_U` 无绑定。
- **步骤**：`probe "$KEY_U" "$MODEL"` 10 次。
- **预期**：行为与 v1.8.0 一致（无并发限制时插件返回 `Handled:false`，由宿主决定）。
- **验证深一层**：给账户 A 设并发上限 1 并占用后，`KEY_U` **应能**改用其他账户（证明未绑定 Key 不受绑定约束）。
- **失败含义**：无绑定分支误判，可能影响所有存量 Key —— 最高回归风险。

### 4.3 TC-03 绑定账户不在宿主候选集时被排除（fail-closed）

- **前置**：`KEY_B` 绑定一个**不存在**的 `auth_id`（如 `account-does-not-exist`）。
- **步骤**：`probe "$KEY_B" "$MODEL"`。
- **预期**：请求失败，错误码 `bound_auth_unavailable`，**绝不**回退到真实账户 A。
- **失败含义**：`len(scoped)==0` 分支未 fail-closed，属于越权风险。

### 4.4 TC-04 多账户按 Key 独立轮询（RR）

- **前置**：账户 A、B 均可用；`KEY_M` 同时绑定 A+B；另有 `KEY_M2` 同样绑定 A+B。
- **步骤**：各自连发 4 次，记录命中序列。
- **预期**：两个 Key 都从 A 开始交替（A,B,A,B…）；**不共享游标**（若共享，第二个 Key 会从 B 开始）。
- **失败含义**：`nextAuthPickLocked` 的 scope 未按 `plugin_key_id` 隔离。
- **注**：需要 ≥2 个账户，当前环境需先做 §2.3。

### 4.5 TC-05 绑定账户达到并发上限时被排除

- **前置**：账户 A 设 `max_concurrent_requests=1`；`KEY_M` 绑定 A+B。
- **步骤**：并发发 2 个请求。
- **预期**：1 个命中 A，另 1 个命中 B（A 已达上限被过滤）；不是失败。
- **失败含义**：绑定路径未叠加并发过滤。
- **设置方式**：
  ```bash
  curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"provider":"codex","auth_id":"<A>","max_concurrent_requests":1}' \
    http://127.0.0.1:8317/v0/management/credit-manager/auth-quotas/concurrency
  ```
- **测试后务必还原为 0（不限）**。

### 4.6 TC-06 绑定账户全部达上限 / 全部 warmup 占用时 fail-closed

- **前置**：`KEY_A` 只绑定 A；A 达到并发上限（或处于 warmup hold）。
- **步骤**：`probe "$KEY_A" "$MODEL"`。
- **预期**：`bound_auth_unavailable`，**不得**改用未授权的 B。
- **失败含义**：`len(available)==0` 分支未 fail-closed。

### 4.7 TC-07 provider 别名 / 大小写规范化

- **前置**：用别名绑定，例如 `{"provider":" OpenAI ","auth_id":"<A>"}` 或 `"anthropic"`（若用 claude）。
- **步骤**：`probe` 该 Key。
- **预期**：仍能匹配到宿主候选里的 `codex`，绑定生效。
- **失败含义**：`normalizeBindingProvider` 与 `authLimitProvider` 归一不一致。
- **交叉验证**：不需要读库。`keys` 列表接口回显的 `auth_bindings[].provider` 已是归一后的值，
  用别名绑定后请求一次列表，确认返回的是 `codex` 而不是 `OpenAI` 即可：
  ```bash
  curl -s -H "Authorization: Bearer $KEY" \
    'http://127.0.0.1:8317/v0/management/credit-manager/keys?q=bind-test&page_size=10' \
    | python3 -c 'import json,sys; [print(k["label"], k.get("auth_bindings")) for k in json.load(sys.stdin)["items"]]'
  ```

---

## 5. L3 层：管理接口与控制台语义

### 5.1 TC-08 更新接口字段语义（三态）

- **步骤与预期**：

| 请求体 | 预期 |
| --- | --- |
| 不含 `auth_bindings` | 绑定**不变** |
| `"auth_bindings": []` | 绑定被**清空**（恢复默认调度） |
| `"auth_bindings": [{...}, {...}]` | 绑定被**整体替换** |
| `"auth_bindings":[{"provider":"codex"}]` | **400**，且原绑定不被破坏 |

```bash
# 省略字段
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"id":"'"$KEY_A_ID"'","label":"renamed"}' \
  http://127.0.0.1:8317/v0/management/credit-manager/keys/update | python3 -m json.tool
# 空数组清空
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"id":"'"$KEY_A_ID"'","auth_bindings":[]}' \
  http://127.0.0.1:8317/v0/management/credit-manager/keys/update | python3 -m json.tool
```

- **失败含义**：控制台"保存时静默清空绑定"或"静默忽略绑定"。

### 5.2 TC-09 控制台端到端（人工）

1. 打开控制台 → 密钥 → 编辑 `bind-test-A`。
2. **预期**：绑定多选区**回显**先前选择；提示文案为"仅允许使用所选账户；若全部不可用，请求将失败，不会使用其他账户。"
3. 改变选择 → 保存 → 重开弹窗 → **预期**回显新选择。
4. 点"清除" → 保存 → 重开 → **预期**为空（不限制）。
5. 用搜索框搜 `auth_id`/`label` → **预期**能过滤。

- **失败含义**：两个数据源（overview 与 keys 分页）不一致会导致回显为空、保存时清空绑定。

### 5.3 TC-10 列表接口带回绑定

```bash
curl -s -H "Authorization: Bearer $KEY" \
  'http://127.0.0.1:8317/v0/management/credit-manager/keys?page=1&page_size=10' \
  | python3 -c 'import json,sys; [print(k["label"], k.get("auth_bindings")) for k in json.load(sys.stdin)["items"]]'
```

- **预期**：每个 Key 都带 `auth_bindings` 字段（无绑定为 `[]`，不是缺字段）。

### 5.4 TC-11 轮换继承绑定

```bash
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"id":"'"$KEY_A_ID"'"}' \
  http://127.0.0.1:8317/v0/management/credit-manager/keys/rotate | python3 -m json.tool
```

- **预期**：新 Key 的 `auth_bindings` 与旧 Key 相同；**不能**变成无绑定。
- **失败含义**：轮换一次即静默绕过访问隔离。

### 5.5 TC-12 删除 Key 清理绑定

```bash
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"id":"<待删Key>"}' \
  http://127.0.0.1:8317/v0/management/credit-manager/keys/delete | python3 -m json.tool
```

- **预期**：`key_auth_bindings` 中该 key 的行被删除，无孤儿绑定。
- **观测**：删除后再查列表确认该 Key 无绑定残留。

---

## 6. L4 层：回归与回退

### 6.1 TC-13 存量 Key 未受影响

- **步骤**：对生产中已有 Key 的调用路径做一次连通性验证（`probe` 或真实客户端调用）。
- **预期**：与升级前一致，无新增失败。
- **失败含义**：迁移或 `PickAuthForKey` 影响了无绑定分支。

### 6.2 TC-14 回退演练（已验证，可复跑）

```bash
# 列出备份
bash /root/credit-manager-ops/dsh-rollback.sh --list
# 回退代码 + 配置（不还原数据库）
bash /root/credit-manager-ops/dsh-rollback.sh /root/credit-manager-backups/<ts>
# 确认加载版本
journalctl -u cliproxyapi -n 20 --no-pager | grep -i 'plugin loaded'
# 重新前进
bash /root/credit-manager-ops/dsh-deploy.sh /root/credit-manager-release/credit-manager-v1.8.1.so 1.8.1
```

- **预期**：回退后日志显示 `version=1.8.0`；重新部署后显示 `version=1.8.1`；两次服务均 `active`。
- **注意**：**不要**轻易用 `--with-data`（会替换生产数据库）。它只用于"新版本写坏了数据"的场景，且会先把当前库移到 `*.pre-rollback-<ts>`。

### 6.3 TC-15 并发安全（需 C 工具链，服务器可跑）

```bash
ssh gdapi
cd /root/credit-manager
CGO_ENABLED=1 go test -race -count=1 -run 'PickAuthForKey' ./internal/service/... ./internal/store/...
```

- **预期**：通过，无 data race。
- **说明**：本机（Windows）无 C 编译器，此项**至今未跑**；服务器有 `gcc`，建议补上。这也是设计文档测试清单第 10 条。

---

## 7. 清理

```bash
# 测试完成后删除测试 Key
for id in "$KEY_A_ID" "$KEY_U_ID" "$KEY_B_ID"; do
  curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"id":"'"$id"'"}' \
    http://127.0.0.1:8317/v0/management/credit-manager/keys/delete >/dev/null
done
# 还原并发上限为不限
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"provider":"codex","auth_id":"<A>","max_concurrent_requests":0}' \
  http://127.0.0.1:8317/v0/management/credit-manager/auth-quotas/concurrency >/dev/null
```

---

## 8. 用例执行顺序（依赖关系）

```
§2 前置
  └─> L1-1 ★透传验证 ──失败──> 停止，走文档 §5 兜底方案
        └─通过─> TC-01 ─> TC-02 ─> TC-03 ─> L3(TC-08..TC-12)
                    │
                    └─(需 §2.3 补第二账户)─> TC-04 ─> TC-05 ─> TC-06
                                                      └─> TC-15
```

**最小可判定集**（时间有限时只跑这些）：**L1-1 → TC-01 → TC-02 → TC-03 → TC-08 → TC-11**。
这 6 项覆盖了全部三个"必须避免的错误"：越权回退、绑定失效、轮换绕过。

---

## 9. 已知缺口与风险

1. **L1-1 未经实测**：宿主透传 `Authorization` 已有强代码证据，但**运行时确认仍缺**。这是当前唯一可能让整个功能静默失效的点。
2. **多数多账户用例暂不可跑**：生产只有 1 个 OAuth 账户，TC-04/05/06 需先补第二账户。
3. **`go test -race` 至今未跑**（本机无 C 编译器；服务器有 gcc）。
4. **控制台 UI 无法由我直测**：管理密钥不可恢复，UI 层只能由执行者人工确认。
5. **v1.8.1 无 GitHub Release**：宿主配置 `install.type: github-release` 指向的 `v1.8.1` 尚不存在。本次为源码编译部署，不影响运行；若日后走插件商店更新会需要该 tag。
6. **`key_auth_bindings` 目前 0 行**：我未向生产库写入任何测试数据。
