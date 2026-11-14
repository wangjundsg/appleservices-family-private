# 设计 v2：账号状态机（Account State Machine）

基于评审意见重写。v1 的关键缺陷（短锁不保护锁外执行、时间接管、unknown 重试规则、阶段语义）全部修正。

---

## 零、结论先行

```
先让现有编排器成为"唯一状态 owner"，统一状态格式与执行入口。
不换语言、不引入数据库、不推倒重来。
Go 化可以后置，甚至不必做。
```

v1 想一步到位重构成 Go 状态机，那是把问题搬进一个新文件。**v2 的目标是先把语义定死。**

---

## 一、两个 ID，不要混

```
AccountID   稳定标识，对应一个 Apple 账号（邮箱或账号主键）
JobID       一次任务实例（重试会生成新 JobID）
```

规则：

- `AccountID → JobID` 一对多
- **所有"一个账号只做一次"的约束挂在 AccountID 上**，不是 JobID
- 换 JobID 不能绕过任何保护（建号、买号、设备登记都要按 AccountID 查历史）

---

## 二、状态模型：里程碑 + 当前健康度

### 2.1 里程碑（单调，只增不减）

```go
type Milestones struct {
    Initialized   bool `json:"initialized"`   // 任务已创建
    Registered    bool `json:"registered"`    // 账号创建已确认
    LoggedIn      bool `json:"logged_in"`     // 首次登录成功过
    TermsAccepted bool `json:"terms_accepted"`// 条款接受过（带版本）
    DeviceBound   bool `json:"device_bound"`  // 设备登记过（带设备身份）
    FirstActive   bool `json:"first_active"`  // 首次保活通过
}
```

**里程碑只记录"曾经发生过"，不代表现在还能用。**

### 2.2 当前健康度（非单调，随时可变）

```go
type Health struct {
    SessionState  string `json:"session_state"`  // valid | expired | unknown
    TermsVersion  string `json:"terms_version"`  // 当前账号接受的版本
    TermsRequired string `json:"terms_required"` // 平台当前要求的版本
    DeviceState   string `json:"device_state"`   // bound | revoked | unknown
    LastCheckedAt time.Time `json:"last_checked_at"`
}
```

**令牌过期不抹掉里程碑，只把 Health 改成 `expired`，并驱动"重新认证"这个操作。**

### 2.3 运行状态（当前这一刻）

```go
type RunState string
const (
    RSQueued       RunState = "queued"        // 排队
    RSRunning      RunState = "running"       // 有执行器在跑
    RSWaitingInput RunState = "waiting_input" // 等验证码/人工
    RSRetryWait    RunState = "retry_wait"    // 退避等待
    RSReconciling  RunState = "reconciling"  // 结果未知，正在核实
    RSNeedsReview  RunState = "needs_review"  // 转人工
    RSSucceeded    RunState = "succeeded"
    RSCancelled    RunState = "cancelled"
)
```

**收尾状态独立**：`cleanup`（浏览器释放 / 号码退款）与主流程结果并存，
一个任务可以同时是 `failed` + `refund_pending`，不能合并成单个 `failed`。

---

## 三、执行权：租约，不是短锁

v1 的错：Claim → 释放锁 → 执行，锁外没有任何保护。

### 3.1 Claim 返回租约

```go
type Lease struct {
    JobID     string
    AccountID string
    Owner     string    // 执行器标识（pid + 随机串）
    Operation string    // 本次操作的幂等键
    Revision  uint64    // Claim 时的 Job 版本
    ExpiresAt time.Time // 心跳续期，不是硬接管
}
```

`Succeed / Fail / Heartbeat` 都必须带上 `Lease`，状态机校验：

```
① job.Revision == lease.Revision        （期间没人改过）
② job.LeaseOwner == lease.Owner         （还是同一个执行器）
③ job.Operation == lease.Operation      （还是同一个操作）
```

校验不过 → 拒绝写入并返回 `stale_lease`，**迟到结果不允许覆盖新结果**。

### 3.2 不做时间接管

```
删除 v1 的"10 分钟陈旧锁自动接管"
租约过期只意味着"可以尝试接管"，接管前必须：
  ① 确认旧执行器进程已退出（或显式 kill 并等待）
  ② 对该操作走一次"核实"（见第五节）
  ③ 核实为 not_applied 才允许重新 Claim
```

**仅作废租约挡不住旧子进程继续访问远端。**

### 3.3 锁与写入

```
· 锁文件独立且稳定（不改名、不删除）
· 临界区只包含：读 job → 校验租约 → 写 job → 落操作记录
· 执行本身在锁外，靠租约保护
· 临时文件与目标同目录同卷，替换失败必须停止推进（不能删旧再补写）
· 依赖"写入可见"前先刷盘；不能把 临时文件+rename 当作跨平台原子保证
```

---

## 四、失败：三层分离

```go
type Failure struct {
    Class    string // transient | rate_limited | invalid | unknown | blocked
    Code     string // 具体原因，如 activation_expired / terms_version_changed
    Recovery string // retry | reconcile | wait_input | manual
    Message  string
    At       time.Time
}
```

### 4.1 关键修正：写请求超时 ≠ 可重试

```
只有能证明"没有执行"、或操作本身可安全重复，才允许自动重试。
写请求发出后断连 / 超时 / 5xx，一律归 unknown → 走核实。
```

### 4.2 rate_limited 不能只等不计数

