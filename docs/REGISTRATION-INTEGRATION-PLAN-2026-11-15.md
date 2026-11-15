# 双路径注册：统一修复方案与整体验收契约

日期：2026-11-15。代码基线：`4bbec7d`；此前复核文档提交：`93fe7ea`。目标：现有 Windows 机器、18741 面板，browser/protocol 并存，注册完成后使用面板既有激活流程。本文替代“逐个条件打补丁”的实施方式，不替代历史缺陷证据。

## 1. 结论与交付边界

**先统一编排语义，再接真实协议适配器，最后整体验收。** 保留 Python pipeline 为业务 owner，保留现有 Go/Node 执行器和面板激活代码；不引入数据库、消息中间件、独立服务或第二套激活链路。协议指纹/请求伪装/提高服务端接受率不属于本方案。

此前返工的主要原因是同一字段承担了不同职责：status 既做显示又充当执行许可和退款依据；stage 同时表示业务阶段和协议内部挑战；operations 同时有字符串和对象；主流程、维护、手动退款各自推导资源是否可释放。测试还曾替换 begin 等关键逻辑，使单点修复通过而入口仍可绕过。

本次目标是交付一批相互兼容的修改和一套统一的测试，**不以每修一条就等待复核为开发流程**。没有任何静态审查或有限测试可以保证找到全部缺陷；本方案用边界清单、真实入口、崩溃注入及发布门槛降低漏检，而不承诺“绝无问题”。

本轮只审查和编写方案/离线复现，不修改业务实现，不运行真实购号、退款、注册、订阅、定时器或部署。价格仍待用户确定；未确定前购买开关关闭。账号、邮件、供应商凭据均不读取、不提交。

## 2. 问题全集：按根因组织，不按历史补丁组织

证据分类：**R** 为离线复现；**S** 为代码路径已确认；**V** 为必须补充验证的风险，不冒充已发生故障。这里的“全集”限定为本次检查的注册衔接边界，不包括未提交的面板其他修改、供应商线上行为或整仓所有业务。

