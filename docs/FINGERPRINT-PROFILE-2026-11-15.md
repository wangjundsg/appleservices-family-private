# 多画像配置与请求引擎接入（协议注册执行器）

2026-11-15。范围：`appleservices-family-private`。
结论口径：**多画像配置及请求引擎接入完成。** 本轮没有解决 Apple 风控，也没有验证 `-34607001` 是否消失；离线测试不访问 Apple、不买号、不调用付费识别。

来源契约：`D:\Workbench\apps\gpt-free-service\docs\PROTOCOL-A-FINGERPRINT-CONTRACT-2026-11-13.md`（版本 2026-11-13）。
只复用其中的**配置模型**；A 的注册逻辑、自动重试、全局补丁和运行状态没有搬过来。Apple 侧运行时**不读取**该目录，契约路径与模板表哈希作为来源记录写进每个画像（`source` 字段）。

---

## 1 模块边界

| 位置 | 内容 |
|---|---|
| `internal/fingerprint/` | 模板、抽样与归一化、UA/Client-Hints/语言/时区耦合校验、画像 ID 与内容签名、来源记录 |
| `internal/engine/` | utls 传输适配层：把画像里的 hello 真正装到 socket 上；能力声明（支持哪些 hello、能模拟到哪个大版本） |
| `tools/signup/profile_mode.go` | 任务级生命周期：选型→落盘→续跑→阻断；把画像装进会话；回执 |
| `tools/signup/*.go` | 会话与状态机（既有实现），新增 `profile*` 字段与 `-profile-mode` 等开关 |

依赖：`github.com/refraction-networking/utls v1.8.2`（本次新增）。

---

## 2 模板与抽样

与 A 对齐的四类、权重按 A 原样：

| 类型 | 系统 | 家族权重 |
|---|---|---:|
| `chrome` | Windows | 95% |
| `firefox` | Windows | 2% |
| `mac_safari` | macOS | 2% |
| `ios_safari` | iOS | 1% |

显式选择：`auto / chrome / firefox / mac_safari / ios_safari`；另接受 `safari`，按契约在 macOS/iOS 之间**等概率**选择（2:1 只属于 auto）。

模板表分两层：

1. **A 声明的版本**（原样保留版本与权重）：Chrome 150(2)/151(3)/152(5)/153(10)/154(80)、Firefox 133(50)/144(50)、macOS Safari 15.3/15.5/17.0/18.0(各25)、iOS Safari 17.2/18.0(各50)。
2. **引擎可达的回退版本**：Chrome 131/133、Firefox 120、Safari 16.0、iOS 14.0。utls 1.8.2 只能模拟到这些大版本，因此当前引擎下第 1 层会被整体过滤，第 2 层参与抽样。

过滤顺序即契约要求：**先按运行时能力过滤，再在剩余集合内归一化权重**。权重是抽样概率，不是批次配额。

---

## 3 生命周期与冻结

```text
创建任务
  → 确定代理与出口地区（显式或已核验）
  → 检查引擎能力（hello + 可模拟的大版本）
  → 选择并校验画像
  → 写入任务状态（首次网络请求之前）
  → 首个请求
  → 后续阶段、退出码 4、等码、重启进程全部复用
```

- 画像落盘时机：`installProfile` 成功即 `writeState`，早于任何 Apple 请求。
- 续跑：状态里有画像就**只校验、不重选**；签名不符或校验失败 → `needs_review` / `profile_blocked`，不会补造。
- 有提交意图（`body`/`verificationId`/任一 operation 有状态）但画像缺失 → 阻断。
- 模板升级不影响已有任务：画像里存了 `templateId`、内容签名和 `templatesSHA256`。
- 每个任务独立 `id`（`fp_<8B hex>`）；不同任务可以选中同一模板，不宣称远端指纹唯一。

地区规则：

- `-egress-country`（ISO2）+ `-egress-source (explicit|verified-egress)` 必填，且**不从代理主机名或端口推断**。
- 语言/时区在首次分配时冻结；执行中出口变化只记录异常，不换画像继续提交。
- 出口国家、Apple 账号地区（`-country`）、短信号码国家（`-phonecc`）是三个独立输入，代码不强制拉平。

---

## 4 请求引擎

`internal/engine` 用 utls 把画像的 hello 应用到真实 TLS 握手：

| 类型 | hello | 可模拟上限 |
|---|---|---|
| chrome | `HelloChrome_Auto`（133） | Chrome 133 |
| firefox | `HelloFirefox_Auto`（120） | Firefox 120 |
| mac_safari | `HelloSafari_Auto`（16.0） | Safari 16 |
| ios_safari | `HelloIOS_Auto`（14） | iOS 14 |

保留项（未因换引擎而丢）：

