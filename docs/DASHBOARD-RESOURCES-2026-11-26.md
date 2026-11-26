# 面板注册资源接线与切换

本次完成 `internal/dashboard` 的资源工作台、配置保存、只读检查和任务清单接线。新自动任务默认 OTPGmail + Hero；没有自动购买邮箱入口。旧 Outlook 读取逻辑仍保留给显式旧配置，既有账号、邮箱清单和运行任务未迁移。

## 页面与接口

在“新建 Apple ID → 注册资源”配置以下内容：

- OTPGmail：当前 Windows 用户的 DPAPI 凭据文件、已经购买并核实归属的私有订单文件。页面不接收明文密钥，邮箱只显示脱敏状态。
- Hero：PL / CO / US、普通激活或 24 小时租期、USD 单号码与单账号累计预算。一天租期需要单独确认价格可能变化；供应商租赁接口没有价格硬上限。
- 代理：沿用现有 `roxy.proxyInfo`，或由本机管理员预置的 `proxyProfiles.711` / `proxyProfiles.dataimpulse`。只选现有配置和端口，密码不返回浏览器。

接口均沿用面板 loopback Host、Origin 与 `X-App-Token` 检查：

| 接口 | 行为 |
| --- | --- |
| `GET /api/register/resources` | 本机配置状态；凭据文件存在不等于供应商验证成功 |
| `POST /api/register/resources` | 保存 `settings` 与 `purchase`；保留原配置其他字段，不开启购买开关 |
| `POST /api/register/resources/check` | `resource=mail/sms/proxy`，只读查询，不购买、发码、退款或创建账号 |

查询只在点击时发生，不随页面轮询重复调用。结果显示时间；保存设置后清除旧检查提示。不存在、错误或未知回执不会变成“就绪”。Hero 显示供应商返回的活跃订单数量，不把数量当作可分配号码。普通报价未声明币种时不会冒充 USD；一天报价使用请求 USD 后的客户零售价，明确不是保证价格。

代理检查通过配置中的 SOCKS5 出口访问固定 HTTPS 连通探测地址。成功只证明连通，未验证实际出口国家、流量余额或 Apple 可接受性；失败不换出口。新任务按账号地区展开 `{country}`（DataImpulse 小写、711 大写），不再把手机国家当作账号地区。默认账号地区 US，家庭任务沿用显式地区。

## 本机启动配置

沿用现有 `APPLESERVICES_REGISTER_CONFIG`，未指定时读取 `-dir` 下的 `registration-config.json`。现有启动器 `tools/apple-pipeline/panel.ps1` 通过 `provider-environment.ps1` 加载短信密钥；本次没有修改这两个文件。

Hero 接受 `SMS_API_KEY_HERO` / `HERO_API_KEY`，Grizzly 接受 `SMS_API_KEY` / `GRIZZLY_API_KEY`；自动和手动任务读取同一优先顺序，启动子进程时映射到购号器已有变量名。不再要求 Hero 单供应商任务配置 Grizzly 密钥。使用现有启动器时仍会检查别名冲突。

以下是新增配置结构示例，路径为占位值，预算空白表示未启用。**合并到既有私有配置，不要用它覆盖 Forge、代理凭据及其他字段。** 也可以直接在新面板中保存这些字段。

```json
{
  "resources": {
    "mailProvider": "otpgmail",
    "credentialFile": "D:\\PRIVATE\\otpgmail\\api-key.dpapi",
    "orderFile": "",
    "smsProvider": "hero",
    "phoneCountry": "PL",
    "durationHours": 24,
    "acceptRentalPriceChange": false,
    "proxyProfile": "current",
    "proxyPort": 10005
  },
  "purchase": {"maxPrice": "", "maxTotal": "", "currency": "USD"}
}
```

首次读取旧配置时，邮箱默认 OTPGmail，短信默认 Hero，国家 PL，租期默认普通激活（`durationHours=0`）；不会暗中确认一天价格变动或购买。旧池用途需要显式 `resources.mailProvider=outlook`，原 `codePoolConfig` 不删除。