| ID / 优先级 | 类型 | 发现、影响与定位 | 统一处理 |
| --- | --- | --- | --- |
| G01 / P1 | R | pipeline.protocol_action 忽略尚未结束的 rate_limited/cooldown_until；真实 resume 触达适配器且覆盖原因 | 一张动作许可表，冷却先于执行，保持原截止时间 |
| G02 / P2 | R | cancelled 且保留等待证据时，若 cancel.request 被归档/缺失，run 仍可 resume；停止事实依赖命令文件 | stop_requested/terminal 持久化，删除命令文件不改变许可 |
| G03 / P2 | R | operations 值是字典时，集合判断抛 TypeError，原核实原因丢失 | 统一 schema，受控拒绝坏记录，不覆盖业务原因 |
| G04 / P1 | S | execute 恢复读取 input.json/result.json 后没有完整 schema/双方式字段/账号路径绑定校验；Job 直接选 snake 或 camel | 恢复与新建同一 loader；新版本拒绝未知值，旧版本显式转换 |
| U01 / P1 | R+S | register.go 输入接口要求 active 且 stage=create；协议等待 active=false/email-sent，返回 409；写的是根目录 email-code.txt 而非 protocol 路径 | 输入按当前 challenge 与方法分流，冻结路径，原子投递 |
| U02 / P1 | R+S | 面板 POST resume 返回 405；registrationProgram 永远 run，已有 run 只是读状态；退出等待后无完整续跑入口 | 明确 resume 命令与唤醒器；重复输入/点击不重复启动 |
| U03 / P2 | S | register.js 不提交 registrationMethod，无方式选择；协议子阶段不在日志白名单，等待输入 UI 隐藏；多数新原因无中文说明 | UI 使用公开状态视图与 allowedActions，显示方式/挑战/停因 |
| U04 / P1 | S | cancel API 只落 cancel.request；等待进程退出后，maintain_once 直接改 cancelled，未统一调用适配器核实；旧命令永久存在又可能重复 cancel | 停止命令持久化、只消费一次；维护只走同一转移入口 |
| I01 / P2 | R | read_input 在适配器调用前写 consumed；该点崩溃后未发送的码也报 input_already_consumed | available/reserved/dispatch_intent/accepted 分离，不能简单改为调用后才记 |
| I02 / P1 | S | challenge 只有阶段+mtime；同阶段重新发码不生成新身份，消费摘要仅 kind+code；文件写入时间不能证明邮件属于当前挑战 | 绑定 job/account/challenge/input_type/source_id；同阶段重发也换 challenge_id |
| I03 / P1 | S | 协议适配器没有完整的 Outlook 轮询/短信交接；现有 protocol_step 不调用 next_code，prepare_followup_sms/OTP 账本仍依赖 create-sms.txt | 自动取码归资源/输入协调器，真实发送时间与 message_id 绑定，OTP事实进入账本 |
| R01 / P1 | R | Supply 已保存 PURCHASED 后 input.json 写失败，重新调用 Job.number 会再购一个号；本次资源层崩溃复现购买两次 | acquisition_id 先保存，同一意图优先从账本恢复订单，不按 rows 数生成新意图 |
| R02 / P1 | R | 手动订单 settle_number 的明确失败分支直接 setStatus=8，不先确认供应商是否已收短信 | 所有来源订单统一结算许可与 readback；已收码不可退 |
| R03 / P1 | S | 自动订单分支在手动分支的 account_created/expiry/safe_cancel 判断之前调用 Supply.settle；force/status 等不是充分退款证据 | 退款只接受决策生成的 eligibility，不能以 cancelled 或 failed 单独授权 |
| R04 / P2 | S | Outlook API 是只读，无跨系统邮箱占用接口；面板用 request.json 本地占位且过滤 outlook.com | 明确只保证本产线内占用；使用可读且受支持的已有邮箱，域名能力按执行器声明，别把猜测写成全局禁用规则 |
| A01 / P1 | S | adapter 可接收整个可变 Job；任意 waiting/ready 字典会提升父状态；缺生产真实适配器、输入输出回执验证 | 薄适配器拿冻结上下文，输出验证后才能改变业务事实 |
| A02 / P2 | S | 注册方式检查较晚，protocol 新任务仍被 Roxy、Node、YesCaptcha 等公共校验限制；adapter 配置同时支持环境与 manifest，但说明写环境唯一 | 公共校验/方法能力校验分层，可信服务配置选择实现，注入假实现只用于测试 |
| H01 / P1 | S | pipeline 与面板各存 handoff intent；回复丢失时未提供统一只读 operation 查询来补齐；sms/start 只按动作名去重，不表达认证周期 | 稳定 operation_id、请求摘要、查询旧结果；按 auth_cycle/challenge 区分合法新动作 |
| H02 / P2 | S | dashboard_handoff 每次入口先 reserve_code，即使 start 已成功也扣预算；后续码可用性在 browser create 检查而 protocol 不一致 | 预算计真实发码意图，读结果/轮询不扣；后半段资源要求在首次花费前统一检查 |
| P01 / P1 | V | Python 父进程被强杀后，子进程是否仍访问远端未验；当前文件锁只证明父进程持锁，不能证明旧 child 已死 | 受控进程树、pid+启动时间+nonce；确认退出后才核实接管，迟到回执隔离 |
| P02 / P1 | V+S | 多文件初始化/业务状态/订单账本/子状态没有跨文件事务；Python、任务 signup、面板 JSON 已有 fsync，device.save 仍直接 WriteFile+Rename；ACL与发布包未验 | 单文件状态转移原子提交；跨文件用稳定引用恢复，Windows ACL/替换失败/断电边界专项验收 |

