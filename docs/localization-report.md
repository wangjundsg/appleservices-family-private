# 评审结论逐条定位（只读核对，未改代码）

日期：2026-11-13
核对对象：`D:/appleservices-family-private` @ `62f5830`
方法：逐条读源码取证，不做任何修改，不调用真实账号/短信/导入接口。

判定口径：
- **属实** = 代码里能直接看到所述行为
- **部分属实** = 现象存在，但结论或影响被放大
- **不成立** = 与代码不符
- **无法定位** = 缺少证据，需要外部输入

---

## 一、必须优先处理的问题

### 1. 创建账号未完成也会打印 done —— ✅ 属实

`tools/capture/create-account.cjs`

| 行 | 行为 |
| --- | --- |
| 405 | iframe 取不到 → `break` 跳出阶段循环 |
| 420-422 | 页面文本不匹配任何已知阶段 → `break` |
| 425-428 | 等码超时 → `break` |
| 436-438 | **无论上面哪种，都会写日志、关浏览器、打印 `done steps=N`，退出码 0** |

确认：整条路径没有任何"成功证据"断言。校验码没通过时 398 行会抛错（这条是唯一有守门的地方），但**通过校验码之后的任何异常都不会导致失败退出**。

### 2. alias 阶段忽略代理 —— ✅ 属实

`icloud/hme.go:112-117`

```go
func (c *HideMyEmailClient) resetHTTP() {
    transport := gsa.target serviceRootTransport().(*http.Transport)
    transport.Proxy = nil          // ← 明确禁用代理
    ...
}
```

确认：GSA 的根传输本身支持 `HTTP(S)_PROXY`（`gsa/apple_root_ca.go:24`），但 alias 这里把它显式置空。**"各阶段同出口"只在配置层成立，实际路由不一致。**

### 3. 导入工具与现有客户端契约不一致 —— ✅ 属实

| 项 | 我们的 `tools/poolimport` | 现有客户端 `icloud_v2_client.py:570-581` |
| --- | --- | --- |
| 账号字段 | `source` + `source_account_id` | **`apple_account_id`** |
| request_id | 仅非空 | **必须是 UUID**（`_pool_uuid`） |
| 条目数 | 不限制 | **1~100** |
| 回执校验 | 无 | 必须含 `request_id` 回显 + `results` 数组 |

确认：字段名与校验规则都对不上，当前工具**不可能**和运行版 8788 真正对接。

### 4. 2xx 即成功、且失败提示要求换键 —— ✅ 属实

`tools/poolimport/main.go`

| 行 | 行为 |
| --- | --- |
| 299-300 | `200 <= status < 300` → 直接 `stateDone`，不解析回执 |
| 313-315 | 非 done 时提示"确需重试请换新的 request_id" |

确认：无效 JSON、空回执、条目级失败都会被永久缓存为完成；pending 场景提示换键，与"同键重放"互相矛盾。

### 5. 校验码与认证响应进入日志/截图 —— ✅ 属实

| 位置 | 行为 |
| --- | --- |
| `gsa-login.cjs:69` | 打印 `sms code <code>` |
| `create-account.cjs:416/419` | 把邮箱校验码/短信校验码写入 `run.jsonl` |
| `create-account.cjs:430` | 填码后截图 |
| `create-account.cjs:279` | 截取响应正文片段入日志 |

确认：调试产物里包含可直接使用的认证材料。

### 6. 等待上限约束不住挂起请求；子进程被杀可能报成功 —— ✅ 属实

| 位置 | 行为 |
| --- | --- |
| `gsa-login.cjs:81` | `process.exit(code \|\| 0)` —— 子进程被信号终止时 `code=null` → 退出码 0 |
| `create-account.cjs:52` | `fetch` 无显式取消期限，外层 deadline 只在 await 返回后才检查 |
| `keepalive-all.ps1:45` | 逐账号串行等待，脚本层无单账号时限 |

确认。

---

## 二、下一优先级

### 7. 台账缺并发保护与服务隔离 —— ✅ 属实（需补证）

- `main.go:241` 附近以 request_id 查缓存，摘要不含 endpoint；
- `main.go:561` 附近读-改-写整个台账文件，rename 只保证单次写原子，不防两个实例并发。

