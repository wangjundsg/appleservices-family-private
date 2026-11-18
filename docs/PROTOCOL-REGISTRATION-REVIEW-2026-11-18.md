# 协议注册路径：问题汇总（交给修复方）

## 实现复核

注册校验返回 `-34607001` 时，回执使用中性原因码 `server_rejected_34607001`，另存实收的 HTTP 状态。任务进入 `needs_review`，重启保留原始失败原因。离线测试涵盖 HTTP 400 及 HTTP 200 携带业务错误两种情形。拒绝原因仍待服务方证据。

`signup` 开始新任务时从 `APPLESERVICES_SIGNUP_PASSWORD` 读取指定密码。单步 email/phone 提交改用 `-code-stdin`，标准输入接收一行六位 ASCII 数字。全流程仍支持私有验证码文件。旧版命令行中的 `-password` 和 `-code` 已移除，脚本在资源操作前检查 `codeStdin` 能力。

面板现有邮箱输入接口仅接受正在等待的浏览器任务。协议任务不能把验证码写进浏览器任务使用的文件。协议输入、续跑及自动取码的业务闭环仍待验收。以下为原始复核请求。

日期：2026-11-18。仓库 `D:\appleservices-family-private`，基线含 `7216e5d` / `8bb9a37`。
本文区分三类内容：**已修复**（别重做）、**已知未修**（可开工）、**根因未明**（需要实验设计，不要盲改）。

---

## 0. 一句话现状

纯协议注册（`tools/signup`）的**流程本身**已经可用（三进程 create → email → phone → finalize 全绿），真正卡住的是服务端风险判定 `-34607001`，它**与 ADI 无关**（见 §3 实测）。

---

## 1. 已修复（回归测试在仓库里，改动前请先跑）

| 编号 | 曾有问题 | 现状 |
|---|---|---|
| 1 | ALPN 声称 http/1.1 实际协商 h2 | 已修：`HelloCustom` + 重写 ALPN，握手后断言 `NegotiatedProtocol`；`TestEngineOffersOnlyHTTP11` |
| 2 | 续跑丢画像、回执仍写 utls | 已修：模式取 `state.ProfileMode`，回执读实际传输；`TestResumeKeepsProfileModeFromState` |
| 3 | 设备 cookie 覆盖任务会话 | 已修：设备 cookie 只喂全新任务；`TestDeviceOnlySeedsFreshTask` |
| 4 | 画像切换后 FD 未重校验 | 已修：`U/L` 必须与画像一致；`TestProfileRejectsForeignFD` |
| 5 | 遥测可打业务端点 | 已修：白名单 `POST /jslog`、`/v2/jslogs`；`TestTelemetryRejectsBusinessEndpoints` |
| 6 | 模板版本与 TLS 预设不对应 | 已修：按版本钉死预设；`TestTemplatesNeedAnExactPreset` |
| 7 | 遥测伪造字段 | 已修：只改已存在字段；`TestTelemetryDoesNotInventFields` |
| 8 | 预检失败不落盘 / 暂停仍发请求 | 已修：`admission.go` 统一准入 + `failureClass`；`TestPreflightFailurePersistsAcrossRestart`、`TestPausedTaskSendsNoRequests` |
| 9 | 未决 intent 仍发请求、拒绝覆盖原原因 | 已修：`unresolved_operation` + `blockedReason`；`TestUnresolvedIntentBlocksBeforeAnyRequest`、`TestRefusalKeepsOriginalFailure` |
| 10 | 显式 legacy 降级、设备串号 | 已修：任务方式/设备绑定冻结；`TestExplicitLegacyCannotDowngrade`、`TestDeviceSwitchIsRejected` |
| 11 | 退款二选一、probe 绕过许可 | 已修：`job_policy.settlement_decision` 需业务许可 + 供应商确认未收码 |
| 12 | 挑战绑定不完整 | 已修：`job_id/kind/stage` 全匹配；`TestForeignChallengeIsNotAccepted` |
| 13 | 重复购号 | 已修：`acquisition_id` 幂等，已收码/退款中/已退款不再买；`AcquisitionIdempotencyTest` |
| 14 | 输入“读即消费” | 已修：`peek → reserve → dispatch → accept`，`reserved` 可回收 |

运行：

```powershell
python -m unittest discover -s tools/apple-pipeline -p 'test_*.py'   # 126 项
go test ./tools/signup ./internal/engine ./internal/fingerprint ./internal/pacing -count=1
python docs/review-fixtures/registration-2026-11-15/review_policy_batch.py
python docs/review-fixtures/registration-2026-11-15/review_gate_edges.py
python docs/review-fixtures/registration-2026-11-15/review_decision_application.py
```

---

## 2. 已知未修（有明确改动面，可直接开工）