R01 的现状需准确理解：完整 run 在某些失败路径会写 unknown 阻止立即自动重购；离线复现证明资源函数不能幂等恢复，而非断言每次崩溃都会立即扣第二笔。I01 当前安全地停住，却把“未投递”误写成“已消费”，不能靠删除 consumed 记录粗暴恢复。G02 是持久停止语义测试，不意味着现有面板会主动删除 cancel.request。

此前已关闭的旧恢复绕过、单凭 intent 续跑、等待提前退号等问题仍保留回归，不在本表重新记作未修复。完整结果见历史 PREDEPLOY-REVIEW 文档。

## 3. 最小目标结构：一个业务 owner，三个清晰边界

```text
18741 UI/API ── submit / input / resume / cancel / inspect ──> pipeline
                                                             │
                                      decide → persist intent → execute → apply
                                         │                   │
                           resources / input inbox        registration adapter
                           邮箱、号码、成本、退款         browser | protocol
                                                             │
                                      注册完成证据 ────────────┘
                                             ↓
                                现有面板登录 / 短信 / 激活
                                             ↓
                           可选母号订阅/家庭组或成员入组（独立操作）
```

- **pipeline 唯一写业务 Job。** UI 和维护提交命令，不自己从 status 推导退款或把任务改成 cancelled。资源账本拥有订单事实；signup 拥有自己的协议会话、端点操作记录，父层不能重写它的 intent。
- **职责不同不等于三个业务真相。** 子状态是执行器检查点，父 Job 存版本化摘要与引用；恢复时由适配器验证任务绑定并对账，不能看到一个文件就推进。
- **先保留现有 Python job 全程 OS 文件锁。** 每次 create/resume/cancel/reconcile 串行执行，waiting 返回即释放；同 job 第二个执行器得到 busy。输入/取消可通过独立原子 inbox 投递，不要求获取被长请求占据的执行锁。暂不引入短锁租约体系；若以后改锁外执行，必须同时实现 fencing 与子进程管理，不能只加 expires_at。
- Outlook 服务不改造、不安装 Apple 模块。其公开接口仍是取邮箱/取码；本系统的账号占位与供应商账本在本地独立维护。

建议最多拆出 `job_store.py`（载入/原子提交/迁移）、`job_policy.py`（纯决策）和适配器文件；保留现有 supply/面板模块。不要按每个状态再造一套框架。

## 4. 权威状态：拆掉歧义，兼容旧展示

新 Job 使用明确 schemaVersion（例如 3），既有 schema=2 经转换器读取。核心结构如下，字段名可采用仓库惯例，但语义不可合并：

```text
identity: job_id, account_id, registration_method, manifest_hash, created_at
progress: business_phase, registration_phase, milestones
run: state, active_run_id, stop_requested, cooldown_until, failure
registration: outcome(unknown|not_applied|registered), evidence_ref, executor_revision
challenge: id, kind, executor_ref, sent_at, expires_at, status
operations[id]: kind, intent_digest, state, account_id, external_ref, result_ref
resources: mailbox_ref, acquisition_id, activation_ref, otp_received
cleanup: browser_state, refund_state, mailbox_state, reason
revision: 每次原子提交递增
```

`business_phase=registration/activation/subscription/family/done`；`registration_phase=captcha/email/phone/finalize/verify`。email-sent 不能再充当业务阶段。`milestones.registered` 表示曾确认注册；登录过期只改变健康度，不删注册事实，不重新建号。

`run.state` 使用 queued/running/waiting_input/retry_wait/reconciling/needs_review/succeeded/cancelled/failed。failure 单独保存 class/code/recovery/at/http_status，展示 reason 由固定错误码映射；轮询和 heartbeat 不覆盖 failure。退款 pending 与注册成功允许同时显示，不混成一个 status。

操作记录统一为对象，支持 intent/executing/succeeded/not_applied/inconclusive。旧字符串只在迁移器中转换；无法证明来源的 succeeded 标记不能生成账号完成证据。每次原子写同时包含阶段、挑战、操作、结果摘要，不能分两次 update 暴露半个状态。

