# tools/console — 注册可视化控制台（薄壳）

单进程、零 npm 依赖（只用 Node 内置模块）、只监听 `127.0.0.1:8801`。
它本身**不做任何注册逻辑**：状态、日志、截图、邮箱池全部从 `.cache/` 里读，
「发起」只写 dry-run 演练记录，真跑要你自己在终端敲命令。

```
http://127.0.0.1:8801
```

---

## 1 启动 / 停止

```powershell
# 启动（幂等：已经在跑就只打印地址，不会起第二个进程）
powershell -ExecutionPolicy Bypass -File tools\console\start.ps1

# 换端口
powershell -ExecutionPolicy Bypass -File tools\console\start.ps1 -Port 8802

# 停止
powershell -ExecutionPolicy Bypass -File tools\console\start.ps1 -Stop
```

等价的手工方式：

```powershell
node tools\console\server.cjs                 # 前台（Ctrl+C 退出）
Stop-Process -Id (Get-Content .cache\console\server.pid) -Force   # 按 PID 停
```

| 项 | 值 |
|---|---|
| 地址 | http://127.0.0.1:8801 |
| 监听 | 127.0.0.1（固定，**不对外**） |
| PID 文件 | `.cache\console\server.pid` |
| 运行日志 | `.cache\console\server.out.log` / `.cache\console\server.err.log` |
| 健康检查 | `http://127.0.0.1:8801/api/health` → `{"ok":true,...,"tasks":N}` |
| 依赖 | 无。Node ≥ 18 即可（本机 v24） |

端口被占且 `/api/health` 不通时，`start.ps1` 会直接报错让你换端口，不会杀别人的进程。

---

## 2 页面结构

| 区块 | 内容 | 数据来源 |
|---|---|---|
| 顶部 KPI | 任务总数 / 成功 / 失败·硬拒 / 演练 / 邮箱池 | 扫描 `.cache/**/*.jsonl` + `.cache/mailboxes.json` |
| 发起任务 | 任务类型（Outlook / Apple）、执行路线、邮箱·号码·取号 ID·代理端口·是否走 Roxy | 表单 → `POST /api/launch`（**只做 dry-run**） |
| 邮箱池 | 地址 / reader / 状态 / 最近邮件 / 「取码」 | `.cache/mailboxes.json` + `.cache/console/mail-state.json`（点过「取码」才有最近邮件时间） |
| 任务列表 | 每个 jsonl 一行：类型、状态、开始、耗时、结果、日志尾、截图数 | 每 2 秒轮询 `/api/tasks` |
| 任务详情 | 日志（最后 300 条）/ 截图缩略图（点开大图）/ 信息 | `/api/task`、`/api/shot` |

任务 id = `jsonl` 文件相对 `.cache/` 的路径（去掉 `.jsonl`），例如：

- `apple-local-1/run` → `.cache/apple-local-1/run.jsonl`
- `cm/G6b/run`、`cm/G6b/session` → 同一目录下的两个 jsonl 各占一行
- `console/jobs/<时间戳>-outlook/run` → 本控制台自己写的演练记录

状态取自每个文件最后一条 `kind=final|close`（或带 `state` 的记录），
无终态但 5 分钟内有写入的标 `进行中?`，其余 `无终态`。

---

## 3 哪些按钮是真跑、哪些是占位

| 按钮 | 行为 | 说明 |
|---|---|---|
| **发起演练（dry-run）** | 真跑：写文件 | 在 `.cache/console/jobs/<ts>-<mode>/run.jsonl` 写 6 条演练记录，**不起浏览器、不连微软、不创建账号**。列表里状态显示「演练(未真跑)」。同时把「计划命令」原样打印出来，方便你复制到终端。 |
| **真跑（未接线）** | 占位，按钮置灰 | 前端 disabled；直接打 API 也会被拒：`/api/launch` 带 `dryRun:false` 返回 **409**，附上可手动执行的命令。即使设 `CONSOLE_ALLOW_REAL=1` 重启，本版本仍返回 501，不执行。 |
| **取码 / 取验证码** | 真跑（只读） | `reader=graph` 的邮箱：调 `tools/console/mail-code.cjs`（内部复用 `tools/capture/graph-mail.cjs` 的 `exchangeToken/listMessages/pickCode/codeFromText`）读最新 8 封邮件并抠出 6 位码。只读收信，不触发任何注册。 |
| **取码（reader=webmail）** | 占位 | 返回提示：验证码要走已登录的 Outlook 网页会话，需要浏览器，本控制台未接线；给出手动命令 `node tools/capture/outlook-web-mail.cjs --outdir <任务目录>`。 |
| **截图缩略图** | 真跑（只读） | `GET /api/shot?path=...`，只允许 `.cache/` 下的图片，路径越界返回 400。 |

