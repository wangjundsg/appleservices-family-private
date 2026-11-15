# 复核包：协议注册执行器（画像 / 引擎 / 预热 / 节奏 / 遥测 / 设备）

2026-11-15。给复核者的单页索引：改了什么、凭什么、怎么验、哪些结论**不**成立。
范围：`appleservices-family-private`。全部离线可验；真实 Apple 请求只发生在人工触发的实验里。

---

## 0 复审修复记录（针对 `0e35f50` 的 7 条意见）

| # | 意见 | 处理 | 证据 |
|---|---|---|---|
| 1 | P1 实际协商 h2，却按 HTTP/1.1 工作 | `HelloCustom` + 重写预设 spec 的 ALPN 为 `http/1.1`；握手后断言 `NegotiatedProtocol == http/1.1`，否则失败 | `TestEngineOffersOnlyHTTP11`（ClientHello 只报 http/1.1，服务端协商 http/1.1） |
| 2 | P1 省略参数续跑丢画像、回执仍写 utls | 模式优先取 `state.profileMode`；有 `state.profile` 时强制 new 并校验；回执的 `transport` 改为读**实际安装的传输**（`session.engineApplied`） | `TestResumeKeepsProfileModeFromState`（第二次不带任何画像参数，仍是 firefox、同一 id、transport=utls） |
| 3 | P1 设备 cookie 覆盖任务续跑 cookie | 设备 cookie 只用于**全新任务**（`deviceSeedsTask`）；续跑以任务快照为准 | `TestDeviceOnlySeedsFreshTask` |
| 4 | P1 画像切换后 Chrome FD 校验失效 | `applyToSession` 重新解析 FD，要求 `U == profile.UserAgent`、`L == profile.Locale`，否则拒绝 | `TestProfileRejectsForeignFD` |
| 5 | P1 遥测可绕过 intent 打业务端点 | 捕获文件**白名单**：只允许 `POST /jslog`、`POST /v2/jslogs`，其他端点整份拒绝；发送前先落 `telemetrySent` 意图（at-most-once），失败只记录不重试 | `TestTelemetryRejectsBusinessEndpoints` |
| 6 | P2 模板版本与 TLS 预设不精确对应 | 模板改为**按版本钉死预设**：Chrome 131→`HelloChrome_131`、133→`HelloChrome_133`、Firefox 120、Safari 16.0、iOS 14；不再使用 `Auto` 别名 | `internal/engine` 的 hello 表 + `TestCapabilitiesCoverFourFamilies` |
| 7 | P2 「只改已有字段」不成立 | 只重写**已存在**的 `eventId`/`timestamp`；字段缺失就原样保留。回放定位为**形状参考**，不再声称是本次任务的行为证据 | `TestTelemetryDoesNotInventFields` |

口径更正：

- 「画像、节奏、设备三处都有版本和签名」不成立。实际是：**画像**有 schema+内容签名；**设备**有 schema 版本，本轮补了设备级锁（`withLock`，30s 陈旧锁可回收）；**节奏**只有配置字段，没有版本与签名。
- 「Edge 普通窗口通过、Chrome 无痕失败」这条对照**同时改变了浏览器与配置状态**，只能作为线索，不足以断言浏览器历史是决定性因素；而且进入邮箱验证页也不等于建号成功。原文已按此降级。

## 0.000 事实更正：`used: true` 不等于账号存在

2026-11-15 实测：`2897667304@qq.com` 在**邮箱验证码从未提交**的情况下，`POST /account/validation/appleid` 返回 `used: true`。

结论：`used` 有两个含义 —— **账号已存在**，或**地址正被一次未完成的注册占用**（reservation/hold）。所以：

- `verify` 阶段先检查 `created + finalize` 回执，再读 `used`；**单独的 `used=true` 永远不会变成 `accountExists`**（`TestUsedAddressWithoutReceiptIsNotOurAccount`）；
- 预检失败文案是 `mailbox_not_confirmed_available`，只陈述"未确认可用"，不声称账号存在；
- 一个未完成的注册会让地址在一段时间内不可复用，具体释放时间未知，不能按"已建号"处理。

## 0.00 第三轮复审修复（针对 `83d211e` 的 2 条）