`operations.create=succeeded` 不能因“进入任意 waiting”就代表账号创建成功。父层可以记录“本次执行器调用确认进入等待”；账号完成只有 finalize/readback 的绑定证据才能认定。父调用 ID 和子端点 ID 分清层级，不让父层替子层完成操作。

## 5. 唯一许可表：先决定，再改状态，再发请求

实现无副作用的 `decide(job, command, now, executor_facts) -> Decision`；面板、CLI、维护、refund 均通过它。Decision 明确动作、是否需执行器/资源、保留原因、允许的状态变更。默认拒绝不在表里的组合，不按“非 unknown”推导可以执行。

| 优先顺序 / 条件 | 决策 | 允许外部动作 |
| --- | --- | --- |
| schema/身份/方式/路径/回执版本不一致 | block/review | 无；不占位、不购买、不创建 |
| inspect/status 请求 | 只读视图 | 无，不能借查询做预热 |
| 有显式 stop 命令 | stop/reconcile | 停止原执行进程、必要只读核实；不是退款许可 |
| 已 stopped/cancelled/failed，且无明确复核决议 | hold | 无，不因命令文件消失而解除 |
| 旧子进程未确认退出、执行锁忙 | busy/review | 无新执行；不能仅按超时接管 |
| 未决 operation 或 unknown/needs_review | reconcile 或 hold | 仅明确允许的只读核实；没有 verifier 就人工 |
| cooldown_until 在未来，或限流缺有效截止时间 | hold/retry_wait | 不 create/resume/purchase/refund；保留截止时间 |
| 当前号码/挑战已到期 | needs_review/number_expired 或 challenge_expired | 停止发码、不自动买号；资源结算另判 |
| 注册有绑定完成证据 | 激活交接 | 不再调用任何注册适配器；激活未决仍须自身核实 |
| 新任务，所有前置资源能力满足、无提交记录 | acquire 后 create | 购买受显式开关和预算限制；意图先持久化 |
| 等待证据、当前挑战、输入可接受、无阻塞 | resume 原任务 | 仅原操作/会话；没有码时只轮询输入服务或返回等待 |
| 冷却结束 | 重新运行本表 | 不是自动重放许可 |
| refund/sweep | 单独计算结算许可 | 仅有完整退款证据的订单允许取消 |

只读 reconcile 需要返回 confirmed_succeeded / confirmed_not_applied / inconclusive，绑定原 operation 与资源。`邮箱被占用`、余额变化、执行器已退出都不是建号成功或未执行证据。没有可用核实接口时停在 needs_review 是正确结果，不通过“试着再注册/登录一次”判断。

## 6. 输入闭环：挑战、投递、消费是三件事

1. 发码前生成 challenge_id，记录 sent_at、邮箱/订单引用与类型；执行器确认后保存它的 verificationId 等私密引用。重新发码即新挑战，即使 phase 不变。
2. 输入接口接收 `job_id + challenge_id + kind + input_id + code`，服务端依据冻结路径写私密 inbox；忽略客户端提供的文件路径。校验 ASCII 六位码/图形码各自规则、任务、挑战有效期；不能只看 mtime。
3. Outlook 调用用发码前的 sent_at 作为 since，保存 message_id/received_at，按邮箱核对；重复轮询带 exclude_message_ids。服务端 429/退避原样映射受控失败，禁止读旧邮件补位。短信绑定 provider/activation/challenge，历史短信不能当第二次登录的新短信。
4. 输入状态为 available → reserved(run_id) → dispatch_intent(operation_id) → accepted/rejected/inconclusive。available/reserved 不等于已被远端消费。
5. 崩溃在本地 reserved 且可证明未到 dispatch 时，可回收 reservation；已有 dispatch_intent 的不确定投递必须核实，禁止直接重发。执行器持久化确认后才 accepted。仅把 consumed 写到调用之后同样存在“远端已执行、本地未记”的漏洞。
6. 空轮询不调用可能发码的 resume，不增加提交/发码预算；有输入或确认只读步骤才调用对应动作。重启持续使用原 task/session/challenge。
7. OTP 不出现在命令行、公开回执、日志、操作请求摘要明文；消费标识使用输入随机 ID/邮件 ID，而不是把六位 OTP 的无盐摘要当作安全隐匿。

