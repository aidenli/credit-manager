# credit-manager 安全审计：密钥泄露与请求篡改

审计对象：commit `26a30ac`（插件 v1.8.3），线上部署版本一致
审计方式：全量静态审计 + 线上只读验证
日期：2026-09-18

---

## 结论摘要

| 审计项 | 结论 |
| --- | --- |
| **A. 插件自身是否泄露密钥** | **未发现泄露**。管理 API 不回传任何密钥材料；插件代码**零日志输出**；审计表不存密钥 |
| **B. 插件是否把客户端 Key 转发给上游** | **代码层面否**。所有 provider 执行器都覆盖或删除 `Authorization`。但存在一处**潜在缺口**，且我**未能取得运行时证据**（见 B-3） |
| **C. 是否修改原始请求信息** | **关键路径完全不修改**。`execute()` 原样透传 body；唯一改写点是 `/v1/models` 目录过滤，属设计功能 |

---

## A. 密钥泄露风险

### A-1 管理 API 响应：只返回非机密字段 ✅

`internal/management/views.go:keyView` 仅输出 `id`、`kid`、`fingerprint`、`label`、额度、模型限制等。
`kid` 是公开标识（明文格式 `tk-<kid>-<secret>` 的前半段），`fingerprint` 是 `sha256("fingerprint:"+kid)`（`keys.go:260`），二者都**不含 secret**。

全仓库检索 `internal/management` 下 `encrypted_key_material` / `key_hash` / `pepper` / `refresh_token` / `access_token` / `api_key` → **0 命中**。

### A-2 明文 Key 的两个出口，均受控 ✅

| 出口 | 位置 | 说明 |
| --- | --- | --- |
| 创建 / 轮换响应 | `management/keys.go:114,144` | 明文仅在 mint 与 rotate 时返回一次 |
| 查看明文 | `management/keys.go:316` | `revealKey`，需管理密钥鉴权；且先 `keys.Verify` 校验哈希一致才返回 |

内部路径 `Material.Plaintext` 仅存在于内存与上述两处，无持久化、无额外回传。

### A-3 插件自身不产生任何日志 ✅

检索 `internal/**/*.go` 的 `fmt.Print*` / `log.Print*` / `os.Stdout` / `println(` → **0 命中**。

插件运行在宿主进程内，若它打印任何东西都会进 `journalctl`/`main.log`。**它什么都不打印**，因此不存在"插件把 Key 写进日志"的通道。

### A-4 审计表不含密钥材料 ✅

`insertAudit` 的 `details_json` 只写元数据：

- `reserve.go:375` → `{"reason":...}`
- `settle.go:103` → `{"source":...,"over_held":...}`
- `usage.go:282` → `{"previous":...,"cost":...}`
- `spend_reset.go:185` → 重置范围

`auditView` 输出 `plugin_key_id`（主键 ID，非 Key 材料）、金额、时间。无 token、无明文、无密文。

### A-5 数据库存储 ✅

- `key_hash`：**HMAC-SHA256(pepper, plaintext)**，单向，pepper 独立存于 `key-peppers`（0600）
- `encrypted_key_material`：AES-GCM，key = `sha256("token-quota:key-encryption:v1\0" + pepper)`
- 明文不可逆推

### A-6 日志扫描（线上）与它的局限 ⚠️

扫描 `/root/.cli-proxy-api/logs`（84 文件，含 `main.log` 1.4MB）中形如 `tk-<kid>-<secret>` 的串：

```
distinct candidates: 1
```

该唯一命中是长度 **917** 的串（`tk-hoP…`），远超真实 Key 的长度：

- 真实 Key = `tk-` + kid(16 base32) + `-` + secret，**总长 46**
- 917 字符的 `tk-` 串是日志正文里用户自己的会话内容中恰好含 `tk-` 文字，**不是 Key**

**必须说明这次扫描的验证缺陷**：我原本设计成"把候选值与 `plugin_keys.key_hash` 比对"来给出密码学结论，但这是**数学上不可能**的——`key_hash = HMAC-SHA256(pepper, 明文)` 是对**我没有的明文**做单向函数，只有恰好扫到真实明文才可能匹配。所以扫描结果只能**按长度/形状分类**，不能作为"确无泄露"的证明。真实结论应表述为：

> 在已扫描的日志中，未发现符合真实 Key 形状（`tk-<16位base32>-<base32>`）的串。

### A-7 附带确认：宿主日志已自动脱敏 ✅

错误日志的 `=== HEADERS ===` 段落中，`Authorization: Bearer <MASKED>` 已被宿主打码，不是原文。

---

## B. 客户端 Key 是否被转发给上游

### B-1 插件确实把请求头整体交给宿主（这是有意的）

`plugin/execute.go:77` → `hostModelExecute(req.HostCallbackID, req.ExecutorRequest, body, false)`
`plugin/execute.go:367` → `Headers: req.Headers`

`req.Headers` 含客户端原始 `Authorization: Bearer <插件Key>`。
**注**：这也是绑定功能能工作的前提（scheduler 靠它识别 Key），不能去掉。

### B-2 宿主侧各 provider 执行器都会覆盖 / 删除该头 ✅

