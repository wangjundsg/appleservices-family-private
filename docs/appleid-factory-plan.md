# account identifier 工厂 v0.2：注册 → 激活 → cloud platform+ → 共享组 → alias 池

> 本轮参数已锁定：地区统一、三家短信中继、自有域名邮箱、双代理源、目标 2 个共享组。
> v0.1 的通用框架保留，本版把「选哪条路、接哪些接口、先做什么」写死。

---

## 0. 决策摘要

| 项 | 决定 | 依据 |
| --- | --- | --- |
| 地区 | **US**（备选 JP） | alias 可用 + 仓库已有 `-confirm-us-50gb` 购买实现 + 存储值码流动性 + 号源最便宜 |
| 规模 | 2 组 = **12 个账号**（2 organizer + 10 member） | 席位 6/组，组织者需自带 cloud platform+ |
| 创建通道 | **Web 流程（Playwright 驱动）** 主力，设备侧 `setup.icloud.com` 兜底 | 老 HTTP 脚本已被 target service 关停（见 §2 实测） |
| 短信中继 | 三家共用 **sms-activate 兼容协议**，写一个适配器 | 三家 base URL 不同、动作集相同 |
| 邮箱 | 自有域名（cloudflare_temp_email）收码；QQ/Gmail 只做 alias 转发目标 | 免费域在 target service 侧信誉差 |
| 代理 | 711proxy / dataimpulse / clash 三源合一 `ProxyPool` | 仓库已支持 HTTP(S)_PROXY 与 SOCKS5 |
| 支付 | 2 个 organizer 手工兑存储值码，购买走仓库现成 `tools/icloudpurchase` | 量小，不值得先自动化充值 |
| 分工 | 本仓库：注册+激活+共享组+生成；alias 池交给 cloud platform-Privacy-Mail-v2 | 见 §6 接口合同 |

---

## 1. 为什么是 US

1. **alias 可用性**：alias service 在美区 cloud platform+ 正常提供；中国大陆区账号不含该能力，直接排除 CN（顺带排除仓库里的 `-confirm-cn-50gb` 路径）。
2. **购买路径已落地**：`tools/icloudpurchase` 现有 `-confirm-us-50gb`（USD 0.99/月），是仓库里唯一实现好的海外区订阅购买；JP 需要新写一套价格/条款/币种验收。
3. **共享组同区要求**：target service 共享组要求成员与组织者同区，所以「统一地区」不是偏好而是硬约束，选 US 等于选定整条链路的币种、条款、存储值码。
4. **号源与价格**：美国号在三家短信中继平台库存最大、单价最低（常见 $0.1~0.5）；JP/HK 号源少且更贵。
5. **存储值码**：美区 App Store 存储值码流通性最好，$5 面额足够开 2 个月的 50GB。
6. **验收信号现成**：`.claude/skills/apple-account-region-change` 已写明美区 50GB 的验收是 `currency=USD && price_minor=99`，直接复用。

**备选 JP**：若实测美国号被 target service 拒得厉害（`valid=false` 或校验码不发），切 JP；代价是要补一套 `-confirm-jp-50gb`（¥130/月）与日区存储值码。

---

## 2. M0 实测（本轮已完成，直接改变路线）

### 2.1 老开源脚本全部失效

`steveaoki23/valid-apple` 用的 `POST https://appleid.apple.com/account/validation/appleid` 现在返回 **HTTP 503**。也就是说 2020~2022 年那批"请求方式"注册脚本，协议层已经不可用，不能照抄。

### 2.2 现代门户的真实结构

`GET https://appleid.apple.com/account`（219,951 字节）→ SPA，`<script id="app_config">` 里给：

- `"scnt": "AAAA-…"`：会话级防伪串，**必带**
- `X-target service-I-Request-Context: ca`：管理页的上下文；创建流是独立上下文
- `X-target service-Api-Key`：**不在首页**，登录后由 `/account/manageData/get` 返回 `apiKey` 再挂到后续请求上
- `X-target service-I-FD-Client-Info`：浏览器指纹 JSON（UA/语言/时区/版本/指纹串）
- 管理 API 前缀：`/account/manage/...`

