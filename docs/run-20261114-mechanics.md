# 创建流程四处机制修复 + 英国号验证（2026-11-14 第四轮）

目标：把 2026-11-14 G5 那轮（`.cache/cm/G5b`，vi-VN）暴露的四个流程机制缺陷修掉，然后只买 **1 个英国号（grizzly id=16，$0.01）** 验证流程是否顺畅。失败即停，不买其他付费服务。

## 1 背景：G5 轮实机观察到的四个缺陷

| # | 现象（G5b/run.jsonl 原文） | 机制病因 |
|---|---|---|
| 1 | 第 1 次提交被拒后，第 2 轮把 `first_name/last_name/apple_id/password/confirm_password/phone_number/生日三项` 全部重打一遍 | 重试循环每轮无条件跑完整 `fillForm()`；`retype()` 先 `fill("")` 再打 |
| 2 | 提交点击不稳，可能落到「Gửi mã mới / Mã Mới」（换一张） | `button[type="submit"], button:has-text("Continue")` 取 `.first()`，按 DOM 顺序命中刷新键 |
| 3 | 邮箱步 graph 只轮询 2 次就整个进程消失（run.jsonl 停在 `graph-poll round 2`，无 close/无 final） | 邮箱步没有宽容期：取不到码即结束，关窗无理由 |
| 4 | 关窗没有原因记录 | 无统一出口 |

## 2 改动

新增 `tools/capture/flow-mechanics.cjs`：把四个机制做成可注入（locator / mouse / clock / log）的纯逻辑，`create-account.cjs` 只做接线。

| 机制 | 位置 | 内容 |
|---|---|---|
| A 只填需要填的 | `flow-mechanics.cjs:86` `fillField`、`:101` `selectField` | 先 `inputValue()`，相等即 `field-kept` 跳过；不等先 `fill("")` 再 `typeHuman` |
| A 接线 | `create-account.cjs:626` `fillIfNeeded`、`:631` `selectIfNeeded`、`:1221` `fillForm` | 六个主表单字段 + 三个生日下拉全部走 read-first；`retype()` 删除，`ensureField` 的修复路径（`:1184`）也改走 `fillIfNeeded` |
| B 提交按钮 | `flow-mechanics.cjs:133` `findSubmit`、`:166` `clickSubmit` | 优先 `button[type="submit"]`，其次文案 `continue/继续/繼續/tiếp tục/下一步`；`NOT_SUBMIT = /换一张|換一張|new code|ma moi|refresh|another code/i` 全排除；点击前等 `enabled` 最多 10s → `scrollIntoViewIfNeeded` → `mouse.move` 到中心 → 停 300~700ms → `locator.click()`（不按坐标点） |
| B 接线 | `create-account.cjs:778` `clickContinue` | 走 `mech.clickSubmit`；`submit-not-found` / `submit-disabled` 直接 `err.closeReason = "submit-failed"` 抛出，不再重试同一按钮；A3 的 force-click 仍只在 timeout 回读证明"没提交"之后（`:833` 之后） |
| C 邮箱码宽容期 | `flow-mechanics.cjs:246` `emailCodeWindow` | 90s 取码 → 点一次「重新发送/Resend/Send a new code/Gửi mã mới」（`:217` `RESEND_LABELS`，找不到记 `email-resend-not-found` 跳过）→ 再等 120s → 才退出，`state=waiting_input`，close 里写 `note: "email-code-timeout waited=210s rescanned=yes"` |
| C 接线 | `create-account.cjs:1525`、`:846` `resendEmailCode` | graph/manual/temp-mail 三个取码器改为「接收窗口时长」参数；`graphEmailCode(since, timeoutMs)`、`manualEmailCode(timeoutMs)` |
| D 关窗留原因 | `flow-mechanics.cjs:42` `CLOSE_REASONS`、`:61` `closeRecord`、`:68` `closeReasonFor` | 任何退出都写一条 `{"kind":"close","reason":...}`；词汇 = 需要的六个（success/captcha-exhausted/email-code-timeout/submit-failed/hard-reject/script-error）+ 两个补充（sms-code-timeout/unknown-screen）；未知值降级 script-error |
| D 接线 | `create-account.cjs:182` `logClose`（单次，`closeLogged` 防重复）、`:1595` 正常出口、`:1625` 异常出口 | close 写在 `final` **之前**，run.jsonl 最后一行仍是 state（review B4 不破） |

