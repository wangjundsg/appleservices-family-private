# 注册机统一契约（Outlook / Apple）

> 适用范围：本仓库 `tools/capture/*` 下所有走「Roxy/Forge 指纹浏览器 + 浏览器自动化」的注册流程。
> 版本：2026-11-14 · 基线实测见 [`AUTOMATION-BASELINE.md`](./AUTOMATION-BASELINE.md)
> 一句话：**一个 profile、一条会话链、四个业务码检查、两套环境表、二十条血坑。**

---

## 0 结论先行

| 项 | 结论 |
|---|---|
| 自动化客户端 | **Playwright（`playwright-core`）`connectOverCDP`**，不用 Selenium |
| 依据 | 同一 profile 上三条检测页实测暴露项完全一致（`webdriver=false`、sannysoft 0 红、注入痕迹 0）；Node 栈已在用，无驱动版本耦合。详见基线报告 |
| 会话层 | `create → open → 校验 → 作业 → close + delete`，**每一步判 `code==0 && success`**，HTTP 200 不算成功 |
| 代理 | `proxyInfo` **显式嵌套**传入；**只准 SOCKS5**（Forge 的 HTTP 代理必 407） |
| 租户常量 | `workspaceId=23340934979096576`、`projectId=23340935079759872`、API `http://127.0.0.1:50100`；token 从 `ROXY_TOKEN` 环境变量取（不入库） |

三条最关键约束（背下来）：

1. **业务码，不是 HTTP 码**：`{code:0, success:true}` 才算成。`/browser/delete` 会返回 `HTTP 200 + success:false + EPERM`。
2. **指纹必须显式钉死再校验**：`create` 不传 `locale`/`timeZone` 会拿到随机指纹（实测拿到过 `da-DK / Europe/Copenhagen`）；`open` 后必须实测 UA/locale/TZ/出口国，对不上就关掉重建，不要带着错的指纹去提交表单。
3. **代理只能 SOCKS5，且必须走 `proxyInfo` 嵌套对象**：Forge 对 `http` 协议代理不发 `Proxy-Authorization`，握手直接 407；账号密码塞在 URL 里也不生效。

---

## 1 统一会话层

### 1.1 状态机

```
create ──dirId──► open ──debuggingPort──► 校验 ──► 作业 ──► close ──► delete
   │                 │                      │                            │
   └─ 失败即终止      └─ 失败先查是否已建出    └─ 不匹配就关掉重来          └─ EPERM 允许退避重试
```

### 1.2 create

```http
POST http://127.0.0.1:50100/browser/create
token: <ROXY_TOKEN>
Content-Type: application/json

{
  "workspaceId": "23340934979096576",
  "projectId":   "23340935079759872",
  "windowName":  "ol-20261114-01",
  "os":          "Windows",
  "locale":      "zh-CN",
  "timeZone":    "Asia/Shanghai",
  "proxyInfo": {
    "moduleId": 0,
    "proxyMethod": "custom",
    "proxyCategory": "SOCKS5",
    "ipType": "IPV4",
    "protocol": "SOCKS5",
    "host": "gw.example-socks.com",
    "port": 1080,
    "proxyUserName": "USER",
    "proxyPassword": "PASS"
  }
}
```

* **直连环境（Outlook）不传 `proxyInfo`**，整块省略；传 `null` 与省略等价，但不要传空对象。
* `locale` / `timeZone` 是可选项，**不传就是随机**。要什么就写什么。
* 成功回包：`data.dirId`（`data.id` 同值）、`data.fingerprint`（含 `locale`/`timeZone`）。
* `data.fingerprint` 只是"它说要给你的"，**不是实际值**；实际值只能在 `open` 之后从页面里读。

### 1.3 open

```http
POST /browser/open
{
  "workspaceId": "...",
  "dirId": "26f9...",
  "args": [],
  "forceOpen": true,
  "headless": false
}
```

回包关键字段：`data.debuggingPort`（整数）、`data.debuggerAddress`（`127.0.0.1:PORT`）、`data.ws`、`data.pid`、`data.browser`、`data.alreadyRunning`。

