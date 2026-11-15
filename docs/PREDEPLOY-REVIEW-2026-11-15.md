# Windows 18741 双路径接入：部署前复核

审查代码基线：`7062c90b3e663f0ee030904d3ebfcaab4b3a525d`。目标：现有 Windows 机器的 18741 面板。日期：2026-11-15。

## 2dca9af 复核与双路径方案决定（最新）

已核对远端 `2dca9af3ee956858ff3f04f07d014ea2b16ac0d1`。其前置提交 `63d4723` 修复了上一节列出的未决 intent 和原始失败原因覆盖问题；本机原补充 overlay 两例全部通过。`TestInsecureRequiresOfflineTarget` 也通过。

**I3 尚未完整关闭：P1，insecure 模式可随 HTTPS 重定向离开 loopback。** `tools/signup/main.go:189` 只校验初始 base；`internal/engine/engine.go:141` 的 RoundTrip 只限制 scheme，CheckRedirect 也只拒绝非 HTTPS。若本地 TLS fixture 返回 Location 指向外部 HTTPS 主机，后续连接仍使用 `InsecureSkipVerify=true`。本轮离线用例保留外部目标 URL/Host，仅把底层拨号映射到本地 TLS fixture，实测外部 Host 的请求到达一次。整个测试没有 DNS 查询外部目标或连接外网。

修复验收：insecure 传输在每一次请求/建连前限制为获准的离线目标，重定向也必须遵守；校验应使用恢复任务之后的实际目标配置。面板不提供该测试开关。补充真正的 TLS fixture、新画像引擎、外部 HTTPS 重定向、初始请求以及恢复会话用例。当前 `TestInsecureRequiresOfflineTarget` 的允许分支使用 HTTP fixture 且未开启新画像引擎，只证明参数准入通过，不能证明关闭证书校验后的传输边界。

```powershell
go test -overlay C:/Users/HUAWEI/AppData/Local/Temp/apple-review-2dca-210e8f27506b4bbcacd58153dfb985bb/overlay.json ./internal/engine -run '^TestReviewInsecureCannotRedirectOutsideLoopback$' -count=1 -v
```

该新增用例在本基线失败。代码在同目录 `engine_test.go`，仅用于本机交接。不要将“四道闸全部在建会话前”作为交付结论：RoundTrip/CheckRedirect 属于运行时请求检查，重定向检查必然发生在收到前一响应之后。

I1 方案决定与调整：

- **创建时可手动选择 browser/protocol；创建后方式冻结，续跑不可切换。** 老任务缺字段按 browser。换方式应新建任务，先核实旧任务的未知副作用及资源归属，不能自动换路重试。
- 分流只替换对应任务的注册执行器，不绕过已有任务管理、邮箱占用、号码管理、退款、成本和取消逻辑。可在统一任务管理下使用两个适配器，不宜仅把 pipeline.py 命令替换为 signup.exe 后直接复用不兼容的回执解析。
- 状态按 job ID 和注册方式分目录，不以邮箱作为唯一任务路径；目录分开防串用，但同一 Windows 用户权限下并非“互不可见”。凭据权限需要单独核验 ACL。
- 退出码 4、blockedReason、原始 failure、冷却截止时间和未决操作应正确映射到面板；不能只看退出码。取消涉及子进程、号码和邮箱状态；建号结果未知时不得盲目释放后重建。
- 协议 ready 仅表示建号阶段完成。交给现有登录、短信验证及激活流程必须有账号绑定、幂等交接和接管后失败的续跑边界，激活失败不能回到注册重建。
- 测试包含双路径 create/wait/resume/cancel、重启、未知结果、交接重复调用与失败恢复；保留浏览器回归。I3 修复后可继续开发 I1，正式启用仍受 I2/I4 与真实验收条件约束。

## bdaeb51 修复复核（历史记录）

复核提交：`bdaeb51a678630b06e0ab78543279ce9c692ad08`，已核对远端一致。原六个 overlay 用例实跑全部通过，构建、定向 vet、Go 五包、Python 44 项测试也通过。原 R1/R3/R4/R5/R6 的已复现场景已修；R2 的暂停状态场景已修，但“未决 intent 一律零请求”尚未满足。不能以原六例全绿替代完整验收。

新增两项需修复：

