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