* 端口取值顺序：`debuggingPort` → 解析 `debuggerAddress` 末尾端口。
* `alreadyRunning=true` 说明复用了已在跑的环境——**上一轮没关干净**，先 `close` 再继续。

### 1.4 校验（作业前的准入闸）

在**作业页同源**的普通 page 里执行（别在要提交的页面里导航探测，会把 goto 撞掉）：

```js
const seen = await page.evaluate(() => ({
  ua: navigator.userAgent,
  platform: navigator.platform,
  language: navigator.language,
  languages: navigator.languages,
  timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
}));
const egress = await page.evaluate(async () =>
  (await (await fetch('http://ip-api.com/json/?fields=status,country,countryCode,timezone,query')).json()));
```

判定四件套：

| # | 判据 | Outlook（本机直连） | Apple（住宅 SOCKS5） |
|---|---|---|---|
| 1 | UA 系统标识 | `Windows NT 10.0; Win64; x64` + `navigator.platform === 'Win32'` | 同左 |
| 2 | locale | `navigator.language === 'zh-CN'`（`languages[0]` 同为 zh-CN） | `en-GB` |
| 3 | timeZone | `Asia/Shanghai` | `Europe/London` |
| 4 | 出口国 | `countryCode === 'CN'` | `countryCode` === 号码国家 |
| 5 | 号码国（仅 Apple） | — | hero 号码的 `phonecc` 与 3、4 同国 |

任一项不符：**记 `geo_mismatch` → close → delete → 重建**。不要带着错的指纹提交表单——苹果的 `Your account cannot be created at this time` 就是这么换来的（`docs/run-20261114-run2.md` §2 卡点 3）。

### 1.5 作业

* 客户端只操作**自己 new 出来的 page**；不要动 profile 的默认空白页。
* 不调用 `browser.close()` 收尾（它不杀 Chrome，但会把 Playwright 侧对象作废）；页面自己 `page.close()`。
* 证据（截图/`run.jsonl`/HTML dump）落在 `.cache/<run>/`，`run.jsonl` **每条立即 append**。

### 1.6 close + delete

```http
POST /browser/close   { workspaceId, dirId }
POST /browser/delete  { workspaceId, dirIds: [dirId] }
```

判定：

```js
ok = res.status === 200 && json.success !== false && [0, 200, '0', '200'].includes(json.code)
```

`delete` 额外看 `data.deleted` 里有没有这个 dirId，以及 `data.dirIds[].error`：

```json
{ "code":1, "success":false,
  "msg":"26f9...: EPERM, Permission denied: D:\\Workbench\\...\\browser-cache\\26f9...",
  "data": { "dirIds":[{"dirId":"26f9...","deleted":false,"error":"EPERM, ..."}], "deleted":[] } }
```

**实测**：`close` 返回 code 0 之后**立刻** `delete` 会 EPERM（Chrome 还在释放目录），等 2~5 秒重试即成功。所以——

### 1.7 失败重试规则（照抄）

| 步骤 | 次数 | 失败含义 | 处置 |
|---|---|---|---|
| `create` | **1** | 拿不到 dirId，或 `success:false` | 立即终止，**不要循环 create**（会留下脏环境、污染指纹池）；打印回包原文 |
| `open` | **1** | `cdp_unreachable` / 超时 / 无端口 | **先查是否已建出环境**：`dirId` 是否已存在、`/browser/open` 回包是否 `alreadyRunning`、`debuggingPort` 是否已监听。确认没建出来 → 终止并 `delete` 该 dirId；确认建出来了 → 复用，别新建 |
| `close` | **1** | 403/404/超时 | 记日志，**继续**执行 `delete`（close 失败不阻断收尾） |
| `delete` | **1 + 有限重试** | `EPERM`（文件系统竞态） | 唯一允许重试的步骤：≤5 次，退避 `1.5s × 次数`。业务性失败（`success:false` 且非 EPERM）不重试 |

> 铁律：**任何路径退出前都要尝试 close + delete**，包括异常和 Ctrl-C（`finally` 里做）。残留 profile 会占端口、污染 `browser-cache`，下一次 `open` 就可能 `cdp_unreachable`。