| provider | 位置 | 行为 |
| --- | --- | --- |
| codex（/v1/responses 主路径） | `codex_executor_request.go:317` | `r.Header.Set("Authorization","Bearer "+token)` — **无条件覆盖** |
| codex | `codex_executor_request.go:59` | `Set(...Bearer apiKey)` — 有 apiKey 时覆盖 |
| codex 直连出图 | `codex_executor_request.go:315` | 同上，无条件覆盖 |
| codex websocket | `codex_websockets_request.go:72` | 新建 header，`Set` |
| claude | `claude_executor_request.go:564` | `Del("Authorization")` 或 `Set` |
| gemini | `gemini_executor.go:90` | `Del("Authorization")` |
| gemini vertex | `gemini_vertex_executor.go:201` | `Del("Authorization")` |

`http.Header.Set` 是**替换**语义（非 `Add` 追加），因此传入的插件 Key 会被覆盖掉。

### B-3 ⚠️ 潜在缺口 + 缺少运行时证据

**缺口**：`codex_executor_request.go:53 PrepareRequest` 只在凭据非空时才覆盖：

```go
apiKey, _ := codexCreds(auth)
if strings.TrimSpace(apiKey) != "" {
    req.Header.Set("Authorization", "Bearer "+apiKey)
}
```

若 `codexCreds(auth)` 返回空（`Attributes["api_key"]` 与 `Metadata["access_token"]` 都取不到），则**不覆盖**，继承进来的插件 Key 会保留在发往上游的请求上。

代码阅读显示主路径（`applyCodexHeadersFromSources`）是无条件 `Set`，所以正常流程安全；但**这个"正常流程"我没有取得运行时证据**。

**如何补上这个证据**（二选一，都需要你授权）：

1. **抓包法**：临时对 `/v1/responses` 的 provider 出口加一次 header dump（或在上游侧看收到的 `Authorization`），用真实 Key 发一次请求，确认上游看到的是 OAuth token 而非 `tk-`。
2. **埋点法**：临时起一个本地假 provider（`base-url` 指向本机 echo 服务），发一次请求，直接读出收到的 header。

我**没有**用生产流量做这个测试，因为我无法在不触碰生产库/流量的前提下构造合法 Key（`Own` 的移交锁 `hot-reload handover timed out` 会阻止离进程 mint）。

---

## C. 是否修改原始请求信息

### C-1 关键路径：完全不修改 ✅

`plugin/execute.go:52` → `body := requestBody(req.ExecutorRequest)`，随后**原样**传给 `hostModelExecute`（`:77`）。全程无解码/重编码/字段改写。

`requestBody` 优先用 `req.OriginalRequest`（`plugin/usage.go:163`），即**客户端原始字节**。

这意味着：model、messages、tools、温度、以及各类签名/缓存字段**都不会被改动**——对 OAuth 签名与 prompt cache 是安全的。

### C-2 唯一的 body 改写点：`/v1/models` 目录过滤（设计功能）

`plugin/auth.go:158` → `svc.FilterModelDirectory(ctx, req.Body)`

`service/models.go:90 filterModelDirectory`：
- 先 `json.Unmarshal`，若不满足 `looksLikeModelDirectory`（`object=="list"`，或有 `models` 且**无** `choices`）→ **原样返回 body**（`:99-101`）
- 只有确实删掉了被禁模型时才 `json.Marshal` 返回新 body（`:132-139`），`changed=false` 时同样原样返回

判定谓词相当保守（要求 `models` 且无 `choices`），普通 chat/responses 请求不会被误判。

### C-3 唯一的 header 改写：自己注入的内部头，且发上游前删除 ✅

`interceptRequestAfterAuth` 为流式请求注入 `X-Credit-Manager-Request-Token`（`request_lifecycle.go:16`），值是宿主的 `requestID`（不透明关联 ID，非机密）。

在嵌套调用上游前删除：

```go
// request_lifecycle.go:133-140
func lifecycleIDFromHeaders(headers http.Header) string {
    requestID := strings.TrimSpace(headers.Get(lifecycleRequestHeader))
    headers.Del(lifecycleRequestHeader)   // 就地删除
    return requestID
}
```

`runStream` 第一步即调用它（`execute.go:117`）。**该头不会到达上游**。

### C-4 未发现的行为 ✅

- 无 `Add`/追加型 header 注入（无重复头污染）
- 无 model 改写（不重写为别名）
- 无 body 大小/编码变换
- 无对响应的注入改写（原样 `Payload` 返回，`:93,:101`）

---

## 风险清单

| 级别 | 项 | 位置 | 建议 |
| --- | --- | --- | --- |
| 中 | `PrepareRequest` 凭据为空时不覆盖 `Authorization`，理论上可透传插件 Key | `codex_executor_request.go:53-59` | 取一次运行时证据（见 B-3）；若确认可触发，向宿主上游反馈或在其前显式 `Del` |
| 低 | 日志扫描无法给出密码学结论 | — | 保持"按形状判定"的表述，勿宣称"已证明无泄露" |
| 低 | `revealKey` 返回明文 | `management/keys.go:303` | 已有明文验证 + 管理鉴权 + `no-store`，符合预期 |

## 本次审计的边界（未做的事）

1. **未**在任何上游/出口抓取运行时 header（B-3 缺口仍在）
2. **未**对宿主 CLIProxyAPI 主程序做完整审计，只审计了与本插件相关的执行器路径
3. **未**审计 nginx / 系统层
4. 审计期间我误将一份含用户会话内容的日志片段打印到会话中（超长正文），已在后续改用**有界**命令（只取长度、去重计数、前 6 字符）