| # | 意见 | 处理 | 证据 |
|---|---|---|---|
| 1 | P2 精确版本校验只在新建时生效，恢复旧档案仍被放行 | `fingerprint.Validate` 加入 `HelloVersions` 比对：hello 声称的版本必须等于 `profile.BrowserVersion`。旧档案 `Safari 15.3 + HelloSafari_16_0` 现在续跑即 `profile_blocked` | `TestRestoredProfileIsRevalidatedForVersion`；复审者用例 `TestReviewExistingSafariProfileRechecked` 通过（`hello HelloSafari_16_0 poses as 16.0 but the profile claims 15.3`） |
| 2 | 旧预设别名（`HelloChrome_Auto` …）兼容处理不明确 | 明确为**默认拒绝**：`HelloChrome_Auto` 等别名不在能力表内 → `profile_blocked`。需要迁移时必须显式 `-migrate-profile`，且只在**存在同版本钉死预设**时才成功，否则拒绝（**不静默重抽**）；迁移结果写进任务状态并带 `migratedFrom/migratedTo` 回执 | `TestLegacyAliasIsNotAcceptedSilently`、`TestLegacyAliasMigratesOnlyOnRequest`、`TestLegacyAliasMigrationRefusesVersionMismatch`；复审者用例 `TestReviewLegacyDeviceHello` 通过 |

二进制：`bin/signup.exe` 已重新构建（`go build -o .\bin\signup.exe .\tools\signup`），`go version -m` 的 `vcs.revision` 与本轮提交一致。

第三轮附带修复（养护链路自查发现）：

- **回执补上真实原因**：失败时把底层错误写进 `failure`（≤160 字符），`reason` 不再只有 `stage_unconfirmed`这一个是非题。
- **预热后不再无条件跳过 Bootstrap**：只有在预热真的拿到 `scnt + sessionID` 时才跳过；否则清空并让 Bootstrap 走自己的最小头引导（旧 cookie + 旧 scnt 会让引导 404）。
- **养护脚本的 `ok` 判定**：改为看 `receipt.ok`，并把 `mailbox_not_confirmed_available`（邮箱已被占用）视为养护成功——访问已发生，与"这个地址还能不能建号"是两件事。

第三轮口径更正：

- 之前说「visits.jsonl 4 条、下一次 15:45」不准确：通过养护脚本只跑过 2 次（14:57 visits=4、15:46 visits=5），更早的访问是直接 CLI 调用、不写脚本日志；定时器在 15:45 确实触发了（本条提醒即来自 `schedule-c6ba89d5…`），但**定时器的持续性仍未验证**，下一轮会贴时间戳。
- 设备档案里的旧画像（chrome 133 + `HelloChrome_Auto`）在新版下默认阻断；本轮已用 `-migrate-profile` 显式迁移。

## 0.0 第二轮复审修复（针对 `b41c911` 的 3 条 + 夹具与文档意见）

| # | 意见 | 处理 | 证据 |
|---|---|---|---|
| 1 | P1 已提交任务画像丢失后，设备入口仍自动补回 | 设备画像只补给**无 intent** 的任务；有 intent 且无画像 → 走 `profile_blocked` | `TestSubmittedTaskDoesNotBorrowDeviceProfile` |
| 2 | P1 设备锁未覆盖"读→改→写"，并发丢更新 | 新增 `updateDevice`：**同一临界区内**重读、修改、写回；锁改为有界重试（5s），30s 陈旧锁可回收 | `TestDeviceUpdatesDoNotLoseVisits`（12 并发 → visits=12） |
| 3 | P2 Safari 15.3/15.5 仍映射到 Safari 16 预设 | 能力表新增 `HelloVersions`，**只保留版本与预设精确一致的模板**；Safari 只剩 16.0、iOS 14.0、Firefox 120、Chrome 131/133 | `TestTemplatesNeedAnExactPreset` |
| 4 | 跨进程 mock 不校验 cookie/scnt/verificationId/请求体 | 夹具重写：绑定 cookie、scnt 轮换、`verificationId` 进入提交体、`captcha`/`account`/`phoneNumberVerification` 形状全部校验，违规计数为 0 才通过 | `TestHumanDeviceTelemetryAcrossProcesses`、`TestFlowFixtureValidatesSessionBindings` |
| 5 | §4 仍保留"浏览器历史是决定性因素" | 已降级为"线索，两个变量同时变化"；并注明 `ageDays` 不能作为接受率证据 | 本文 §4 |

## 0.1 结论口径（请按这个审）

- **已交付**：多画像配置 + utls 引擎 + 浏览器式预热 + 节奏 + jslog 回放 + 设备档案。
- **未交付 / 未声称**：没有解决 Apple 风控；`-34607001` 仍未解释；没有伪造遥测；没有伪造 FD；没有 HTTP/2。

---

## 1 本轮提交

| 提交 | 内容 |
|---|---|
| `2d58cf9` | `internal/fingerprint`（画像模型）+ `internal/engine`（utls 适配）+ 任务级冻结/阻断 + 回执分栏 |
| `55c0941` | `internal/pacing`（确定性节奏）+ 预热（导航/子资源头、cookie 链） |
| `803b67f` | `tools/capture/jslog-harvest.cjs` + `-telemetry` 回放（真语料，不新增事件） |
| `9bdea2c` | `tools/signup/device.go`：跨任务设备档案（cookie/画像/年龄/访问/历史） |
| 本次 | `tools/signup/warm-device.ps1`（养护脚本）+ 本复核包 |

