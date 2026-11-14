# 设计：账号状态机（Account State Machine）

目标：给 Apple 产线一个**唯一真相**——任何时刻都能回答"这个账号在哪一步、为什么停、下一步该谁做"。
本文只做设计，不含实现。

---

## 一、设计目标与非目标

**目标**

1. 账号状态**只有一个owner**，其余组件只读或提交转移请求
2. 每个阶段是**幂等任务**：重试不会重复花钱、不会重复提交
3. 任何失败都能回答：**在哪一步、什么原因、是否可重试**
4. 面板 / CLI / 定时任务都只是"发起者"，不含编排逻辑
5. 文件系统只放**证据**，不放状态

**非目标**

- 不做分布式（单机单进程足够）
- 不引入数据库（一个 JSON + 文件锁够用）
- 不做通用工作流引擎（只管这五个阶段）

---

## 二、数据模型

```go
// Job 是一个账号的全生命周期记录
type Job struct {
    ID        string    `json:"id"`          // 账号标识，与目录同名
    Email     string    `json:"email"`       // Apple ID 邮箱
    Stage     Stage     `json:"stage"`       // 当前阶段
    State     State     `json:"state"`       // 阶段内状态

    Attempt   int       `json:"attempt"`     // 当前阶段尝试次数
    Operation string    `json:"operation_id"`// 当前进行中的操作 id（幂等键）

    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
    NextRunAt time.Time `json:"next_run_at"` // 限流/冷却后的最早可执行时间

    LastError *Failure  `json:"last_error,omitempty"`
    Evidence  string    `json:"evidence_dir"`// 证据目录（只写不改）
    Meta      Meta      `json:"meta"`        // 该账号的运行配置快照
}

type Failure struct {
    Code      string    `json:"code"`       // 见第四节
    Message   string    `json:"message"`    // 人读
    Retryable bool      `json:"retryable"`
    At        time.Time `json:"at"`
}

type Meta struct {
    ProfileRef string `json:"profile_ref"` // 指纹配置引用
    EgressRef  string `json:"egress_ref"`  // 出口引用
    PhoneCC    string `json:"phone_cc"`
    Engine     string `json:"engine"`      // roxy | playwright | patchright
}
```

**存储**：`state/jobs/<id>.json`，原子写（临时文件 + rename），文件锁保护读改写。

---

## 三、阶段与转移

```
created ──► registered ──► logged_in ──► terms_accepted ──► device_bound ──► active
   │            │              │              │                 │
   └────────────┴──────────────┴──────────────┴─────────────────┘
                        任一阶段失败 → failed / blocked
```

| 阶段 | 由谁执行 | 成功判据（可验证） |
| --- | --- | --- |
| `created` | Node（浏览器） | 账号确实存在（登录能识别该邮箱） |
| `registered` | — | 建号产物落盘（profile/session.json 存在且校验通过） |
| `logged_in` | Go `login.exe` | GSA 会话建立，能取到 webservices |
| `terms_accepted` | Go `applelogin.exe` | `terms_update_needed=false` |
| `device_bound` | Go `trusted-device.exe` | 设备登记回执 + 推送令牌有效 |
| `active` | Go `applekeepalive.exe` | 一次保活校验通过 |

**阶段是单调前进的**：只能 forward，不能回退；失败不重置阶段，只记 failure。

---

## 四、失败语义（统一到一处）

```go
const (
    FailTransient   = "transient"   // 网络抖动、5xx：可重试
    FailRateLimited = "rate_limited"// 423/冷却：等到 NextRunAt
    FailInvalid     = "invalid"     // 参数/配置错：不重试，需人工
    FailUnknown     = "unknown"     // 提交已发出但结果未知：先回读，不重放
    FailBlocked     = "blocked"     // 平台判环境：不自动重试，转人工
)
```

规则：

```
transient    → attempt++，指数退避（1m/5m/15m/1h），最多 4 次
rate_limited → 设置 next_run_at（15~60 分钟），attempt 不变
invalid      → 终态，需要人工修配置
unknown      → 先执行"回读"动作确认实际状态；确认不到就停在原地等人
blocked      → 终态 + needs_review，不在同一环境重试
```

---

## 五、幂等

**每个"会产生外部副作用"的操作必须有 operation_id**：

```
建号：operation = "create:" + job.ID            （一个账号只建一次）
买号：operation = "sms:" + job.ID + ":" + attempt
登录：operation = "gsa:" + job.ID
条款：operation = "terms:" + job.ID
设备：operation = "device:" + job.ID
```

执行器开工前：

```go
if job.Operation == opID {
    // 上次已在做这件事，先回读，不重复执行
}
```

**花钱的动作（买号）额外记一张 spend 表**，按 job 汇总成本。

---

## 六、并发与锁

```
粒度：一个账号一把锁（state/locks/<id>.lock）
范围：只覆盖"读 job → 决策 → 写 job"这段，不覆盖执行本身
超时：10 分钟自动视为陈旧锁（记录 pid + 时间）
```

面板/CLI/定时任务抢的是**同一把锁**，所以不会出现两个执行器同时推进同一账号。

---

## 七、证据层（只写不改）

```
state/accounts/<id>/evidence/
  create/       建号阶段的 run.jsonl + 截图
  login/        GSA 日志
  terms/        条款截图
  device/       登记回执
  keepalive/    校验日志
```

Job 里只存 `evidence_dir` 引用。**证据可以被清理，状态不能。**

---

## 八、对外接口（给面板/CLI 用）

```go
// 读
func Get(id string) (Job, error)
func List(filter Filter) ([]Job, error)

// 推进（内部会加锁、校验转移合法性）
func Claim(id string, stage Stage, op string) (Job, error)   // 抢到执行权
func Succeed(id string, stage Stage, next Stage) error        // 阶段完成
func Fail(id string, f Failure) error                         // 记录失败
func Release(id string) error                                 // 释放锁

// 调度
func Due(now time.Time) ([]Job, error)   // next_run_at <= now 的任务
```

**面板只调这些，不碰文件。**

---

## 九、和现有代码的关系

| 现有 | 怎么接 |
| --- | --- |
| `bin/login.exe` 等 | 不改，只当"执行器"；由状态机调用并把结果写回 job |
| `tools/capture/create-account.cjs` | 同上，退出码映射到 Failure.Code |
| `internal/profile` | 作为 Meta.ProfileRef 被引用 |
| `internal/proxypool` | 作为 Meta.EgressRef 被引用 |
| `internal/dashboard/register.go`（621 行） | 拆：编排搬进状态机，面板只留 UI + 调接口 |

---

## 十、迁移步骤（不推倒重来）

```
第 1 步：加内部包 internal/account（Job + 读写 + 锁），不改任何现有脚本
第 2 步：写一个迁移命令，把 state/accounts/*/ 现有文件扫描成 Job
第 3 步：让 login.exe 的调用方改成"先 Claim 再执行"
第 4 步：面板的编排逻辑改成读 Job + 调 Claim/Succeed
第 5 步：删掉 pipeline.py，面板直接调 Node/Go 执行器
```

每步都能独立验证，且任何一步停下来系统仍然可用。

---

## 十一、验收标准

```
① 给定一个账号 id，能在 1 秒内回答"现在在哪一步、下一步谁做、为什么停"
② 同一账号并发发起两次推进，只有一次生效
③ 任何阶段重试，不会重复花钱（spend 表可核对）
④ 删掉整个 evidence/ 目录，状态依然完整
⑤ 面板重启后，任务列表与进度完全一致
```