从 bundle 里读出的 alias 端点，与本仓库 `icloud/hme.go` 的既有实现一致：

```
GET  /account/manage/email/private            列出别名
GET  /account/manage/email/private/{id}.em    单个别名
POST /account/manage/email/private/add        新增
POST /account/manage/email/private/add/complete
GET  /account/manage/forwardemail             转发目标
```

> 结论：**alias 这一段不用重造**，仓库已有 `List / Generate / Reserve / Update / SetActive / Delete / UpdateForwardTo`。

### 2.3 创建流程已完整抓通（详见 [signup-protocol.md](signup-protocol.md)）

浏览器驱动的全套端点、表单字段、请求头、`x-apple-hc` hashcash、校验码接口与创建请求体已实测落地。

### 2.4 已落地的探针

新增 [`tools/appleidcheck`](../tools/appleidcheck/main.go)：

- 抓 `app_config` 里的 `scnt`（已跑通，输出 `bootstrap ok`）
- 对候选邮箱做创建前校验（target service 返回 `valid` / `used` / `appleOwnedDomain` / `isRecycledDomain`）
- 支持 `-proxy`、`-json`、`-dump`

当前瓶颈：target service 把创建前校验挪到了新上下文，`/account/validation/appleid` 已 503。
下一步就是 **用本机 Chrome 抓一次真实创建流**，把这套头/端点补齐。本机已具备 Node 24 + 原生 Chrome，装 `playwright-core` 即可用 `channel: 'chrome'`，不需要下载浏览器。

---

## 3. 创建通道与推进顺序

### 通道 1（主力）Web 创建 + Playwright 驱动

- 用 `playwright-core` 起真实 Chrome（非 headless 首发，便于人工过校验码）
- 首次运行只做**抓包**：记录每次 XHR 的 URL、请求头、请求体、响应体，落 HAR/JSON
- 抓到完整序列后，把「填表 + 取邮箱校验码 + 取短信校验码 + 提交」脚本化；校验码环节保留人工/识别服务平台兜底
- 优势：真实浏览器指纹，绕过 SPA 反自动化成本最低；劣势：需要浏览器进程

### 通道 2（兜底）设备侧 setup.icloud.com

- 仓库已有 anisette/nativeadi/aps/device 全套设备身份
- `icloud/delegate.go:76` 的 `windows-aos` profile 指向 `/setup/login_or_create_account`（"登录或创建"），值得用不存在账号打一发看响应字典
- 适合：Web 通道被账号受限率高时的第二批

### 通道 3（备选）纯 HTTP 复刻

- 只有把通道 1 的 HAR 吃透之后才做，收益是可无头并发
- 现在做等于瞎猜，不做

---

## 4. 资源适配器

### 4.1 短信中继：一个适配器覆盖三家

三家都是 **sms-activate 兼容协议**，只是 base URL 不同：

| 供应商 | base URL | 备注 |
| --- | --- | --- |
| GrizzlySMS | `https://api.grizzlysms.com/stubs/handler_api.php` | 官方文档明确「与 sms-activate API 兼容」 |
| Hero-SMS | `https://api.hero-sms.com/stubs/handler_api.php` | 同一套 handler_api 形态，以控制台给的地址为准 |
| SMSIndia | `https://api.smsindia.pro/stubs/handler_api.php` | 文档原文给出该地址 |

动作映射（全部 GET，`api_key` 必带）：

| 用途 | action | 关键参数 | 返回 |
| --- | --- | --- | --- |
| 取号 | `getNumber` | `service`、`country`、`maxPrice`、`providerIds` | `ACCESS_NUMBER:<id>:<phone>` |
| 查码 | `getStatus` | `id` | `STATUS_WAIT_CODE` / `STATUS_OK:<code>` / `STATUS_CANCEL` |
| 改状态 | `setStatus` | `id`、`status` | 1=就绪 3=重发 6=完成 8=取消 |
| 余额 | `getBalance` | — | `ACCESS_BALANCE:<n>` |
| 实时价 | `getPrices` | `service`、`country` | 价格/库存表 |
| 服务码表 | `getServices` | — | 用它反查 target service 的服务码 |