- Cookie：沿用 `sessionState` 的 Set-Cookie 记录（域/路径/过期/跨进程恢复），curl 会话仍走独立 jar 并写入同一快照。
- `scnt`、session ID、hashcash 挑战、验证码挑战的既有管理不变。
- 显式代理（socks5/socks5h，支持认证）、超时、取消、证书校验（默认开启，`-engine-insecure` 仅供离线测试）。
- intent 防重放原样保留：本包不含任何重试逻辑。

明确不做：

- 不继承 A 的自动重试；失败不静默切换浏览器类型或传输引擎。
- 不迁移自定义 TLS/HTTP2 随机扰动；hello 固定，保证回执可核对。
- **不提供 HTTP/2**：ALPN 只报 `http/1.1`，回执里 `http: "1.1"`、`h2Disabled: true`，不声称发了 h2 SETTINGS。
- 屏幕/硬件字段只作为配置元数据，回执不把它写成"已生效"。
- FD（`X-Apple-I-FD-Client-Info`）独立处理：非 Chrome 画像携带 FD 会被直接拒绝；不生成伪造 FD。

回执分两栏：

```json
"profile": {"mode":"new","id":"fp_...","template":"chrome-win-131","kind":"chrome",
            "locale":"en-US","timezone":"America/New_York","region":{...},"hints":true,
            "engine":{"name":"utls","version":"v1.8.2","hello":"HelloChrome_Auto",
                      "applied":true,"http":"1.1","h2Disabled":true,"proxySet":true}}
```

`profile.*` 是选择结果，`profile.engine.*` 是实际生效的引擎；两者分开，避免"导入了画像"被当成"已应用浏览器模拟"。

---

## 5 迁移

- `-profile-mode legacy|new`，默认 `legacy`。
- 迁移规则：**全新任务**（无 session、无 stage、无画像）且显式给了 `-egress-country/-egress-source` 时，自动走 `new`；已有任务与未给地区输入的任务保持 `legacy`，不需要新入口即可恢复。
- 旧 schema 的任务继续用原配置；本版不改写它们的会话材料。

---

## 6 验收（离线）

| # | 项 | 证据 |
|---|---|---|
| 1 | 四类均能生成与校验；Firefox/Safari 不带 Chrome Client Hints | `internal/fingerprint` `TestSelectProducesAllFourKinds`、`TestNonChromeKindsCarryNoClientHints`；`tools/signup` `TestClientHintsFollowTheProfile` |
| 2 | 不支持的模板明确拒绝，auto 只用支持集合 | `TestExplicitKindNeedsEngineSupport`、`TestNewerTemplatesAreFilteredByEngineCap`、`TestAutoSelectionRenormalisesAroundUnsupportedKinds` |
| 3 | 新进程续跑恢复同一画像/会话/验证 ID | `TestProfileIsFrozenAndResumed`、`TestProfileModeRunsTheStateMachineOverTheEngine` |
| 4 | 画像被改/损坏/缺失均阻断 | `TestProfileModeRejectsTamperedProfile`、`TestProfileModeBlocksTaskWithIntentButNoProfile`、`TestProfileModeRejectsRequestedKindMismatch` |
| 5 | 新引擎访问本地 mock 跑完整状态机 | `TestProfileModeRunsTheStateMachineOverTheEngine`（TLS mock + `-engine-insecure`，`state=ready`、`accountExists=true`） |
| 6 | 超时/结果未知不重放 | 沿用 `TestUnknownFinalCreationNeverReplays`、`TestInterruptedIntentAndLegacyStateAreNotReplayed`（引擎层不含重试） |
| 7 | 输出不泄露凭据 | `TestProfileReceiptCarriesNoSecrets`；`invokeSignup` 的既有 secret 断言继续生效 |

引擎层另有独立证据（`internal/engine`）：四种 hello 在 socket 上产生**不同的 cipher 列表**、ALPN 只报 `http/1.1`、UA 确实到达服务端、未知 hello 与大版本超限被拒、自签证书默认校验失败、HTTP 代理明确拒绝。

```powershell
go build ./...
go vet ./...
go test ./internal/fingerprint ./internal/engine ./tools/signup -count=1
```

---

## 7 已知限制与下一步

1. **utls 版本上限**：只能模拟到 Chrome 133 / Firefox 120 / Safari 16 / iOS 14，因此在当前引擎下 A 的第 1 层模板全部被过滤。换更新的引擎（或 wreq 适配层）后无需改画像逻辑，权重会自动回到 A 的分布。
2. **没有 HTTP/2**：真浏览器会协商 h2，本适配层只走 HTTP/1.1。这是公开声明的差异，不是隐藏假设。
3. **画像 ≠ 人的形状**：本轮的交付是"配置 + 引擎"。步间节奏、jslog 遥测、页面级信号等"人的形状"是下一阶段，不在本次范围。
4. **真实注册未验收**：`-34607001` 是否与画像/引擎有关，仍需一次真实建号实验；本轮不做任何真实请求。
