# 证据说明（Apple 产线）

排查时**先看证据，再下结论**。本文件说明 Apple 建号/登录/取码的证据在哪、字段什么意思。

---

## 1. 证据放在哪

```
.cache/<任务目录>/            单次建号
  run.jsonl                   全事件流（逐条 append，进程被杀也保留）
  fail-<原因>-<时间>.png       失败瞬间整页截图
  fail-<原因>-<时间>.txt       失败瞬间页面文本
  captcha-attempt-N.png/.txt  第 N 次验证码：图 + 我们填的答案 + 页面红字
  01-submit-N.png/.txt        第 N 次提交后的整页
  summary.json                配置、身份、出口、结果
  actual-fingerprint.json     实测 UA / platform / locale / timezone

.cache/mailboxes.json         邮箱池（含密码与 refresh_token，**只读不复制**）
.cache/<任务>/graph.json      某个邮箱的令牌（同上）
state/accounts/<id>/          GSA 会话、iCloud web 会话
bin/*.exe                     登录/条款/保活程序
```

常见任务目录：`cm/G1..G7`（国家矩阵）、`os-ab/M|W`（系统对照）、`apple-local-1`、`apple-run2`。

---

## 2. run.jsonl 的字段含义

一条一行：

| kind | 含义 | 关键字段 |
| --- | --- | --- |
| `step.at=typed` | 打了字 | `field`、`length` |
| `step.at=field-kept` | 该字段值已正确，跳过重填 | `field` |
| `step.at=selected` | 选了下拉 | `field`、`value` |
| `step.at=ocr-answer` | 图形码识别结果 | `accepted`、`length`、`ms`（**不记码值**） |
| `step.at=captcha-answer` | 提交了图形码 | `accepted` |
| `step.at=captcha-image` | 取到的验证码图 | `natural`、`src_kind`、`duplicate`、`sha256_prefix` |
| `step.at=email-code` | 邮箱验证码 | `code_received`、`elapsed_ms`、`message_id` |
| `step.at=phone_step` | 手机步 | `code_received`、`elapsed_ms` |
| `step.at=dial-code` | 拨号国家码核对 | `want`、`got` |
| `http` | 一次网络请求 | `status`、`path`、`error_code` |
| `refund` / `refund-queued` | 退号 | `ok`、`response` |
| `close` | 退出 | `reason` |
| `final` | 终态（**最后一行**） | `state`、`evidence` |

---

## 3. 退出码 → 处置

| 码 | state | 含义 | 处置 |
| --- | --- | --- | --- |
| 0 | success | 有明确终态证据 | 进入登录 |
| 1 | failed | 可证明没发生 | 可重来 |
| 2 | usage | 参数错，未启动 | 修参数 |
| 3 | unknown | 提交可能已生效 | **不重放**，先回读 |
| 4 | waiting_input | 码没到 | 等，不重来 |
| 5 | likely_success | 提交后 iframe 销毁 | 用登录确认 |

---

## 4. 失败断点的判读顺序

```
① 看 final.evidence / fail-*.txt 的页面原文
   "Your account cannot be created at this time"  → 平台硬拒
   "Enter a valid phone number"                    → 号码/国家码
   "Type the characters in the image"              → 卡在图形码
② 看最后几条 http
   /account/validate 400 且前面有两次 captcha 被拒 → 多为验证码质量导致
   /account/validation/appleid 503                → 上游抖动，未必是我们的错
③ 看 captcha-attempt-*.txt
   对比「我们填的答案」和 captcha-attempt-*.png 上的字符 → 判断识别是否准确
④ 看 field-kept 数量
   重试时若大量 field-kept → 只填一次的机制生效（正常）
```

---

## 5. 已知的坑（看到这些现象先对号入座）

| 现象 | 根因 |
| --- | --- |
| Continue 一直灰、日志出现 `click-timeout` | 密码缺特殊字符 |
| 提交后 iframe 消失 | 是成功，不是失败（`likely_success`） |
| 界面国家码变成 US，但号码是英国 | 控件水合重置，需填完读回 |
| 两次图形码被拒后紧接着硬拒 | 验证码质量拖累，换手工/换服务 |
| `423 tooManyCodesSent` | 2FA 取码太频繁，等 15~60 分钟 |
| 退号返回 `EARLY_CANCEL_DENIED` | 120 秒冷却，挂账后 sweep |
| 邮箱收不到苹果的信 | 域名问题：`outlook.com` 被拒；`qq.com`/`hotmail.com` 可用 |

---

## 6. 敏感内容

```
.cache/mailboxes.json、graph.json、state/accounts/** —— 只读，不复制、不提交、不贴文档
run.jsonl 里的验证码本身已脱敏（只记长度/布尔）
```

---

## 7. 复现命令

```powershell
# 建号（示例，参数按需）
node tools/capture/create-account.cjs --outdir .cache/probe `
  --email <地址> --password <含特殊字符> --phone <号码> --phonecc GB `
  --activation <id> --hero-key <key> --auto-refund `
  --captcha-cmd 'python tools/captcha/solver.py --provider yes --skip-local' `
  --graph-token-file .cache/probe/graph.json

# 设备登录（2FA）
node tools/capture/gsa-login.cjs --appleid <地址> --password <口令> --dir .cache/probe `
  --hero-key <key> --activation <id> --anisette https://mac-ani.jkcoxson.com

# 退号回收
node tools/capture/refund-sweep.cjs --hero-key <key> --dir .cache
```
