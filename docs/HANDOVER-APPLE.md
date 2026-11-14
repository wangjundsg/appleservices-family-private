# 交接文档：Apple 独立产线

2026-11-14 用户最终确认：**本轮只完善 Apple 产线，不安装、不接入 gpt-free-service。**
产线独立负责注册、登录、条款、设备登记、保活与状态恢复。将来工作台如需使用，只通过接口或文件获取结果。
家庭组以后新建，本轮暂不处理。

**当前入口、参数、恢复规则和审查结论见 [APPLE-PIPELINE.md](./APPLE-PIPELINE.md)。**
下方保留早期交接背景；凡“工作台负责落库/调度”、非零退出统一退号、退出统一删浏览器等旧描述，与新契约冲突时以新契约为准。

---

## 一、这条链现在有什么

### 1.1 四个可执行件（都在 `D:\appleservices-family-private`）

| 阶段 | 程序 | 类型 | 产出 |
| --- | --- | --- | --- |
| 建号 | `node tools/capture/create-account.cjs` | 浏览器（Roxy 或 Playwright） | `run.jsonl`、截图、账号可用性 |
| GSA 登录 + 2FA | `bin/login.exe -checkpoint`（`cmd/login`） | 协议 | `session.json` + `icloud-accepted.json` |
| 网页会话 + 条款 | `bin/appleweb.exe`（`cmd/appleweb`） | 协议 | `icloud-web-session.json`（cookie 集，含 `can_create_hme`） |
| 保活 | `bin/appleweb.exe -mode keepalive` | 协议 | 会话有效/失效，成功后保存轮换 Cookie |

三者共用账号目录约定：`state/accounts/<id>/` 或任意 `--dir`。

### 1.2 取码能力

| 模块 | 适用 | 说明 |
| --- | --- | --- |
| `tools/capture/graph-mail.cjs` | 有 refresh_token 的邮箱 | Graph API，支持多语言主题 |
| `tools/capture/outlook-web-mail.cjs` | 无 token 的自建邮箱 | 复用持久化 Chrome profile 读网页收件箱 |
| 临时邮箱 API | abc-temp 那套 | `GET /admin/mails`，header `x-admin-auth` |

**邮件码是自动的；图形码是手工或 YesCaptcha。**

---

## 二、建号脚本的参数契约（`create-account.cjs`）

```
--email <地址>              必填
--password <口令>           必填，**必须含特殊字符**（否则 Continue 灰着）
--phone <号码去国家码> --phonecc <GB|US|...>
--activation <接码平台 id> --hero-key <key>
--proxy / --proxy-user / --proxy-pass     可选
--profile <profile.json>    指纹（Roxy 模式下可省略，由 Forge 决定）
--captcha-cmd 'python tools/captcha/solver.py --provider yes --skip-local'
--graph-token-file <json>   自动取邮件码
--await-email-code <file>   手动取邮件码（与上面二选一）
--await-captcha <file>      手工图形码通道
--auto-refund               失败自动退号（撞 120s 冷却会挂账）
```

**退出码（调度必须按这个判）**

| 码 | 含义 |
| --- | --- |
| 0 | 成功（有明确终态证据） |
| 1 | failed（可证明没发生） |
| 2 | usage（参数错，未启动） |
| 3 | unknown（提交可能已生效，**不要重放**） |
| 4 | waiting_input（码没到，账号仍在中途） |
| 5 | likely_success（提交后 iframe 被销毁，需用登录确认） |

---

## 三、工作台侧需要提供/承接的四件事

### 3.1 邮箱供给
- 来源：自建 Outlook（面板 A 产出 `地址+密码+profile目录`）或购买
- 要求：**能用 hotmail.com / qq.com；不要用 outlook.com**（苹果对它硬拒）
- 输出给建号脚本：地址 + 取码方式（graph token / webmail profile / 临时邮箱）

### 3.2 号码供给
- 灰熊 / hero 等，接口形如 `getNumber` / `getStatus` / `setStatus=8`
- **退号有 120 秒冷却**，失败要先挂账再 sweep
- 号码 20 分钟有效（灰熊），流程要一气呵成

### 3.3 Roxy 会话
- Forge `127.0.0.1:50100`：`/browser/create`（带 `proxyInfo`，**只能 SOCKS5**）→ `/browser/open` → `connectOverCDP`
- 校验：`code==0 && success`，HTTP 200 不代表成功
- 关闭与删除分开；删除要给 Chrome 时间释放目录（首次可能 `EPERM`，重试第二次成功）
- **指纹由 Forge 生成**，我们只记录，不注入

### 3.4 落库与续跑
- 账号状态、阶段、尝试次数、下次可跑时间
- 证据：`run.jsonl`（逐条 append）+ 截图 + `fail-<原因>-<时间>.png`
- 凭据单独存放，不进任务表

---

## 四、Apple 侧的 9 条硬规则（全部实测踩出来）

1. **密码必须含特殊字符**，否则提交按钮一直灰着，日志会伪装成 `click-timeout`
2. **图形码一张图只有一次机会**；被拒后页面自己换图，**绝不点"新验证码"**
3. 连续两次图形码被拒 → 紧接着常出现 `Your account cannot be created at this time`（硬拒），所以图形码质量直接决定成败
4. **短信码提交后 iframe 被销毁是成功**（`likely_success`），不是失败——要用登录确认，不要重放
5. 手机国家码会被控件**水合重置**（4~6 秒后弹回 US），填完必须读回核对
6. 2FA 取码有频率上限（`tooManyCodesSent` / HTTP 423）：**每个激活最多请求 2 次**，撞上就等 15~60 分钟
7. 表单国家字段、号码国家、出口国家三者**不一致**是最可疑的硬拒来源（尚未定论，但优先级最高）
8. 邮箱域名差异极大：`qq.com` ✅、`hotmail.com` ✅、`outlook.com` ❌、`icloud.com` ❌（不能当 Apple ID）
9. 成功判定只认**明确证据**：终态页、或登录能过；其余一律 `unknown` 且不重放

---

## 五、原交接时的卡点（历史记录，当前实现见顶部链接）

| # | 问题 | 现状 |
| --- | --- | --- |
| 1 | 建号成功率 | 历史上成功过数次（QQ 邮箱 + 英国号 + 住宅出口），但最近连续硬拒，原因未定 |
| 2 | 2FA 完成后拿会话 | `applelogin.exe` 在 2FA 环节会列出可用手机号后退出，**"选号→发码→提交"这段没接完** |
| 3 | 设备登记 | `trusted-device` 需要本机推送令牌（已能取到 32 字节），但 2FA 未通所以未验证 |
| 4 | 条款确认 | 逻辑已有，缺真实会话验证 |
| 5 | 保活 | 有程序，需会话存在才有意义 |

---

## 六、建议的接入方式（工作台视角）

```
任务表一行 = 一个 Apple 账号
  stage: create → login → terms → device → keepalive
  每阶段独立重试，失败带上限与回退

建号：调用 create-account.cjs（Roxy 模式），按退出码推进
  exit 5 → 直接进 login（不要重建）
  exit 4 → 等码，不重放
  exit 3 → 先查账号是否已存在，再决定

登录：调用 login.exe / applelogin.exe，2FA 码从接码平台取
  ★ 每个激活最多 2 次取码，撞 423 立刻停
```

---

## 七、纪律

```
· 密码 / token / cookie / 验证码不进日志与 git
· .cache/ 与 state/ 不入库
· 号码、图形码都要记花费，便于核算单号成本
· 任何"结果未知"的写操作，先回读，不重放
```
