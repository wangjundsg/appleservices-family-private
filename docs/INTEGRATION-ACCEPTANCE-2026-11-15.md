# T01–T16 验收证据表（W1/W2 部分，W3–W5 未实现）

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
| W2 资源与输入 | `open_challenge()` + `active_challenge()`：挑战拥有独立 `challenge_id`（同阶段重发也是新挑战）；`inputs` 状态机 `reserved → dispatch_intent → accepted/rejected`；结算改由 `settlement_decision()` 决定 | **部分实现**（I02 关闭；I01 状态机已建但未接入全部调用点；R01 未做） |
| W3 协议适配器 | 仍为测试注入的假适配器；薄上下文、子进程树、PID 复用校验 **未做** | **未实现** |
| W4 面板衔接 | 面板 input/resume/cancel API 未改（本轮未动 `internal/dashboard` 的注册路由） | **未实现** |
| W5 迁移与发布 | schema 迁移器、ACL、包清单、故障演练 **未做** | **未实现** |

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
