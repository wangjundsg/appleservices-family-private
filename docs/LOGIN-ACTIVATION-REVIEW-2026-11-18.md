# 登录 / 2FA / 设备激活：问题汇总（交给修复方）

## 实现复核

GSA 收到 `repair` 后返回 `gsa_account_repair_required`，需要账号持有人在官方页面或受信任设备上完成提示。短信验证 POST 返回 HTTP 412 时，CLI 和流水线保留 `apple_sms_verify_precondition_failed` 并进入人工复核。代码会保留服务端提供的 `nonFTEU` 值，目前没有可以确定 412 成因的证据。

密码探测返回 409 时输出 `requires_2fa`。密码认证通过后，只有已有网页会话经服务端校验、账号和 DSID 均匹配时才输出 `authenticated`，其余输出 `account_exists`。三种回执均不推进设备激活。

设备恢复仍依赖同账号的 GSA 会话及已接受的 iCloud delegate 检查点。恢复入口在建立推送连接前读取检查点并检查绑定。以下为原始现场报告及当时提出的假设。

日期：2026-11-18。环境：Windows，Python 3.12.10，MinGW `D:\mingw64\bin\gcc.exe`，仓库 `D:\appleservices-family-private`。
本文只列**已复现**的问题与证据；所有结论都可用文中命令重跑。**没有真实扣费、没有批量建号；账号 3624475735@qq.com 是用户自有账号。**

---

## 0. 背景与边界

- 目标：把这台 Windows 登记为 `3624475735@qq.com` 的**受信任设备**（即“激活设备”），之后交给面板走登录/保活/家庭组。
- 账号状态：**用户可在浏览器正常登录**（`appleid.apple.com` 账户页可见“双重认证 · 1 个受信任电话号码”），因此账号本身可用。
- 本轮所有尝试均由本仓库二进制发起：`bin/login.exe`、`bin/appleweb.exe`、`bin/trusted-device.exe`、`bin/dashboard.exe`。

---

## 1. GSA 登录被 `repair` 拦住（P1）

**现象**：`login.exe` 无论 `-twofactor sms` 还是 `trusted-device`，都在密码阶段后立即失败。

```
Authenticating Apple ID ...
login: appleservices: login: gsa: unsupported authentication continuation "repair"
```

复现：

```powershell
$env:APPLESERVICES_APPLEID='3624475735@qq.com'; $env:APPLESERVICES_PASSWORD='<password>'
.\bin\login.exe -twofactor sms
.\bin\login.exe -twofactor trusted-device
```

**证据**：`APPLESERVICES_APPLEID`/`PASSWORD` 由 `cmd/login/main.go` 读取；错误串来自 GSA 的 continuation 分支，仓库内没有对应处理。

**相关代码**：

- `internal/appleweb/apple_auth_client.go:144-145` —— 网页链路把 `isRepairNeeded` 映射为 `icloud_account_repair_needed`（有处理）；
- GSA 链路（`gsa/`）**没有** repair 分支，只把原串抛给上层。

**期望**：recognised-repair 应给出稳定错误码（例如 `gsa_account_repair_required`）并说明“需要用户在设备/网页完成账号设置”，而不是把 Apple 内部串直接外抛。

---

## 2. 无头网页登录：验证码恒 412（P1，核心）

**现象**：`appleweb.exe -mode login` 能成功触发短信（`sms_send status=200`），但提交**任意有效验证码**都返回 412。

实测四次（全部为用户手机实际收到的码，均在发出后 15 秒内提交）：

```
step=sms_send    status=200  scnt: 93568ed5 → 94f62826
step=sms_verify  status=412  req_scnt=94f62826（正是上一步的新值）
                              req_session == next_session == cf826c05
                              sent_phone=356a192b/1 stored_phone=356a192b/1
                              valid=true locked=false
                              too_many_sent=false too_many_validated=false cooldown=false
                              service_codes=[]
```

**已排除**：scnt 过期、session 串号、号码不匹配、账号锁定、限流、验证码过期。

**唯一未排除的差异**：发码时 `non_fteu=unknown`，提交时 `non_fteu=false`。

**相关代码**：

- 发码：`internal/appleweb/apple_auth_client.go:613` `PUT /verify/phone`（允许 412 并解析 `trustedPhoneNumbers`）；
- 提交：`internal/appleweb/apple_auth_client.go:656` `POST /verify/phone/securitycode`，
  body 为 `{"phoneNumber":<原样 payload>,"securityCode":{"code":"…"},"mode":"sms"}`；
- 回退电话 payload 见 `appleAccountFallbackPhoneNumber`（`{"id":1,"nonFTEU":true}`）——说明某些账号需要 `nonFTEU=true`。

复现：

