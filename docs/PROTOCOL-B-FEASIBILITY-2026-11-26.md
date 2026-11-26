# 协议B（Rust / sentinel-rs）接入工作台：排查与施工方案

> ## ⚠ 目标工作台更正（2026-11-26 23:2x）
>
> 本文最初把「工作台」认成了 Apple 的 18741 dashboard（`D:\appleservices-family-private`），**认错了**。
>
> 真正的协议B 落地目标工作台是 **`D:\Workbench\apps\gpt-free-service`**（`python web.py --port 50694`，
> UI 在 `webui/templates/index.html:2391-2395`，方式下拉已有 `roxy_selenium` / `protocol`）。
>
> **施工一律以 `D:\Workbench\apps\gpt-free-service\docs\PROTOCOL-B-SENTINEL-RS-2026-11-26.md` 为准。**
>
> 本文只保留两节作参考：§3「8788 取码契约」与 §1.2/§6「sentinel-rs CLI 与二进制前置」。
> 其它关于 `/api/register`、`registration_method.go`、`pipeline.py` 的内容不适用于本次接入。

日期：2026-11-26（23:0x 更新）　状态：**只做排查与方案，未改任何工作台代码、未构建、未注册、未发请求。**

- 证据口径：只读源码 + 只读原型仓库（`.cache/sentinel-rs-src`，commit `f222371`）+ 只读 8788 源码（`D:\Workbench\sources\iCloud-Privacy-Mail-v2`）+ 本机工具链探测 + 现有测试基线。
- 凭据一律只记字段名，不落明文。
- 本文是给实现方（GPT）的**施工规格**；本仓库当前一行代码都没动。

## 本轮更新的三项决定

| # | 事项 | 决定 |
|---|---|---|
| 1 | 邮箱供给 | **走 8788**，和协议A/roxy 一样调用；不写新 bridge（§3 已实测：8788 自己生成 `api_url`，sentinel-rs 直接能吃） |
| 2 | 前端 | **后端验收通过后接前端**：注册表单多一个「注册方式」= 协议B（§5 字段矩阵） |
| 3 | 二进制 | 待定（§6 解释「这到底在问什么」） |

---

## 0. 结论速览

1. 协议B **必须独立创建接口 + 独立任务根 + 独立 runner**，不能塞进现有 `browser`/`protocol` 冻结语义（`internal/dashboard/registration_method.go:15-33`）。
2. 协议B **不能复用 pipeline 的 protocol 分支**：那条链绑着 Apple 购号、退款、号码结算、handoff（`tools/apple-pipeline/pipeline.py:363-437,1379-1397`）。
3. **2FA 不用另建后台**：sentinel-rs 自己有 TOTP enroll→activate（`src/flow/twofa.rs:1-20`），secret 落它自己的 SQLite（`src/flow/store.rs:83-93`、`src/flow/mod.rs:940-962`）。工作台只透传 `--2fa` 并回读状态。
4. **邮箱零桥接**：8788 的 `POST /api/v1/mailboxes/claim` 返回的 `data.mailbox.api_url` 就是现成的 `code_url`
   （`internal/httpapi/p2.go:749-758`），拼成 `email----api_url` 写进 `mail.txt`，sentinel-rs `--email-file` 直接读；
   它的 JSON 解析已支持嵌套 `data.code` 和 `success:false/no_code` 待机态（`src/flow/otp.rs:338-401`）。
   **不需要改 sentinel-rs，也不需要新写取码服务。**
5. 唯一未决：`sentinel-rs.exe` 从哪来（本机无 Rust/MSVC/Perl/LLVM，全盘也无该 exe，§6）。

---

## 1. 现状对照

### 1.1 工作台现有两条产线（同一个入口 `/api/register`）

| 维度 | browser | protocol（协议A） |
|---|---|---|
| 冻结值 | `browser` | `protocol`（`registration_method.go:16-17`） |
| 执行体 | `pipeline.py` + Forge/Playwright | `pipeline.py` → `signup_adapter` → `bin/signup.exe` |
| 适配器 | — | `APPLESERVICES_PROTOCOL_ADAPTER`，缺省**故意不配**（`tools/apple-pipeline/protocol_adapter.py:32-52`） |
| 回执 | `result.json` | signup.exe 一行 JSON + `protocol/state.json` 双证（`signup_adapter.py:166-214`） |
| 资源 | 购号 + 接码 + 取码 + handoff | 同一套（协议A 也要买号） |

