# Windows 18741 双路径接入：部署前复核

审查代码基线：`7062c90b3e663f0ee030904d3ebfcaab4b3a525d`。目标：现有 Windows 机器的 18741 面板。日期：2026-11-15。

## da2bc04：协议适配器边界复核（最新，暂不接真实执行器）

基线 `da2bc042895d9c02163372e3a574dec5e0f73c94`，远端一致。独立 git archive 快照中 Python 原有 52 项测试通过；本轮新增四个真实控制流边界用例均失败。没有混入工作区其他未提交修改，没有访问真实资源服务。

1. **P1 / 自动购号调用错误。** `Job.run` 对无订单的 auto_purchase 任务直接调用 claim_number，但该方法只登记已有订单，立即读取 sms["activation"]；真正购买在 number → Supply.purchase 中。实测真实 claim_number 配合假的 Supply 客户端时，purchase 调用 0 次，任务落为 unknown/interrupted_or_local_error，适配器未运行。原测试把 claim_number 替换成计数 lambda，掩盖了此错误。修复应复用现有受预算控制的订单获取路径，再领取已确认的订单；未知购买结果不能重新买。
2. **P1 / 等码恢复误交接登录。** protocol_create 将父任务 stage 改为 email-sent；恢复 Job.run 时 stage 不再是 create，handoff=dashboard 分支直接执行 dashboard_handoff。实测恢复后登录交接调用 1 次，adapter.resume 为 0。父任务的 create 阶段和协议子阶段应分别保存，恢复应路由到原适配器 resume；若暂不恢复，必须明确停住，不能转登录。即使适配器保留 stage=create，现有 create 恢复分支仍会清浏览器并转登录，需要一并按注册方式修正。
3. **P1 / 未确认结果仍会结算号码。** 新 guard 只改 finally，但 except PipelineError 分支仍无条件 settle_number。适配器抛 needs_review 后，实测 settle 调用 1 次。自动号码的 Supply.settle 对未收码订单可能发送取消请求，因此仅跳过 finally 不足以保护未知结果。应集中判定结算准入，覆盖异常、finally、取消、维护 sweep 等全部入口；不是简单删除正常终态清理。
4. **P2 / 双字段冲突被静默取舍。** registration_method=protocol 与 registrationMethod=browser 同时存在时 validate 仍通过；Python 用前者，面板读取后者。应只存一个规范字段，或兼容读取时验证两者一致；新建和恢复均需相同规则。

交付说明与测试范围也需修正：所谓“跨进程重启”用例只是重新构造 Job 并读取 result.json，没有实际调用恢复流程；“释放邮箱”计数绑定的是 cleanup_browser，不是真实邮箱生命周期；缺适配器的异常映射测试在测试体里重写 try/except，而非调用 execute。应替换为入口级验收。适配器解析还支持 cfg.protocol.adapter，因此并非只能通过环境变量注入；配置来源边界应如实定义。

下一步顺序：修复以上控制流 → 用测试适配器验证输入、有效期和取消的资源边界 → 再接真实 signup 子进程。等待到期应停止自动发码/恢复并转核实，再按已确认短信/账号/订单状态收尾；不要无限等待过期号码或自动重买。取消需区分提交前、已提交未知和已收短信，不能统一释放。适配器可用性应在新建和恢复两种入口、资源动作前都检查。

本机复现目录为 `C:/Users/HUAWEI/AppData/Local/Temp/apple-review-da2-02f3d18a134c400eae7eaea2efa5af53/`，source 为提交快照：

```powershell
python C:/Users/HUAWEI/AppData/Local/Temp/apple-review-da2-02f3d18a134c400eae7eaea2efa5af53/review_pipeline.py
```

用例保留真实 Job.run、claim_number 和状态写入，替换适配器与资源/交接外部边界。四个失败分别对应上列四项，不能因既有测试全绿将这批边界标成已通过。

## 43af8bf：面板修复通过，协议入口仍须停止误分流（历史记录）