```powershell
$env:APPLESERVICES_APPLEID='3624475735@qq.com'; $env:APPLESERVICES_PASSWORD='<password>'
.\bin\appleweb.exe -mode login -session .cache\accounts\3624475735\session.json `
  -code-file .cache\accounts\3624475735\code.txt -timeout 420s
# 收到短信后把 6 位码写入 code.txt（无换行），进程会读取并提交
```

**期望**：

1. 提交前用 `stored phone payload` 的原始 `nonFTEU` 值（不要默认 false）；
2. 若 Apple 返回 412，应二次尝试 `nonFTEU:true` 并在诊断中记录两次的结果；
3. 诊断里增加“提交时 phone payload 的完整字段名集合”（值可脱敏）。

---

## 3. `-mode probe` 语义混淆（P2）

`appleweb -mode probe` 输出 `{"state":"account_exists"}`，同时诊断是 `status=409`（需要二次验证）。
即：**探测结果把“账号存在”当成“会话可用”**，容易被上层误判为已登录。

**期望**：区分 `account_exists`（存在）/ `authenticated`（会话有效）/ `requires_2fa`（需要验证）三种状态。

---

## 4. 设备激活依赖“已 provisioning 的身份”，但缺登录后的交接通路（P1）

`trusted-device.exe` 的行为：

```powershell
.\bin\trusted-device.exe -native-adi -check -adi-dll .cache\openbubbles-store\rust_lib_bluebubbles.dll -dir <dir>
# exit=1  trusted-device: a provisioned device state is required; complete the existing login first

.\bin\trusted-device.exe -native-adi -probe-native-push -adi-dll … -dir <dir>
# exit=0  Native APS token length: 32. Local COM callback verified.
#         Account trust and remote code delivery have not been verified.
```

也就是说：**原生 ADI 与 APS 链路本机可用**，但 `-check` 要求“已 provisioning”，而 provisioning 需要一次成功的账号会话——而会话恰好卡在 §1/§2。

**缺口**：仓库没有“把一次已完成的登录会话交给 trusted-device 做 provisioning/登记”的入口（`-resume-session` 只吃它自己的状态格式，不接受外部会话导入）。

**期望**：提供 `trusted-device -import-session <file>`（或等价 CLI），接受 `appleweb` 成功登录产出的会话文件，完成：
1. 用该会话 provisioning 本机 ADI（写 `adi_pb`）；
2. `-register-only` 完成设备登记并输出回执；
3. 全程不重放密码、不重发 2FA。

---

## 5. ADI 不能跨账号挪用（已实测，作为设计约束）

同一匿名请求 `GET https://appleid.apple.com/account?countryCode=USA`，只改 ADI 头：

| ADI 来源 | 头生成 | 结果 |
|---|---|---|
| 无 ADI | — | `200`（网页 HTML） |
| 公共 anisette 池 | 成功 | `417` |
| 本机原生 + 合成身份（随机 UDID/序列号/占位 token） | 成功（11 头） | `417` |
| 本机原生 + **iCloud 守护进程真 token**（属另一账号 sjywjlowe@gmail.com） | 成功（11 头） | `417` |

结论：Apple 拒绝的是**身份与账号的绑定不符**，不是头的算法。ADI provisioning 会登记到某个 DSID 名下，跨账号必然 417。

**给修复方的约束**：不要试图复用别的账号的 token/身份；激活必须“用目标账号自己的会话来 provisioning”。

（复现脚本为临时文件，逻辑见 `docs/ADI-RESEARCH-2026-11-18.md`。）

---

## 6. 面板 API 的调用约定（已踩过，供修复方参考）

- `POST /api/accounts/add` 需要 `X-App-Token: <页面内 64 位 hex>` 与 `Origin: http://127.0.0.1:<port>`；
- 凭据字段名是 **`apple_id`**（不是 `appleid`），见 `internal/dashboard/server.go:107-111`；
- 字段名写错时只返回 `invalid input`，无细节。

**期望**：错误响应带上缺失/非法字段名，避免调用方盲猜。

---

## 7. 验收标准（修复方自测用）

| 编号 | 期望结果 | 验证方式 |
|---|---|---|
| A1 | GSA repair 有稳定错误码，不泄漏 Apple 原始串 | `login.exe` 对该账号返回 `gsa_account_repair_required` |
| A2 | 网页登录能在诊断中证明“提交时 payload 与发码一致” | 诊断行新增 `phone_payload_fields=` |
| A3 | 对需要 `nonFTEU=true` 的账号，412 后自动二次尝试并成功 | 一次登录拿到会话文件 |
| A4 | `probe` 三态区分 | `{"state":"requires_2fa"}` |
| A5 | `import-session` 可用：给定合法会话，完成 provisioning + 设备登记 | 产出 `adi_pb` 与登记回执，且无第二次 2FA |

**不允许的实现方式**：复用其他账号 ADI、伪造 APS token、跳过 2FA 直接登记。