创建入口：`register.go:614-645`；Apple 专属校验：`register.go:169-229`。
**注意**：现有前端提交体里没有 `registrationMethod`（`internal/dashboard/web/register.js:128-131`），所以 UI 实际只走默认 `browser`；协议A 目前只能靠 API 直接 POST。

### 1.2 协议B 原型（sentinel-rs）形态

目标不是 Apple，是 **OpenAI / ChatGPT**（`src/flow/auth.rs:21-79`、`src/env.rs:359-365`）。

CLI 只有 4 个子命令（`src/main.rs:30-47`），`register` 全部参数（`src/main.rs:354-427`）：

```
-n/--count N  --db PATH  --proxy URL  --proxy-file PATH  --email-file PATH
--2fa  --dry-run  --preflight  --live
```

**没有 `--email`、没有 `--password`、没有 `--json`**：
- 邮箱从池里领（静态池 `email----code_url`，或 `REG_MAIL_API_BASE_URL` 契约）；
- 密码内部随机（`src/flow/auth.rs:959-984`，16 位四类字符）；
- 成功只有一行：`注册完成: <email>  account_id=Some(N)  access_token=有/无  2fa=true/false`（`src/main.rs:558-565`）；
- **`exit 0` 不代表成功**（只有「全失败」才 bail，`src/main.rs:574-578`）。

结果落 SQLite（`src/flow/store.rs:83-93`）：`email/password/access_token/totp_secret/cookies/status/created_at/updated_at`；
`status ∈ {pending, pending_otp, registered, registered_no_token, …}`（`src/flow/mod.rs:855,954-962`）。

### 1.3 本机环境（实测）

go1.26.11；`go test ./internal/dashboard/...` = **ok 2.439s（基线绿）**；
`cargo`/`rustc`/`rustup`/MSVC(`vswhere`)/`LLVM`/`Strawberry Perl` **全缺**；全盘（深度 6）无 `sentinel-rs.exe`。

---

## 2. 四个缺口与当前处置

| # | 缺口 | 证据 | 处置 |
|---|---|---|---|
| 1 | 现有入口把任务冻结成 browser/protocol，且强制 Apple 字段 | `registration_method.go:15-33`、`register.go:169-229` | 新增独立端点族（§4） |
| 2 | pipeline 的 protocol 链绑 Apple 购号/退款/handoff | `pipeline.py:363-437,1379-1397` | 不复用，独立 runner（§4.5） |
| 3 | 邮箱/取码 | 原判断：OTPGmail/Outlook 读码器只认 apple（`tools/capture/otpgmail.cjs:74-76`、`mail-reader.cjs:22-26`） | **已解决**：走 8788（§3），不碰读码器 |
| 4 | 二进制不可得 | §1.3、§6 | 待定（唯一未决项） |

---

## 3. 8788（ipm-server）调用契约（协议B 的邮箱面）

8788 源码位置：`D:\Workbench\sources\iCloud-Privacy-Mail-v2`（**不是** `services\icloud-mail` 那份旧快照）。

### 3.1 前置开关（8788 侧既有设置，本仓库不改）

- `Settings.EnablePublicMailboxAPI = true`，否则一律 403 `public_api_disabled`（`p2.go:136-139`、`mailbox_lease.go:167-170`）。
- `Settings.PublicAPIKey`（或 `cfg.api_key`）非空 → 这是**全局 API Key**（`p2.go:778-783`）。

### 3.2 领号：`POST /api/v1/mailboxes/claim`

鉴权：`X-API-Key: <全局APIKey>`（或 query `key=` / `Authorization: Bearer`，`p2.go:761-767`）。
请求体（可选）：`{"project","purpose","request_id","note","ttl_seconds"}`（`p2.go:144-156`）。

