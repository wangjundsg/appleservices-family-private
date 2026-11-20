# 与原作（上游）的差异：激活/设备/家庭链路

日期：2026-11-20。仓库：`wangjundsg/appleservices-family-private`。

## 0. 结论先行

**上游已经被原作者清空，无法逐行对照。**

- 上游 `wangshen233/appleservices-family-private`（frozen: fork=false）当前 HEAD 是 `b0a8404 chore: clear public repository`；
- 该提交的树是**空的**（`git ls-tree -r upstream/master` 无任何文件），且与我们没有共同祖先（无 merge-base）；
- 我们能找到的“原始代码”是本仓库最早的导入提交 `d6fb108 chore: import original project`。

`d6fb108` 的实际内容（**只有协议库**）：

```
anisette  ckks  cloudkit  escrow  gsa  icloud  keychain  octagon
appleservices.go  devices.go  flow.go
internal/{httpx,protobuf,srp,uuid}
cmd/passwords
```

**没有** 面板、没有 `trusted-device`、没有 APS/原生 ADI、没有注册管线、没有家庭组。

也就是说：**我们讨论的“激活设备 → 组建 → 一条龙”整条链路，全部是本仓库自己加出来的，不是原作者的实现。**

---

## 1. 我们相对 `d6fb108` 的规模

```
git diff --stat d6fb108..HEAD
212 files changed, 37599 insertions(+), 639 deletions(-)
```

其中 `internal/sap/unicorn`、`nativeadi/`、`tools/mac-push-check.py`、`tools/accountsetupcheck/` 等是**新增**（`+`），不是改写。

---

## 2. 激活/设备链路的来源（首个提交）

| 子系统 | 首个提交 | 说明 |
|---|---|---|
| Windows 原生 ADI | `0c0d00d` 2026-10-05 | `nativeadi/`：CoreADI 桥 + 硬件绑定 + 头稳定性测试 |
| Windows APS + 受信任设备 | `479ff3b` 2026-10-05 | `aps/`、`cmd/trusted-device`：推送接收、设备登记、验证码 |
| 本地面板 + 控制通道 | `ca55e9d` 2026-10-05 | `cmd/dashboard`、`internal/dashboard`、`internal/control` |
| 独立登录器 | `6423680` 2026-10-05 | `cmd/login`：GSA 密码登录 + 2FA |
| Mac NAC 激活探针 / 账号级推送身份 | `304eedd` 2026-10-08 | `tools/mac-push-check.py` |
| 面板内完成 iCloud 激活 | `ce59965` 2026-10-30 | `internal/dashboard/activation.go` |
| 原生美国区 iCloud+ 购买流程 | `8c3c7ac` 2026-10-29 | `internal/dashboard/mac_push.go` 等 |
| 网页登录 + 独立注册管线 | `2e4636e` 2026-11-14 | `cmd/appleweb`、`tools/apple-pipeline` |
| 纯协议注册器 | `a321620` 2026-11-15 | `tools/signup`（纯 HTTP 建号） |

上游库（`d6fb108`）提供的是：GSA 认证、anisette 客户端、iCloud/CloudKit/escrow/octagon 协议实现、SRP、protobuf。**我们所有“设备激活”能力都建立在这层之上。**

---

## 3. 对排查问题的直接含义

1. **不要在“上游差异”上找原因**——上游为空，激活链路的每一行都出自本仓库；
2. 排查激活失败，应以本仓库的提交与文档为准：
   - `docs/LOGIN-ACTIVATION-REVIEW-2026-11-18.md`（失败点：`/verify/phone/securitycode` 412、GSA `repair`）
   - `docs/ACTIVATION-EVIDENCE-2026-11-18.md`（时间线；面板链路实跑结论 `failure_reason=gsa_account_repair_required`）
   - `docs/ADI-RESEARCH-2026-11-18.md`（ADI 三次来源全部 417，账号绑定）
3. 若要与“原作者版本”对照，只能对照 `d6fb108` 这一层（协议库），它**不包含**任何设备激活逻辑，因此对照不出激活问题。

---

## 4. 复现这次对照

```powershell
git remote add upstream https://github.com/wangshen233/appleservices-family-private.git
git fetch upstream master
git log -1 --oneline upstream/master          # b0a8404 chore: clear public repository
git ls-tree -r --name-only upstream/master     # 空
git rev-list --count upstream/master..HEAD     # 203
git diff --stat d6fb108..HEAD                  # 212 files, +37599 / -639
```