任务类型对应的脚本（真跑命令由 dry-run 面板打印）：

| 类型 | 路线 | 脚本 |
|---|---|---|
| Outlook | `outlook-cn` | `tools/capture/outlook-cn.cjs` |
| Outlook | `outlook-roxy` | `tools/capture/outlook-roxy.cjs` |
| Outlook | `outlook-native` | `tools/capture/outlook-native.cjs` |
| Apple | `create-account` | `tools/capture/create-account.cjs` |

> 本控制台**不改动** `tools/capture/*` 任何一行；只是拼命令行、读文件。

---

## 4 HTTP 接口（都是只读 + 一个演练写入口）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 版本、端口、任务数、`allowReal` |
| GET | `/api/tasks?q=&limit=` | 任务列表（含最后 40 条日志尾） |
| GET | `/api/task?id=` | 单任务详情（日志尾 300 条 + 图片清单） |
| GET | `/api/mailboxes` | 邮箱池（**已剔除** password / refresh_token / client_id） |
| POST | `/api/mail/code` | `{id:"mb1"}` → 读信 + 抠码 |
| POST | `/api/launch` | `{mode,dryRun,route,params}` → 演练；`dryRun:false` 一律拒绝 |
| GET | `/api/shot?path=` | `.cache` 下的 png/jpg |
| GET | `/api/routes` | 可选路线清单 |

---

## 5 安全 / 纪律

- 只监听 `127.0.0.1`，没有鉴权——本机单人用，不要改成 `0.0.0.0`。
- **不返回、不打印** `password` / `refresh_token` / `client_id`；`/api/mailboxes` 只给 `hasToken: true|false`。
  日志里出现的 `"field":"password","length":16` 是原脚本的记录方式（只有字段名和长度），控制台不再加工。
- 读 `.cache` 为主。唯一的写操作：
  1. 演练记录 `.cache/console/jobs/...`（`.cache/` 已被 `.gitignore` 忽略）；
  2. 取码时把 token 落到 `.cache/console/tokens/<id>.json`（600），因为 `graph-mail.cjs` 会把**轮换后的** refresh_token 写回它拿到的文件；
  3. 若确实发生轮换，控制台会把新 token 同步回 `.cache/mailboxes.json`，并先备份为 `.cache/console/mailboxes.backup-<时间戳>.json`——否则池里那条会因微软 retire 旧 token 而作废。
- 截图/日志里的验证码、邮箱地址属于本机数据，控制台不外发任何请求（除了你自己点「取码」时的微软 Graph 只读调用）。

---

## 6 排错

| 现象 | 处理 |
|---|---|
| 页面空、任务 0 个 | 确认 `.cache/*/run.jsonl` 存在；`/api/health` 的 `tasks` 是扫描结果 |
| 点「取码」返回 `token: AADSTS...` | 该邮箱 refresh_token 失效，换 `mb` 记录或重新导号 |
| `start.ps1` 在管道里调用后不返回 | 脚本用 `Start-Process` 起了常驻子进程，父进程 stdout 被继承是 PowerShell 5.1 的老毛病；直接在普通终端窗口执行即可，或看 `.cache\console\server.out.log` 确认已就绪 |
| 端口 8801 占用 | `start.ps1 -Port 8802`，页面地址同步换 |

---

## 7 文件清单

| 文件 | 作用 |
|---|---|
| `server.cjs` | 控制台服务（静态页 + API + 扫描/汇总） |
| `public/index.html` | 单页 UI（无构建工具） |
| `mail-code.cjs` | 取码助手，复用 `tools/capture/graph-mail.cjs` 导出函数 |
| `start.ps1` | 启动/停止（幂等，记 PID） |
| `README.md` | 本文 |
