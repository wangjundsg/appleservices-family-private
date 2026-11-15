# 注册衔接契约审查夹具

配套 [统一方案](../../REGISTRATION-INTEGRATION-PLAN-2026-11-15.md)。全部使用合成账号、号码、验证码及本地 mock，不使用真实凭据，不调用供应商或 Apple。Go 夹具会创建本机临时测试服务。

这是一组**当前实现应修复的失败用例**，不是生产运行脚本，也不自动加入常规测试发现。基线 `4bbec7d`：Python 6 项、Go 2 项均失败。不能为了全绿删除安全断言。新接口引入 challenge_id 等必填字段后，应升级夹具请求形状，保留相同的业务安全断言。

推荐从仓库根目录导出待审提交，在临时目录验证，避免混入工作区改动：

```powershell
$reviewDir = Join-Path $env:TEMP ('registration-review-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $reviewDir | Out-Null
$reviewZip = Join-Path $reviewDir 'source.zip'
git archive --format=zip --output=$reviewZip HEAD
if ($LASTEXITCODE -ne 0) { throw 'git archive failed' }
$reviewSource = Join-Path $reviewDir 'source'
Expand-Archive -LiteralPath $reviewZip -DestinationPath $reviewSource
$fixtureDir = Join-Path $PWD 'docs/review-fixtures/registration-2026-11-15'
$priorReviewRoot = $env:APPLESERVICES_REVIEW_ROOT
try {
    $env:APPLESERVICES_REVIEW_ROOT = $reviewSource
    python (Join-Path $fixtureDir 'review_gate_edges.py') -v
    python (Join-Path $fixtureDir 'review_system.py') -v
    Copy-Item -LiteralPath (Join-Path $fixtureDir 'review_system_test.go.txt') -Destination (Join-Path $reviewSource 'internal/dashboard/review_system_test.go')
    Push-Location $reviewSource
    try { go test ./internal/dashboard -run '^TestReviewProtocol' -count=1 } finally { Pop-Location }
} finally {
    if ($null -eq $priorReviewRoot) { Remove-Item Env:APPLESERVICES_REVIEW_ROOT -ErrorAction SilentlyContinue }
    else { $env:APPLESERVICES_REVIEW_ROOT = $priorReviewRoot }
}
```

逐组记录退出码，不能把最后一条命令的结果当作全部通过。临时目录保留用于核查，不修改原仓库业务文件。

| 用例 | 当前基线实测 |
| --- | --- |
| future_cooldown_stops_real_resume | 冷却尚有 30 分钟仍 resume=1，最终 waiting_input |
| structured_pending_operation_preserves_review_reason | 字典 operation 导致 unknown/interrupted_or_local_error |
| purchase_is_recovered_after_manifest_write_crash | 账本成功后绑定失败，再调用 number 导致购买 mock 调用 2 次 |
| manual_refund_checks_received_sms | 未 getStatus，直接 setStatus=8 |
| stopped_task_cannot_resume_after_cancel_command_archived | cancelled 任务无命令文件时仍 resume=1 |
| input_read_before_dispatch_is_not_consumption | 尚未 dispatch，重读已报 input_already_consumed |
| TestReviewProtocolWaitingInputReachable | 协议等待任务输入 HTTP 409 |
| TestReviewProtocolResumeEndpointExists | 恢复接口 HTTP 405（应进入准入，待核实任务被受控拒绝） |

购号用例检查资源函数的崩溃恢复，不证明完整 run 在 unknown 后会自动重购；取消用例检查持久终态，不主张现有系统会删除 cancel.request。测试协议输入的 HTTP 200 是当前输入接口兼容约定；若最终契约采用 202，应同时验证持久 inbox 与正确挑战投递，而不只修改状态码断言。

## ad19776 中途复核补充（2026-11-16）

`review_policy_batch.py` 使用相同 APPLESERVICES_REVIEW_ROOT 选择待审源码，新增六组纯策略/真实 run 用例：冷却时间格式与时区、未知 operation、业务与短信双重退款依据、当前挑战绑定、持久停止标记。在 ad19776 六组均失败（含十个子场景失败断言）；已有 review_gate_edges.py 两项中结构化操作通过，冷却状态/截止保留仍失败。

```powershell
python docs/review-fixtures/registration-2026-11-15/review_policy_batch.py -v
```

这些是 W1/W2 同一批验收的补充，不替代总方案 T01–T16，也不要求每条修完单独申请确认。升级最终状态 schema 时同步升级夹具输入，不放宽“错误状态不得执行、退款需完整证据”的断言。

## 76454cb 应用层补充（2026-11-16）

在此提交上，review_policy_batch.py 六组已通过；review_gate_edges.py 两项仍因原始停因被覆盖失败。新增 `review_decision_application.py` 五项验证：probe 不得越过已收码退款拒绝、其他任务挑战拒绝、缺绑定字段拒绝、同阶段新 verification_id 更换挑战、持久停止真正触发停止协调。五项在 76454cb 均失败；退款只记录 mock 请求，不访问供应商。

```powershell
python docs/review-fixtures/registration-2026-11-15/review_decision_application.py -v
```

## 2837794 复验与 R01 边界（2026-11-16）

最新快照上 review_policy_batch.py、review_gate_edges.py、review_decision_application.py 共 13/13 通过。对应旧缺陷关闭，不能继续引用修复前结果。

`review_acquisition.py` 验证完整读取持久 Job 后的采购恢复：账本成功/input.json 绑定写失败的场景通过；同一 acquisition_id 在 OTP_RECEIVED/REFUND_PENDING/REFUNDED 三种状态下不得再采购的子场景失败，同 ID 改国家须拒绝的场景失败。全部购买调用都是 Mock。

```powershell
python docs/review-fixtures/registration-2026-11-15/review_acquisition.py -v
```

review_system.py 的采购恢复夹具已补上读取 result.json，避免测试自己丢弃已持久化 acquisition_id。其当前四项三过一失败，未通过的输入提前消费仍属既定 I01 待完成范围。