浏览器旧任务保留其活页面等待机制；协议新任务通过 inbox 唤醒。统一 API 不要求两条执行器的进程生命周期相同。

## 7. 资源闭环：统一购号、收码与结算

### 7.1 获取与恢复

- 生成稳定 acquisition_id 并写 Job 后，才执行采购；账本以这个 ID 查找或创建一条意图。成功订单写到账本后即使 Job 绑定写失败，下次也返回原订单，不新增采购意图。
- 所有订单（自动买或用户提供）使用相同资源结构和结算规则；existing 不购买，但需确认订单归属、时限和状态。
- 未决购买、非法回执不切供应商、不新建订单。仅明确 NO_NUMBERS 可走备用；新号码必须是明确的 replacement 决策、新 acquisition_id，不能由普通 resume 触发。
- 按 AccountID 跨 Job 统计次数和支出，不回收退款预算来无限购买。原“最多两号”上限保留；价格和总预算不填默认值，不授权真实付费。
- 保存最终 provider/订单/cost/currency/expiry/can_get_another_sms；注册后需要再次接码的能力应在花费前或拿号后、注册前统一校验。
- 账号占位用规范化 AccountID 持久化并跨 job 校验；manifest 与订单引用写坏时受控核实，不把缺文件当新业务。邮箱占用只承诺 Apple 系统内互斥；跨其他消费者的独占需要另行 API 协作，本次不假装已有。

### 7.2 停止不等于退款

将 `execution_stopped`、`registration_outcome`、`sms_receipt_state`、`refund_state` 分开。adapter.cancel 回 cancelled 仅确认停止，不能一并设置 refund_deferred=false。明确未执行/未建成和短信未收码等证据满足后才允许申请退款；已建成账号的号码仍可能用于后续认证，不能因工作进程结束自动结算。

统一 `settlement_decision(job, order, provider_observation)`，供 finally、维护、手动 refund 调用：

| 事实 | 处理 |
| --- | --- |
| 注册/购买结果未决或仍需原号码 | hold，保留核实理由 |
| 供应商确认收到短信 | OTP_RECEIVED，计成本，不请求退款 |
| 具备释放业务依据且供应商确认未收短信 | 持久 refund intent，再 setStatus=8 |
| EARLY_CANCEL_DENIED | 持久 refund_pending + next_attempt_at；到期 sweep，同一退款操作 |
| 请求超时/不明返回 | 先查询原订单状态，不以重试次数猜成功 |
| 达到重试预算或无可核实结果 | needs_review，显示待处理成本；不静默丢单 |

不要以 `force=True` 绕过证据；人工决议也应记录具体依据、操作人/时间和目标订单。供应商从查询到取消之间可能发生状态变化，最终以供应商确认收口，不能承诺客户端原子保证。provider cancel 状态与资金到账证据若不同，分别记 cancelled 和 refund_confirmed，成本不可凭猜测减少。

## 8. 真适配器与激活交接契约

真实适配器只做参数映射、子进程管理、回执验证和本地状态核实，不选邮箱、不购买、不退款、不另开登录链。

输入为不可变 ExecutorContext：job/account/method/run_id、允许的 action、原状态路径、challenge、输入引用、超时、冻结出口/画像引用。未知参数拒绝；秘密用受限私密文件或既有安全输入通道，不放 argv。

