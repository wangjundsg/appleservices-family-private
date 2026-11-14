# 18741 面板：新建 Apple ID

本页以用户最后确认的契约为准：**仅完善 Apple 仓库，不接入或安装工作台**。
面板地址 `http://127.0.0.1:18741`；Roxy/Forge 地址 `http://127.0.0.1:50100`。
家庭组、隐藏邮箱和付费订阅不属于本轮新建链路。

2026-11-15 更新：新任务使用 Outlook 已有邮箱池，建号成功后交给 **18741 原有登录、短信验证、iCloud 激活和后台保活**。旧任务仍保留原执行方式，不迁移其账号身份。以下规则区分新自动任务与历史任务。

## 已确认的规则

- 建号后不固定等 10 分钟；立即继续登录，遇 `423` / `tooManyCodesSent` 停止，记录一小时冷却截止时间。冷却结束也不自动重新登录，需先核实号码与账号状态。
- **退出码 4 是验证码等待超时，不是注册失败。** 普通等待窗口结束后，在原页面、原号码上继续等到实际有效期结束；真正退出后转待核实，不能重复提交表单。
- 退出码 `0` 成功，`1` 明确失败，`2` 参数错，`3` 未知，`5` 疑似成功。新任务只有 `0` 才进入面板登录；其他不确定结果不通过重新登录发码来“核实”。
- 取消会写入持久取消请求，终止当前子进程树、清理自己的浏览器和面板后台。自动任务结束时未收到短信则申请退号；收到短信记 `OTP_RECEIVED`，不退款。取消不代表账号不存在。
- 自动任务允许首次购号及一次安全替换，账号累计最多两个号码。明确不支持后续短信，或注册被明确拒绝且未收到短信，才允许替换；未知结果不替换。第二次失败转 `needs_review`。手动模式仍允许明确失败后用新号码、新激活 ID 重试。
- **价格尚未商定。没有默认单价或总预算；示例中的价格留空。** 必须同时配置 `purchase.maxPrice/maxTotal/currency=USD` 并显式启用 `--allow-purchase`，才能购买。测试中的价格只是模拟数据。
- 主平台 grizzly，只有明确返回 `NO_NUMBERS` 才切 hero；网络超时、未知回执不切平台。国家为 GB/16、CA/36、ID/6、US/12、VN/10，服务 `wx`。墨西哥不在自动范围内。
- 退款复用产线的按供应商选择接口的 `setStatus=8` 实现，不再启动只适配旧流程的 `refund.cjs`。平台拒绝立即取消时挂账，130 秒后再试，最多 5 次；只把平台确认记为退号完成。

## 启动与私密配置

构建并启动（若 18741 已在运行，脚本只打开原面板；更新二进制前先关闭旧面板进程）：

```powershell
& .\tools\apple-pipeline\panel.ps1 -Build
```

这个仓库管理的启动脚本构建面板、旧产线程序及现有激活依赖 `accountsetupcheck/nacexchange`，窗口隐藏运行。默认不允许真实购号；价格确认后才使用 `-AllowPurchase`，对应面板参数 `--allow-purchase`。已经运行的面板不会因再次执行脚本改变开关。
原来的 `bin\2-panel.cmd` / `run-ui.ps1` 是本机启动文件；代码更新后应使用上面的入口，以加载接码环境变量。

将 `tools/apple-pipeline/registration-config.example.json` 复制为 `state/registration-config.json`，只在本地填写 Forge token、工作区、项目和 SOCKS5 信息。
`codePoolConfig` 指向 Outlook 服务生成的私密客户端文件（`baseUrl/apiToken`），只读接口默认 `http://127.0.0.1:8802`。不读取或复制 Outlook 邮箱密码、RT。自动购号密钥分别从 **`SMS_API_KEY`、`SMS_API_KEY_HERO`** 读取；不从旧密钥文件隐式补齐这两个变量。
也可通过服务端环境变量 `APPLESERVICES_REGISTER_CONFIG` 指定私密配置路径。
代理用户名中的 `{country}` 替换为号码国小写代码，端口使用表单值；所有模式均由 Forge 创建浏览器，直连模式不传 `proxyInfo`，协议进程也清除继承的代理变量。
语言、时区按号码国设置；预检实际出口国、语言、时区和 Windows 指纹，不一致即停止，不注入指纹。