---

## 2 文件地图

```text
internal/fingerprint/     模板、权重、过滤+归一化、UA/CH/OS 耦合校验、画像签名、来源记录
internal/engine/          utls 传输：HelloChrome/Firefox/Safari/IOS，socks5，证书校验，HTTP/1.1
internal/pacing/          (seed,from,to)→等待；请求前等待；可注入时钟
tools/signup/
  profile_mode.go         选型→落盘→续跑→阻断；把画像装进会话
  warmup.go               落地页→配置→widget→N 个 bundle；导航头 vs 子资源头
  telemetry.go            捕获回放：数组类重定时间/重生成 id，对象类原样
  device.go               设备档案：cookie/画像/年龄/访问/历史；损坏即拒
  warm-device.ps1         养护脚本：只读 preflight，写 visits.jsonl
tools/capture/
  jslog-harvest.cjs       真机抓 jslog 信封（只加载页面）
  graph-code.cjs          Graph 邮箱取码（自动化用）
```

---

## 3 测试清单（审这 12 条即可）

| 包 | 测试 | 证明什么 |
|---|---|---|
| fingerprint | `TestFamilyWeightsMatchContract` / `TestVersionWeightsMatchContract` | 权重与 A 契约一致（95/2/2/1；150–154 = 2/3/5/10/80 等） |
| fingerprint | `TestSelectProducesAllFourKinds` | 四类都能生成且通过校验 |
| fingerprint | `TestNonChromeKindsCarryNoClientHints` | Firefox/Safari 不带 Chrome Client Hints |
| fingerprint | `TestExplicitKindNeedsEngineSupport` / `TestAutoSelectionRenormalisesAroundUnsupportedKinds` | 先过滤后归一化；不可模拟的家族永不被抽中 |
| fingerprint | `TestNewerTemplatesAreFilteredByEngineCap` | 引擎上限生效（Chrome 133 以下才可选） |
| fingerprint | `TestValidateRejectsTamperingAndBadInputs` | UA/CH/region 改一项即拒 |
| engine | `TestEnginePutsTheProfileOnTheWire` | 四种 hello 的 **cipher 列表互不相同**、ALPN 只报 http/1.1、UA 到达服务端 |
| engine | `TestEngineRefusesUnknownHello` / `...NewerThanItCanEmulate` / `TestEngineVerifiesCertificatesByDefault` | 未知 hello、超版本、自签证书都拒绝 |
| pacing | 四个测试 | 确定性、上下界、可取消、可禁用 |
| signup | `TestProfileIsFrozenAndResumed` / `TestProfileModeBlocksTaskWithIntentButNoProfile` / `TestProfileModeRejectsTamperedProfile` | 画像冻结、缺失/篡改阻断 |
| signup | `TestWarmupFollowsTheBrowserPath` / `TestAssetRequestsLookLikeSubresources` | 预热顺序与头部分型、cookie 链 |
| signup | `TestHumanModeFreezesPacingAndWarmup` | 节奏冻结、续跑 seed 一致、预热不重跑 |
| signup | `TestTelemetryRefusesCaptureWithoutBodies` / `...RebaseKeepsShapeAndRestamps` / `...SummaryHasNoBodies` | 不虚构、只重定时间/ id、回执无 body |
| signup | `TestDeviceWarmsCookiesIntoSession` / `TestDeviceStateSurvivesAcrossCLIRuns` / `TestDeviceReceiptHasNoCookieValues` | cookie 真恢复、跨任务复用画像、回执无 cookie 值 |

命令：

```powershell
go build ./...
go vet ./internal/fingerprint ./internal/engine ./internal/pacing ./tools/signup
go test ./internal/fingerprint ./internal/engine ./internal/pacing ./tools/signup -count=1
```

（`go vet ./...` 里 `internal/sap/unicorn` 的 `unsafe.Pointer` 告警是仓库既有问题，不在本轮范围。）

---

## 4 真实证据

**① 浏览器 vs 无痕（线索，不是结论）**

同一台机器、同网络、同美国号、同邮箱 `2897667304@qq.com`：

- Edge 普通窗口 → 通过创建，进入「验证电子邮件地址」页；
- Chrome 无痕 → 被拒。

**这条对照同时改变了浏览器与配置状态两个变量，只能作为线索，不足以断言"浏览器历史是决定性因素"；进入邮箱验证页也不等于建号成功。**

