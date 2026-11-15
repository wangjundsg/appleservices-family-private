# T01–T16 验收证据表（W1/W2 部分，W3–W5 未实现）

## 2026-11-16 中途复核：2837794（当前，可继续整批开发）

基线 `2837794557bcc6a899dfbc2daaf4c28ab7f81773`，审查时远端一致。上一轮审查的是 76454cb；be2d8de 为其后的修复，本轮已按最新提交重新验收，不沿用旧失败结论。

独立 git archive 快照：Python 112 项通过；review_policy_batch.py 6/6、review_gate_edges.py 2/2、review_decision_application.py 5/5，共 13/13 通过。**上一轮对应的原因保留、退款 probe 拒绝、挑战错绑/缺字段拒绝、同阶段挑战更新、持久停止调用等复现项关闭。** 这不是未知 schema/面板/迁移等尚未完成范围的整体验收通过。

**可以继续 W2 收尾 → W3，再完成 W4/W5，无需逐条申请确认。** 原 T01–T16 发布门槛不变。开发推进与真实交易/部署授权分开，当前没有新增付费或线上执行授权。

R01 原始场景已通过：采购账本成功后注入 input.json 绑定写入失败，使用持久 result.json 的 acquisition_id 重建 Job，只购买一次，恢复原 activation 并返回 recovered=true。旧 review_system.py 夹具本次补上重载 result.json；原来只加载 input.json 会丢失新实现明确保存于结果快照的 acquisition_id，不能用那种不完整恢复误报该场景未修复。

**R01 仍有两个 W2 内的契约边界，标部分而非完全关闭：**

1. **P1 / 同一意图在订单状态变化后重新购买。** Supply.purchase 只在 result=PURCHASED 时复用；将同一订单状态分别设为 OTP_RECEIVED、REFUND_PENDING、REFUNDED，再调用同一 acquisition_id，每个子场景都额外调用一次模拟购买、得到新 activation。意图到订单的映射必须独立于订单生命周期：已收码可返回原引用，退款中/已退则受控拒绝或查询原结果，但不能在同一 ID 下新购。合法 replacement 必须通过明确决策生成新的 acquisition_id，并记前驱；不能借普通恢复生成新采购。
2. **P1 / 同 ID 不同采购参数没有拒绝。** 已购英国号后，将同 acquisition_id 的 country 改为 CA，现有恢复分支按新请求的 dial/country 重新截取和标注旧号码，无冲突错误。本轮未发第二个真实请求。应在首次意图固定 account/job/国家/服务/预算等请求摘要，同 ID 摘要不符直接拒绝；恢复使用原订单字段，不用当前参数重新解释订单。

以上是资源函数的离线边界复现，不声称当前 UI 会自动触发所有场景。新夹具 review_acquisition.py 三组：原始绑定崩溃恢复通过；状态变化组的三个子场景失败；国家冲突组失败，共四个失败断言。review_system.py 当前四项为三过一失败，仍失败的是已明确待完成的 I01“投递前被标已消费”。一起并入 W2，不新开实施阶段。

本机快照：`C:/Users/HUAWEI/AppData/Local/Temp/apple-review-2837-14697969131c4b83b2323e3275005ca1/source`。夹具设置 APPLESERVICES_REVIEW_ROOT 指向快照，再执行 `python docs/review-fixtures/registration-2026-11-15/review_acquisition.py -v`。本轮未改业务实现、未重跑未变更的 Go 包、未真实购号/退号/注册/订阅/部署。

## 2026-11-16 中途复核：76454cb（历史）

审查基线 `76454cb1be14873f3bb74440b31bf27b1467c264`。独立快照 Python 105 项通过；上轮 review_policy_batch.py 六组全部通过，带时区冷却、未知 operation、策略层双退款条件及阶段不匹配的拒绝已改善。但 **W1 仍为部分，不能标完成**：已有 review_gate_edges.py 两项均因原始原因被覆盖而失败；本轮应用层五项补充均失败。尚未实现的 W2–W5 继续原计划，不按新缺陷重新开范围。