1. **P1 / 未决 intent 未纳入提前准入检查。** `tools/signup/admission.go:24` 只检查 `needs_review` 和 `rate_limited`，没有检查 Operations。副作用请求写入 intent 后进程中断，正常遗留状态是 `running + intent`。以该状态续跑并开启预热，本轮仍复现 **3 次 HTTP 请求**，之后才被 `workflow.run` 拦住。应在任何请求和设备写入之前检查全部未完成操作并转人工核实；冷却到期不能越过这个检查。验收应覆盖 running+intent、到期冷却+intent、预热和遥测两类入口，请求计数均为 0。
2. **P2 / 拒绝续跑会覆盖原始失败原因。** `tools/signup/main.go:226` 将 `st.Failure` 改为准入拒绝原因并落盘。实测 `final_account_binding_unconfirmed` 在一次被拒的续跑后变成 `task_needs_review`。应保留原始失败事实，把“本次为何不执行”与“上次为何失败”分开表达；公开输出仍只使用受控错误码。验收应连续恢复两次，验证原始原因、HTTP 状态和操作信息不丢失且不发请求。

新增复现用例仍在本机临时 overlay，未改业务代码：

```powershell
go test -overlay C:/Users/HUAWEI/AppData/Local/Temp/apple-review-bda-fd67c8cd6c2843e5854659be88ab6d31/overlay.json ./tools/signup -run '^TestReview(IntentBlocksBeforeWarmup|AdmissionPreservesFailureCause)$' -count=1 -v
```

两例在 bdaeb51 均失败。`signup_test.go` 位于同目录；修复时应将这两个边界纳入正式测试。

交付口径更正：bdaeb51 只有 8 个 Go 文件变更，并未包含此前报告声称的本文“修复状态”更新；本节补充实际核验结果。I3 准确状态是 **CLI 尚未限制非离线环境使用 engine-insecure**，而不是“面板已经透传该参数”——I1 协议入口尚不存在。

下一步建议：先补上上述两项及 I3 的离线证书开关硬性限制，再推进 I1 独立协议分支。双路径并存，任务方式冻结，不自动跨路径重试，共用后续面板激活；正式开放自动资源前仍需完成 I2，发布前仍需核验 I4。本轮没有核验重建二进制或运行中服务，没有真实注册和扣费。

## 结论与范围

**暂不批准协议注册作为正式业务入口上线。** 本轮发现并离线复现六处原有测试未覆盖的问题，另有接入和部署配置缺口。

按用户最新要求，浏览器注册、协议注册是两条并存路径；本报告不要求替换浏览器路径。协议执行器可以继续作为隔离的开发测试入口；接入面板前应完成下面的阻断项。

本轮只读审查实现，运行构建、静态检查、离线测试，并新增本报告；没有修改业务实现、部署服务、重启面板、购买号码、调用识别服务或请求 Apple。复现数据均为本地测试数据。

这里的缺陷是状态完整性、网络安全、资源管理和接入正确性问题，不代表已定位 Apple 的拒绝原因。没有依据保证任何配置不会被服务端拒绝；本报告不提供绕过服务端反自动化判定的方案。

## 六项已复现缺陷

### R1 / P1：预检失败没有可靠落盘，429 冷却状态丢失

- 位置：`tools/signup/workflow.go:144` 的 `prepare`；`tools/signup/main.go:393` 的返回处理。
- 触发：本地服务在邮箱预检返回 429，运行新模式任务的 preflight。
- 实际：公开回执正确给出 `preflight_unavailable` 和 HTTP 429；磁盘任务仍为 `failure=""`、`httpStatus=0`、`status=""`。新模式先写过画像，因此文件存在但没有失败事实；没有此前落盘的旧模式任务还可能根本没有状态文件。
- 影响：重启后不能回答为什么停，调度方只读任务文件时可能再次预检，无法可靠遵守冷却。
- 修复验收：所有退出路径按统一规则保存经过脱敏的失败、HTTP 状态和运行状态；明确预检 423/429 的暂停规则。失败落盘失败也要有独立错误。模拟失败后启动第二进程，应读取相同原因并遵守暂停策略，不能只断言 stdout。
- 用例：`TestDeployFailureSurvivesRestart`。

### R2 / P1：暂停检查晚于网络副作用

- 位置：`tools/signup/main.go:305` 至 `381`；真正的暂停检查在 `tools/signup/workflow.go:368`。
- 触发：已有 preflight 任务标记 `rate_limited`，续跑时开启 human，关闭 journey、延迟和额外资源抓取。
- 实际：返回 `server_requires_cooldown` 之前仍向本地服务发了 **3 次 HTTP 请求**。原因是预热在 `w.run` 的停止检查之前。遥测入口也位于这一检查之前。
- 影响：面板显示暂停并不代表执行器没有继续请求；可能改变原会话，破坏人工核实与冷却边界。
- 修复验收：在任何预热、遥测、设备写入之前完成任务准入判断。`needs_review`、`rate_limited` 和未决 intent 的恢复应明确走只读诊断或拒绝执行，离线断言请求计数为 0。
- 用例：`TestDeployBlockedTaskDoesNoIO`。

### R3 / P2：已提交新模式任务可通过显式 legacy 绕过缺失画像检查