响应（`p2.go:173-178`）：

```json
{"success":true,"data":{
  "mailbox":{"id":"…","account_id":"…","email":"abc@icloud.com","api_active":true,"icloud_active":true,
             "status":"…","retired":false,"active_lease_id":"…",
             "api_url":"http://127.0.0.1:8788/api/v1/mailboxes/abc%40icloud.com/code?key=<该邮箱API Token>",
             "api_token_mask":"abcd****wxyz"},
  "lease":{"…": "…"},
  "created":true,"idempotent":false}}
```

关键：**`api_url` 由 8788 自己拼好，里面带的 `key` 是该邮箱的 APIToken**（`p2.go:749-758`），
不是全局键 —— 所以 sentinel-rs 不需要任何额外鉴权头，把 `api_url` 当 `code_url` 用就行。
无可用号时返回 HTTP 200 + `{"success":false,"code":"no_available_mailbox"}`（`p2.go:166-168`）。

**幂等**：`request_id` 用本任务 `jobId`，重复 claim 返回 `idempotent:true` 且同一号（`p2.go:177`）。

### 3.3 取码：`GET {api_url}`（即 `/api/v1/mailboxes/{email}/code`）

- 鉴权：query `key` / `X-API-Key` / `Bearer`，接受该邮箱 APIToken 或全局键（`p2.go:273-277`）。
- 可用 query：`after`(RFC3339)、`wait_ms`(≤30000 长轮询)、`keyword`(**默认 `OpenAI`**，`p2.go:308-311`)、`allow_stale`、`cache`、`peek/preview`、`account_id`。
- 有码：`{"success":true,"data":{ …"code":"123456"… }}`（`p2.go:334`）。
- 无码：HTTP **200** + `{"success":false,"code":"no_code","message":"…"}`（`p2.go:338,381`、`server.go:1300-1302`）。
- 取到即**标记已消费**（`MarkAsServed`，`p2.go:315-316`）。

**与 sentinel-rs 的兼容性（已核对源码，不用改造）**：
`select_code_from_json` 会递归找 `code` / `data.code` / `result.code` / `data.otp`（`src/flow/otp.rs:383-401`），
`success:false` 或 `code="no_code"` 被判为“还没有码”继续轮询（`src/flow/otp.rs:338-354`）。
→ 8788 的两种响应都落在它既有的支持范围内。

### 3.4 我们的取码 URL 拼法

```
{email}----{api_url}&wait_ms=0&keyword=OpenAI
```

- 追加 `wait_ms=0`：把“等”交给 sentinel-rs 自己的 `OTP_MAX_WAIT`/`OTP_POLL_INTERVAL`，避免两层超时叠加（8788 默认长轮询，`p2.go:296-307`）。
- `keyword` 保留默认即可（8788 默认就是 OpenAI）；如遇主题差异再改这一处。
- `api_url` 已含 `?key=`，所以后续参数一律用 `&` 拼。

### 3.5 生命周期：租约收尾

- 注册成功 → `POST /api/v1/mailbox-leases/{lease_id}/commit`（`server.go:124`）。
- **明确未建号** → `POST /api/v1/mailbox-leases/{lease_id}/release`（`server.go:125`），或 `POST /api/v1/mailboxes/{email}/return-unused`（`server.go:114`）。
- 结果未知 → **什么都不做**，让租约自然到期；绝不自动重试、绝不自动换号。
- 上述租约端点同样要 `X-API-Key` 全局键（`mailbox_lease.go:166-176`）。

### 3.6 三个必须写死的安全约束

1. `api_url` 是 `http://127.0.0.1:8788/...`（`PublicBaseURL` 未配 https 时）→ sentinel-rs 静态池默认拒绝明文 http（`src/flow/email.rs:512-513`），
   必须设 `REG_MAIL_ALLOW_INSECURE=1`；**runner 必须校验 host ∈ {127.0.0.1, localhost, ::1}**，否则拒绝启动（防止这个逃生门被用在公网地址上）。
