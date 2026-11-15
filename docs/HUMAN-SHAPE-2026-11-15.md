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

## 2.1 旅程前缀：为什么不是"多逛几个网站"

Apple 只能观察**自己域名的流量**，所以去逛 Google/知乎对它的判断没有任何帮助。真正异常的是另一件事：**这台浏览器的第一个、也是唯一的 Apple 接触点就是注册页**——普通顾客不会这样。

`-journey`（默认开）在预热前加三步，并且让落地页带上**点击来源**：

```text
GET www.apple.com/                        （首次导航，site=none）
GET www.apple.com/shop/buy-iphone         （同站，referer=上一步）
GET support.apple.com/                    （跨站，referer=上一步）
GET account.apple.com/account             （referer=support.apple.com，模拟点击进入）
→ 之后才是配置 / widget / 静态资源
```

实测（2026-11-15 16:48）：设备历史里出现 `/`、`/shop/buy-iphone`、`/`，设备 cookie 里新增了 **www.apple.com** 域的 cookie。

离线测试：`TestWarmupStartsWithAnAppleJourney`（顺序 + referer + 关闭时回退）。

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

1. **不复刻 jslog 遥测——已改为「回放真捕获」**（见 §5.1）。没有语料时不发；有语料时逐字节回放，不新增事件。
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

---

## 5.1 jslog 遥测回放（2026-11-15 补）

### 抓的是什么

`tools/capture/jslog-harvest.cjs` 打开一次创建页（**只加载，不填表、不提交**），把 widget 发往
`POST /jslog` 与 `POST /v2/jslogs` 的完整信封抓下来：方法、路径、query、content-type、关键头、请求体。

一次真实抓包（2026-11-15 05:52Z）得到 4 个信封 / 2477 字节，两类结构：

```json
POST /jslog        {"message":"account","title":"ROUTE CHANGED","type":"INFO",
                    "messageMap":{"ACTION":"FE_INFO"},"details":"{}"}
POST /v2/jslogs    [{"eventId":"<uuid>","timestamp":1794721932856,"eventType":"view",
                     "componentName":"account","action":"view","metadata":{"path":"account"}}]
```

抓包落在 `.cache/jslog/capture-*.json`（0600，不入库）。

### 回放规则（`-telemetry <capture>`）

- **不新增事件**：只发抓到的信封；数组类（`/v2/jslogs`）保留每个事件的字段与顺序。
- **重新定时间**：`timestamp` 平移到本次会话，按 250ms 步进；`eventId` 重新生成（新会话本来就会发新 id）。
- **单对象类**（`/jslog`）原样回放，它本身不带时间戳。
- **每任务只发一次**：`state.telemetrySent` 记录，续跑不重发；回执只写来源、信封数、事件数、字节数，不写 body。
- **失败不阻断**：遥测是附加信号，500 只记录在 `telemetry.failures`，不影响注册状态机。
- 抓包里没有 body 时**直接报错**（`telemetry_capture_invalid`），不会静默跳过。

### 证据

| 项 | 测试 |
|---|---|
| 无 body 的抓包被拒 | `TestTelemetryRefusesCaptureWithoutBodies` |
| 未知 schema 被拒 | `TestTelemetryRejectsUnknownSchema` |
| 字段不变、id 重生成、时间重定 | `TestTelemetryRebaseKeepsShapeAndRestamps` |
| 打到抓到的路径、content-type 一致 | `TestTelemetryReplayHitsTheCapturedPaths` |
| 端点失败不阻断 | `TestTelemetryFailureDoesNotAbort` |
| 回执不含 body | `TestTelemetrySummaryHasNoBodies` |

### 它仍然不解决的事

遥测只是「不像纯机器人」的一个补丁。Apple 的判定是否看这个，未知；`-34607001` 仍未解释。