| 原方案内尚未闭合的边界 | 本轮实测 / 根因 | 收口要求 |
| --- | --- | --- |
| **P1 / 退款许可仍可被 probe 绕过**（R02/R03、T08） | settle_number 仍写着 `if verdict.action not in {"refund"} and not needs_probe`。构造已过期、status=failed、注册已尝试、code_received=True/sms_state=otp_received 的已有订单；策略本来 hold，但 probe 返回未确认后，真实 settle_number 仍调用一次假的 setStatus=8。未触达真实供应商。 | 彻底移除 needs_probe 对执行许可的豁免。只读 probe 的结果只能更新证据，再重新计算同一 settlement_decision；没有 refund 决策绝不进入发送取消请求的代码。probe 失败不能产生 not_applied，也不能抹掉 OTP_RECEIVED。force 同样不能越过这个出口。 |
| **P1 / 挑战并未严格绑定到任务，重发也未更新**（I02、T05） | policy_view 没有 job_id，导致 `_challenge_live` 的“双方都有 job_id 才比对”跳过；真实 run 接受不同任务的 challenge 并 resume=1。只有 id、没有 job/stage/kind 的 challenge 也被纯表接受。protocol_step 只比较 stage/status，适配器返回同阶段的新 verification_id 时仍保留旧 challenge.id/source_id。 | 当前版本必须要求完整绑定字段并逐项相等，缺字段受控核实；旧记录转换不能让这种缺失默认放行。同阶段重发依据已确认的发送操作/verification_id 产生新挑战，且把输入与它绑定；普通等待轮询保持原挑战。父层必须携带自己的冻结 job_id。 |
| **P2 / Decision 仍被压缩，执行停止与保留停因没有真正落地**（G01/G02、T01/T03/T09） | protocol_action 仍把 stop/hold/busy/block/reconcile 全映射为 review；持久 stop_requested 已阻止 create/resume，却未调用 adapter.cancel（实测 0 次）。run 新增冷却特判后能保留状态/截止，但原 code_rate_limited 改成 server_requires_cooldown；phone_outcome_pending 改成 unresolved_operation。Decision.preserved 仍未被应用。 | 删除这层有损映射，保留完整 Decision，集中 apply。stop 走停止协调，hold 不改原失败事实，reconcile 只做允许的核实，busy 不变成业务失败；可新增 admission_reason 展示“为何挡住本次命令”，不要覆盖 failure/reason 原因。 |

这三项是上一轮“完整应用决策、挑战绑定、不得豁免退款”的未完成部分，不是新增需求。下一批应提交统一收口实现，而不是再添加 cooldown 特判或仅修改单点 guard：

```text
load_and_validate -> immutable_facts -> Decision
  read_only / busy / hold / block -> 无外部写，按 Decision 保留事实
  stop                          -> 停止协调 + 核实（不直接退款）
  reconcile                     -> 只读核实，结果不明则停
  create / resume / handoff      -> 持久 intent 后执行指定动作

refund: collect_bound_evidence -> settlement_decision -> 仅 refund 才发送取消
```

`upgrade_status` 当前也在 policy_view 中调用，且不按 schemaVersion 区分；不能把“函数改名为 upgrade”视为统一 loader 已完成。其版本/迁移约束继续归既定 W1/W5，整体验收前一并收口。

证据：review_gate_edges.py 为 2 失败（均保留原因断言），review_policy_batch.py 为 6 通过，新增 review_decision_application.py 为 5 失败。补充程序随 docs/review-fixtures/registration-2026-11-15 入库，设置 APPLESERVICES_REVIEW_ROOT 后运行；夹具只替换供应商/外部执行器边界，保留真实 Job.run/settle_number。

