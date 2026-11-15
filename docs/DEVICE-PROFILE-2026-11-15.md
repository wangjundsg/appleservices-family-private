# 设备档案：让协议会话「有历史」

2026-11-15。范围：`appleservices-family-private`。
本轮只解决一件事：协议会话此前每次都是全新 cookie 罐、全新画像，**与无痕窗口同构**。现在改为按设备持久化。

---

## 1 依据：一次干净的对照

同一台机器、同一网络、同一个美国号码、同一个邮箱（`2897667304@qq.com`）：

| 浏览器 | 结果 |
|---|---|
| **Edge 普通窗口**（有历史、有 cookie、有扩展） | ✅ 通过创建，进入「验证电子邮件地址」页 |
| **Chrome 无痕窗口**（零历史、零 cookie、零扩展） | ❌ 被拒 |

差异不在 UA、不在 IP，在**浏览器是否「被用过」**。而协议此前的每一轮都是新的 cookie 罐 + 新的画像 —— 结构上就是无痕。

---

## 2 实现（`tools/signup/device.go`）

一个设备档案（`.cache/devices/<id>.json`，0600）保存：

| 字段 | 用途 |
|---|---|
| `cookies` | 沿用会话已有的 Set-Cookie 日志（域/路径/过期），**跨任务恢复** |
| `profile` | 冻结的画像；同一设备永远同一 UA/Client Hints/TLS hello |
| `createdAt` / `lastUsedAt` / `visits` | 设备年龄与访问次数（回执里报 `ageDays`/`visits`） |
| `history` | 预热走过的路径（上限 64），说明这台设备看过什么 |
| `tasks` | 这台设备建过的账号（上限 64），只记邮箱 |

加载顺序（决定了信号强度）：

```text
读设备档案
  → 恢复 cookie 到会话（首个请求之前）
  → 复用设备画像（不重新抽样）
  → 预热（落地页/配置/widget/静态资源）
  → 遥测回放
  → 业务阶段
  → 回写：visits+1、history、tasks、最新 cookie、画像
```

规则：

- 设备文件**损坏或 id 不符**直接拒绝（`device_state_invalid`），不会悄悄换一个空白设备——那正好丢掉我们要保留的信号。
- 设备画像一旦冻结，后续任务只校验不重选；换设备才换画像。
- 设备 cookie 只回放不新建；恢复失败报 `device_cookie_restore_failed`。

---

## 3 用法

```powershell
# 第一次：只做只读预检，把设备「养」起来（不买号、不建号、零成本）
.\bin\signup.exe -email a@b.c -stage preflight -device shao-xing-home `
  -profile-mode new -profile auto -egress-country CN -egress-source verified-egress `
  -human -telemetry .cache\jslog\capture-1.json -json

# 隔一段时间再跑一次，visits/ageDays 累积
# 正式建号：同一 -device
.\bin\signup.exe -email a@b.c -password '...' -phone <national> -phonecc US -stage all `
  -device shao-xing-home -profile-mode new -profile auto `
  -egress-country CN -egress-source verified-egress -human -telemetry .cache\jslog\capture-1.json `
  -captcha-key $env:YESCAPTCHA_KEY -state .cache\signup\task.json -json
```

回执新增：

```json
"device": {"id":"shao-xing-home","ageDays":0,"visits":3,"history":9,
           "cookies":6,"cookiesLoaded":6,"profile":"fp_...","profileReused":true,
           "distinctTasks":1}
```

`cookiesLoaded` 是本次从设备恢复的 cookie 数，`profileReused` 说明画像是否沿用。

---

## 4 验收（离线）

| 项 | 测试 |
|---|---|
| 设备存档往返、损坏/越权 id 被拒 | `TestDeviceRoundTripRejectsCorruption` |
| cookie 真的恢复到会话并可读 | `TestDeviceWarmsCookiesIntoSession` |
| 回执不含 cookie 值 | `TestDeviceReceiptHasNoCookieValues` |
| 两个任务共用同一设备：visits 递增、画像复用、id 不变 | `TestDeviceStateSurvivesAcrossCLIRuns` |

```powershell
go test ./tools/signup -run Device -count=1
```

---

## 5 边界

- 设备的「历史」目前只有：cookie、访问次数、路径记录、画像。**没有** localStorage / IndexedDB / 扩展 / 真实浏览历史——协议层没有这些设施，也不能凭空声称有。
- 设备老化是**真实时间**的函数：`ageDays` 从第一次使用算起，不能伪造。
- 仍然不承诺能过 Apple 的风控；它只是把「全新壳」这个最明显的信号去掉，剩下的交给号码与邮箱质量。
