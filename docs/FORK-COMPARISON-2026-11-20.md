# 与同类 fork 的对照：激活链路差在哪

日期：2026-11-20。对比对象：`IT-NuanxinPro/appleservices-family-private`（HEAD `9d69153`，2026-11-06，283 文件）。
参照：本仓库 HEAD。上游原始仓库已被原作者清空（见 `docs/UPSTREAM-DIFF-2026-11-20.md`），因此只能用**仍在维护的 fork** 做对照。

---

## 1. 结论：核心链路几乎一样，差在“登录续接”和外围能力

激活链路的**主干文件两边一致**（行数相同，内容无差异）：

| 文件 | 对方行数 | 我方行数 | 结论 |
|---|---|---|---|
| `internal/dashboard/activation.go` | 224 | 224 | **一致** |
| `internal/dashboard/mac_push.go` | 91 | 91 | **一致** |
| `nativeadi/client.go` | 316 | 316 | **一致** |
| `tools/mac-push-check.py` | 142 | 142 | **一致** |
| `aps/client.go` | 232 | 232 | **一致** |

唯一有差异的主干文件是 `cmd/trusted-device/main.go`（对方 1590 行 / 我方 1556 行）。差异方向：

- **我方多了** `gsa.ErrAccountRepairRequired` 的稳定错误码处理（提交 `9e67d91`）；
- **对方多了** 我们完全没有的能力（见下）。

---

## 2. 对方有、我们没有的 42 个文件（按用途分组）

| 用途 | 文件 | 对我们的价值 |
|---|---|---|
| **登录续接（关键）** | `cmd/trusted-device/login_continuation.go` | `continuingLogin` 接口 + `completeICloudLogin` + `awaitICloudActivation`：**用续接令牌继续登录**，不再走密码 |
| 凭据保险库 | `internal/credentialvault/*`、`cmd/credential-import/main.go` | 加密保存账号凭据，供续接使用 |
| 家庭组增强 | `cmd/trusted-device/family_add_credential.go`、`internal/dashboard/family_credential.go`、`family_roster.go` | 家庭成员凭据与名单管理 |
| 主邮箱 | `internal/dashboard/primary_mail.go`、`cmd/primary-mail-native/main.go` | iCloud 主邮箱开关 |
| 其它 | `cmd/v2gateway/*`、`internal/dashboard/web/*` | v2 网关、前端测试 |

---

## 3. 为什么这解释了我们的激活失败

我们的失败模式是：

```
GSA 登录 → gsa_account_repair_required
```

也就是说 **密码登录被 Apple 要求“先修复账号”**，流程停在 `signing_in`，走不到设备登记。

对方的方案不是硬闯密码登录，而是：

```
已有会话/凭据 → 续接登录（continuation）→ 继续激活/设备登记
```

`login_continuation.go` 的两个入口正是为此存在：

```go
type continuingLogin interface { ... }
func completeICloudLogin(ctx, login continuingLogin, store acceptedSessionLoader, commands, status, dir, wait) (*appleservices.Client, error)
func awaitICloudActivation(ctx, login continuingLogin, store, commands, tick, status, dir) (*appleservices.Client, error)
```

**含义**：他们并不要求每次都重新密码登录；只要手里有一个可续接的会话状态，就能跳过 `repair` 这一步继续做设备登记。

---

## 4. 建议的移植顺序（最小改动）

1. **先移植 `cmd/trusted-device/login_continuation.go`**（约 200 行）与它依赖的 `acceptedSessionLoader`；
2. 在 `cmd/trusted-device/main.go` 的登录失败分支里，遇到 `gsa_account_repair_required` 时**先尝试续接**，只有续接也失败才报 repair；
3. 续接所需的会话来源，优先用本仓库已有的：
   - `state/accounts/us-02/session.json`（11-13 的已接受会话，注意账号绑定）；
   - 或 `appleweb` 成功登录后落盘的会话（当前因 412 拿不到）；
4. 凭据保险库（`credentialvault`）属于增强项，可以后做。

**验收**：同一账号，密码登录报 repair 时，续接路径能把流程推进到 `device registration`（`runtime.json` 出现 `state=running`/`device_count>0`），且不重复发 2FA。

---

## 5. 复现这次对照

```powershell
git remote add fork-it https://github.com/IT-NuanxinPro/appleservices-family-private.git
git fetch --depth 1 fork-it master
git diff --numstat fork-it/master:cmd/trusted-device/main.go HEAD:cmd/trusted-device/main.go
git ls-tree -r --name-only fork-it/master | Select-String '^(cmd|internal)/'
```

（另一个仍在的 fork：`17871935087h-spec/appleservices-family-private`，HEAD `2737907`，277 文件，能力集与上面接近。）