**② 设备养护（本仓库，刚才实测）**

```json
{"at":"2026-11-15T06:57:46Z","device":"shao-xing-home","exit":0,"ok":true,
 "visits":4,"ageDays":0,"cookies":12,"loaded":51,"reused":true,
 "profile":"fp_373433f7c75a0c6a","stage":"preflight"}
```

`loaded=51` 是本次从设备恢复的 cookie 记录数；`reused=true` 说明画像没被重抽；四轮运行画像 id 恒定。
`ageDays` 只是本地档案年龄（首次见到到现在），**不能作为 Apple 接受率变化的证据**。

**③ 协议侧真实跑过的组合（全部 `-34607001`，作为反向证据）**

号码：US(hero/grizzly)、GB、FR；邮箱：outlook/hotmail/qq/自建临时域；
出口：CN 家宽、DataImpulse US×4、OVH GB、台湾 ISP；交互：协议 2.5s / 协议人形 30s / 真浏览器 67s。

---

## 4.5 跨进程续跑验收（本轮补）

`TestHumanDeviceTelemetryAcrossProcesses`：把 **画像引擎 + 预热 + 节奏 + 遥测 + 设备** 五件同时打开，用三个**独立 CLI 子进程**跑完 create → 邮箱码 → 短信码 → finalize。

服务端是 TLS mock（`httptest.NewTLSServer` + `-engine-insecure`），断言：

| 断言 | 说明 |
|---|---|
| 进程 1 退出码 4 | 停在等邮箱码，而非失败 |
| `warmup.fetched ≥ 3` | 预热真的走了落地页/配置/widget |
| `telemetry.sent == 2` 且全程 `/jslog`+`/v2/jslogs` 合计 **2** 次 | 遥测只发一次，续跑不重放 |
| `pacing.enabled == true` | 节奏随任务生效 |
| `device.visits` 1 → 2 → 3 | 每次进程都记一次访问 |
| 进程 2/3 `device.cookiesLoaded == 0` | 续跑不吃设备 cookie（以任务快照为准） |
| 三次进程 `profile.id` 完全一致 | 画像不漂移 |
| `POST /account` 合计 **1** 次、`POST /account/validate` **1** 次 | finalize / validate 不重放 |
| 进程 3 `accountExists == true` | 终态可读回 |

```powershell
go test ./tools/signup -run TestHumanDeviceTelemetryAcrossProcesses -count=1 -v
```

## 5 养护与建号（操作面）

```powershell
# 养护（只读、零成本、可长期挂）
powershell -File tools\signup\warm-device.ps1 -Email 2897667304@qq.com -Device shao-xing-home

# 建号（同一个 -device）
.\bin\signup.exe -email 2897667304@qq.com -password '...' -phone <national> -phonecc US `
  -stage all -device shao-xing-home -profile-mode new -profile auto `
  -egress-country CN -egress-source verified-egress -human -telemetry .cache\jslog\capture-1.json `
  -captcha-key $env:YESCAPTCHA_KEY -state .cache\signup\task.json -json
```

已挂定时任务：每 45 分钟跑一次养护（本会话内），只写 `visits.jsonl`，失败不重试。

---

## 6 请复核者重点挑刺的地方

1. **是否真的"不虚构"**：`telemetry.go` 只允许重定时间与重生成 eventId，请确认真没有字段生成逻辑。
2. **冻结语义**：画像/节奏/设备三处都有 `schemaVersion` + 签名/哈希，请确认"改一项即拒"没有被某条路径绕过。
3. **阻断而非补造**：有 intent 无画像、设备损坏、cookie 恢复失败，是否都走 `needs_review` / `profile_blocked` 而不是静默重建。
4. **回执脱敏**：`profileReceipt` / `telemetrySummary` / `deviceReceipt` 只输出计数与 id，请确认没有 body、cookie 值、密码、代理凭据。
5. **引擎诚实度**：回执是否始终写 `http:"1.1"`、`h2Disabled:true`、`applied:true`；不允许出现"声称 h2"。
6. **不重放**：`internal/engine` 与 workflow 是否确实没有任何自动重试；遥测失败是否只记录不阻断。

---

## 7 已知限制（不要当成缺陷，也请不要被绕过）

- utls 只能模拟到 Chrome 133 / Firefox 120 / Safari 16 / iOS 14；因此 A 的第一层模板在当前引擎下被整体过滤。
- 无 HTTP/2；无自定义 TLS 随机扰动。
- 设备档案里没有 localStorage / IndexedDB / 扩展 / 真实浏览历史——协议层没有这些设施，也不声称有。
- `ageDays` 是真实时间，不能通过参数加快。
- `-34607001` 未解释；本包不构成"能过 Apple 风控"的证据。