平台密钥**只从服务端环境读取**：

| 环境变量 | 启动脚本可读取的已有本机私密文件 |
|---|---|
| `GRIZZLY_API_KEY` | `.cache/grizzly.key` |
| `HERO_API_KEY` | `.cache/apple-os-ab/hero.key` |
| `YESCAPTCHA_KEY` | `.cache/yescaptcha.key` |

已设置的环境变量优先；脚本只将文件内容加载到新面板进程环境，不打印值，不传命令行。
依赖 Node、Python 3.10+、`.tools/js/node_modules/playwright-core`、本机 Apple APS 服务和运行中的 Forge。

## 面板使用

点击「新建账号」默认自动模式：选择号码国、出口并同意条款。邮箱池筛选 `reader=graph/status=ok/authorization_status=ready` 且未被 Apple 任务占用的邮箱，生成随机密码。当前注册器明确不支持 `outlook.com/icloud.com`，会在购号前跳过这两类邮箱；Outlook 工作台中的受支持地址（如 Hotmail）可用。
邮箱占用以 Apple 的持久请求记录为准；Outlook API 为只读，不能阻止其他独立消费方同时选取同一邮箱。

关闭自动模式后，可填邮箱、强密码、已购买的号码/激活 ID、**平台真实到期时间**和网络出口。
密码在浏览器和后端均校验：至少 8 位，大小写字母、数字、特殊字符均有。
自动模式通过 `/api/v1/code` 取码，发信前记录 `since`，校验邮箱、Apple 发件域、时间窗及已处理的邮件 ID，保留六位码前导零。手动模式保留邮箱验证码入口。短信由所选平台读取，注册用过的码不能再次用于登录。图形码使用原有 YesCaptcha 配置，可能产生识别费用。

页面显示阶段、用时、脱敏运行记录、截图、取消，以及明确失败时的重试按钮。
刷新页面后恢复任务列表，前端不保存密码/OTP，提交成功后清空相应输入。
日志只返回已知阶段/状态和时间；原始 `run.jsonl`、认证响应、密码、密钥、Cookie、DSID 和令牌均不通过接口返回。
截图接口只允许任务内 PNG；所有整页截图路径统一遮住输入框，不提供任意目录浏览或私密 JSON 下载。

## 实际链路与账号列表

```text
queued → creating → 交接账号 → 面板登录/短信验证 → 原有 iCloud 激活 → 后台在线 → done
```

Python 负责阶段决策；Go 内部适配接口复用 `startAccount/activationAction/startSavedAccount`，不复制激活逻辑。新账号按邮箱稳定 ID 存在 `state/accounts/auto-…`。操作前记录 intent，成功回执按相同参数复用，未知结果停止自动重放。登录 checkpoint 建立后停止旧登录进程，再按读回的条款版本执行原激活。
账号在交接时加入列表；任务未完成时由原任务独占控制。只有与邮箱绑定的 accepted checkpoint 及后台认证在线均成立，才记 `handoff_complete`，释放给正常面板管理。完成不证明账号长期存活或可信设备推送接码已验证。
历史 `reg-*` CLI 账号仍走原网页保活，不自动改成 Mac worker。

## 接口

沿用面板已有的 loopback Host、Origin 和 `X-App-Token` 校验，响应 `no-store`。

```text
POST /api/register
GET  /api/register/config                  → 购号开关、预算/邮箱配置是否就绪（无密钥）
GET  /api/register                         → {jobs:[...]}
GET  /api/register/{jobId}                 → 进度
GET  /api/register/{jobId}/log?tail=200     → 脱敏 JSON 行数组
POST /api/register/{jobId}/input           → {code:"邮箱验证码"}
POST /api/register/{jobId}/cancel          → 202
GET  /api/register/{jobId}/evidence        → PNG 文件名列表
GET  /api/register/{jobId}/evidence?name=…  → 单张 PNG
```

