# 18741 面板：新建 Apple ID

本页以用户最后确认的契约为准：**仅完善 Apple 仓库，不接入或安装工作台**。
面板地址 `http://127.0.0.1:18741`；Roxy/Forge 地址 `http://127.0.0.1:50100`。
家庭组、隐藏邮箱和付费订阅不属于本轮新建链路。

## 已确认的规则

- 建号后不固定等 10 分钟；立即继续登录，遇 `423` / `tooManyCodesSent` 停止，记录一小时冷却截止时间。冷却结束也不自动重新登录，需先核实号码与账号状态。
- **退出码 4 是验证码等待超时，不是注册失败。** 普通等待窗口结束后，在原页面、原号码上继续等到实际有效期结束；到期收尾，再用不显式发码的密码认证核实账号是否存在。
- 退出码 `0` 成功，`1` 明确失败，`2` 参数错，`3` 未知，`5` 疑似成功。未知或疑似成功进入登录核实，绝不重新注册。
- 取消会写入持久取消请求。产线检查到请求后终止当前子进程树、清理自己的浏览器。尚未提交的注册可以申请退号；已经提交但结果未知时保留号码至到期，不把取消当作“账号不存在”。
- 明确失败可以用**新号码、新激活 ID、新任务 ID**重试；未知结果、已建成账号和活动任务禁止重试。
- 退款复用产线的按供应商选择接口的 `setStatus=8` 实现，不再启动只适配旧流程的 `refund.cjs`。平台拒绝立即取消时挂账，130 秒后再试，最多 5 次；只把平台确认记为退号完成。

## 启动与私密配置

构建并启动（若 18741 已在运行，脚本只打开原面板；更新二进制前先关闭旧面板进程）：

```powershell
& .\tools\apple-pipeline\panel.ps1 -Build
```

这个仓库管理的启动脚本构建 `dashboard`、`login`、`appleweb`、`trusted-device`，窗口隐藏运行。
原来的 `bin\2-panel.cmd` / `run-ui.ps1` 是本机启动文件；代码更新后应使用上面的入口，以加载接码环境变量。

将 `tools/apple-pipeline/registration-config.example.json` 复制为 `state/registration-config.json`，只在本地填写 Forge token、工作区、项目和 SOCKS5 信息。
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

点击「新建账号」，填邮箱、强密码、已购买的号码/激活 ID、**平台真实到期时间**和网络出口，确认同意条款。
密码在浏览器和后端均校验：至少 8 位，大小写字母、数字、特殊字符均有。
邮箱目前用手动验证码入口；收到邮件后在任务卡片输入，短信自动从所选平台读取。图形码使用 YesCaptcha，可能产生识别费用。

页面显示阶段、用时、脱敏运行记录、截图、取消，以及明确失败时的重试按钮。
刷新页面后恢复任务列表，前端不保存密码/OTP，提交成功后清空相应输入。
日志只返回已知阶段/状态和时间；原始 `run.jsonl`、认证响应、密码、密钥、Cookie、DSID 和令牌均不通过接口返回。
截图接口只允许任务内 PNG；所有整页截图路径统一遮住输入框，不提供任意目录浏览或私密 JSON 下载。

## 实际链路与账号列表

```text
queued → creating → login → terms → twofa (GSA) → device → keepalive → done
```

复用 `tools/apple-pipeline/pipeline.py`，网页登录与条款由 `bin/appleweb.exe` 完成，GSA checkpoint 由 `bin/login.exe` 保存，设备登记由 `bin/trusted-device.exe -register-only` 完成。
网页登录先于 GSA，避免未接受条款的新号卡在 delegate。首次保活验证成功、GSA/accepted checkpoint 与邮箱绑定一致后，写入现有 `state/accounts.json`。
原账号及目录不迁移。产线账号不会交给面板旧的 Mac worker 自动启动，避免覆盖 Windows 登记身份。

**完成表示网页会话、GSA、设备登记和首次保活已验证；不表示可信设备推送接码已经验证。**
产线账号单独标识这一限制，现有 Mac 身份相关家庭/订阅操作不自动对其启用。

## 接口

沿用面板已有的 loopback Host、Origin 和 `X-App-Token` 校验，响应 `no-store`。

```text
POST /api/register
GET  /api/register                         → {jobs:[...]}
GET  /api/register/{jobId}                 → 进度
GET  /api/register/{jobId}/log?tail=200     → 脱敏 JSON 行数组
POST /api/register/{jobId}/input           → {code:"邮箱验证码"}
POST /api/register/{jobId}/cancel          → 202
GET  /api/register/{jobId}/evidence        → PNG 文件名列表
GET  /api/register/{jobId}/evidence?name=…  → 单张 PNG
```

创建请求字段：`email,password,phone,phonecc,platform,activation,expiresAt,acceptTerms,captchaProvider,egress`。
`expiresAt` 为带时区的 RFC3339 时间；`captchaProvider` 为 `yes`；`egress` 为 `{mode:"roxy"|"direct",proxyPort:10005}`。
重试增加 `retryOf`，后端再次验证前次结果及新的激活 ID。禁止请求体中的 `platformKey` 等未知字段。
成功返回 `202 {jobId}`，校验失败返回 `400 {message}`，不会开始注册或扣费。

进度包含 `state,stage,startedAt,elapsedMs,exitCode,active,canRetry,imported,cooldownUntil` 和退款/清理标记。
`exitCode` 是产线进程退出码；注册子阶段原始退出码仅保存在本地回执 `create_exit_code`，不混用二者。
`waiting_input` 只有 `active=true` 时表示原进程仍可收码；`unknown/needs_review` 必须先核实，禁止前端自动重放。

## 持久化、恢复和保活

面板任务及证据保存在 `.cache/panel/<jobId>/`，这是**私密运行目录**，不要清理为普通缓存、提交或共享整目录。
其中 `request.json/input.json` 持有凭据，`panel.json/result.json/events.jsonl` 保存进度，PNG/`run.jsonl` 保存本地证据。
CLI 默认仍使用 `state/pipeline`；面板通过子进程环境 `APPLESERVICES_PIPELINE_STATE` 指向 `.cache/panel`。

关闭浏览器页面不影响面板服务。面板服务运行时每 15 秒检查任务；成功账号每 10 分钟验证网页会话并原子保存轮换 Cookie，失败只记需登录；同时重试浏览器清理和退款挂账。
关闭面板进程后，已启动的产线子进程继续当前任务，但周期维护停止。重启面板后依靠跨进程锁恢复观察和维护；失去进程的活动任务标记需核实，不自动重新注册或登录。
维护和注册共享锁，两个面板实例不能同时处理同一任务。

## 验证范围

Go API 测试覆盖鉴权、输入校验、私密文件隔离、日志脱敏、取消、邮箱验证码、模拟子进程、重复注册、新号码重试、账号绑定和幂等入库。
Python 测试覆盖完整阶段交接、未知结果不重放、取消、退款、冷却、重启恢复、并发锁和代理隔离；Node 测试覆盖表单状态机与等码生命周期。
`tools/apple-pipeline/panel-ui-test.cjs` 用于隔离面板的 Playwright 测试，拦截全部注册请求，不访问 Apple 或付费平台。
自动化和浏览器测试不等于真实账号验收；没有使用真实号码或账户完成本轮付费端到端验证。