---

## 2 两份环境表

| 项 | Outlook 注册 | Apple 注册 |
|---|---|---|
| **出口** | 本机家宽直连（**不传 `proxyInfo`**） | 与号码同国的住宅（英国号 → 英国 SOCKS5） |
| **locale / 时区** | `zh-CN` / `Asia/Shanghai` | `en-GB` / `Europe/London` |
| **OS** | Windows | Windows |
| **校验** | 三要素：UA 系统标识、locale、出口国 | 同左 **+ 号码国家一致** |
| 指纹来源 | `create` 显式传 `locale`/`timeZone`，`os: "Windows"` | 同左 |
| 代理写法 | 省略 `proxyInfo` 整块 | `proxyInfo` 嵌套、`protocol: "SOCKS5"`、带 `proxyUserName`/`proxyPassword` |
| 邮箱 | 微软表单内直开地址（Route A），建完再加保护邮箱 | 不使用临时域名；用真实 QQ/Outlook 兜底（见坑 A-10） |
| 号码 | 不需要 | hero-sms，国别必须等于出口国 |
| 配额 | 一个家宽 IP 每天 2~3 个（见坑 O-7） | 每个出口 + 号码 + 指纹组合一次性 |
| 失败典型 | `Your account cannot be created at this time` = 三要素不齐 | 同左 |

> Outlook 反直觉点：**美国住宅 + 英文环境反而直接封**。中文家宽 + zh-CN + Asia/Shanghai 才是它的"正常用户"画像。

---

## 3 历史坑清单

> 格式：**现象 / 根因 / 正确做法**。每条都是真金白银烧出来的，一条别删。

### 3.1 Apple 侧（11 条）