| 编号 | 问题 | 位置 | 期望 |
|---|---|---|---|
| P-1 | 注册失败原因太粗：`-34607001` 一律落到 `stage_unconfirmed` | `tools/signup/workflow.go`（validate 阶段） | 把服务端 `service_errors[].code` 映射成稳定码（如 `server_rejected_risk_34607001`），并保留 `httpStatus` |
| P-2 | 协议任务自动取码未接 | `tools/apple-pipeline/pipeline.py`（`next_code` / `prepare_followup_sms`） | 与注册适配器共用，`create-sms.txt` 不再依赖 |
| P-3 | 面板 input/resume/cancel 路由缺失 | `internal/dashboard/register.go` | 等待输入时 `POST /api/register/<id>/input` 应 200（现为 409/405） |
| P-4 | 密码/验证码仍可能经 argv | `tools/signup` 既有参数 | 统一走私密文件或环境变量，回执与日志不出现明文 |

---

## 3. 根因未明：`-34607001`（重点，需要实验而不是改代码）

**现象**：`POST /account/validate` 返回

```
HTTP 400  service_errors: [{"code":"-34607001"}]
message: "Your account cannot be created at this time."
```

该码只在 validate 出现；`bootstrap / 邮箱可用性检查 / 密码策略检查` 都能通过。

**已排除的方向（请勿重复）**：

| 假设 | 证据 |
|---|---|
| 缺 ADI（真机码） | 本仓库协议链路确实 0 处 `X-Apple-I-MD`，但 ADI 实测三次来源全部 417（见下），且 ADI 只能在 provisioning 时绑定到**已有账号**，新号无从获取 |
| 号码国家 | hero US×4、grizzly US、hero GB、hero FR、grizzly GB 全为 `-34607001` |
| 邮箱供应商 | outlook.com / hotmail.com / qq.com / 自建域 全为同一码 |
| 出口 IP | 家宽 CN 原生、DataImpulse US×4、OVH GB、台湾 ISP 全为同一码 |

ADI 实测（同一匿名 `GET /account`）：

| ADI 来源 | 结果 |
|---|---|
| 无 ADI | 200（网页 HTML） |
| 公共 anisette 池 | 417 |
| 本机原生 + 合成身份 | 417 |
| 本机原生 + iCloud 守护进程真 token（另一账号） | 417 |

→ **ADI 与 `-34607001` 无因果**；证书/设备头不是这条链路的杠杆。

**尚未验证的假设（建议按顺序做）**：

1. **出口 IP 的“原生度”**：用户唯一成功的一次是「Edge 普通窗口 + TUN 美国 ISP 原生 IP（154.44.14.218，AS979，risk 23%）」。需要一组对照：同一号码/邮箱，只换 IP 类型（住宅原生 vs 机房 vs 代理池），记录 `validate` 结果；
2. **设备历史**：设备档案 `ageDays` 与 `visits` 对 `validate` 的影响（现有 `shao-xing-home`：ageDays=2、visits=19）；
3. **人机节奏**：2.5s / 30s / 真实 Chrome 67s 三档对 `validate` 的影响；
4. **请求一致性**：同一会话内 `bootstrap → validation/appleid → validate` 的 `scnt`/`X-Apple-ID-Session-Id`/`X-Apple-I-FD-Client-Info` 是否全程一致（怀疑点：FD 是固定字符串，可能与 UA/版本漂移）。

**实验纪律**：每次只改一个变量；每次实验前记录 `IP/号码/邮箱/设备/节奏` 五元组；失败不得自动重试（会污染计数）。

---

## 4. 与登录/激活的关系（避免混合排查）

- 登录/2FA/设备激活的问题在 `docs/LOGIN-ACTIVATION-REVIEW-2026-11-18.md`；
- 两条链路**共用**：出口代理、设备身份、面板任务管理；
- 但**失败机理不同**：注册卡在服务端风险判定（`-34607001`），登录卡在 `repair` 与 412；
- 修注册时不要把 ADI 引入 `tools/signup`——实测无效，只会增加复杂度。

---

## 5. 验收标准

| 编号 | 期望 | 验证 |
|---|---|---|
| R1 | `-34607001` 有独立稳定错误码与 `httpStatus=400` | 回执 `reason=server_rejected_risk_34607001` |
| R2 | 协议任务等待输入时零退号、零重购 | `test_waiting_pauses_the_run_loop_for_both_number_modes` 仍绿 |
| R3 | 自动取码闭环（邮件 + 短信） | 新增用例：`create → waiting → 输入 → phone-sent` 全自动 |
| R4 | 面板 input/resume 返回 200 且幂等 | 新 API 用例 |
| R5 | 密码/验证码不出现在 argv、日志、回执 | 回执扫描用例 |

**禁止**：为“过验证”而伪造设备证据、复用他人 ADI、跳过服务端返回的真实错误。