基线 `43af8bf51cd350e6d8c4b5d546960254cec7e1a5`，远端一致。本轮继续使用 git archive 独立快照，不混入工作区其他 dashboard 修改。上一轮三个补充用例全部通过：新建任务不再被错误冻结、非法持久方式拒绝、损坏 manifest 拒绝。signup/dashboard 测试、定向 vet、Python 44 项测试通过。

**仍有 P1：声明 protocol 的任务实际进入浏览器路径。** 面板现在正确把任务交给 pipeline，但 pipeline.py 完全没有读取 registration_method/registrationMethod，create 仍无条件调用浏览器实现。在真实 `execute → Job.run → create` 控制流中，以离线替身截断号码和浏览器外部动作，实测 protocol 任务调用号码入口 1 次、Forge 浏览器入口 1 次。没有真实购号或浏览器服务调用。这是当前可达的错误分流，不只是“适配器尚未实现”。

接入适配器前，pipeline 必须先验证方式与执行器能力；protocol 未实现时明确返回类似 `protocol_executor_unavailable` 的错误，在号码获取、账号资源占用及浏览器启动前停止，不能默认为 browser。面板也不应开放一个会错误执行的协议入口。两个方式字段应使用单一规范字段，或明确验证双字段一致；不能由 Go/Python 分别信任相互矛盾的值。

可以继续 B＋D 的测试适配器工作，但实施约束如下：

1. 假适配器由测试注入，生产缺少真实实现时拒绝执行。不要发布一个固定返回 waiting_input 的生产 stub，把未执行伪装成正常等待。
2. 复用真实 pipeline 管理逻辑。只有资源客户端和执行器替换为离线替身；计数必须来自真实生命周期调用，而不是三个没有连接实际流程的函数。
3. **“其余逻辑保持不变”不可行。** 当前 `while status != ready` 会继续迭代，`finally` 对自动购号任务调用 settle_number，恢复 create 阶段又会切换到 login。应明确 waiting_input 的暂停、持久化、资源保留与恢复语义；一轮等待只调用一次适配器，退出主循环，不进入登录，不重发注册。
4. 对自动购号和已有号码两种配置分别验收：有效租期内等待时取消订单、释放邮箱、切换登录均为 0；重启后仍等待同一挑战，状态与输入文件路径一致；收到输入后从原阶段推进；过期、取消、未知结果按各自策略处理。
5. “取消释放”不能无条件成立。提交前安全取消、已提交结果未知、已收到短信、号码过期等情况需要分别断言。未知副作用不得通过释放资源后重建来恢复，已收到短信的订单不能假定可退。
6. 后续接真实 CLI 时，应验证实际 CLI 参数及私密输入映射，而非仅比较命令字符串；交接激活仍需账号绑定、幂等和失败恢复。

本机复现文件：`C:/Users/HUAWEI/AppData/Local/Temp/apple-review-43af-a84192a983894cdfad5724b5b0dfe4cb/review_pipeline.py`。同目录 source 为独立提交快照，执行下列命令得到 browser calls=1 的失败断言：

```powershell
python C:/Users/HUAWEI/AppData/Local/Temp/apple-review-43af-a84192a983894cdfad5724b5b0dfe4cb/review_pipeline.py
```

本轮只更新复核文档，不部署、不扣费、不修改业务实现或他人的未提交修改。

## c399782：面板 A＋C 再复核（历史记录）

基线 `c3997826d6d5a636ed0e5e16ed8a20c4b092be63`，远端一致。工作区另有非本任务的 dashboard 未提交修改；本轮将该提交通过 git archive 导出到独立临时目录，测试不混入这些修改。独立快照的定向 vet、Go 五包测试、Python 44 项测试全部通过，signup 可构建。没有部署、请求真实服务或改动那些工作区文件。

CLI 只接受 protocol、拒绝非法值不写任务、按任务名生成默认目录等修正方向正确。但面板 A＋C 仍不能按完成验收：

