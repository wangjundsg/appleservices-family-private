# T01–T16 验收证据表（W1 已实现，W2 部分，W3–W5 未实现）

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