输出为版本化 AdapterOutcome：task/account/run/action 绑定、executor_revision、outcome、registration_phase、challenge、operation 摘要、failure/cooldown、evidence_ref。旧 CLI 未输出的绑定字段由适配器结合受锁私密 state 验证，不能编造服务端成功证据。

| CLI 结果 | 父流程处理 |
| --- | --- |
| exit 4 + 合法等待回执/原状态一致 | waiting_input，保留号码和会话，等待对应输入 |
| exit 0 + 与账号绑定的完成/回读证据 | milestone.registered，激活交接待办 |
| exit 与回执矛盾、损坏/缺回执、超时、被杀、未知版本 | reconcile/needs_review，不换方法、不换账号、不重建 |
| 明确限流 | 保存原因和截止时间；取父/子已知限制中不提前的截止时间，禁止固定每次续跑加时 |
| 明确 not_applied 失败 | 根据失败及资源证据判断可否重试，不由非零退出码统统推出退款 |

终止时先停止实际子进程树并确认退出；重启记录 PID 还要校验启动时间/可执行路径/run nonce，防 PID 复用。迟到回执只能补证据，不能无条件覆盖新的 owner/revision。实验 TLS 选项仅 fixture loopback 可用；生产面板不开放任意 CLI 参数透传。

激活继续使用现有面板实现。增加查询操作结果入口（或复用已保存结果的只读查询），使“远端已做、本地未收到”能补齐。operation_id 按 `account + kind + auth_cycle/challenge/terms_version/device/family_target` 定义；同 ID 不同摘要拒绝，重复同 ID 返回旧结果。发码预算在首次实际 start/send intent 计一次，attach/query/重复读取不扣预算。

本次不重写 iCloud+、家庭组实现；它们是激活后的独立操作，保留开关、订阅同意和预算约束。母号与成员绑定冻结，未确认订阅/建组不重复购买或创建；后段失败绝不回退注册。隐藏邮箱规模仍不在已确定范围内。

## 9. 面板与调度：一次接完整

- 创建 API/UI 显式选择 browser/protocol；旧任务缺字段按旧 browser 语义，方式写入后不可改。协议入口只检查它自身与共享后半段必需依赖，不因缺 Chrome/Roxy 阻止纯协议。
- GET 统一公开视图：businessPhase、registrationPhase、state、reasonCode/message、method、challenge(kind/id/expiresAt)、cooldownUntil、allowedActions、资源/费用摘要；不得返回私密路径和 OTP。活动进程与等待输入分开显示。
- POST input：校验任务挑战并投递；重复同 input_id 幂等，不回显码。POST resume：通过同一准入器；不得调用 run 再假装已恢复。未决返回受控 409，已排队/执行返回相同运行引用。
- POST cancel：持久 stop_requested，唤醒停止协调；重复提交幂等。等待无进程也能收口；维护不得自行把 cancelled 当退款凭证。
- 自动输入轮询/到期维护共用 job owner；调度仅处理明确 due_at、无运行锁的任务，不能恢复所有非 ready 状态。取消优先于业务动作，冷却期间不发注册/激活请求。
- 日志白名单识别协议子阶段与固定原因；只记录脱敏转移，不能把“过滤掉未知阶段”当安全且可诊断的完整实现。

## 10. Windows、持久化与发布

不用“chmod 0600/0700”作为 Windows ACL 已通过的证据。验收发布目录、state、临时文件、备份和日志的有效访问权限；确认实际运行用户可访问、其他不应访问的用户无法读取。权限失败应在任何付费/外部写请求前阻止启动。

Python atomic、signup writeState、面板 writeRegisterJSON 当前已有 Flush/Sync 或 fsync，不能继续笼统说“全部未刷盘”；设备保存等仍需逐个确认。临时文件同目录、唯一名、写完 Sync、关闭后替换；替换失败保留旧状态并停止推进，不删旧文件。磁盘满/文件被占用/杀毒软件短暂锁定均需要故障注入；不声称 rename 能覆盖掉电时所有硬件持久性风险。

