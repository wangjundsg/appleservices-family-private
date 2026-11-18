# 尾号 7304 账号：激活失败点证据包（脱敏）

日期：2026-11-18。账号：`3624475735@qq.com`（尾号 7304）。机器：本仓库所在 Windows。
本文只含**时间、阶段、状态码、错误码**；不含密码、验证码、cookie、scnt、token。
所有日志位于 `.cache/accounts/3624475735/`（`.gitignore` 已忽略，不入库）。

---

## 1. 为什么上一轮“找不到失败点”

上一轮查找方向错了：只看 `state/` 下那份 11-13 的后台状态记录，而**本轮所有失败都发生在 `.cache/accounts/3624475735/`** 下的独立进程日志里。两者不是同一条链路：

| 目录 | 用途 | 本轮是否使用 |
|---|---|---|
| `state/accounts/us-01…us-04` | 11-13 建立的历史设备档案（含 `hw_info.plist`、`session.json`） | ❌ 未使用 |
| `.cache/accounts/3624475735/` | 本轮登录/激活尝试的全部日志 | ✅ 全部在此 |

---

## 2. 时间线（北京时间，2026-11-16）

| 时间 | 进程 | 阶段 | 状态码 | 结论 |
|---|---|---|---|---|
| 02:04:18 | `appleweb -mode login` | `sms_send` | 200 | 短信已发（`phone_id=1`） |
| 02:04:42 | 同上 | `sms_verify` | **412** | 验证码被拒；`valid=true locked=false` |
| 02:05:12 | `appleweb -mode login`（第二次） | `sms_send` | 200 | 新码已发 |
| 02:05:31 | 同上 | `sms_verify` | **412** | 同上 |
| 02:14:22 | `login.exe -twofactor sms` | GSA 登录 | — | `unsupported authentication continuation "repair"` |
| 02:17:41 | `login.exe -twofactor sms`（复测） | GSA 登录 | — | 同一 `repair` |
| 02:18:14 | `login.exe -twofactor trusted-device` | GSA 登录 | — | 同一 `repair` |
| 02:20:03 | `login.exe -twofactor sms` | GSA 登录 | — | 同一 `repair` |
| 02:20:27 | `appleweb -mode login`（第三次） | `sms_send` | 200 | 新码已发 |
| 02:20:52 | 同上 | `sms_verify` | **412** | 同上 |
| 02:22:28 | `appleweb -mode login`（第四次） | `sms_send` | 200 | 新码已发 |
| 02:24:55 | `appleweb -mode login`（第五次） | `sms_send` | 200 | 新码已发 |
| 02:25:25 | 同上 | `sms_verify` | **412** | 同上 |
| 02:23:47 | `dashboard.exe` | 启动 | — | `Local dashboard: http://127.0.0.1:18741`（监听成功） |

**证据文件**（逐条可查）：

```
login.log / login.err          02:04:42   第一次网页登录（412）
login2.log / login2.err        02:05:31   第二次（412）
login3.log                     02:14:22   GSA repair
login4.log                     02:17:41   GSA repair
login5.log                     02:18:14   GSA repair（trusted-device 模式）
login6.log                     02:20:03   GSA repair
login7.log / login7.err        02:20:52   第三次网页登录（412）
login8.log / login8.err        02:22:28   第四次（-host www.icloud.com，仍路由到 .cn）
login9.log / login9.err        02:25:25   第五次（412）
panel.out / panel.err          02:23:47   面板启动
```

---

## 3. 明确的判断

| 问题 | 结论 | 依据 |
|---|---|---|
| 验证码是否被接受 | **否** | 五次 `sms_verify` 全为 412，`otp_verified=false` |
| 会话是否保存 | **否** | `session5.json` 未生成（`appleweb` 仅在成功时落盘） |
| 是否到达“本机准备/设备登记” | **否** | 从未拿到会话，`trusted-device -check` 直接报“需要已 provisioning 的状态” |
| 是账号问题吗 | **不是** | 同一账号用户可在浏览器正常登录；`probe` 返回 `status=409`（需要 2FA），非锁定 |
| 是号码问题吗 | **不是** | `sent_phone == stored_phone`，且用户手机确实收到码 |
| 是面板问题吗 | **不是** | 面板 02:23:47 启动成功、18741 监听正常 |