### 8. 取邮件只取第一个六位数 —— ✅ 属实

`create-account.cjs:52-66`：取前 10 封、按返回顺序、不做发件人/收件人/主题校验、不排序，命中第一个 `<b>6位</b>` 就返回。

### 9. hmecli reserve 未在写前持久化 —— ✅ 属实

`hmecli/main.go:126-137`：候选为空时先 `Generate` 再 `Reserve`，候选没有落盘；远端成功但响应丢失时重跑会生成另一个候选。`delete` 的"停用→删除"两步同样没有恢复点（143-148）。

### 10. -session 与 -dir 缺交叉校验 —— ⚠️ 部分属实

`hmecli/main.go:157` 起，`-session` 内容作为账号来源。是否与 `-dir/session.json` 交叉比对，取决于 `accountBinding` 后续分支；本次只读到文件解析部分，**未读到显式交叉校验**，但也没有证据表明它完全缺失——标记为待补证。

---

## 三、评审"尚不能下结论"的四项，我这边补证

| 评审说法 | 补充事实 |
| --- | --- |
| 未定位保活程序源码 | 源码在 `D:/Workbench/sources/cloud platform-Privacy-Mail-v2/cmd/applekeepalive/main.go`（本仓库外），构建产物 `bin/applekeepalive.exe`。失败路径**不覆盖**会话（写入前先 `ValidateSession`，失败只更新 `last_check_*` 字段）。需要把该目录纳入版本管理才能被评审。 |
| 出口与受限的因果 | **同意评审。我撤回"IP 混用导致受限"的结论**：alias 阶段根本没用代理，实际出口从未被逐阶段核验，因果不成立。 |
| APNs 令牌 | 同意：32 字节只是本次观测，不能当契约；令牌可变、可刷新，需分别核验"令牌更新 / 连接存活 / 账号授权 / 设备登记"四件事。 |
| 图像挑战 20% | 属实：该数字来自我们自己的样本统计，非受控实验；本次未复测。 |
| 8788 retire 语义 | 未验证，同意保留"首期不接自动删除"。 |

---

## 四、结论

评审的 10 条里，**9 条属实、1 条待补证**；没有一条与代码矛盾。

我之前的三处判断需要修正：

1. "创建账号失败会退出非零" —— 只在校验码阶段成立，之后不成立；
2. "各阶段同出口" —— 配置层成立，实际路由不成立（alias 显式禁代理）；
3. "IP 混用导致受限" —— **撤回**，无证据。

## 五、下一步（待批准后执行）

1. 先定"什么算成功"：从已成功的样本里提取可判定的成功证据（页面标识/后续可登录），再改输出契约；
2. 逐阶段出口探测（只读），确认真实路由；
3. 按现有客户端 schema 重写导入工具；
4. 回执解析 + 台账绑定 endpoint + 单实例锁；
5. 日志脱敏（只记布尔与错误码）；
6. 超时与子进程回收；
7. 取邮件改为"发件人 + 收件地址 + 用途"三重匹配。

---

## 六、账号生命周期实测样本（2026-11-13，us-04）

| 时刻 | 事件 | 观测 |
| --- | --- | --- |
| 10:30 | 创建完成 | `validation/appleid` 复查 `used=true` |
| 11:09 | 网页会话 + 条款 | 18 cookie，`terms_update_needed=false` |
| 11:55 | 保活复验 | `webservices=27`，会话有效 |
| 12:31 | 保活复验 | `HTTP 403`，`webservices=0` |
| 14:2x | 设备端登录 | 页面提示「此账户已锁定 / 请求访问」 |

关键事实：

1. **没有任何一次失败密码尝试**，锁定不是试错触发；
2. 期间存在出口不一致（创建走住宅、登录与保活走本机隧道），但**未能证明因果**；
3. 锁定发生在最后一次成功保活之后约 36 分钟；
4. 未收到任何通知邮件（属正常，平台一般不通知）。

结论：锁定机制尚未定位。下一个样本必须满足「创建 → 立即设备登录 → 立即消费」
的连续路径，才能把"新号太脆"与"行为异常"两个变量分开。

---

