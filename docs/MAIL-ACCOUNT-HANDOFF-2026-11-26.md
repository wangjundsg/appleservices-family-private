# 账号开通与统一邮箱取码核对

核对日期：2026-11-26，北京时间。基础提交 `878d251`。本轮只做本地只读调查、代码与离线测试；没有登录 Apple、请求验证码、刷新真实邮箱授权、购买、重启或部署服务。新注册完整端到端尚未验收。

## 邮箱来源与占用

直接读取 JSON，未构造 Outlook Store，未导入或复制凭据。两份清单按 trim + 小写邮箱地址去重：

| 项目 | 当前证据 |
| --- | --- |
| 本项目 `.cache/mailboxes.json` | 7 条，6 条保存 client_id + refresh_token；另 1 条为 webmail/no_token |
| Outlook `.cache/workbench/mailboxes.json` | 1 条，与本项目 Hotmail 重复；合并仍为 7 个邮箱 |
| 重复项来源 | Apple 清单为 purchased，Outlook 为 owned；保留两份声明，不能据此认定真实取得方式 |
| 授权有效性 | 未联网验证；保存材料、status=ok 与当前可读是不同证据 |
| `.cache/mailbox-usage.json` | 1 条历史使用记录，不是租约或可安全释放的结果回执 |
| `.cache/panel` | 本次核对时不存在；不能拿不存在当作所有旧账号都未使用 |
| Outlook 保护邮箱占用 | `tools/console/backup-mail.cjs` 已有 claimed/active/released 与 binding_uncertain；默认读取 `.cache/workbench/backup_email_usage.json`，本次现场未找到该文件 |
| 另一产线 `data/backup_email_usage.json` | 在所查的 8788 源码根目录未找到；不能宣称已纳入统一锁 |

新增 `node tools/capture/mailbox-inventory.cjs` 提供只读汇总：去重、原始来源声明、来源差异、建议读取 owner、历史使用和面板占用证据。支持 `--repo-root`、`--outlook-root`、`--backup-usage` 指定实际路径。输出不含密码/令牌/client_id；属于本地运行清单，不提交其输出。

重复邮箱优先复用 8802；其余旧清单保持原位，没有要求重复导入。汇总明确标出授权未检查、是否找到保护占用表以及未包含账号注册表，不能直接作为“可领取邮箱”列表。

18741 的现有自动选择仍从 8802 `GET /api/v1/mailboxes` 读取，并排除账号注册表与任务 request.json 的占用。本轮补充排除旧 mailbox-usage.json；损坏记录阻止选择。unknown、失败或过期不自动释放已有记录。

这不是跨服务租约实现。旧 apple-local-run 仍只按时间排序，不能与面板并行作为两个分配者；8802 读接口不提供全业务领取互斥。本轮未改动旧付费建号运行器，也未新增后台写进程。

## 统一取码入口

`tools/capture/mail-reader.cjs` 导出 `readCode`，注册器的 Graph 和 8802 分支已接入。每个任务明确传入：

```javascript
const {readCode} = require('./tools/capture/mail-reader.cjs');
const since = Date.now(); // 在业务请求发送验证码之前记录
// 业务请求发信后，在内存使用结果；不要打印 code 或整个返回对象。
const hit = await readCode({
  reader: 'graph', // 注册优先使用 outlook（8802）；这里只演示已有 Graph 适配器
  file: privateCredentialPath,
  mailbox: 'owner@example.test', // 实际读取的邮箱，必须匹配凭据文件
  recipient: 'alias@icloud.com', // 本任务的目标地址
  source: 'forwarded', // registration 的 recipient 必须等于 mailbox
  platform: 'apple', // Graph 支持 apple / microsoft / openai 的固定发件域规则
  since, until: undefined, excluded: seenMessageIds, timeoutMs: 30000,
});
if (hit) seenMessageIds.push(hit.messageId);
```

- 统一入口要求任务 since，范围近 24 小时，匹配 `since < received <= until`；没有向前放宽 90 秒。可传固定 until，未传则每轮使用当前时间。
- 使用完整 message_id 去重，保留字符串验证码的前导零；最新符合条件邮件无码、多码或同时刻多封有歧义时明确报错，不回退旧码。
- Graph 匹配精确 To/Cc 与固定发件域，拒绝 evilapple.com 等近似域。转发只支持 To/Cc 保留目标地址的邮件，不能用正文提及地址或“这封邮件进了真实邮箱”代替收件人证据。
- 复用既有 Graph 读取；不读取 Outlook vault，不把微软 Graph 授权解释为通用 IMAP 授权。Graph 仍是旧适配器，尚未迁入 8802；不要为 8802 已管理的重复邮箱再建立并发令牌刷新 owner。
- 8802 当前代码使用全局 codePlatform（本次配置为 apple），Graph select 也未请求原始收件人头。统一入口对它仅开放 Apple 注册用途，并明确拒绝 forwarded 或其他平台。请求携带 source/recipient/platform 不表示旧服务器已经实现这些字段。
- 8802 的拒绝保留固定 error code/status，不把缺授权、歧义全部变成“没有收到邮件”；Graph 上游错误原文不通过统一入口输出。
- 旧 webmail 脚本缺少可靠目标收件人与 message_id 证据，未接成自动兜底。IMAP 待供应商、授权方式和实际读取证据确定后再接。