要点：

- **target service 的服务码需要实测**：候选 `ts`，以 `getServices` 返回为准，别写死
- 号码 20 分钟内可取消退款（sms-activate 系规则），失败要立刻 `setStatus=8` 止损
- 号码不复用：一个号只喂一个 account identifier

### 4.2 邮箱：cloudflare_temp_email 当发号器，QQ/Gmail 只做转发

分工：

- **注册用址**：自有域名随机前缀，走 cloudflare_temp_email
  - 建址：`POST /api/new_address`（返回 `jwt`）或管理端 `/admin/new_address`
  - 读信：`GET /api/mails?limit=&offset=`，头 `Authorization: Bearer <address_jwt>`
  - 私有站点另加 `x-custom-auth`
- **alias 转发目标**：QQ / Gmail 各一个，用仓库 `HideMyEmailClient.UpdateForwardTo` 设置
- **不要**拿 QQ/163 当注册用址：免费域在 target service 侧有 `appleOwnedDomain` / `isRecycledDomain` 标记，浪费号码成本

上线前先做域名打分：用 `tools/appleidcheck` 对两个自有域名各跑 3 个随机地址，记录 `valid` 结果与响应耗时。

### 4.3 代理：三源合一

- 统一形态 `http://USER:PASS@HOST:PORT`，粘性用 `sid` 或固定端口
- 优先级：dataimpulse（住宅）> 711proxy 动态 > clash 机场（只做备用/连通性测试）
- 仓库现状：`HTTP_PROXY`/`HTTPS_PROXY` 已生效；SOCKS5 走 `APPLESERVICES_SOCKS_PROXY`，`tools/proxycheck` 可验出口
- 铁律：**一账号一出口，出口国 = US，注册全程不换 IP**；换 IP 等于换人

---

## 5. 状态机（12 个账号的实际链路）

```
S0 资源预留   email(自有域名) / phone(短信中继) / proxy(US 粘性) / device identity
S1 创建提交   email + password + birthday + name + region=US
S2 邮箱验证   6 位码（temp_email 轮询）
S3 手机验证   SMS 6 位码（短信中继）
S4 账号落地   DSID + ADSID + idmsa 会话
S5 激活       条款 → loginDelegates → mmeAuthToken
S5.5 设备注册 受信任设备（购买与本地出码的前置，仓库 cmd/trusted-device 已有）
S6 冷却       静置 ≥24h，禁止新号立刻消费
S7 订阅       organizer 兑存储值码 → 买 US 50GB（tools/icloudpurchase -confirm-us-50gb）
S8 共享组     create-family → invite/accept（PET in-person）→ 回读两账号
S9 存储共享   JOIN_FAMILY → participating/available/enabled 三真
S10 alias       每账号 Generate/Reserve → 导出到 alias 池
```

失败分类不变：可重试 / 换资源 / 永久失败 / **结果未知（禁止回放，先回读）**。

---

## 6. 三个仓库的接口合同

```
cloudflare_temp_email  ── 地址 + 收码 ──▶  appleservices-family-private
                                              │  注册 / 激活 / 设备注册 / 购买 / 共享组
                                              │  生成 alias 别名
                                              ▼
                                   cloud platform-Privacy-Mail-v2（只做池管理）
```

建议的池导入格式（他仓库私有，我这里先定个最小合同，落地时按他实际字段改名）：

```json
{
  "account": "xxx@yourdomain.tld",
  "alias": "abc123@icloud.com",
  "label": "site-a",
  "note": "",
  "forward_to": "you@gmail.com",
  "family_id": "org-01",
  "created_at": "2026-11-13T01:00:00Z",
  "state": "active"
}
```

投递方式二选一：本仓库写 `state/hme-pool.jsonl`（owner-only），cloud platform-Privacy-Mail-v2 定时拉；或者本仓库直接 POST 到它暴露的 import 端点。**先按文件落地**，两边都能跑起来再谈接口。