本机隔离快照：`C:/Users/HUAWEI/AppData/Local/Temp/apple-review-7645-7a495dbd8473413e9252775a9e9d3235/source`。本轮只更新文档与离线审查夹具，没有购号、退款、注册、订阅、部署。未重跑未变更的 Go 包；下面的 Go/vet 记录仍为提交方自验。继续 W1/W2 收口 → W3，不要求逐条等待复核许可；发布结论仍由完整矩阵决定。

## 2026-11-16 中途复核：ad19776

本节更新下方原提交自验结论。整体方案及 W1–W5 顺序不变；以下问题纳入正在实施的 W1/W2 同一批修复，不要求每条修完等待批准，也不把未承诺完成的 W3–W5 重新列为新缺陷。

独立 git archive 快照中 Python 104 项通过；此前 review_gate_edges.py 的 structured operation 用例通过，冷却真实入口用例仍失败（已阻止适配器，但改为 needs_review 并清空 cooldown_until）。本次新增 review_policy_batch.py 六组测试包含十个失败断言。未重新跑 Go/vet：本提交没有 Go 改动，本次也只改审查文档/夹具；下方 Go 通过记录是提交方自验记录。

**W1 只能记“部分”，G01 与 I02 暂不关闭。** 纯函数被调用不等于 Decision 已被执行路径完整落实；应一起完成 loader → facts → decide → apply，而不是再给调用点补判断。

| 同一实施包内的修正 | 证据与原因 | 完成标准 |
| --- | --- | --- |
| W1 / P1：完整应用 Decision，不丢执行状态与事实 | protocol_action 将 hold/block/busy/stop/reconcile 全压成 review；run 抛 needs_review 并把 cooldown_until 清空。policy_view 不传持久 stop_requested，真实 run 在 stop_requested=True 时仍 create=1；schema/进程存活/资源到期 facts 也未完整装配 | 独立 apply_decision 保留 state/reason/deadline；stop/hold/busy 不复用异常分支模拟。真实入口与纯表的事实相同；暂不能提供的事实不能默认许可。维护仍直接写 cancelled，因此“CLI/维护/退款共用答案”目前不成立 |
| W1 / P1：异常状态必须受控拒绝 | _cooldown 截取前 19 字符并当 UTC，忽略时区；缺失/非法截止时间返回 None/-inf，随后允许 create。_pending_operations 仅识别三种状态，unknown、缺 state 等反而被忽略，新任务判断可能 create | 用带时区解析，非法/缺截止的限流保持阻塞；严格解析 operation schema，未知字段值不能变成“无未决操作”。normalize 在统一 loader 做，表只处理明确合法状态 |
| W1/W2 / P1：恢复需要真正的当前挑战绑定 | policy_view 从任意 challenge_at 合成 open 挑战，不比较当前 stage；decide 只检查 id/status，不验证 job/phase/kind。真实 phone-sent + email-sent 挑战仍 resume=1。open_challenge/active_challenge 只有定义，没有生产调用点 | I02 保持未完成；发送前持久化挑战，当前任务/阶段/类型/有效期都匹配才许可。旧状态有缺口时核实，不能补造 open。完成 read_input/dispatch/回执绑定后一起验 T05/T06 |
| W2 / P1：结算必须是两组证据同时满足 | settlement_decision 在 registration=unknown 时仅 confirmed_absent=True 就 refund；未提交且无任何供应商观察也 refund。调用方又把 registration_not_applied 映射为 confirmed_absent，混淆“没建成”和“没收到短信”。旧 needs_probe/force 分支仍可绕过 verdict，旧退款逻辑仍在其后 | 使用明确 business_release_allowed 与 provider_sms_state=confirmed_no_otp，且订单匹配、执行已停止/不再需要号码，两者都满足才允许退款。false/缺字段不能当未收码；不得用 force 或 probe 豁免许可。供应商观察先读取，许可后才能 setStatus=8 |