顺带修掉的（不属于四处，但会直接毁掉一次付费）：
`tools/capture/apple-country-matrix.cjs` 的 `--dial` 默认为空 → `localPhone()` 不剥国家码 → 表单里塞进 12 位 `447759461688`（G6 实证）。现加 `DIAL_CODES` 默认表 + 未知国家在**买号前**拒绝（`dial_unknown`），号码记录里带 `dial`。

## 3 单测

```
node --check tools/capture/flow-mechanics.cjs      OK
node --check tools/capture/create-account.cjs      OK
node --test tools/capture/*.test.cjs               96 pass / 0 fail（原 79 + 新增 17）
node --test tools/capture/flow-mechanics.test.cjs  17 pass / 0 fail
```

`flow-mechanics.test.cjs`：A 三条（已填不重打 / 脏值先清空 / 生日下拉相等跳过）、B 五条（type=submit 优先且 hover+300~700ms / 跳过「Mã Mới」 / 永不 enabled 记 `submit-disabled` 等满 10s / 全无按钮记 `submit-not-found` / 点击超时只报一次）、C 三条（90+120=210s 且只 resend 一次 / 全程无码仍等满 210s / resend 抛错不终结）、D 两条（词汇 + 证据到原因映射）、接线四条（六个字段都走 fillIfNeeded、无 `retype(`、clickSubmit/emailCodeWindow/findAction 已接、两个出口 close 都在 final 之前）；locator/mouse/clock 全是假对象。

## 4 真跑

### 4.1 G6：付费英国号（唯一一次买号）

| 项 | 值 |
|---|---|
| 组/目录 | `G6` / `.cache/cm/G6` |
| 窗口 | Roxy，Windows，core 155.0.8059.40（pinned），en-GB / Europe/London |
| 出口 | GB `217.69.127.81`（`gw.dataimpulse.com:10003`，六项指纹检查全绿） |
| 身份 | Emma Davies 05/11/1952（随机） |
| 号码 | +44 …688（grizzly `service=wx&country=16&maxPrice=0.01`，activation 7169\*\*\*） |
| 邮箱 | mb3 meaji62167@outlook.com（Graph 预检可读） |
| 填表 | 第 1 轮 6 个文本字段 + 3 个下拉 + 手机号全部 typed；`dial_code` 二次断言出现 `field-kept`（A 生效） |
| 提交 | `submit-clicked via=type=submit text="Continue"` —— 点对按钮（B 生效） |
| 验证码 | 1 轮；OCR 4 字符（SF9E）→ `/account/validate` **400**，Apple 换图（`/captcha` 201） |
| 邮箱码 | **未走到**：Apple 在表单页直接硬拒 |
| 结果 | `state=failed`，`fail-shot` 整页文本含 `Your account cannot be created at this time.`（另有 `Type the characters in the image`） |
| close 记录 | `{"kind":"close","reason":"hard-reject","state":"failed","evidence":"hard-reject:1"}`（D 生效） |
| 收尾 | `/browser/close` code 0、`/browser/delete` deleted true；退号 `ACCESS_CANCEL`（第 1 次 `EARLY_CANCEL_DENIED`，2 次成功）→ **净花费 $0**；耗时 89.1 s |

### 4.2 G6b：免费窗口（不买号，专为验证 C 而跑）

付费号已被 Apple 拒，按"只买 1 个号、失败即停"不再买，改用 `.cache/cm/free-window.cjs`（不调 grizzly）开一个同配置窗口，手机号用 Ofcom 保留的虚构段 `07700 900123`，只为把流程推进到邮箱步。

