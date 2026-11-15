# 协议注册：会话续接与最终创建

2026-11-15。本版补齐协议执行器的完整状态转换，通过本地严格回放和多进程测试；尚未完成真实 Apple 注册验收，也没有证明解决了 `-34607001`。面板继续使用已有浏览器注册入口，没有自动切换到实验执行器。

## 修复依据

依据本机保存的公开页面 bundle `widget-4675b275d70356cae8fb.js` 的调用点，而非旧 mock 的推测：

- `Bi` 构造邮件发送体：`account.name/person.name` 和 `countryCode`。
- 邮件发送结果中的 `verificationId` 被保存，`ol` 提交时使用它和邮件码，后续完整表单仍携带此验证信息。
- `Dl/Te` 和 `Il/Fe` 分别以完整表单发送、验证短信；`securityCode` 与 `phoneNumber` 同层。
- `Il` 返回已验证的完整表单后，`we` 调用 **POST `/account`**。页面把其响应中的账号名称作为创建结果。仅 validate 成功或短信提交成功不等于创建完成。

这些调用点证明了原执行器缺少的步骤和字段。真实服务是否接受具体账号、是否出现额外验证，仍需实际验收；本版不会绕过或自动重复服务端拒绝。

## 状态与执行

```text
preflight → captcha → validated → email-sent → email-verified
          → phone-sent → phone-verified → created → done
```

`created` 要求 POST `/account` 成功且响应账号匹配本任务；`done/ready` 还要求随后只读查询返回已占用。单独 `used=true` 不再被视作本任务创建成功。

默认私密状态文件仍为 `.cache/signup/<邮箱转换名>.json`，schema version 为 1。保存业务阶段、写意图、绑定的完整请求、scnt、会话 ID、hashcash 状态、Cookie 和冻结的运行配置。此文件含密码、验证码及代理认证信息，不能当作可删缓存或提交仓库。

后续 CLI 启动恢复同一会话，不重新 Bootstrap。请求参数显式改变账号或已冻结的代理、UA、FD、locale、时区、传输时，网络请求前拒绝。代理配置未变不代表实际出口 IP 一定未变。

- Go cookie journal 保留原始 Set-Cookie 的作用域、删除和到期语义；curl 使用独立 jar，并将内容保存在同一私密状态快照中。
- 同任务用独占 `.lock` 防并发；状态先写同目录临时文件、同步、再替换。Windows 文件占用导致替换失败时停止，不继续发送。
- 有副作用的请求先保存 intent；仅成功回执才能推进。网络中断、响应缺必要字段、写盘失败或未完成 intent 都不自动重放。423/429 保留冷却状态，重启不会重新发码。
- 崩溃留下的锁需要先核对进程和原操作结果，不能自动删锁并重跑；本版没有人工修复状态的 UI。
- 旧 version 0 业务文件缺会话材料，拒绝直接续跑。不要删除已有记录来规避这一检查。

## 命令入口

构建及离线能力检查（不访问 Apple）：

```powershell
go build -o .\bin\signup.exe .\tools\signup
.\bin\signup.exe -capabilities
```

返回 `stateVersion:1/sessionResume:true/finalize:true`。如已有 signup 进程运行，先等该任务结束，不覆盖其二进制。

分步调试顺序为 `preflight/captcha/create/email/phone/finalize/verify`。`email` 或 `phone` 无 `-code` 时发送，有 `-code` 时提交；自动化优先使用 `all` 的私密 code-file 通道，避免验证码出现在进程参数。`create` 是表单验证，之后必须继续完成验证和 finalize。

`-stage all` 从保存的阶段继续：没有当前所需验证码文件时返回退出码 **4 / waiting_input**，保留会话；提供 `-email-code-file` 或 `-phone-code-file` 后重新执行同一任务，不重新发码。文件只接受六位数字，保留前导零，拒绝修改时间早于本次挑战的旧文件。`-code-wait` 可指定在原进程等待文件的时长，默认不阻塞等待。

验证码文件时间检查只是防旧文件误用，不能代替验证码来源与任务绑定。Graph 自动取码需 `--mailbox` 与配置邮箱一致、`--since` 为发送请求前时间，本入口不使用默认 90 秒回溯窗口。

所有公开回执只包含阶段、状态、传输选择、HTTP 状态和受限错误码，不输出密码、OTP、Cookie、scnt 或原始认证响应。失败返回非零；只有最终创建及回读成功才有 `accountExists:true`。

## 自动资源脚本的边界

`tools/signup/attempt.ps1` 已调整为：检查新版二进制能力及 Graph 邮箱绑定 → 原会话预检 → 购号 → validate → 邮件验证 → 短信验证 → finalize → verify。原会话预检之后补入号码，后续阶段仍恢复该会话。