- 位置：`tools/signup/main.go:249` 至 `265`；`tools/signup/profile_mode.go:65` 至 `70`。
- 触发：任务保留 `profileMode=new` 和已提交操作，`profile` 丢失，调用方显式传入 `-profile-mode legacy`。
- 实际：退出码 0，传输变成 `go-net-http`，没有返回 `profile_blocked`。默认续跑路径已修，但显式参数路径仍可跳过校验。
- 影响：损坏任务被当作可续跑任务，原有配置冻结契约不成立。
- 修复验收：先校验持久任务的完整性和模式，再处理调用参数；已提交 new 任务画像缺失必须停止，不能通过参数降级。正常旧模式任务仍应保留自身模式。
- 用例：`TestDeployMissingProfileCannotDowngrade`。

### R4 / P1：更换 device 参数可覆盖另一设备档案

- 位置：`tools/signup/main.go:222` 至 `246`、`341` 至 `348`。
- 触发：Chrome 任务在 original 设备上完成 preflight，随后以 `-device other` 续跑；other 原有 Firefox 档案。
- 实际：CLI 成功，other 档案的画像从 Firefox 被改写为 Chrome。
- 根因：任务没有冻结设备归属；更新锁虽然保护计数，却没有检查当前档案画像与任务画像是否冲突。更新回调直接写 `d.Profile = st.Profile`。
- 影响：独立任务的设备状态串用。两个首次使用同一设备的并发任务也存在先各自选定状态、再覆盖的代码窗口；本轮直接复现的是切换 device 参数，不将并发场景表述为已实测。
- 修复验收：冻结设备归属，冲突在网络请求前拒绝；同一临界区内确认初始设备状态，后续写入不得覆盖不匹配档案。补充切换设备和并发首次使用的测试。
- 用例：`TestDeployTaskCannotOverwriteAnotherDevice`。

### R5 / P1：HTTP 请求绕过配置代理

- 位置：`internal/engine/engine.go:121` 至 `133`；`tools/signup/client.go:167`。
- 触发：配置不可达的 SOCKS5 代理，向本地 HTTP 服务请求。
- 实际：仍直接到达目标，返回 204。传输仅安装 `DialTLSContext`，HTTP 没有经过该代理拨号逻辑。
- 影响：HTTP URL 或 HTTPS 降级重定向可以破坏配置的网络边界。没有证据说明 Apple 当前端点实际发生了这种降级；这是条件性安全缺陷，不是现有拒绝原因的证明。
- 修复验收：生产请求拒绝明文 HTTP 和 HTTPS→HTTP 重定向；离线 loopback 例外应显式限定。若确有 HTTP 支持需求，必须沿统一代理路径。不可达必需代理不能静默变成直连。
- 用例：`TestDeployHTTPDoesNotBypassProxy`。

### R6 / P1：Windows 陈旧设备锁可绕过超时无限循环

- 位置：`tools/signup/device.go:55` 至 `80`。
- 触发：父进程持有设备锁文件句柄，把 mtime 调整到一分钟前；子进程获取同一锁。
- 实际：删除仍打开的锁失败，代码忽略删除错误后立即 `continue`，绕过后面的 5 秒 deadline 与 sleep。7 秒后测试必须终止子进程。
- 影响：设备操作可能挂住并忙循环；“最多等 5 秒”的承诺不成立。锁年龄本身也不能证明持有者已退出。
- 修复验收：所有重试分支都受统一 deadline 控制并处理删除错误；不得仅以 mtime 自动抢占活跃锁。补充 Windows 打开句柄、超时、崩溃遗留锁、持有者退出和清理失败用例。
- 用例：`TestDeployStaleWindowsLockIsBounded`。

## 接入与部署缺口

### I1 / 阻断正式接入：18741 尚没有协议分支

`internal/dashboard/register.go:275` 仍只构造 `pipeline.py` 命令，后者的 create 使用浏览器；`tools/apple-pipeline/panel.ps1:25` 的构建列表没有 signup。这里不是要求更换旧分支，而是必须新增独立协议分支。

两条路径的最小接入契约：

1. 创建任务时明确选择注册方式，并将方式冻结在任务里。旧任务缺少该字段时保留浏览器语义。
2. 两条路径使用独立执行器和私密状态目录；按所选方式检查依赖，协议入口不应被无关浏览器依赖阻挡。
3. 退出码 4 映射为等待输入，使用同一个任务续跑；重启、取消、超时都能定位原子进程与资源。未决结果不自动换另一条路径重新注册。
4. 协议任务自身 `ready` 只表示建号阶段完成，不能直接冒充面板整条产线 ready。建号证据确认后，幂等交接给现有面板登录、短信验证及激活流程；激活和家庭业务保持各自现有准入条件。
5. 增加两条路径各自的创建、等待、恢复、取消及交接离线验收；回归浏览器路径。构建、能力检查、二进制路径和版本记录必须覆盖 signup。