本轮离线夹具验证了注册邮件和保留 To 的转发邮件筛选。尚未验证真实隐私转发邮件的头部形态、供应商可读性、当前授权或新码接收。Graph 旧读取仍只取最近一页；没有命中不能证明邮件不存在，不自动扩大时间窗或猜旧码。

## 登录 423、会话与交接

本地 `.cache` 与 `state` 的限定文本证据检查未找到可归属本次新注册任务的 HTTP 423/tooManyCodesSent 响应，不能断言历史 423 发生在短信发送还是验证阶段。未用锁定账号重现。

已有 `internal/appleweb/apple_auth_client.go` 的脱敏诊断分别记录 `signin_complete`、`auth_state`、`sms_send`、`sms_verify`、`trust`、`account_login`、条款读取/接受，以及 status、locked、too_many_sent、too_many_validated、已验证/已信任布尔值。后续只需核对这些白名单字段与同次 trace，不需记录响应正文、OTP 或令牌。

已有网页登录模块用 TwoFactorVerified / TrustEstablished 避免同一会话重复提交已接受的验证码及信任操作，离线用例覆盖下游出错后的复用。面板交接先 observe；activated + online 才跳过登录。写操作有 intent/回执，unknown 不重放，失败不会统一送回登录。

仍有两个边界不能混淆：

1. CLI `cmd/appleweb` 普通 login 模式仍走 Start；现存磁盘 web session 的复用主要在 keepalive/probe，不等于注册浏览器会话直接交接。此轮未擅自重构该路径。
2. `pipeline.py` 的每阶段两次预算按 Job 保存，`reserve_code` 的两次预算按供应商 + activation 保存；这不等于“跨所有 Job、所有入口每账号最多两次”。不要通过新 Job/新激活绕过保护。

## 里程碑与当前健康

| 阶段 | 实现与证据 | 当前结论 |
| --- | --- | --- |
| 注册 | 已有终态判定；用户报告表单/邮件码/短信码曾实跑 | 本轮未重跑；iframe 消失不单独算成功 |
| 登录 | GSA checkpoint、网页会话与面板 observe | 新注册后半链路没有本轮验收证据 |
| 条款/设备激活 | 既有 plan → activate、accepted checkpoint | checkpoint 是历史里程碑，不能替代当前健康 |
| 家庭 | 既有地区/角色校验与创建、加入、共享回读 | 已有账号历史成功不能代替新注册成功 |
| 8788 交接 | 18741 `/api/mail-bridge/accounts`、`/credentials` 已存在 | 要求账号未受保护、交接已完成、会话已验证、家庭及空间缓存新鲜；不等于8788已导入该新账号 |
| 邮件回流 | 统一入口和 Graph 严格筛选已实现 | 真实转发匹配、自动接入8788及后续取码仍待端到端验收 |

只读账号目录可见已有 accepted checkpoint 与三个 account-guard 并存；原健康会话报告 5735 设备查询 401、手动设备码可生成，三个锁定账号仍禁止发码。此轮没有重新探测或改变这些账号。设备码可生成不等于 Apple 接受，也不能承诺以后永远不需要短信。

Outlook 绑定的 8788 保护邮箱是长期依赖，不等于 Outlook 自动转发设置；不能因为普通库存业务空闲就释放保护地址。具体退役保护由既有协作任务负责，本轮没有修改 8788。

后续最小验收条件：在 8802 已明确支持逐请求平台和目标地址后接入该服务路径；用获授权、未受限制的单个对象核对新邮件元数据与一次完整交接。出现锁定、限流或不确定写结果即停，保留占用，不用批量登录或重复发码获取证据。

## 本轮验证

47 项 Node 离线测试通过，覆盖取码、库存汇总、等码生命周期及既有脱敏约束；注册脚本语法检查通过。Go 邮箱选择、自动请求与交接用例通过，网页认证及 CLI 既有回归通过。

另在 `878d251` 的隔离 checkout 仅应用本任务暂存补丁，重新运行上述 Node 测试和本次相关 Go 面板测试，全部通过，确认未依赖其他会话的未提交修改。`git diff --check` 通过。所有账号凭据、状态、响应、日志及生成物均未纳入提交。