2. `mail.txt` 里含邮箱级 API Token → 文件 0600、只落任务目录、不进 `result.json`、不进日志、不进 git（`.cache/` 已忽略）。
3. 同一邮箱**同时只能有一个任务**：8788 取码是「取到即消费」，并发会导致第二个任务永远等不到码。→ 领取后用 `request_id=jobId` 幂等 + 工作台侧同 email 排重双保险。

---

## 4. 接入设计（独立产线）

### 4.1 形态

新增一个独立 runner 可执行文件（`cmd/protocolb`），dashboard 只负责「建任务 → 拉起 → 回读」。理由：进程隔离、可单独重跑、面板崩溃不影响任务；同时不碰 `pipeline.py`。

### 4.2 端点族（与 `/api/register*` 平级）

| 方法 | 路径 | 行为 |
|---|---|---|
| POST | `/api/protocol-b/jobs` | 创建任务，返回 `jobId`（`pb-YYYYMMDD-<hex16>`） |
| GET | `/api/protocol-b/jobs` | 列表（只回枚举状态，不回密码/token/secret） |
| GET | `/api/protocol-b/jobs/{id}` | 单任务视图 |
| GET | `/api/protocol-b/jobs/{id}/log?tail=N` | 事件流（stage/status 白名单，照 `register.go:713-741` 的做法） |
| POST | `/api/protocol-b/jobs/{id}/cancel` | 写 `cancel.request`，runner 终止进程树 |
| GET | `/api/protocol-b/config` | 只读：二进制是否存在、8788 是否可达、默认 2FA |

路由挂载：`internal/dashboard/server.go:1247` 那组 case 里**新增一条** `/api/protocol-b` → `serveProtocolB`；鉴权沿用现有 loopback + `X-App-Token` + Origin（`server.go:1223-1239`）。

### 4.3 请求体（不复用 `registerRequest`）

```json
{
  "country": "US",
  "enable2fa": true,
  "egress": {"mode": "roxy", "proxyPort": 10005},
  "acceptTerms": true,
  "live": false
}
```

校验：
- `country` 取原型 50 国与工作台 `registrationCountries`（`register.go:147-155`）的交集；
- `egress.mode ∈ {roxy,direct}`，roxy 端口 1–65535；复用 `resourceProxy` 的 `{country}` 展开（`internal/dashboard/resources.go:132-160`）；
- `live=false` 只允许 dry-run；请求里不得携带任何 `SENTINEL_LIVE_REGISTER` 之类 env 覆盖；
- 同 email/同 8788 邮箱已有未终结任务 → 拒绝（照 `register.go:399-421` 的排重风格，只扫 `.cache/panel-b`）。

### 4.4 任务目录（私有，0600）

```
.cache/panel-b/pb-20261126-<hex16>/
  request.json     冻结请求（写一次）
  panel.json       startedAt/finishedAt/exitCode
  result.json      终态回执（schema=1，绑定 jobId+email+runId）
  events.jsonl     只含 stage/status/at
  protocol-b/
    sentinel.db         每任务独立 SQLite
    sentinel.log        stdout+stderr
    mail.txt            一行：email----{api_url}&wait_ms=0&keyword=OpenAI
    mail.txt.used       sentinel-rs 自己去重写
    totp.secret         2FA secret（仅成功且有 2FA）
    lease.json          {lease_id, email, claimed_at}
```

### 4.5 runner 执行序列（`cmd/protocolb`）

1. 读 `request.json`，生成 `runId`；
2. **预检（全部通过才继续）**：`bin/sentinel-rs.exe` 存在可执行；8788 配置存在；`country`/`egress` 合法；
   任一失败 → `result.json{outcome:"not_started",reason:"…"}`，**不领号、不发请求**；
3. 领号（§3.2）→ 拿到 `email` + `api_url` + `lease_id` → 写 `mail.txt`(0600) 与 `lease.json`；
   若 `no_available_mailbox` → `outcome:"not_started",reason:"no_mailbox"`；