创建请求字段：`email,password,phone,phonecc,platform,activation,expiresAt,acceptTerms,captchaProvider,egress`。
自动请求改为 `automatic:true,phonecc,acceptTerms,captchaProvider,egress`，不传邮箱、密码、订单或价格；价格只取服务端配置。默认禁止真实购买。
`expiresAt` 为带时区的 RFC3339 时间；`captchaProvider` 为 `yes`；`egress` 为 `{mode:"roxy"|"direct",proxyPort:10005}`。
重试增加 `retryOf`，后端再次验证前次结果及新的激活 ID。禁止请求体中的 `platformKey` 等未知字段。
成功返回 `202 {jobId}`，校验失败返回 `400 {message}`，不会开始注册或扣费。

进度包含 `state,stage,startedAt,elapsedMs,exitCode,active,canRetry,imported,cooldownUntil` 和退款/清理标记。
新增 `automatic,handoffComplete,spend`；`spend` 只公开次数、币种、累计花费、已退款金额和结算状态，不公开号码或订单 ID。
`exitCode` 是产线进程退出码；注册子阶段原始退出码仅保存在本地回执 `create_exit_code`，不混用二者。
`waiting_input` 只有 `active=true` 时表示原进程仍可收码；`unknown/needs_review` 必须先核实，禁止前端自动重放。

## 持久化、恢复和保活

面板任务及证据保存在 `.cache/panel/<jobId>/`，这是**私密运行目录**，不要清理为普通缓存、提交或共享整目录。
其中 `request.json/input.json` 持有凭据，`panel.json/result.json/events.jsonl` 保存进度，PNG/`run.jsonl` 保存本地证据。
CLI 默认仍使用 `state/pipeline`；面板通过子进程环境 `APPLESERVICES_PIPELINE_STATE` 指向 `.cache/panel`。
购号账本在 `state/sms-supply/<account-hash>.json`，保留跨 Job 的意图、供应商、国家、订单、实际单价、币种和退款结果。单账号总预算按累计购买额检查，退款不释放自动重购额度。

关闭浏览器页面不影响服务。面板每 15 秒维护清理及退款挂账；新账号完成后由现有账号后台保活，历史 CLI 账号保留原网页会话验证。
不要在活动注册/交接中重启面板：内部能力 token 随面板进程变化。重启不重放注册或登录，未完成交接的后台停止并保留待核实状态。长激活调用有明确心跳截止，不能仅因超过普通 45 秒心跳窗口就当作进程死亡。
任务执行仍持有跨进程文件锁，维护不能并发推进同一任务。这是渐进接入，不是 v2 全部状态存储迁移完成；`.cache/panel` 中的权威状态/凭据仍不能按证据清理。

## 验证范围

Go API 测试覆盖鉴权、输入校验、私密文件隔离、日志脱敏、取消、邮箱验证码、模拟子进程、重复注册、新号码重试、账号绑定和幂等入库。
Python 测试覆盖完整阶段交接、未知结果不重放、取消、退款、冷却、重启恢复、并发锁和代理隔离；Node 测试覆盖表单状态机与等码生命周期。
`tools/apple-pipeline/panel-ui-test.cjs` 用于隔离面板的 Playwright 测试，拦截全部注册请求，不访问 Apple 或付费平台。
自动化和浏览器测试不等于真实账号验收；没有使用真实号码或账户完成本轮付费端到端验证。
本次新增服务端及 Python/Node 模拟测试已通过；新 UI 的隔离面板启动被自动审批拦截，Playwright 脚本已更新但尚未执行。本次没有替换运行中的 18741 面板。供应商真实回执（包括 hero 的 V2 兼容性）仍待价格确认后的受控验收。