| 项 | 值 |
|---|---|
| 出口 | GB（`gw.dataimpulse.com:10004`） |
| 身份/邮箱 | Olivia Smith 06/11/1974 / mb4 medorabaudoin025952@outlook.com |
| 填表 | 第 1 轮 typed 8 条（6 字段 + 手机号 + 验证码）；第 2 轮 **typed 只有验证码**，13 条 `field-kept`（含 `first_name/last_name/country/生日三项/apple_id/password/confirm_password/dial_code×2/phone_number`）→ **A 的"只填一次"在真实重试里成立** |
| 提交 | 第 1 轮 `via=type=submit text="Continue"` 点对；被拒后第 2 轮 `submit-disabled`（等满 `waited_ms=10000`），未点、未重试 → **B 的失败语义成立** |
| 邮箱码 | 未走到（表单未通过） |
| close | `{"kind":"close","reason":"submit-failed","state":"unknown","evidence":"submit-disabled:create-submit-2"}` |
| 收尾 | close 0 / delete true；无号可退；耗时 115.5 s |

grizzly 余额：G6 前 3.0410 → 退号后 3.0310 → 本轮结束 3.0050（差额来自并行会话，不是本次）。

## 5 结论

1. **A/B/D 三个机制在真实流程里都按设计动作**：A 在 G6b 的第 2 轮里只打了验证码（13 条 kept）；B 两次都点中 `type=submit` 的 Continue，禁用时如实记 `submit-disabled` 且不重试；D 两个出口都写了 close 原因且 final 仍是最后一行。
2. **C 只在单测里验证过，真跑两次都没走到邮箱步**：G6/G6b 都在表单提交后被 Apple 拒（G6 出 `Your account cannot be created at this time.`，G6b 的 Continue 10s 内始终 disabled、widget 随后消失）。GB 出口在 04:14 UTC（G1）还能过表单，04:48 之后连拒两次，更像出口/地址段的信誉问题，不像机制问题。
3. **验证码读数仍是主要随机项**：两轮 OCR 都是 4 字符、都被 400。镜像里那套"整图 10 票 + 单字 16 倍拼版 + ddddocr"这次没能重现 G1 的一次过。

## 6 还没做好的机制

1. **表单被拒后 widget 的去向没有分类**：G6b 的 Continue 变灰后 widget 直接消失，驱动只能报 `submit-disabled`（unknown）。"按钮变灰 → widget 消失"应识别为 Apple 侧的拒单（`hard-reject` 或新状态 `page-lost`），而不是笼统 unknown。
2. **禁用等待只有 10s**：Apple 校验期间会短暂 disable，10s 不够时会把"正在校验"误判成"被拒"。建议先读页面上是否有 inline error/spinner，再决定等多久。
3. **验证码通道没有长度自适应到底**：ACC 结束后 Apple 的图有 4 字符也有 5 字符，`vote.py` 固定按一个长度出答案时命中率不稳；读到 400 后应换通道（换图再读一次、或按 5 字符重读同一张）。
4. **邮箱步的"宽容期"只在没人点它的时候才有效**：resend 只按文案找，找不到只记日志；Apple 若把 resend 做成 `<a>` 包 `<span>` 或放进 shadow DOM，会静默跳过。需要在真机上验证一次文案匹配（两次真跑都没到这一步）。
5. **表单国家仍写死 `United States`**，而号码/出口是 GB（G1、G6、G6b 都是）。本轮与 OS 无关的硬拒最可疑的来源就是这里，未改。
6. **`--no-number` 这种"不花钱验证流程"的能力目前只在 `.cache/cm/free-window.cjs` 这份脚本里**，没进 `apple-country-matrix.cjs`；下次要复验 C，还得再拷一份。

## 7 证据索引

- `.cache/cm/G6/run.jsonl`、`create-account.log`、`session.json`、`session.jsonl`、`01-submit-1.png/.txt`（含硬拒原文）、`captcha-1.png`、`fail-failed_hard-reject-1-*.png/.txt`、`password.txt`
- `.cache/cm/G6b/run.jsonl`、`create-account.log`、`session.json`、`01-submit-1.png/.txt`、`captcha-1.png`、`captcha-2.png`、`fail-unknown_submit-disabled-create-submit-2-*.png/.txt`
- `.cache/cm/free-window.cjs`（零成本验证窗口脚本）
- 单测：`tools/capture/flow-mechanics.test.cjs`