4. 组 env（**白名单，不继承调用方全部环境**）：
   `SENTINEL_LIVE_REGISTER`(仅 live) / `SENTINEL_DB_PATH` / `REG_MAIL_EMAIL_FILE` / `REG_MAIL_ALLOW_INSECURE=1`
   / `SENTINEL_PROXY` / `SENTINEL_ENABLE_2FA` / `SENTINEL_FP_COUNTRY` / `SENTINEL_FP_GEO_PROBE`
   / `OTP_MAX_WAIT=180` / `OTP_POLL_INTERVAL=3` / `SENTINEL_LOG=info`；
   **清除** `SENTINEL_USER_AGENT`、`SENTINEL_ACCEPT_LANGUAGE`、`SENTINEL_PROXY_FILE`（否则指纹被外部钉死）；
5. 子进程：`sentinel-rs register -n 1 --db <…> --email-file <…> [--proxy <…>] [--2fa] [--live|--dry-run]`，
   cwd=任务目录，stdout/stderr 落 `sentinel.log`，逐行匹配已知前缀写 `events.jsonl`；
6. 退出后**三源校验**：
   - 源1 stdout：`注册完成: <email>`（`src/main.rs:558-565`）；
   - 源2 SQLite：`SELECT email,status,totp_secret,access_token FROM accounts`（`sqlite3` 标准库读取，**不新增 Go 依赖**）；
   - 源3 进程：exit code。
   判定：三源同一 email 且 `status ∈ {registered,registered_no_token}` → `registered`；
   有行但 `status∈{pending,pending_otp}` → `inconclusive`；无行且 exit≠0 → `not_applied`（带脱敏 reason）；
   **其余一切不一致 → `needs_review`，绝不宣告成功。**
7. 收尾（§3.5）：`registered`→commit；`not_applied`→release；`inconclusive/needs_review`→不动。

### 4.6 状态机

```
queued → claiming → running → (waiting_otp) → registered
                          ↘ inconclusive / needs_review
                          ↘ failed(明确未建号) / cancelled
```

`waiting_otp` 只做展示（来自日志推断），不做决策；回执以 DB 为准，不自动重试。

### 4.7 取消

`cancel.request` → `taskkill /PID <pid> /T /F`（与 `signup_adapter.py:99-114` 同款）→ 确认进程消失 →
`outcome:"unknown", needs_review`（可能已建号）；**不退号、不重试**。

---

## 5. 前端接线规格（后端验收通过后做）

### 5.1 现状冲突（必须先知道）

现有表单**没有注册方式选择器**，提交体里也没有 `registrationMethod`（`register.js:128-131`）。
所以「多一个协议B 的方式」= 需要在表单里**新增一个下拉**，并让字段随方式切换。
这会让协议A 首次在 UI 可见（服务端本来就支持，语义不变）——这是唯一会“看得见”的旧路径变化。

### 5.2 字段矩阵

| 表单字段 | browser | protocol（A） | **protocol-b** |
|---|---|---|---|
| 邮箱 / 密码 | 需要 | 需要 | **隐藏**（邮箱由 8788 领；密码由 sentinel-rs 生成） |
| 手机号 / 平台 / 激活 ID / 到期 | 需要 | 需要 | **隐藏** |
| 手机国家 `phonecc` | 需要 | 需要 | 改义为**账号地区 `country`**（透传 `SENTINEL_FP_COUNTRY`） |
| 网络出口 `mode`/`proxyPort` | 需要 | 需要 | 保留（复用 roxy） |
| Apple 条款 `acceptTerms` | 需要 | 需要 | 保留（改为服务条款确认，文案另写） |
| 家庭角色 `familyRole` | 需要 | 需要 | **隐藏** |
| **2FA 开关** | — | — | **新增**（默认开，透传 `--2fa`） |
| 自动模式 `automatic` | 支持 | 支持 | **隐藏**（协议B 不购号） |

### 5.3 提交映射

`protocol-b` 提交到 `/api/protocol-b/jobs`，body 按 §4.3 组装（不带 email/password/phone/family）；
其余两种方式维持现状走 `/api/register`（**不改它的行为**）。

### 5.4 展示

- 任务列表单独一块（`GET /api/protocol-b/jobs`），不与 Apple 任务混列（字段语义不同）。
- 阶段文案：`claiming=领取邮箱 / running=注册中 / waiting_otp=等待邮箱验证码 / registered=已注册 / needs_review=需要核实`。
- 2FA 只显示「已绑定/未绑定」，**不提供查看 secret 的入口**。

