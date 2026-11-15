# 给协议补「人的形状」：预热与节奏

2026-11-15。范围：`appleservices-family-private`，接在「多画像 + 引擎」之后。
口径：**预热与节奏接入完成。** 没有解决 Apple 风控，没有真实注册，没有伪造 FD，也没有复刻 jslog 遥测。

---

## 1 为什么做这件事

实测差距（今天两轮真实建号）：

| | 浏览器建号 | 纯协议建号 |
|---|---|---|
| 请求数 | 几十个（文档、配置、widget、多个 bundle、jslog） | 5 个 |
| 耗时 | 90~150 秒 | ~2.5 秒 |
| 落地页 cookie | 有（`aidsp`/`dslang`/`site`/`idclient`） | 无（`-skip-landing`） |
| 资源类型 | 导航 + 子资源 + 遥测 | 全是 API 调用 |

画像和引擎解决的是"TLS 像不像"，这一轮解决的是"**这次会话有没有人味**"。

---

## 2 预热（`-human`）

按真实页面顺序走一遍，全部是**公开文档与公开 CDN 资源**，不伪造任何数据：

```text
1. GET account.apple.com/account                       导航（document / navigate / site=none）
2. GET appleid.apple.com/account?countryCode=USA       导航（same-site，referer=1）
3. GET appleid.apple.com/widget/account/?...           导航（same-origin，referer=2）
4. GET <widget 里解析出的 js> × N（默认 2）            子资源（script / no-cors / cross-site，referer=3）
```

关键点：

- **导航头与子资源头分开**：导航带 `Sec-Fetch-Mode: navigate`、`Sec-Fetch-Dest: document`、`Sec-Fetch-User: ?1`、`Upgrade-Insecure-Requests: 1`、`Accept: text/html,...`；bundle 带 `Sec-Fetch-Dest: script`、`Mode: no-cors`、`Site: cross-site`，且**不带**用户手势。
- **Cookie 链**：三步各留 cookie，预热结束时统计 `warmup.cookies`。
- **不重复 Bootstrap**：预热已经拿到 `scnt`/session 时，`Bootstrap` 直接返回，不再走第二遍落地页（`TestHumanModeFreezesPacingAndWarmup` 钉住了这条）。
- 预热失败不阻断：逐步记状态，只有整个文档面都拿不到才继续按原流程跑（并由 Bootstrap 兜底）。

---

## 3 节奏（`internal/pacing`）

- 每个请求**发之前**等待，绝不在有副作用的请求之后补睡——崩溃不会留下"做了一半 + 安静"的状态。
- 时长由 `(seed, from, to)` 决定（FNV-1a），范围 `[min,max)`；同一任务续跑得到完全相同的节奏，测试里注入零时延即可跑满速。
- 默认 `1.5s ~ 9s`，`-pacing-min/-pacing-max` 可调；`seed` 由任务身份（邮箱 + 画像 ID + locale/timezone）推导，**不依赖时钟**。
- 配置随任务冻结（`state.human`）；续跑复用原 seed，不会换节奏。

覆盖的边：`start→preflight`、`preflight→captcha`、`captcha→validate`、`email→…`、`phone→…` 等每个 workflow 请求边。

---

## 4 回执

```json
"warmup": {"stairs": [...], "fetched": 4, "ok": 4, "cookies": 6},
"pacing": {"enabled": true, "minMs": 1500, "maxMs": 9000, "applied": ["start->landing=4321ms", ...]}
```

`warmup.steps` 只登记 `step/host/path/status/bytes`，**不存响应体**；回执里没有 cookie 值、scnt、验证码或代理凭据。

---

## 5 明确没做的三件事

1. **不复刻 jslog 遥测**。没有真实会话语料，编造事件就是伪造证据；需要时应当先抓一次真实会话的 jslog 结构再决定。
2. **不伪造 `fdcBrowserData` / FD**。非 Chrome 画像带 Chrome FD 仍会被直接拒绝。
3. **不提供 HTTP/2**。引擎仍只协商 `http/1.1`，回执里如实写 `h2Disabled: true`。

---

## 6 验收（全部离线）

| 项 | 证据 |
|---|---|
| 预热顺序与资源类型正确 | `TestWarmupFollowsTheBrowserPath`（landing→config→widget→asset，重复 script 只取一次） |
| 导航/子资源请求头分开 | `TestAssetRequestsLookLikeSubresources`（实现内另断言 landing 的 `navigate/document/none`） |
| 预热留下 cookie | `TestWarmupFollowsTheBrowserPath`（`warmupCookieCount ≥ 2`） |
| 节奏确定性、上下界、可取消、可禁用 | `internal/pacing` 四个测试 |
| 节奏冻结与续跑不变、预热不重跑 | `TestHumanModeFreezesPacingAndWarmup`（seed 与 `warmup.fetched` 两次一致） |
| 预热后不重复引导 | 同上（fixture 的 "original session was bootstrapped again" 断言） |
| 回执不含秘密 | `invokeSignup` 既有 secret 断言 + 回执只登记元数据 |

```powershell
go build ./...
go vet ./internal/pacing ./internal/fingerprint ./internal/engine ./tools/signup
go test ./internal/pacing ./internal/fingerprint ./internal/engine ./tools/signup -count=1
```

---

## 7 怎么跑

```powershell
# 人味模式：预热 + 节奏（推荐与画像模式一起用）
.\bin\signup.exe -email a@b.c -password '...' -phone 4155550134 -phonecc US `
  -stage all -human `
  -profile-mode new -profile auto -egress-country US -egress-source verified-egress `
  -pacing-min 1500ms -pacing-max 9000ms -warmup-assets 2 `
  -proxy 'socks5h://user:pass@ip:port' -state .cache\signup\task.json -json

# 关掉节奏（排障/离线测试）
.\bin\signup.exe ... -human -pacing-min 0s -pacing-max 1ms -warmup-assets 0
```

---

## 8 与风控的关系（口径）

本轮把「画像 + 引擎 + 预热 + 节奏」四件事凑齐了，但**没有任何证据表明它能让 Apple 改变判定**。`-34607001` 仍未解释；下一步只有在真实建号里跑一次「四件套全开」才知道有没有用，且失败也不能反推"协议一定不行"或"一定行"。