权威快照与操作状态放同一次提交；events.jsonl 是审计视图，不驱动流程。多文件之间依靠稳定 intent/ref 恢复，不宣称事务原子性。证据截图可清理，任务状态、账号占位、订单账本、会话和输入私密 inbox 不属于可随意清理的 evidence。

发布包采用文件白名单，记录源码提交、二进制版本/哈希与运行配置版本；不包含 .cache、state、OTP、cookie、账号、供应商密钥、真实日志。回滚可回退程序，但不得让旧程序读取不支持的新 schema 后重新建号。

## 11. 实施顺序：一份方案、一次整体验收

以下是可独立提交的工作包，不要求每包等待用户/GPT点头；完成整批离线证据后统一交付。遇未定义业务才集中列问题，不再把“可以开始下一小步吗”作为推进条件。

| 包 | 改动范围 | 完成条件 |
| --- | --- | --- |
| W1 状态与许可 | job loader/store/policy、schema 转换、公开视图 | G01–G04；纯函数许可表+真实 run/execute 不可绕过 |
| W2 资源与输入 | supply、account reservation、challenge/inbox、取消结算 | I01–I03/R01–R04；崩溃恢复不重复购买、不吞码、不提前退号 |
| W3 协议适配器 | context/outcome schema、signup subprocess、local reconcile、process tree | A01/A02/P01；缺依赖零资源，跨进程绑定一致，退出与回执核对 |
| W4 面板衔接 | register API/JS、唤醒/维护、handoff 查询与幂等 | U01–U04/H01/H02；浏览器旧路径回归，协议从 UI 可输入/恢复/取消 |
| W5 迁移与发布 | 影子视图、能力开关、ACL、包清单、故障演练 | P02；新任务试用、旧任务排空，回滚不会重放 |

旧任务运行中不改 owner、不迁移会话目录。先只读生成新视图；新建 protocol 任务走新版本，旧 browser 继续原执行器；所有路径共享退款许可保护。新版本可停用创建入口，但保留既有任务的 inspect/reconcile/cancel 能力。需要切换 schema 的任务只在无活进程、无未决写的边界迁移；否则保留旧版本或转人工，不能猜。

## 12. 一次性验收矩阵

测试要验证允许发生什么和禁止发生什么。对供应商/Apple/执行器网络使用离线替身，但不得替换 begin、decide、update、run、锁、维护与 API 路由。没有外部成功基线时，不把 mock 全绿当作线上注册成功。