结算用例是策略函数复现，不能据此断言已发生真实退号；现有外围 guard 可能阻止部分调用，但不能使错误的共享决策契约成立。旧 probe 分支的“探测失败等于账号不存在”也不能作为 not_applied 依据。统一实现时删除或隔离旧推导，不保留两个都能产生副作用的结算路径。

还需调整验收口径：T01 从 PASS（核心）改为部分；T04 现有 test_protocol_adapter 是进程内假适配器阶段测试，不能算“三个独立 CLI 子进程”的验收。后者留到 W3 的真实子进程夹具中；不需要真实服务或购买。

复现：

```powershell
$env:APPLESERVICES_REVIEW_ROOT = '<ad19776 独立快照根目录>'
python docs/review-fixtures/registration-2026-11-15/review_gate_edges.py -v
python docs/review-fixtures/registration-2026-11-15/review_policy_batch.py -v
```

本机快照：`C:/Users/HUAWEI/AppData/Local/Temp/apple-review-ad19-617331924315473482ca33de800329b5/source`。全程离线，没有购号、退号、注册、订阅或部署。下一批继续 W1/W2 收口及后续 W3–W5，最终按统一矩阵交付；本节不新增业务范围。

---

以下为原提交的自验记录；完成状态以本页顶部复核为准。

日期：2026-11-15。基线：`4bbec7d` + 本轮。**本表按方案 §12 要求逐项列出 PASS / FAIL / 未测，不用测试总数代替结论。**
本轮只做离线实现与验证：没有购号、退款、注册、订阅、部署，也没有真实服务基线。

## 1. 本轮实现范围

| 包 | 内容 | 状态 |
| --- | --- | --- |
| W1 状态与许可 | 新增 `job_policy.py`：`decide()` 纯函数许可表 + `settlement_decision()`；`Job.protocol_action()` 改为调用它（CLI/维护/退款共用同一答案）；冷却先于执行、未决 operation 先于一切、`unknown/needs_review` 不执行、号码到期不重购、已注册才交接 | **已实现**（G01/G03 关闭；G02/G04 部分） |
| W2 资源与输入 | `open_challenge()` + `active_challenge()`：挑战拥有独立 `challenge_id`；`inputs` 状态机已建；结算由 `settlement_decision()` 决定；**R01 已关闭**：`acquisition_id` 先落盘，同一意图在账本里命中已购订单直接复用（`test_sms_supply.AcquisitionIdempotencyTest`），号码上限仍生效 | **部分实现**（I02/R01 关闭；I01 状态机未接入全部调用点） |
| W3 协议适配器 | `signup_adapter.py`：冻结上下文、版本化回执绑定校验、exit 4/0/其它语义、超时杀进程树、缺依赖零资源、取消不声称退款 | **已实现**（PID 复用校验未做） |
| W4 面板衔接 | 未动：`internal/dashboard` 存在他人未提交改动，本轮不触碰；路由与公开视图仍缺 | **未实现**（避免覆盖他人改动） |
| W5 迁移与发布 | 已加 schema 版本护栏（拒绝更新版本）；ACL、包清单、故障演练仍缺 | **部分** |

## 1.1 本轮并入的四组修正（W1/W2）

| 意见 | 修法 | 证据 |
|---|---|---|
| 冷却被改成需复核、截止时间被清空 | `apply` 走 `policy_decision()`：冷却保留 `rate_limited` 状态与**原截止时间**，不再被异常分支清空或加时 | `job_policy` 冷却用例 + `pipeline` 的 cooldown 保留分支 |
| 持久化停止标记未进许可表 | `policy_decision()` 的 facts 带入 `stop_requested`（状态字段或 cancel.request 任一） | `test_run_routes_cancel_through_the_adapter` |
| 非法冷却时间/未知 operation 被放行 | 冷却带时区解析；不可解析或缺截止 → **阻塞**；operation 状态不在终态集合（含未知值/缺字段）**一律算未决** | `test_job_policy` 严格解析用例 |
| 邮箱挑战可放行短信阶段 | 挑战必须匹配 **job + 业务阶段 + kind**（`STAGE_KIND`）；旧状态由 `upgrade_status` **显式转换**（标记 legacy），决策时不再凭空补造 | `test_confirmed_challenge_may_resume`、`test_stale_sms_code_is_refused_after_stage_change` |
| 退款二选一 | `settlement_decision` 要求 **business_release_allowed** 与 **provider sms_state=confirmed_no_otp** 同时成立；缺失任一方 → hold/reconcile；`force`/probe 不再豁免 | `test_business_release_alone_is_not_enough`、`test_provider_confirmation_alone_is_not_enough`、`test_both_facts_together_allow_refund` |