默认禁止购号，单价无默认值；只有显式 `-AllowPurchase` 和 `-MaxPrice` 才能进入购买逻辑。供应商 key 从 `SMS_API_KEY/SMS_API_KEY_HERO` 读取，识别 key 从 `YESCAPTCHA_KEY` 读取。不能将资源清单中的历史报价解释为购买授权。

该脚本仍是实验入口：其退款挂账、未知购买结果核实及成本账本尚未与面板的成熟接码模块统一。本轮没有据此进行真实购买，不应替换生产面板。供应商订单到期后的号码恢复和 Apple 额外验证不由程序推测处理。

## 本轮验收证据

- Go 本地严格服务端验证邮件 ID、完整短信体、最终 POST、账号绑定、原 session/scnt/Cookie 连续性。
- 真正启动独立 CLI 子进程逐阶段运行，分别覆盖 Go 和系统 curl；只 Bootstrap 一次、只 finalize 一次。
- 模拟最终请求已被处理但响应断线：重启停在 needs_review，不重复创建。
- 覆盖旧 schema 拒绝、冻结配置、未知 intent、冷却状态保持、旧验证码文件、Cookie 作用域/到期和同任务锁。
- Graph 离线测试验证邮箱、发件域及时间范围；PowerShell 语法解析通过。Windows PowerShell 脚本执行测试受本机执行策略限制跳过，没有修改策略。
- 对已有三条号码订单只读核查，均返回 STATUS_CANCEL；未购买新号、调用付费识别或提交真实注册。

真实验收尚需一个有效、能继续收短信的订单，或另行明确单次购买预算。`-34607001` 仍是待核实的外部拒绝原因，不能用本地测试结果宣称已经解决。

## 2026-11-15 图形验证码拒绝复核

收到外部测试报告后，只读核对两份私密状态 `megan-v1-test.json`、`megan-v1-b.json`，没有重放请求，也没有购买号码。

- 两份状态中，保存的 captcha ID/token 均与 `body.captcha` 相同。第二份状态的原始图片字节与本地 `captcha-v1.jpg` 完全一致，答案长度为四、没有 BOM，并与测试者报告的人工答案一致。普通报告不收录验证码、令牌或账号。
- 同步调用链中，`answerCaptcha` 仅在没有已保存挑战时取图；等待答案期间没有后台取图。新延迟回归测试验证显示图片、答案回调及提交挑战一致，并验证同任务锁阻止另一个进程取图覆盖。**目前未发现报告所猜测的二次取图竞态。** 这些检查不能替代真实 wire 抓包或证明服务端接受；第一轮没有找到独立图片文件供字节对照。
- `stage=captcha` 是“最近成功阶段”；`operations.validate=intent` 加服务端 `captchaAnswer.Invalid` 表示已调用 POST `/account/validate`，但未成功推进到 `validated`。不是“从未提交 create/validate”。邮件、短信和最终创建仍未在这两轮真实验收。
- 多个 OCR/人工结果一致不是服务端答案正确的证明。现有材料不能确定是字符识别、挑战有效期、会话/请求上下文还是其他校验因素，不能保证改完某一处后必然进入 validated。

另复现并修复了一个独立问题：有 `YESCAPTCHA_KEY` 时，显式 `-captcha-answer-file` 原先仍选择付费识别。现在优先顺序为 **显式答案 → 显式答案文件 → 识别服务**；答案文件去掉 UTF-8 BOM，并在公开回执中以 `captchaSource=manual/answer_file/solver` 标明实际来源。测试通过本地拦截确认显式文件通道不会调用识别服务。该修复不能解释已确认提交了人工答案的第二轮拒绝。

网络报告中的 `198.18.*` 与 fake-IP 配置相符，但不能由一次失败推断 Go SOCKS 在所有此类环境中都失败，或所有代理主机必须改为 DoH。先区分代理网关解析和 SOCKS 目标域名解析，并检查 TUN 接管与路由；mihomo 提供 fake-ip-filter、代理节点 DNS 等配置，见 [官方 DNS 说明](https://wiki.metacubex.one/en/config/dns/)。本轮未改系统 DNS/TUN、未固定供应商 IP，也未增加自动 DoH 回退。

交给测试者：先核验新版能力和 `captchaSource`，保留原失败任务；若另行授权真实复测，记录本次取图次数、图片文件与挑战绑定、时间及脱敏错误码。不要先买号码等待一个尚未证实存在的“竞态修复”，也不要把旧失败任务删掉后重新提交。