---

## 6. 二进制前置：问题1 到底在问什么

**白话**：协议B 的执行体是 `sentinel-rs.exe` 这个**程序文件**。现在它不在本机，而且本机也编不出来——
编译它不只需要 Rust，还要 MSVC C++ 工具链、nasm、Strawberry Perl、LLVM/libclang，因为它的传输层要现场编 BoringSSL
（`Cargo.toml` 钉死 `wreq = "=0.16.1"`，`README.md` 的「构建」一节写明必须走 `build.ps1`）。
实测本机：`cargo`/`rustc`/`rustup`/`vswhere`/`LLVM`/`Strawberry` 全部缺失，全盘也没有这个 exe。

所以问题1 问的是三件事：**谁产出这个 exe、放到哪、怎么确认它没被改过。** 不是问“要不要装 Rust”。

| 选项 | 做法 | 代价 | 风险 |
|---|---|---|---|
| **A** 别处构建后投送（推荐） | 在装好 MSVC+nasm+Perl+LLVM 的机器上 `build.ps1`，取 `target\release\sentinel-rs.exe` 放到 `bin\sentinel-rs.exe` | 0（本机不装工具链） | 要确认 exe 来源与版本；回执里记录 SHA256 + mtime |
| **B** 本机构建 | 装 Rust + VS2022 C++ + UCRT + nasm + Strawberry Perl + LLVM，跑 `build.ps1` | 1–2 小时（BoringSSL 编译慢） | 环境坑多；`bin/` 被 `.gitignore` 忽略，产物不入库 |
| **C** 先只干跑 | 接线全做完，`live=false` 只验 dry-run/preflight，exe 到位后再开 live | 0 | 没有真实注册验证；L5 验收要延后 |

无论哪个选项，runner 都必须 **fail-closed**：exe 缺失 → `not_started`，不领号、不发请求（§4.5 第2步）。
建议：**A + C 并行**——你先投送 exe，我这边按 C 把接线做完，exe 一到就跑 L5。

---

## 7. 改动清单（给实现方的施工单）

| 文件 | 改动 | 规模 |
|---|---|---|
| `internal/dashboard/protocolb.go` | **新增**：端点族、请求校验、任务冻结、拉起 runner、视图聚合 | ~350 行 |
| `internal/dashboard/protocolb_test.go` | **新增**：校验/冻结/排重/回执绑定/二进制缺失 fail-closed | ~250 行 |
| `cmd/protocolb/main.go` | **新增**：runner（领号、组 env、跑子进程、三源回执、租约收尾） | ~320 行 |
| `cmd/protocolb/main_test.go` | **新增**：假 exe/假 DB/假 8788 验证回执判定与收尾分支 | ~200 行 |
| `internal/dashboard/server.go` | **改一处**：`/api/` case 增加 `/api/protocol-b` 分支（约 3 行） | XS |
| `tools/apple-pipeline/build.ps1` | **追加**一行构建 `cmd/protocolb`（可选） | XS |
| `internal/dashboard/web/index.html` + `register.js` | 注册方式下拉 + 字段矩阵 + 协议B 提交与列表（§5） | L（二期） |

**明确不改**：`register.go`、`registration_method.go`、`pipeline.py`、`tools/signup/*`、`tools/capture/*`、`mail_bridge.go`、
现有 Apple 注册表单的 server 端行为、账号列表与 worker 生命周期、8788 仓库任何一行。

---

## 8. 验收矩阵