| 组 | 必测输入/故障 | 关键断言 |
| --- | --- | --- |
| T01 准入表 | 新任务、绑定等待、unknown/review、未决 intent、冷却未到/到期、cancelled、号码到期、已注册 | 允许动作精确；拒绝时 create/resume/buy/refund/handoff 均 0；原原因/截止不变 |
| T02 恢复校验 | 两方式字段冲突、未知 schema/枚举、账号/任务/路径错绑、manifest 损坏 | 不当新任务，不覆盖、不花费；受控错误 |
| T03 真实入口一致 | API、CLI run/resume、维护、refund、cancel 全入口 | 同一事实得到相同 Decision，绕过入口不可执行 |
| T04 三进程注册 | create→邮箱等待退出→邮箱→短信等待退出→短信→finalize | 任务/会话/画像/订单不变；相应创建操作仅一次；completion 绑定 |
| T05 输入 | 空、格式错、旧 challenge、同阶段重发、重复 input_id、两类文件同时存在、并发写、输入过期 | 仅当前输入可投递；旧码不送；没有码不消耗发码/提交预算 |
| T06 输入崩溃 | reserve 前后、dispatch intent 前后、远端执行后 ACK 前、本地 commit 前 | 未投递可恢复，未知投递只核实；无盲目重送/永久误消费 |
| T07 购号崩溃 | intent 前后、供应商回复前后、账本成功后 manifest 绑定前 | 同 acquisition_id 最多一个确认订单；未知不切平台；原订单可恢复 |
| T08 成本与退款 | existing/auto × 未收码/已收码/未知 × 注册未执行/成功/未决 × 冷却退号/超时 | 所有结算入口遵守相同证据；OTP_RECEIVED 不退；pending 可重启扫尾 |
| T09 取消 | 新任务、captcha/email/phone/finalize/activation、无进程等待、unknown、连续 cancel、取消与输入并发 | 停止事实持久；不把停止当未建号；未知保留资源；没有第二次 create |
| T10 并发与进程 | 两个 CLI/维护同 job、不同 job 同账号/同订单、父崩溃 child 存活、迟到回执、PID复用 | 执行唯一；不能接管活子进程；旧回执不覆盖新状态 |
| T11 面板贯通 | 浏览器/协议选择→创建→显示→输入→恢复→取消；自动邮箱/短信轮询 | 私密路径一致；405/错误409消失；UI选择确实影响 manifest；等待可操作 |
| T12 激活交接 | 成功、响应丢失、服务重启、重复操作、不同 auth_cycle、错误摘要 | 不重建账号；已做动作只读回；预算不重复扣；新合法周期有新键 |
| T13 回执契约 | exit0/4/其它 × 正常/缺失/损坏/过期/错账号回执 | 凭绑定事实推进，不凭退出码或文件存在推进 |
| T14 持久化权限 | 磁盘满、写/替换失败、文件被占用、事件日志失败、ACL拒绝、删除截图目录 | 不丢权威事实；无写前副作用；证据删除不影响恢复 |
| T15 迁移与回滚 | 旧 browser 运行中升级、新 protocol 中断、停新建、版本回退 | 活任务无中途换 owner；未知版本拒绝；旧任务可查可收尾 |
| T16 发布安全 | 包文件白名单、秘密标记扫描、生产 insecure/外部 base 拒绝、环境清理 | 包无运行秘密，真实服务无法进入 fixture 不安全模式 |

每个副作用动作至少打断四个点：意图持久化前、持久化后调用前、远端确认后本地确认前、本地确认后父任务推进前。各点记录期望操作次数与恢复判决；不能以“进程返回非零”代替断言。

保留现有 85 项 Python 与 Go 五包测试；新增矩阵测试必须通过真实入口。Windows 文件锁/子进程/ACL用原生环境验，不用 Linux mock 宣称通过。最终交付列出逐项 PASS/FAIL/未测、对应命令和提交，不写“全部完成”掩盖真实服务未测。

## 13. 本轮复现与最终放行条件

上一轮独立快照验证：Python 85 项、Go 五包、定向 vet、历史 16 项均通过。冷却与 structured operation 两项失败。本轮另加四项 Python 崩溃/资源/终态测试、两项 Go 面板接口测试，均复现预期缺口。新增测试为契约审查用例，**当前基线失败是发现证据，不是已修复回归测试**。

复现程序随本文放在 `docs/review-fixtures/registration-2026-11-15/`；README 说明隔离运行方法。临时现场位于 `C:/Users/HUAWEI/AppData/Local/Temp/apple-review-4bbe-f8d7fc802f87402692d486bd4575fa98/`，其 source 为独立 4bbec7d 快照，所有数据为合成夹具。

离线整体验收通过后可以交付待部署包；上线前仍需 Windows 权限/版本/恢复验证。真实注册、购号、订阅与付费识别必须另有明确资源及费用授权；本方案不包含授权，不保证 Apple 接受注册，也不评价指纹绕过效果。

**下一次复核提交要求：W1–W5 的实现与 T01–T16 的证据一起交，不再只贴“又加了几项测试全绿”。** 可以明确列出暂未实现的非必需项，但注册等待、恢复、取消、退款、交接和秘密保护不能列为部署后的补做项。