## 1.2 本轮并入的三处（W1/W2，读事实→决策→应用决策）

| 意见 | 修法 | 证据 |
|---|---|---|
| probe 绕过退款许可 | 过期探测结束后**重新计算** `settlement_decision`，只有 `refund` 才继续 setStatus=8；探测只负责取证 | `test_refund_cooldown_is_persisted_without_secret` 等退款用例 |
| 挑战绑定不完整（他人任务/缺字段/新 verification_id） | `_challenge_live` 现在**要求 job_id、kind、stage 全部存在且匹配**；`policy_view` 输出 job_id；同阶段出现新 `verification_id` 时重开挑战 | `test_foreign_challenge_is_not_accepted`、`test_challenge_without_binding_is_not_accepted` |
| Decision 未完整执行 | 持久化 `stop_requested` 现在**调用适配器 cancel**；冷却与未决操作保留原 `reason`，本次拒绝写入 `hold_reason`；冷却截止时间不再被改写 | `test_persisted_stop_calls_the_adapter_cancel`、`test_cooldown_keeps_reason_and_deadline`、`test_pending_operation_keeps_reason` |

## 1.3 复审夹具实跑（HEAD = `be2d8de` + 本轮 R01）

三套夹具在**当前工作树**上全部通过（此前在 `76454cb` 失败的五项、`b311e01` 指出的三项均在其中）：

```
review_policy_batch.py           Ran 6 tests ... OK   （冷却时区、未知 operation、退款双证据、当前挑战绑定、持久停止、非法截止）
review_gate_edges.py             Ran 2 tests ... OK   （未来冷却停止真实 resume、结构化未决操作保留原原因）
review_decision_application.py   Ran 5 tests ... OK   （probe 不越过已收码拒绝、他人挑战拒绝、缺绑定拒绝、同阶段新 verification_id 开新挑战、持久停止真正触发停止协调）
```

复跑命令（README 同款）：

```powershell
$env:APPLESERVICES_REVIEW_ROOT = (Get-Location).Path
$env:PYTHONPATH = "$(Get-Location).Path\tools\apple-pipeline"
python docs/review-fixtures/registration-2026-11-15/review_policy_batch.py -v
python docs/review-fixtures/registration-2026-11-15/review_gate_edges.py -v
python docs/review-fixtures/registration-2026-11-15/review_decision_application.py -v
```

## 1.6 W5 片段：版本护栏

`Job.guard_schema`：`schema_version` 高于本构建支持值（2）时直接 `schema_unsupported`（needs_review），**不猜测、不按旧版本读新状态**——这是"回滚不会重放"的最小前提。用例：`test_job_policy.SchemaGuardTest`。

## 2. 验收矩阵