邮箱订单文件沿用现有取码器契约：`provider=otpgmail`、`orderId`、`email`、`warehouse`、`credentialFile`。必须为 Gmail、合法订单标识和支持的仓库，凭据路径必须与资源设置一致。不要将文件、凭据、认证回执或截图加入 Git。

自动任务开始前：检查预算与购买开关、Hero 凭据、邮箱订单本地占用，再读取该订单的供应商状态。只接受 `waiting_code`；排队、已完成、取消、绑定不符或读取失败均停止，尚未购号。订单快照写入新任务的受限目录，通过 `otpgmail_order_file` 交给现有执行器。邮箱和订单 ID 双重排重，失败/未知任务不自动释放。同一订单的外部 CLI 操作仍须由操作者协调归属，不能同时交给另一个独立分配者。

任务清单传递 `sms.providers=["hero"]`、`duration_hours`、`accept_rental_price_change` 和既有预算；购号、归属、消费回执和未知结果防重继续由现有 `sms_supply.py` 负责。协议自动任务同时设置 `protocol.auto_codes=true`，读取同一邮箱订单；人工校验及实际创建流程未改动。已创建任务保留自己的订单和代理快照，修改设置不迁移旧任务。

## 验证结果

2026-11-26 北京时间，使用独立状态目录与 18743 端口验证，购买开关关闭，无账号登记，worker 路径禁用。没有启动注册、购买资源、发送验证码或操作退款。

- Go dashboard、proxypool 和 dashboard 命令包测试通过；新增覆盖配置保存和脱敏、授权保护、坏配置不覆盖、Hero 只读请求、报价结构、供应商拒绝、订单占用、任务快照、取码配置及账号地区。
- Node 供应商桥接用例通过，覆盖订单绑定、取消/完成/排队状态、授权失败不重试和敏感字段不返回。
- Playwright 实际页面检查通过：配置保存后重载、一天价格确认不默认勾选、关闭购号或缺少订单时禁用提交、真实查询结果展示、模拟供应商错误展示、390px 手机宽度无横向溢出。测试拦截所有注册 POST。
- OTPGmail 余额与 Gmail Apple 库存真实只读查询成功；未绑定邮箱订单，不能据此声明邮箱已可用于创建。
- Hero 余额、活跃订单及 PL 一天客户报价真实只读查询成功。数值随主会话操作和供应商库存变化，不把快照写成长期配置或注册成功证据。
- 现有 DataImpulse 固定代理真实连通检查成功。711 仅通过配置展开单元测试，本轮未用 711 凭据联网验证。

认证响应、余额明细、订单资料、日志、二进制、截图和隔离状态仅保留本机忽略目录。生产 18741 及已激活账号 worker 没有重启。

## 可审查的生产切换顺序

1. 与创建流程负责会话核对没有正在创建、等待输入或未知写结果的任务；先核实所有待定购买/创建回执。当前正在创建时，不切换生产。
2. 在独立文件名构建新版 dashboard，核对本次提交及启动环境；保留旧 `dashboard-activation-fixed.exe` 作为回退。不得全进程结束、修改已激活账号或删除状态。
3. 确认实际启动参数中的 `-dir`、`-repo`、worker/native 配置和端口。复用已有 provider-environment 读取器及本机私有配置，密钥不放在命令行。隔离验证用的空状态目录、禁用 worker 和测试预算不得带入生产。
4. 只替换持有 18741 的 dashboard 进程，保留所有账号 worker。启动时 Bootstrap 可能检查任务归属，因此必须先完成步骤 1，不能用并行生产面板规避该检查。
5. 回读新资源接口、账号在线/已激活状态及供应商只读结果；确认没有新增注册任务或购号意图。然后由创建流程负责会话绑定本任务订单并执行已有验收。
6. 切换失败时只回退 dashboard 程序和启动参数，不回滚订单/账号状态，不自动重试购买或创建。

本次交付包含隔离预览与切换方案，**未宣称生产部署或新账号创建完成**。