| 层 | 动作 | 通过标准 | 消耗 |
|---|---|---|---|
| L1 | `go build ./...` + `go test ./...` | 全绿，且 `internal/dashboard` 原有用例一条不红（基线 2.439s） | 无 |
| L2 | 删/改名 `bin/sentinel-rs.exe` 后创建任务 | `not_started + sentinel_binary_missing`，任务目录无 `mail.txt`，无任何网络 | 无 |
| L3 | `live=false` dry-run | `outcome:"dry_run"`，SQLite 无新增账号，租约 release | 1 邮箱（可复用） |
| L4 | `live=false` preflight | 会话建立成功；DB 仍无账号 | 联网 |
| L5 | `live=true` + `enable2fa=true` | 三源一致 → `registered`；`totp.secret` 存在 0600；租约 commit；面板显示「已绑定」 | 1 邮箱 + 代理 |
| L6 | 前端（二期） | 三种方式切换字段正确；协议B 任务独立列出；无凭据泄漏 | 无 |

回滚：协议B 全是新增文件 + `server.go` 一个 case；撤掉即回到现状，Apple 产线零影响。

---

## 9. 风险登记

1. `exit 0` ≠ 成功（`src/main.rs:574-578`）→ 永不看单一信号，三源交叉。
2. 8788 取码**取到即消费**（`p2.go:315-316`）→ 同邮箱禁止并发；失败重试要换号或人工确认。
3. 明文 http 逃生门（`REG_MAIL_ALLOW_INSECURE=1`）→ runner 硬校验 loopback。
4. 邮箱级 Token 落在 `mail.txt` → 0600 + 不出任务目录。
5. 代理国家 vs 指纹国家（`SENTINEL_FP_GEO_PROBE` 默认开，`src/flow/mod.rs:483-492`）→ 创建时校验 roxy `{country}` 与请求 `country` 一致。
6. 二进制不可控 → 回执记录 exe 的 SHA256 与 mtime。
7. 2FA secret 只落任务目录；`result.json` 只记「有/无」。

---

## 10. 唯一待确认项

**`bin/sentinel-rs.exe` 怎么来**：A 别处构建投送 / B 本机构建 / C 先干跑等 exe。
（邮箱已定走 8788；前端已定后端验收后再接。）

---

## 11. 证据索引

**工作台**
- 冻结逻辑 `internal/dashboard/registration_method.go:15-33,38-80`
- 入口与 Apple 校验 `internal/dashboard/register.go:169-229,399-421,614-645`
- manifest 与子进程 `register.go:448-500,514-535`
- 事件白名单 `register.go:713-741`
- 路由与鉴权 `internal/dashboard/server.go:1223-1249`
- roxy 展开 `internal/dashboard/resources.go:132-160`
- 前端提交体（无 method）`internal/dashboard/web/register.js:128-131`
- 协议A 适配器 `tools/apple-pipeline/protocol_adapter.py:32-52`、`signup_adapter.py:99-214,166-214`
- pipeline protocol 分支 `tools/apple-pipeline/pipeline.py:199,363-437,1379-1397`
- 读码器平台限制 `tools/capture/otpgmail.cjs:74-76`、`tools/capture/mail-reader.cjs:22-26`

**8788（`D:\Workbench\sources\iCloud-Privacy-Mail-v2`）**
- 路由表 `internal/httpapi/server.go:95-131`（claim 103、code 118、lease 123-127）
- 领号 `internal/httpapi/p2.go:135-179`（鉴权 140-143、响应 173-178）
- 取码 `p2.go:220-460`（鉴权 273-277、keyword 默认 OpenAI 308-311、空码 338,381）
- `api_url` 生成 `p2.go:749-758`；`publicMailbox` `p2.go:730-747`
- 全局键 `p2.go:761-783`；错误体 `internal/httpapi/server.go:1300-1302`
- 租约鉴权 `internal/httpapi/mailbox_lease.go:166-176`

**协议B 原型（`.cache/sentinel-rs-src`，commit `f222371`）**
- CLI `src/main.rs:30-47,354-427,544-578,614-694`
- 取码轮询（JSON 嵌套/pending/HTML）`src/flow/otp.rs:338-401`、`src/flow/email.rs:403-486,503-521`
- 静态池与明文 http `src/flow/email.rs:512-513`
- 2FA `src/flow/twofa.rs:1-60`
- SQLite `src/flow/store.rs:83-93,125-171`、`src/flow/mod.rs:855-860,940-962`
- 密码生成 `src/flow/auth.rs:959-984`
- 出口国家探测 `src/flow/mod.rs:483-530`
