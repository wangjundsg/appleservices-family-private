# Apple 独立产线：运行与恢复

本轮按用户最终确认执行：**只完善本仓库，不安装或接入 gpt-free-service**。
Apple 产线独立持有资源、凭据、进度和结果。家庭组将来新建；本轮不执行订阅购买、家庭组或隐藏邮箱操作。

## 审查结论与实际链路

交接文件的可执行件清单和源码归属已过时。网页登录原实现已有短信发送/提交，主要缺口是
统一调度、无 2FA 的条款分支、持久化交接和未知结果处理。本仓库现提供：

```
create → login（网页登录）→ terms（同一次网页登录）
       → gsa（登录及保存 delegate checkpoint）→ device → keepalive
```

网页登录和条款先完成，避免新号的 GSA delegate 交换被条款挡住。GSA 与网页会话是两种不同状态，不能互相改名替代。
设备阶段通过本机 APS 登记，报告 `registration_accepted=true`；**不把登记成功当成可信设备接码验证通过**。

已处理的契约冲突：

| 问题 | 本轮行为 |
|---|---|
| 任意非零退出都退号 | 只对明确失败/参数错误退号；未知、疑似成功和等码保留号码 |
| 等码超时就关闭页面 | 传入原始号码到期时间后，在同一进程、同一页面继续等待；不重发、不重建 |
| 邮箱码也标记“短信已提交” | 仅短信步骤设置该标记；提交后的意外异常保守判为 unknown |
| 表单国家固定 USA | 根据号码国设置，提交前同时读回表单国家与电话国家 |
| 外部网页登录源码 | 所需协议和测试迁到 `internal/appleweb`，本仓库直接构建 |
| 保活丢失轮换 Cookie | 成功时原子保存新 Cookie；失败保留原文件 |
| 发码预算写盘失败仍继续 | 跨进程锁 + 先持久化再启动；每个 provider/activation 最多两次 2FA 登录尝试 |

## 准备与构建

依赖：Go（按根目录 go.mod）、Python 3.10+、Node、`.tools/js/node_modules/playwright-core`、运行中的 Roxy/Forge、可用住宅 SOCKS5、本机 Apple APS 服务。
构建不会启动账号业务：

```powershell
& .\tools\apple-pipeline\build.ps1
```

把 `tools/apple-pipeline/input.example.json` 复制到 `.cache/apple-input.json`，在**本地私密文件**中填写。
不要在聊天或提交中放密码、API key、验证码、代理密码或邮箱 token。

- `job_id`：一个账号一个稳定任务 ID，重复 `run` 只返回已有状态。同一邮箱不能换 ID 自动重建。
- `graph_token_file`：已有 Graph token 文件，格式沿用 `graph-mail.cjs`。
- 手动邮箱取码：删除 `graph_token_file`，改用 `email_code_file` 的绝对路径；将验证码写入该文件。已有 webmail 取码器也可以写同一文件。
- 图形码：本入口默认人工，将当前答案写到 `state/pipeline/<job_id>/captcha.txt`。当前截图在同目录。一次图像只提交一次答案。
- 自动图形码：设置 `captcha_solver: "yes"` 与私密的 `yescaptcha_key`；调用现有 YesCaptcha solver，禁用本地 OCR 兜底。识别费用由服务商扣取。
- `sms.activation/phone/expires_at`：已有接码号码，号码不带国家码；到期时间必须使用提供商实际到期时间。
- 自动购买号码：删除这三个字段，提供 `sms.country`（供应商国家 ID）、`sms.dial_code`、`sms.max_price`；运行时仅发起一次 `getNumber(service=wx)`。该模式沿用灰熊 20 分钟有效期，其他提供商请使用已有激活的实际到期时间。购买结果未知不会再次购买。
- `accept_terms: true`：本次登录明确接受当前 iCloud 条款，版本变化由协议层拒绝。此项不授权购买订阅。
- Roxy 的国家/语言/时区与号码一致；先读浏览器实际出口国和指纹，再操作注册表单。协议阶段沿用同一 SOCKS5。