## 七、导入前置条件（读运行源码后的结论）

依据：`D:/Workbench/sources/iCloud-Privacy-Mail-v2/internal/store/mailbox_pool.go`

### 7.1 真实请求契约

```go
// PoolImportContext (131-135)
if !ValidPoolUUID(request_id) || apple_account_id == "" || len(items) < 1 || len(items) > 100 {
    return POOL_INVALID_REQUEST(400)
}
// item 只接受两个字段
domain.NormalizePoolIdentity(item.BaseAddress, item.TagAddress)
// 同一批次内 base 重复 → POOL_INVALID_REQUEST
```

请求体应为：

```json
{ "request_id": "<uuid>", "apple_account_id": "<id>",
  "items": [ { "base_address": "...", "tag_address": "..." } ] }
```

**不是** `source / source_account_id / address / alias / delivery_ref / tag`。

### 7.2 回执结构

```go
type PoolImportResult struct {
    BaseAddress string
    Success     bool
    Code        string   // OK / POOL_BASE_NOT_FOUND / POOL_OWNER_MISMATCH ...
    HTTPStatus  int
    Outcome     string   // imported / unchanged / rejected
}
// 汇总：Imported / Failed / Unchanged + Results[]
```

同一 `request_id` 重放会直接返回**已存储的回执**（`poolReplayTx`），所以"同键重放"在服务端是被支持的。

### 7.3 两个硬前置（这才是关键）

```go
poolAppleExistsTx(apple_account_id)   // 163 行
  → apple_accounts 表里没有这条 → 404 POOL_APPLE_ACCOUNT_NOT_FOUND

poolMailboxTx(apple, base)            // 88-105 行，逐条
  → mailboxes 表里没有这个 base  → 404 POOL_BASE_NOT_FOUND
  → mailbox.AccountID != apple  → 409 POOL_OWNER_MISMATCH
```

**结论：import 不是"创建入口"，是"纳管入口"。**
一个新地址要能进池，必须先满足：

1. 该账号已存在于邮箱台的 `apple_accounts`（即账号要在邮箱台里登记/登录过）；
2. 该 base 地址已存在于邮箱台的 `mailboxes`（即地址要由邮箱台自己的创建流程产生）。

### 7.4 对生产方的影响

原来的设想"生产方建 HME → 直接 import"**行不通**。可行顺序只有两种：

| 顺序 | 做法 | 代价 |
| --- | --- | --- |
| A | 生产方建号 → 账号在邮箱台登录 → **由邮箱台补水流程创建地址并自动纳管** | 生产方不碰创建，只提供账号 |
| B | 生产方建号 + 建 HME → 先调邮箱台的地址登记接口写入 `mailboxes` → 再 import | 需要邮箱台开放登记接口 |

A 更符合现有分工（评审也建议如此）；B 需要对方新增接口。

---

## 八、指纹层落地状态（2026-11-13）

已实现（commit 35add2d + 本次）：

| 项 | 状态 |
| --- | --- |
| ① `internal/profile` 生成/校验/冻结/观察 | ✅ 8 条规则，22 个子用例 |
| ② `tools/profilegen` | ✅ 生成/校验/回填 actual |
| ③ 建号脚本 `--profile` | ✅ 复用账号级配置，回写 actual |
| ④ 协议阶段读 profile 并记录 actual | ⚠️ 部分：GSA 包装层已读配置并回写 actual.engine；**网页登录/保活的 UA 覆盖未做**（外部仓库 CLI） |
| ⑤ 单元测试 | ✅ |

### 一个必须写清楚的技术边界

GSA 阶段的 `User-Agent` **由 anisette 设备身份决定**（`gsa/client.go:142-156` 使用 akd/CFNetwork 串），
不能用浏览器 UA 覆盖——覆盖会破坏设备身份一致性，属于自相矛盾。
所以 profile 只在"浏览器 HTTP"阶段生效（网页登录、保活、可选的 Store 路径）。

### 已修的一个缺陷

`gsa-login.cjs` 原来 `process.exit(code || 0)`：子进程被信号终止（code=null）会被当成功。
现在改为 `code === 0 && !signal` 才算成功，并记录 `signal`。