| # | 现象 | 根因 | 正确做法 |
|---|---|---|---|
| A-1 | `Continue` 一直灰的，Playwright 报 `click-timeout`，日志看着像验证码错 | 密码不含特殊字符，苹果表单内联报 `At least one special character` 并 `disabled` 提交按钮；click 超时被误读成验证码问题，白烧 5 张图 | 生成口令强保证四类字符（大写/小写/数字/**符号** `!@#$%^&*-_=+?`）；`clickContinue` 的 catch 里必须抓 `button.disabled` + `[role=alert]/.form-message/.error` 内联文案再判（`tools/capture/create-account.cjs:620-650`） |
| A-2 | 图形验证码老是被拒，肉眼一看是 `DhCBB`，识别结果 `DHCBR` | 160×70 带噪线小图，原图直送付费识别命中率不够 | 识别前预处理：autocontrast + 锐化 + LANCZOS 放大 4 倍（`tools/captcha/solver.py --upscale`）；**关键提交走 YesCaptcha 或手工通道**，OCR 只当第二意见（`--await-captcha` 人工读图 4/5 字符全对） |
| A-3 | 同一个答案提交第二次，明明第一次已被拒，第二次直接判死 | 答案文件没删，循环里把**上一轮被拒的答案**又提交了一遍；苹果对复用答案的处理是直接拒 | 每轮结束**删答案文件**再取新图（`create-account.cjs` 手工通道路径）；同一张图连续 2 次哈希相同即中止（`CaptchaTracker`），别把同一张图连解 16 次 |
| A-4 | 短信码填完，程序抛 `detached / target closed / navigated`，被记成失败，实际号已建成 | 短信码提交后苹果会导航跳走，我们持有的 widget iframe 被销毁——句柄失效不等于业务失败 | 这种撕裂归为 **`likely_success`**，退出码 **5**（`tools/capture/account-flow.cjs:28`、`create-account.cjs:1188-1200`）；用一次真实登录去确认，别自动退号 |
| A-5 | 明明选了 `+44`，提交时变成 `+1`，表单永远不过 | 国家码控件在表单渲染后约 4~6 秒**水合**，会静默把 dial code 重置成 US | `selectOption(phoneCode)` 之后**等 7000ms 再重写一次**，然后才填号码（`create-account.cjs:936-944`）；提交前断言当前选中值 |
| A-6 | 页面底部红字 `Your account cannot be created at this time` | 出口国 / 号码国 / 浏览器指纹三者不同国（例：US 指纹 + CN 家宽 + GB 号） | 三者必须同国，且用 §1.4 的五项校验当准入闸；不齐就别提交，提交只会把号打脏 |
| A-7 | 2FA 取码返回 HTTP 423，`ec=-22981`、`tooManyCodesSent=true` | Apple 对同一账号的短信/验证码下发有频率上限，hero 每启一次 `login.exe` 就发一次码 | 每个激活最多申请 **2 次**码（`tools/capture/gsa-login.cjs` 的 `--max-code-requests`，预算落 `.cache/gsa-budget-*.json`）；撞上 423 就**等 15~60 分钟**或换号，别连打 |
| A-8 | 退号被拒：`EARLY_CANCEL_DENIED` / `OTP_RECEIVED` | hero 有最短持有期（`minActivationTime=120` 秒），且号一旦收到过短信就退不掉 | 失败先 `setStatus=8`；`EARLY_CANCEL_DENIED` **不原地死等**，挂账 `refunds-pending.json`，由 `tools/capture/refund-sweep.cjs` 冷却后统一 sweep（`create-account.cjs:180-200`） |
| A-9 | 进程被 kill，整轮证据全没了，只剩一张最后的截图 | 日志在内存里攒着最后一次性写盘 | `run.jsonl` **每条立即 `appendFileSync`**（`create-account.cjs:132`），最后一条永远是终态；被 kill 也能读到最后一步 |
| A-10 | 临时域名邮箱在苹果侧直接被拒 | 苹果对一次性/临时邮箱域名的黑名单远比微软激进 | 临时域名只当候选，**主邮箱用真实 QQ / Outlook**（实测能过）；这条和 Outlook 侧相反，别把两边的经验互抄 |
| A-11 | 用 iCloud 隐藏邮箱当 Apple ID 主邮箱，提示"此地址不可用" | Hide My Email 生成的 alias 不能作为 Apple ID 的登录名（它只能做转发目标） | 主邮箱用真实邮箱；隐藏邮箱 alias 只用于**转发接收**（`HideMyEmailClient.UpdateForwardTo` 设转发目标） |

### 3.2 Outlook 侧（8 条）

| # | 现象 | 根因 | 正确做法 |
|---|---|---|---|
| O-1 | 美国住宅 + 英文环境建 Outlook，直接封 | 微软对"境外 IP + 英文"的新号注册风控极严；本机中文环境才是它的正常画像 | **必须本机中文环境**：家宽直连（不传 `proxyInfo`）+ `locale=zh-CN` + `timeZone=Asia/Shanghai` + Windows 指纹 |
| O-2 | PX 长按（`按住`）过不去，进度条走到一半回弹 | 它是**两步式**验证：先短按 800ms → **等 800ms** → 再**静止**按住 8s；按住期间**抖动会重置进度** | 严格两步：`tap 800ms` → `gap 800ms` → `hold 8000ms`；hold 期间指针**零位移**（`tools/capture/px-hold-flow-selftest.cjs`，hold 时长钳制在 6000~8000ms） |
| O-3 | 按住了但没进度，元素定位对但 PX 不认 | 落点用了胶囊中心，PX 只认**「按住」文字节点**中心 | 定位 `text='按住'` 的文字节点中心坐标去 `mouse.move`，不要用容器的 bounding box 中心 |
| O-4 | 按住 8s 中截了图/扫了帧，进度直接回弹 | 按住期间任何额外的 CDP 动作（截图、DOM 扫描）都会打断渲染/输入节奏 | hold 期间**禁止截图、禁止扫帧、禁止 evaluate**；只在前后各取一次状态（`text_before` / `text_during`） |
| O-5 | Roxy 里配 HTTP 代理，页面全 407 | Forge 的 HTTP 代理链路不发 `Proxy-Authorization`，上游直接 407 | 代理**只用 SOCKS5**，并且**显式传嵌套 `proxyInfo`**（§1.2）；Chromium 不接受带认证的 `socks5://user:pass@`，需要时用本地无认证中继（`tools/capture/socks5-relay.cjs`） |
| O-6 | 号建完卡在"添加保护邮箱"，没有邮箱可填 | 微软新建号后会强制推进到加安全邮箱（`account.live.com/interrupt/...`），OAuth 设备码路会被这道拦死 | 保护邮箱用自有临时邮箱：`POST /admin/new_address`（header `x-admin-auth`）建地址，验证码从 `GET /api/mails?limit=5` 取；浏览器网页登录路线不受这道拦（`docs/run-20261114-run2.md` §1）。**注意**：同一新号连续填错 2 次保护邮箱，微软会把密码登录也关掉 |
| O-7 | 同一条家宽 IP 建第 4 个号开始全失败 | 微软按 IP 计新号配额，家宽约每天 2~3 个 | 一个家宽 IP **每天 2~3 个**封顶；超了换线或隔天。不要拿代理硬刷代替（会踩 O-1） |
| O-8 | 换了个代理端口，出口标称 US，实际落在 CA，指纹全对但被判 | 脏端口：`10013` / `10030`（TLS 握手失败）、`10033`（名义 US 实际 CA） | 每次开跑前用 `http://ip-api.com/json/?fields=countryCode,timezone,query` 实测出口国，别信端口名；命中脏端口直接换（`docs/run-20261114-run2.md` §5） |

### 3.3 本次基线验证新增（4 条，都是工程坑）

| # | 现象 | 根因 | 正确做法 |
|---|---|---|---|
| E-1 | `create` 出来的 profile 语言是丹麦语（`da-DK` / `Europe/Copenhagen`） | `create` 的 `locale`/`timeZone` 是可选项，不传就按服务默认随机 | 显式传 `locale` + `timeZone`；`open` 后再实测复核（§1.4），**不要信 create 回包的 `fingerprint`** |
| E-2 | `open` 偶发 `OPEN_FAILED reason=cdp_unreachable`，紧接着手工重试同一套参数 3.8s 就成功 | 上一轮 Chrome 进程还没完全退出，端口/目录仍被占 | 按 §1.7：open 只调一次；失败先确认环境是否已建出（`dirId` / `alreadyRunning` / 端口监听），已建出就复用，没建出就 close+delete 后终止 |
| E-3 | `close` 成功（code 0）后立刻 `delete` 报 EPERM | Chrome 退出与目录释放之间有竞态 | delete 允许 ≤5 次退避重试（唯一例外），见 §1.7 |
| E-4 | Selenium 挂 Roxy 的 Chrome：`unrecognized chrome option: excludeSwitches`；异步探针返回全 `None` | chromedriver 155 已移除 `excludeSwitches`；attach 模式下启动参数本来就不生效；ChromeDriver 的 Execute Script **不 await Promise** | 选 Playwright（见基线报告）；若必须用 Selenium：只设 `debugger_address`，异步脚本一律走 `execute_async_script` + 回调包装 |

---

## 4 排障速查

| 症状 | 先看 |
|---|---|
| 页面打不开 / 全 407 | `proxyInfo` 是不是 SOCKS5、是不是嵌套对象、账号密码字段名对不对 |
| `create` 成功但语言不对 | 有没有传 `locale`/`timeZone`；传了还被改就看 `open` 后实测值 |
| `open` 失败 | `alreadyRunning`、`debuggingPort` 是否已监听、上一轮是否残留 profile（`GET /` 看 `running`/`leaked`） |
| 表单提交没反应 | 提交按钮 `disabled` + 内联错误文案（A-1） |
| 苹果红字拒绝 | 出口/号码/指纹是否同国（A-6） |
| 退号失败 | `EARLY_CANCEL_DENIED` → 入 `refunds-pending.json`，跑 `refund-sweep.cjs`（A-8） |
| 进程被杀后断电 | 读 `run.jsonl` 最后一条（A-9） |

**一键复现基线**：

```powershell
node tools/detect/detect-clients.cjs --label my-baseline --locale zh-CN --tz Asia/Shanghai
python tools/detect/summarize.py .cache/detect-baseline/my-baseline
```