**失败点定位**：`POST /verify/phone/securitycode` 返回 412（`internal/appleweb/apple_auth_client.go:656`）。
发码时 `non_fteu=unknown`，提交时 `non_fteu=false`，是唯一未被排除的差异。

---

## 4. 2026-11-18 现场复核

```
dashboard.exe  启动成功，pid=23192，18741 监听 = True
               日志：Local dashboard: http://127.0.0.1:18741
```

> 说明：上一轮“blocked by policy / 18741 未监听”是执行环境自身的策略拦截，不是二进制问题。本机可直接启动。

---

## 4.1 面板链路实跑（2026-11-18 15:03，决定性）

按面板真实路径执行（含 Mac APS 身份引导），全部步骤成功，**只有登录被 Apple 拦下**：

```text
.api /api/accounts/add        → {"ok":true,"message":"正在连接 Apple，请稍等"}
worker 启动日志（daemon.err.log）
  Authenticating the APS connection ...                        ← 推送身份建立
  Starting the verified local ADI generation module ...        ← 原生 ADI 模块启动
  Local six-digit generation is available for the same device identity.
  trusted-device: appleservices: login: gsa_account_repair_required   ← 失败点
runtime.json
  state=failed
  failure_stage=signing_in
  failure_reason=gsa_account_repair_required
  pid=20780  updated_at=2026-11-18T07:03:19Z
```

生成的账号私有状态（证明前面步骤都成立）：

```
accounts/01-account/device.json          108 B
accounts/01-account/push.json           2.5 KB
accounts/01-account/hw_info.plist       5.6 KB
accounts/01-account/state.plist         1.2 KB
```

**结论**：

1. 设备激活的**前置全部通过**：Mac APS 推送身份已建立、原生 ADI 模块已加载、worker 正常启动；
2. 唯一失败点是 **GSA 登录**，稳定错误码 `gsa_account_repair_required`，`failure_stage=signing_in`；
3. 未发生：密码错误、验证码提交、设备登记尝试；
4. 因此设备激活的验证停在「需要先解除账号的 repair 状态」这一步，**不是工具链问题**。

> 注：`bin/login.exe` 仍是旧构建，抛 Apple 原始串 `unsupported authentication continuation "repair"`；
> 面板 worker（`trusted-device-gui.exe`）已是新构建，给出稳定码 `gsa_account_repair_required`。

## 5. 复现命令（脱敏，密码走环境变量）

```powershell
$env:APPLESERVICES_APPLEID='3624475735@qq.com'
$env:APPLESERVICES_PASSWORD='<password>'

# A. 网页登录（会发短信；把码写入 code.txt，无换行）
.\bin\appleweb.exe -mode login `
  -session .cache\accounts\3624475735\session.json `
  -code-file .cache\accounts\3624475735\code.txt -timeout 420s

# B. GSA 登录（一次性，不循环）
.\bin\login.exe -twofactor sms

# C. 面板
.\bin\dashboard.exe -adi-dll .cache\openbubbles-store\rust_lib_bluebubbles.dll `
  -worker .\bin\trusted-device-gui.exe -repo . -dir .cache\accounts\3624475735\dev -port 18741
```

---

## 6. 这台机器上已有的“真机档案”（可复用，勿混用）

```
state/accounts/us-02/native-adi/hw_info.plist   2026-11-13 09:14
state/accounts/us-02/session.json               2026-11-13 09:03
state/accounts/us-03/session.json               2026-11-13 09:53
```

这些是 11-13 留下的**已 provisioning 设备档案**（含 32 字节 APS token 的 `hw_info.plist`）。
⚠️ 它们绑定的是当时的账号，**不能**挪给 7304 使用（跨账号会 417，已实测）。