1. **P1 / 新建协议任务被自己刚写的 manifest 锁成 browser。** `register.go:436` 先写不含 registrationMethod 的 request.json，第 443 行才调用 registrationMethodFor。读取到存在但缺字段的文件后，函数应用“旧任务=browser”规则，拒绝本次 protocol 请求。本轮走真实 `startRegistration` 入口、只使用临时假资源配置，复现错误“任务已冻结为 browser，不能切换到 protocol”。校验应在新任务首次写入之前完成，首次持久化就包含冻结值；读取旧任务和创建新任务必须区分。非法请求也不能先留下含凭据的 request.json/panel.json，避免后续被重复任务检查挡住。
2. **P1 / 面板直连协议命令既绕过管理层，又不能执行。** `registrationProgram` 直接返回 signup.exe，违背统一 pipeline 管理边界。参数含不存在的 `-cap-image`（CLI 实际为 `-captcha-dump`），实际构建后运行所生成的参数立即报 unknown flag；还没有邮箱、号码、账号绑定输入和注册阶段，默认阶段仅 preflight。当前创建阻断使这条分支尚不可达，但修复第 1 项后不能直接开放它。应移除这条不完整直连，在 pipeline 的注册阶段接适配器，协议参数由适配器按私密任务配置构造。
3. **P2 / 持久 manifest 的非法或损坏状态被放行。** `frozenRegistrationMethod` 把任何读取错误都当作“没有旧任务”；`registrationMethodFor` 不验证 stored。实测保存非法方式后仍返回成功，保存破损 JSON 后也被当成新 protocol 任务。只有文件确实不存在才可走新建；解析、权限等错误必须停止，持久方式也必须校验枚举。
4. **接入未闭合。** 面板 input 仍写 jobDir/email-code.txt，协议参数指向 jobDir/protocol/email-code.txt；进度读取 pipeline result.json，取消写 cancel.request，直接 signup 不消费这些管理协议。新增测试只检查 helper 返回字符串和目录存在，尚未验证用户创建、输入、查看进度、取消和恢复。原有依赖检查也尚未按方式分流。

因此可以继续做 B＋D 的假适配器，但先纠正上面的首次冻结时序并撤下不完整直连，协议业务入口在接线验收前保持不可执行。测试应启动真实 pipeline Job 管理逻辑，以可替换的注册执行器和资源客户端为测试边界；不能另建一个 stub 返回 waiting_input 后，仅检查三个从未接入真实管理逻辑的计数器。

最低验收：创建 protocol 任务成功且首次 manifest 即冻结；wait 时不退号、不释放邮箱、不切登录；重启仍续同一 protocol 状态；输入路径一致；取消和过期按统一资源规则收尾；ready 才交接且幂等；未知结果不换路径；browser 原有任务保持可用。已有的“协议执行器不得经过 pipeline”测试断言应改为“协议注册不得调用浏览器执行器”，统一资源管理仍必须经过 pipeline。

本机复现目录：`C:/Users/HUAWEI/AppData/Local/Temp/apple-review-c399-a21b5d55ea7a45e783bdc2bf04a40c3e/`。source 为独立提交快照，dashboard_test.go/overlay.json 为复核用例：

```powershell
Set-Location C:/Users/HUAWEI/AppData/Local/Temp/apple-review-c399-a21b5d55ea7a45e783bdc2bf04a40c3e/source
go test -overlay ../overlay.json ./internal/dashboard -run '^TestReview(CreateProtocolThroughPanel|StoredPanelMethodMustBeValid|UnreadableManifestMustNotBecomeNewTask)$' -count=1 -v
```

三例在该基线全部失败；新建协议用例只检查真实入口的冻结判断，不启动真实执行器，也不连接供应商。

## 7898145：A＋C 复核（历史记录）

基线 `7898145a0ff1b3b7b5236d48529b1d97eccb3009`，远端一致。signup、dashboard 两包测试及定向 vet 通过。该提交仅修改 `tools/signup/admission.go`、`main.go` 和 `predeploy_test.go`，没有修改面板、pipeline 或文档。

先记录前置修复：`b98968b` 的原外部 HTTPS 重定向 overlay 和入库同型测试均已复核通过，恢复后目标检查已移到正确位置；此前明确指出的 I3 重定向问题关闭。它与本次 A＋C 的完整性是两个独立结论。