```
· 不增加"普通失败"次数
· 但必须累计：实际请求次数、发码次数、总时限
· 超出预算 → 转 needs_review，不无限循环
```

---

## 五、操作记录与核实协议

### 5.1 操作列表（不是单个字符串）

```go
type Operation struct {
    ID        string    // 幂等键
    Kind      string    // create | sms_purchase | gsa_login | terms | device | refund ...
    AccountID string
    Intent    any       // 调用前持久化的意图（请求摘要、目标、参数）
    State     string    // intent | executing | succeeded | failed | inconclusive
    Result    string    // 摘要
    ExternalRef string  // 供应商订单号 / 账号 id / 设备身份
    StartedAt time.Time
    EndedAt   *time.Time
}
```

Job 里存**操作列表**，至少保留最近 N 条 + 所有未收口的操作。

### 5.2 幂等键按 AccountID 与语义生成

| 动作 | 键 | 说明 |
| --- | --- | --- |
| 建号 | `create:<AccountID>` | 跨 Job 生效，换 Job 不能重建 |
| 买号 | `sms:<AccountID>:<purchaseIntentID>` | 意图固定后 ID 不变；传输重试不换键 |
| 登录 | `gsa:<AccountID>:<authCycleID>` | 首次登录与重新认证各一个周期 |
| 条款 | `terms:<AccountID>:<termsKind>:<version>` | 版本变更是新操作 |
| 设备 | `device:<AccountID>:<deviceIdentity>` | 设备变更是新操作 |
| 退款 | `refund:<AccountID>:<activationID>` | 独立收口状态 |

**同一 ID 带不同 Intent 必须拒绝执行。**

### 5.3 核实（reconcile）协议

每个适配器实现：

```go
type Verifier interface {
    Verify(op Operation) (Verdict, error)
}
type Verdict string
const (
    VConfirmedSucceeded Verdict = "confirmed_succeeded"
    VConfirmedNotApplied Verdict = "confirmed_not_applied"
    VInconclusive       Verdict = "inconclusive"
)
```

核实必须绑定原资源：

```
建号   → 用账号维度确认（该邮箱是否已成为账号），不能只看本地文件
买号   → 查供应商订单/激活记录，余额变化不足以定位某一笔
登录   → 同一认证周期内查会话是否建立，不能靠"再登一次"证明
条款   → 查账号已接受的条款版本
设备   → 查同一设备身份是否已登记，不能换身份重登
```

`inconclusive` 一律**不自动重放**，转人工。

---

## 六、状态归属

> 只有状态存储层可以读写权威状态；证据目录不参与任何判断。

```
state/jobs/<AccountID>/<JobID>.json   权威状态（版本化、带 revision）
state/ops/<JobID>.jsonl               操作记录（append-only）
state/runtime/                        会话/凭据/设备身份（运行资产，不可随证据删除）
state/evidence/<JobID>/               截图与日志（可清理）
```

**证据可删、状态不可丢、运行资产不可混入证据。**

---

## 七、迁移：五步，每步可停

```
① 只读状态视图
   新模块只读现有 state/accounts + .cache/panel + state/pipeline
   输出统一的 Job 视图，供比对；不驱动执行，不写权威状态

② 影子 Job 比对
   与现有编排器的状态并排显示，差异列出
   无法确认的事实标 inconclusive（不猜）

③ 统一执行入口
   新旧共享同一个 Claim/Succeed 入口
   旧任务继续跑完；新任务由新入口接管
   同一 JobID 始终只有一个 owner

④ 全量切调用方
   面板 / CLI / 定时器全部改为通过入口调用
   验证取消、收尾、退款、保活、入库都已迁移

⑤ 排空旧编排器
   旧任务跑完、故障恢复演练通过后，才删除旧编排
   执行器由服务调用，面板只提交请求与展示
```

**"不停机" = 服务可用 + 旧任务排空 + 逐任务切换**，不是活动任务中途换 owner。

---

## 八、验收（含故障场景）

```
功能
① 1 秒内回答：在哪个里程碑、当前健康度如何、正在做什么、为什么停
② 同一 AccountID 并发两次推进，只有一次生效
③ 换 JobID 不能绕过建号/买号保护

故障
④ 提交后未收到回执时进程崩溃 → 重启后状态为 reconciling，不重放
⑤ 远端成功但本地落盘失败 → 恢复后能补齐，不重复执行
⑥ 旧执行器迟到回写 → 被 stale_lease 拒绝，不覆盖新结果
⑦ 进程重启后旧子进程仍存活 → 接管前能识别并处理
⑧ 磁盘写满 / 替换失败 → 停在原地，不损坏状态
⑨ 删除 evidence/ → 状态完整，可继续
```

---

## 九、与现有代码的关系

| 现有组件 | v2 中的角色 |
| --- | --- |
| `tools/apple-pipeline/pipeline.py` | **先作为唯一状态 owner**（不重写） |
| `internal/dashboard/register.go` | 只保留 UI + 调入口；编排搬走 |
| `bin/login.exe` / `applelogin.exe` / `trusted-device.exe` / `applekeepalive.exe` | 执行器；加一层适配器把结果映射为 Operation/Health |
| `tools/capture/create-account.cjs` | 同上，退出码映射到 Failure.Class/Code |
| `internal/profile` / `internal/proxypool` | 作为 Meta 引用，保持无状态 |

**先统一语义，再谈语言。**