### I2 / 阻断自动资源上线：实验脚本不等于资源管理层

`tools/signup/attempt.ps1` 已有默认不购买和显式价格门槛，应保留。但仍有以下已知缺口：

- `getNumber` 前没有持久化购买意图；超时后的未知订单缺少核实流程。
- 退款失败返回 `parked:...` 字符串，不是可跨进程恢复的 pending 队列和 sweep。
- 没有完整单任务成本账本和总预算执行逻辑。
- 没有传入新模式参数；新任务默认走 legacy，不能凭二进制更新推断启用了新配置。
- 取码等阶段使用命令行 `-code`；面板接入应使用受控的私密输入通道。

协议分支应接入已有资源生命周期管理，或补齐等价能力后再开放自动购买。用户已撤回历史报价，不能把旧 $0.06/$0.12 当作当前授权；本轮没有购买或重新设定价格。

### I3 / P2：测试关闭证书校验的开关未限定为离线

静态检查：`tools/signup/main.go:93` 的 `-engine-insecure` 只有“offline tests only”帮助文字，经第 269 行传入引擎；`internal/engine/engine.go:168` 直接设置 `InsecureSkipVerify`。没有要求 base 为 loopback 的联动检查。

默认值 false 是正确的，但复制 TLS mock 命令到生产可能关闭证书验证。面板不得透传此开关；CLI 应在非离线目标拒绝。验收应证明错误配置在任何网络请求前失败。本项没有发起真实中间人测试。

### I4 / Windows 文件与发布条件

- `0600/0700` 不能独立证明 Windows NTFS 访问控制已隔离。状态包含密码和会话材料，发布时需核对运行账户及目录 ACL；本轮未核验实际目录 ACL。
- `writeState` 有临时文件、Sync 和 Rename，比直接覆盖稳健；但不能把它宣称为 Windows 下完整的事务与断电恢复保证。设备写入还使用固定 `.tmp` 且没有 Sync。需要验证占用目标文件、拒绝替换、进程中断后的旧版本保留与恢复，不能吞掉写失败。
- 本机 Go 源码 `D:/go/go/src/os/file.go:435` 明确不保证非 Unix 平台 Rename 原子性；第 636 行说明 Windows Chmod 只使用写属性位。这里不据此声称实际文件已经泄漏或损坏。
- 发布包只包含受控源码/构建产物和无秘密配置模板。不得上传 `.cache`、`state`、订单、Cookie、认证响应或运行日志。保留原任务状态和上一可用版本，运行中子进程不覆盖二进制。

## 证据与测试

基线验证全部通过：

```powershell
go build ./...
go vet ./internal/engine ./internal/fingerprint ./internal/pacing ./tools/signup ./internal/dashboard
go test ./internal/engine ./internal/fingerprint ./internal/pacing ./tools/signup ./internal/dashboard -count=1
python -m unittest discover -s tools/apple-pipeline -p 'test_*.py'
```

Go 五包通过，Python 44 个测试通过。这证明已覆盖行为没有发现回归，不能覆盖上面的新增边界。

新增六个复核用例使用临时 Go overlay，不修改仓库实现，按期望的安全行为断言，在本基线上全部失败，分别对应 R1–R6。本机交接目录：

```text
C:/Users/HUAWEI/AppData/Local/Temp/apple-predeploy-b2716ddaa9d84b17b47837e5de25705c/
  overlay.json
  signup_test.go
  engine_test.go
  lock_test.go
```

本机复现：

```powershell
go test -overlay C:/Users/HUAWEI/AppData/Local/Temp/apple-predeploy-b2716ddaa9d84b17b47837e5de25705c/overlay.json ./tools/signup ./internal/engine -run '^TestDeploy' -count=1 -v
```

这些临时文件没有入库，路径仅对当前机器有效。修复者应把对应回归用例随修复纳入正式测试，避免依赖临时目录长期存在。

## 不能从现有证据推出的结论

- 画像选中、TLS 预设应用、跨进程 ID 不变，不等于真实浏览器行为完整等价，也不等于 Apple 接受该请求。
- 本地 visits、ageDays、Cookie 数量和实验性请求序列不构成服务端接受率证据。
- 严格 mock 能验证本地请求/状态契约，不能证明未实测的真实最终响应形状一定符合解析器，也不能证明 `-34607001` 已解决。
- 本轮没有核验运行中面板、定时任务或真实协议建号的完整成功基线。因此结论是“需要修复并完成接入验收”，不是“换一套配置即可通过判定”。