### P1：方式冻结落在了错误的层，协议执行器被标为 browser

`registrationMethod` 目前只出现在 signup CLI，`internal/dashboard/register.go:24` 的 registerRequest 和 pipeline 任务尚无此字段。`signup.exe` 本身执行的是协议注册，传 browser 也不会变成浏览器流程；当前默认值却会把协议任务回执标记为 browser。旧面板任务默认 browser 的规则被错误应用到了旧 signup 协议状态。

修复要求：面板任务 manifest/pipeline 配置拥有方式选择与冻结，校验输入、持久值和恢复值；旧面板任务维持 browser，旧 signup 任务仍按其真实的 protocol 语义恢复。协议执行器只处理 protocol，或明确校验父任务传入的方式，不应声称能执行 browser。未知 schema/值应拒绝，不能被当作可执行任务。

### P2：旧任务及非法值的边界未覆盖

三个补充用例已离线复现：

- `resolveRegistrationMethod` 没区分新任务和无字段旧任务，旧文件可被显式 protocol 参数改标。该用例检验的是实现自己声明的“旧任务默认 browser”契约；重构后该契约应在面板任务层测试，不能继续错误地约束旧协议 CLI 文件。
- `main.go:234` 在方式校验失败时仍写出整个 state。非法方式虽然返回错误，却在磁盘留下含账号、生成密码等私密字段的任务文件，违背“不建任务”的交付说明。新任务应先验证再创建；已有任务拒绝更改时也不应改写原状态。
- 已保存 `registrationMethod=carrier-pigeon` 时，不传该参数仍返回成功，回执带这个非法值。当前仅校验 requested，没有验证 stored。

### P1 / 接入缺口：C 仅改变设备默认目录

`main.go:287` 只调整 device-dir；state、图形验证码输出、验证码输入等路径仍由原参数决定，没有统一归入任务 protocol 子目录。`methodDir` 仅取 state 文件的父目录；同目录下 task-a.json、task-b.json 得到同一个 protocol/devices 目录，不能独立证明任务隔离。

最小落地方式：保留现有 `<panelRoot>/<jobID>/` 的管理文件与旧任务位置，由父任务为新协议执行器明确指定 `<jobDir>/protocol/state.json` 及其他必要私密输入输出路径。不要再把 state 所在目录无条件追加 protocol，造成 protocol/protocol 嵌套。目录根应由可信任务上下文确定；共享设备档案若有业务需要，应单独明确归属，不混称为任务私有产物。

### 验收范围：退出码 4 目前只覆盖 CLI

`TestWaitingInputKeepsTaskSessionAndPhone` 通过 `invokeSignup` 调用 CLI，验证了协议状态、号码字符串和会话续跑，但没有启动 pipeline、没有模拟接码订单、没有观察 settle/refund 调用。它不能证明管理层不会在退出码 4 后退号，也不能证明 pipeline 恢复时不会跳到登录。

B＋D 的方案方向仍成立，但先修正 A＋C 的归属，再进行实际接线。可先写适配器假实现及管理层测试：明确断言等待时取消订单/释放邮箱/切到登录的调用次数都是 0；再覆盖到期、取消、重启、交接超时、激活失败。scnt 在提交邮箱验证码后变化，只能说明后续响应刷新会话，不能据此声称空闲等待期间存在会话保活。

补充复现文件在本机临时目录，未修改业务实现：

```powershell
go test -overlay C:/Users/HUAWEI/AppData/Local/Temp/apple-review-789-1143e6222fb74fce98922a716386fe81/overlay.json ./tools/signup -run '^TestReview(OldPanelPolicyCannotBeOverridden|InvalidMethodDoesNotCreatePrivateState|CorruptSavedMethodIsRejected)$' -count=1 -v
```

三例在本基线均失败。源文件为同目录 signup_test.go。没有调用真实注册、接码或付费服务，没有部署或改动运行任务。

## 2dca9af 复核与双路径方案决定（历史记录）

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