---

## 7. 要写的代码（按文件）

```
tools/appleidcheck/           已建：邮箱预检 + bootstrap 提取；下一步加 -capture（Playwright 抓包）
cmd/registrar/                新建：单号/批量入口，读 accounts 库，跑状态机
internal/signup/web.go        新建：Playwright 驱动（先脚本化表单，再逐步替换为直连）
internal/signup/aos.go        新建：设备侧探针（/setup/login_or_create_account）
internal/mailbox/tempemail.go 新建：new_address + mails 轮询取码
internal/smscode/smsactivate.go 新建：三家共用，base URL + key 从配置读
internal/proxypool/pool.go    新建：三源统一，粘性绑定账号
internal/accountdb/sqlite.go  新建：账号/资源/尝试/共享组四张表
```

---

## 8. 开源参考清单（能抄什么、不能抄什么）

| 项目 | 能抄 | 不能抄 |
| --- | --- | --- |
| [steveaoki23/valid-apple](https://github.com/steveaoki23/valid-apple) | 邮箱预检的字段语义（`valid`/`used`/`appleOwnedDomain`/`isRecycledDomain`） | 端点已 503，代码废了 |
| [liukelin/create_appleid](https://github.com/liukelin/create_appleid) | Selenium 注册全流程、校验码环节的人工/识别服务兜底、**"新号登录 App Store 要激活"这个坑** | Python 3.4 + 老页面选择器 |
| [pplulee/appleid_auto](https://github.com/pplulee/appleid_auto) | 锁号检测→改密→上报的闭环、删设备、Selenium 集群、代理池架构 | 它是"保号"不是"造号" |
| [dadoum/Provision](https://github.com/dadoum/Provision) / anisette-v3-server | 设备指纹与 ADI provisioning 的规范做法 | — |
| OpenBubbles / rustpush | `setup.icloud.com` 设备侧协议细节（与本仓库 `icloud/delegate.go` 的注释同源） | — |
| [dreamhunter2333/cloudflare_temp_email](https://github.com/dreamhunter2333/cloudflare_temp_email) | 邮箱 API（new_address / mails / admin） | — |
| [wanghaisheng/auto-gen-apple-ids](https://github.com/wanghaisheng/auto-gen-apple-ids) | 索引页，把上面几条路线串起来 | 本身不含实现 |
| [badstreff/target service-ID-Verifier](https://github.com/badstreff/target service-ID-Verifier) | 账号存活校验思路（造完的号要验活） | — |

---

## 9. 里程碑（按 12 个账号排，不按产能排）

| 阶段 | 内容 | 完成标志 | 估时 |
| --- | --- | --- | --- |
| M0 | Playwright 抓一次真实创建流，补齐头/端点 | `docs/signup-protocol.md` + HAR | 0.5~1 天 |
| M1 | 单号脚本化：填表→邮箱校验码→短信校验码→落地 | 1 个新号能登录 `cmd/login` | 1~2 天 |
| M2 | 资源接管：temp_email 取码、短信中继取码、代理绑定 | 全自动无人值守跑出 3 个号 | 1~2 天 |
| M3 | 激活+设备注册+冷却 | 3 个号全部 `mmeAuthToken` 成功 | 1 天 |
| M4 | organizer 兑卡买 50GB、建组、拉 5 个成员 | 第一组 6/6 且存储共享三真 | 1~2 天 |
| M5 | 第二组复制 + alias 池导出 | 12 号、2 组、池子能出别名 | 1 天 |

**成本估算（12 号）**：号码 ~$0.5×12≈$6；代理流量 <$5；存储值码 2×$5；合计 **≈¥100~150**（不含时间）。

---

## 10. 红线

1. 新号 24h 内不购买、不入组、不大量生成 alias
2. 一账号一 IP 一设备身份，绝不共用
3. 写请求超时 = 结果未知，**先回读 target service 状态再决定**
4. 密码/令牌/校验码只进账号目录，不进日志、不进 Git
5. 存储值码手工兑（2 个号，自动化性价比为负）
