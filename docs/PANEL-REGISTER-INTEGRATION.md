# 面板接入说明：在 18741 面板里加「新建 Apple ID」

目标：把现有面板（`internal/dashboard`，端口 18741）从"只看账号"升级成"能建账号"，
**一条龙**：建号 → GSA 登录 → 2FA → 条款 → 设备登记 → 保活 → 账号出现在列表里。

浏览器走 **Roxy/Forge**（`127.0.0.1:50100`）。

---

## 一、现有面板

```
启动：bin\2-panel.cmd（内部调 run-ui.ps1）
地址：http://127.0.0.1:18741
源码：internal/dashboard/
现状：账号列表 + 会话状态 + 保活；**没有新建入口**
```

---

## 二、要加的接口

### 2.1 发起一次建号

```
POST /api/register
{
  "email": "xxx@hotmail.com",       // 必填
  "password": "<含特殊字符>",        // 必填；缺失或过弱直接 400
  "phone": "7700900123",            // 号码（不含国家码）
  "phonecc": "GB",                  // 拨号国家
  "platform": "grizzly|hero",       // 接码平台
  "activation": "<id>",             // 已购买的激活 id
  "platformKey": "<key>",           // 或用环境变量，不要回显
  "egress": { "mode": "roxy|direct", "proxyPort": 10005 },
  "captchaProvider": "yes"
}
→ 202 { "jobId": "reg-20261114-153012" }
```

要点：
- **密码强度校验在前端与后端都做一次**（苹果要求含特殊字符，否则按钮一直灰）
- 平台 key 只从服务端环境变量读，不经过请求体，也不回显

### 2.2 进度

```
GET /api/register/{jobId}          → { state, stage, startedAt, elapsedMs, exitCode }
GET /api/register/{jobId}/log?tail=200
   → text/event-stream 或纯 JSON 行数组，直接转发 run.jsonl
```

`stage` 取值：`queued → creating → created → login → twofa → terms → device → keepalive → done | failed`

### 2.3 取消

```
POST /api/register/{jobId}/cancel  → 杀子进程 + 退号（调 refund.cjs）
```

---

## 三、建号子进程契约

```
node tools/capture/create-account.cjs --outdir .cache/panel/<jobId>
  --email <...> --password <...> --phone <...> --phonecc <...>
  --activation <...> --hero-key <env>
  --profile <可选，Roxy 模式下不传>
  --captcha-cmd 'python tools/captcha/solver.py --provider yes --skip-local'
  --graph-token-file <可选>  或  --await-email-code <文件>
  --auto-refund
  [--proxy http://gw.dataimpulse.com:<port> --proxy-user ... --proxy-pass ...]
```

**退出码必须按这个分支**（不要用输出文本判断）：

| 码 | 含义 | 编排动作 |
| --- | --- | --- |
| 0 | 成功 | 进入 login |
| 1 | 失败（可证伪） | 结束，退号 |
| 2 | 参数错 | 不重试，报错给前端 |
| 3 | unknown | **不重放**，先进 login 试一次确认 |
| 4 | 等码超时 | 结束，退号 |
| 5 | likely_success | 直接进 login（不要重建） |

---

## 四、Roxy/Forge 使用契约

```
create: POST /browser/create  { workspaceId, windowName, os:"Windows", proxyInfo? }
open:   POST /browser/open    { workspaceId, dirId, headless:false }
连接:   playwright.chromium.connectOverCDP(debuggerAddress)
关闭:   POST /browser/close   → 再 POST /browser/delete（分开调用）
```

四条硬规则：
1. **业务码优先**：`{code:0, success:true}` 才算成功，HTTP 200 不算
2. **代理只能 SOCKS5**，且要嵌套 `proxyInfo`；Forge 的 HTTP 代理不发 `Proxy-Authorization`，必 407
3. **`delete` 首次可能 `EPERM`**（Chrome 还没释放目录），重试第二次通常成功
4. **指纹由 Forge 生成**，我们只读不注入；`create` 不传 locale/timeZone 会随机

---

## 五、一条龙的编排

```
creating  → create-account.cjs（Roxy 建号）
created   → 等 10 分钟冷却（2FA 取码有频率上限）
login     → node tools/capture/gsa-login.cjs（只启动一次，撞 423 就停）
twofa     → 同上（gsa-login 内部完成）
terms     → bin\applelogin.exe（网页会话 + 条款）
device    → bin\trusted-device.exe（需本机推送令牌，先跑 -probe-native-push 确认）
keepalive → bin\applekeepalive.exe
done      → 账号写入面板列表（与现有 us-01/us-02 同一套存储）
```

**卡点必读**（详见 `docs/EVIDENCE-APPLE.md`）：
- `applelogin.exe` 目前只到"列出可用手机号"就退出，**"选号→发码→提交"这段还没接完**
- `423 tooManyCodesSent` 出现就停，等 15~60 分钟
- 建号后立刻登录必撞限流

---

## 六、前端要点

- 一个「新建账号」卡片：邮箱、号码、国家、平台、出口选择（直连 / Roxy + 端口）
- 一个进度条 + 折叠日志窗（读 `/log`）
- 失败时给三个按钮：`查看证据`（跳到 `.cache/panel/<jobId>/` 的截图列表）、`重试`（新 jobId）、`取消`
- **危险操作二次确认**：`重试` 会再花一个号码

---

## 七、纪律

1. 密码、平台 key、token 不进日志、不进前端响应
2. 任何"结果未知"的写操作先回读，不自动重放
3. 证据目录 `.cache/panel/<jobId>/` 保留截图与 run.jsonl，供事后复盘
4. 号码花了钱：取消/失败要走 `refund.cjs`，撞 120 秒冷却就挂账等 `refund-sweep.cjs`