## 运行

以下 `run` 会访问真实 Apple 和配置的服务；自动购买模式会产生号码费用：

```powershell
python tools/apple-pipeline/pipeline.py run --input .cache/apple-input.json
python tools/apple-pipeline/pipeline.py status --job apple-001
python tools/apple-pipeline/pipeline.py resume --job apple-001
python tools/apple-pipeline/pipeline.py keepalive --job apple-001
```

`state/pipeline/<job_id>/result.json` 是不含凭据的状态回执，`events.jsonl` 是阶段事件。
私密的 `input.json`、`session.json`、`icloud-accepted.json`、`icloud-web-session.json` 分别保存执行输入、GSA 状态、delegate checkpoint、网页会话。
程序不把子进程原始输出写入任务日志，也不把验证码放进命令行。

| 状态 | 含义与处置 |
|---|---|
| `queued` / `running` | 等待或执行当前阶段 |
| `waiting_input` | 页面或登录在等码；原进程仍运行时向原输入文件提供验证码 |
| `ready` | 网页会话、GSA 交接、设备登记、首次保活均已确认；不代表可信接码已验证 |
| `failed` / `usage` | 明确拒绝或输入错误；读取 reason，修正输入再人工决定后续 |
| `unknown` / `needs_review` | 写操作结果未确认；禁止重新注册、重复登记；先回读 |
| `rate_limited` | 立即停止发码，人工确认冷却与号码状态 |
| `needs_login` | 保活失败，旧会话保留；不会后台自动反复登录 |

注册结果未知时，`resume` 转入登录核实，不再次创建账号。明确失败时，手动 `resume` 可以重试，注册阶段最多两次尝试。
已经退号或挂账退号的激活不会复用；重试前需在私密 `input.json` 更新为新的已确认激活。
号码购买结果未知且没有激活 ID 时必须先向供应商核实，不能再次购买。号码已到期时只用 `probe` 核实密码认证，不显式发验证码；
能认证则报告账号已存在并等待后续接码条件，认证不通过也不据此认定账号不存在。
网页登录或设备写操作中断后，恢复会要求人工回读；本版本不会凭退出码盲目重试。
租号、开窗和写操作在执行前先落状态；单账号进程锁避免两个进程同时操作。
锁与发码预算覆盖本入口；不要同时对同一账号运行旧的独立脚本，否则它们不会共享本入口的调度锁。

## 保活与收尾

```powershell
# 显式前台运行：每 10 分钟检查 ready 账号；到期扫退款挂账。Ctrl-C 停止。
python tools/apple-pipeline/pipeline.py maintain
# 单独处理某个账号已到期的退款挂账
python tools/apple-pipeline/pipeline.py refund --job apple-001
# 重试本任务尚未完成的浏览器收尾
python tools/apple-pipeline/pipeline.py cleanup --job apple-001
```

保活失败只报告需重新登录，不循环发送 2FA。退号冷却后再尝试，最多五次；不把“退号请求已发出”当成退款到账。
开窗、关闭和删除都核对业务码；删除遇 EPERM 最多五次退避，未删掉时回执标记 `cleanup_pending`。
异常时不会删除其他任务的目录。号码购买使用价格上限，实际扣款仍以提供商账单为准；本入口默认人工图形码，没有识别服务费用。

## 验证范围

本地测试覆盖：阶段串联、重复执行、跨进程锁、短信预算、晚到验证码、未知结果不重放、退款挂账、会话账户绑定、条款分支与 Cookie 轮换。
这些测试不注册真实账号、不请求真实短信、不购买号码。真实成功率、住宅出口可用性和当前账号的可信设备接码仍需独立验收。

2026-11-14 本机验证：全量 `go test ./...`、相关 Node/Python 测试、三个程序构建通过；
`trusted-device -probe-native-push` 返回 32 字节令牌并验证本地 COM 回调。
修改涉及的 Go 包 `go vet` 通过；全仓 vet 仍有原有 `internal/sap/unicorn` Windows 原生指针告警。