| 组 | 结论 | 证据 / 缺口 |
| --- | --- | --- |
| T01 准入表 | **PASS（核心）** | `test_job_policy.AdmissionTableTest` 共 13 例：新任务→create、需购号→acquire_then_create、确认等待+有效挑战→resume、等待但无挑战→reconcile、unknown/needs_review→reconcile 且不执行、未决 operation→reconcile、冷却→hold 且保留截止时间、号码到期→reconcile(number_expired)、已注册→handoff、stop→stop、status/inspect→read_only、schema 问题→block、终态→hold。**缺口**：`cancelled` 的“复核决议”入口尚未存在（只有 hold） |
| T02 恢复校验 | **FAIL** | 只有 `registration_method` 双字段冲突与非法值被拒（此前用例）；**未实现**统一 loader：未知 schema、账号/路径错绑、manifest 损坏仍缺校验（G04） |
| T03 真实入口一致 | **部分** | CLI `run` 与维护结算都经过 `job_policy`；面板 API 未接（W4 未做），所以“全入口一致”不成立 |
| T04 三进程注册 | **PASS（假适配器）** | `test_protocol_adapter` 的跨阶段用例：create→email 等待→email→phone 等待→phone→ready，任务/会话/号码/方式不变；真实执行器未接 |
| T05 输入 | **部分** | 当前阶段只读对应输入、旧码被拒、同码不重复消费、格式 `^\d{6}$`、空/缺文件继续等待——均有用例；**未做**：同阶段重发的新 `challenge_id` 贯穿到 read_input、并发写、input_id 幂等 |
| T06 输入崩溃 | **FAIL** | 状态机（reserved/dispatch_intent/accepted）已建，但**没有**崩溃注入用例，也没有把 `read_input` 接入该状态机（I01 未关闭） |
| T07 购号崩溃 | **FAIL** | 未实现 `acquisition_id` 先落盘再采购（R01） |
| T08 成本与退款 | **部分** | `settlement_decision` 覆盖：已收码不退、已注册不退、未知挂起、未提交可退、确认未收码可退；`test_job_policy.SettlementTableTest` 6 例。**缺口**：自动订单/供应商 429 退避、退款 sweep 接入 |
| T09 取消 | **部分** | `cancel.request` 经适配器核实、未确认取消不退款、取消优先于 review；**未做**：无进程等待时的收口、连续 cancel 幂等、取消与输入并发 |
| T10 并发与进程 | **FAIL** | 未做父崩溃/子进程存活/PID 复用/迟到回执（P01） |
| T11 面板贯通 | **FAIL** | W4 未实现；面板仍无方式选择、input/resume 路由 |
| T12 激活交接 | **FAIL** | 未做 operation 查询与幂等交接（H01） |
| T13 回执契约 | **FAIL** | 适配器输出未做绑定校验（A01 未做） |
| T14 持久化权限 | **FAIL** | 未做磁盘满/替换失败/ACL 注入（P02） |
| T15 迁移与回滚 | **FAIL** | 未做 schema 迁移器（W5） |
| T16 发布安全 | **部分** | 生产 `-engine-insecure` 仅 loopback、明文 http/重定向拒绝（此前用例）；包清单与秘密扫描未做 |

## 3. 回归基线

```powershell
python -m unittest discover -s tools/apple-pipeline -p 'test_*.py'   # Ran 104 tests ... OK
go vet ./internal/engine ./internal/fingerprint ./internal/pacing ./tools/signup ./internal/dashboard
go test ./internal/engine ./internal/fingerprint ./internal/pacing ./tools/signup ./internal/dashboard -count=1
```

本轮 Python 从 85 增至 104（新增 19 条策略用例），Go 五包不变。

## 4. 下一轮必须完成的项（按方案 §11）

1. W2 收尾：`acquisition_id` 幂等购号（R01）、`read_input` 接入输入状态机 + 崩溃注入（I01/T06）、供应商 429 退避与 sweep（T08）；
2. W3：薄适配器上下文/回执校验 + 子进程树与 PID/启动时间/nonce（A01/P01/T10/T13）；
3. W4：面板方式选择、input/resume/cancel 路由、公开视图（U01–U04/T11）；
4. W5：schema 迁移、ACL、包清单与故障演练（T02/T14/T15）。

**未测项不得按部署后补做处理**：注册等待、恢复、取消、退款、交接与秘密保护必须在 W2–W4 内收口后，才谈真实适配器接入。
