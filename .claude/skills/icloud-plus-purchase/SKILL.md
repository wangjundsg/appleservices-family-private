---
name: icloud-plus-purchase
description: 通过苹果原生商店链路完成 iCloud+ 订阅购买、关闭自动续费与状态核验。当需要为一个 Apple 账号购买 iCloud+ 储存方案（如中国大陆 50GB ¥6/月）、取消或关闭其自动续费、核验配额与订阅状态，或排查购买被苹果拒绝（failureType 3038 / 3064 / 2002 / 2034 / MZCommerce.SystemError）时使用。覆盖条款接受、确认下单、账号前置条件判定与全部已验证的参数形状。不适用于 App Store 应用内购买、非储存类订阅，或需要付款方式录入/变更的场景。
---

# iCloud+ 订阅购买与核验

本 skill 记录了一条**已实测走通**的 iCloud+ 购买链路：中国大陆 50GB ¥6.00/月
（`salableAdamId=6449562876`），并包含关闭自动续费与状态核验。

全程使用原生商店协议（plist over HTTPS），不依赖浏览器、不驱动官方客户端 UI。

本项目 WebUI 必须先读取 Apple 实时 50GB 报价再启用购买：`CNY 600` 路由到已验证的中国区流程，`USD 99` 路由到 `storage-skill-purchase-us`；其他币种、价格或缺失报价保持禁用。不要相信可能滞后的独立 `region` 字段。提交前必须再次显示精确套餐、币种、月价和自动续费后果并取得确认；读取方案或验证 Store 会话都不等于购买授权。

## 当前项目入口与证据边界

本文的具体成功参数来自一次已完成购买的历史实测。HTTP 状态、放行率、令牌寿命及某个失败的归因都是该次观测，不能保证另一个账号或当前服务器具有相同行为。尤其不能仅凭一次 delegate 失败断言账号未激活；激活验收使用 `.claude/skills/icloud-mac-nac-activation/SKILL.md` 的有效 MME 与实时配额标准。

取得用户对精确套餐的购买授权后，从项目根目录运行：

```powershell
go build -o bin/icloudpurchase.exe ./tools/icloudpurchase
.\bin\icloudpurchase.exe -claude-skill -native-store -confirm-cn-50gb `
  -dir state/accounts/<selected-account> `
  -accepted <private-accepted-icloud.json> -journal <new-private-attempt.jsonl> `
  -local-store-code -store-session-out <new-private-store.json>
```

关闭已购买套餐的自动续费时，必须复用同一账号、同一物理 GUID 的 Store 会话：

```powershell
.\bin\icloudpurchase.exe -claude-skill -native-store -cancel-auto-renew `
  -dir state/accounts/<selected-account> `
  -accepted <private-accepted-icloud.json> `
  -journal <new-private-cancel.jsonl> `
  -store-session-in <private-store-session.json>
```

取消流程是固定的：读取当前订阅 ID → 发起 `cancelSubscription=true` → 只提交 Apple 返回的确认参数 → 重新读取订阅详情。只有详情中的 `isAutoRenewEnabled=false` 才算完成；服务容量应仍为 50GB，直到当前周期结束。若会话失效，停止并重新建立同账号商店会话，不能猜测订阅 ID 或重复提交取消请求。

凭据通过进程环境输入。所有输出路径必须位于仅当前用户可访问的 Git 忽略目录。`-local-store-code` 生成成功不代表商店接受；仅在 Apple 明确请求 2FA 后向同一会话附加新验证码。没有本账号已验证的本地码时使用新的用户设备码，不能借用其他账号的码。

可用 `-store-check-only` 只验证商店认证，或用 `-store-session-in` 复用本账号和物理 GUID 绑定的有效商店 checkpoint。该模式最多执行 20 次认证路由尝试；应用错误及 429 停止，购买或不明确认响应不自动重放。

### 验证码时序与当前 bag 路径

默认 CLI 与面板共用 `AuthenticateMatchedSkillStore`：从同一个 Apple bag 取得认证地址、SAP setup 和证书，并保留该地址及签名请求体。文末固定 `native/fast/` 示例是历史实测路径，不应覆盖当前 bag。仅调整 User-Agent 或反复重试空白 HTTP 响应不等于完成账户审核。

新账户还可能缺少媒体商店首次使用初始化。一次对照中，匹配 bag 的认证返回 HTTP 200、`failureType=5005`；同账号通过官方 Apple Music 网页登录和双重认证后，网页自动完成账户转换/注册，随后同一签名认证路径取得 Store 令牌。过程中没有订阅 Apple Music。此案例只支持将首次使用初始化列为检查项，不能把所有 5005 都断定为同一原因，也不能把 iCloud 条款接受当成媒体商店初始化。应检查官方页面实际提示，沿用它返回的流程，并在完成后重新核验原生 Store 认证；不要硬编码或盲目重放账户转换接口。

验证码只能提交到已经收到 Apple 2FA 应用响应的同一个 cookie 会话。不要在挑战出现前反复重启登录，也不要把多个验证码写死在命令行：提前到达的验证码可能在真正提交前失效。优先使用 `-store-code-file`，由程序在挑战阶段持续读取该文件；文件应只包含最新六位数字、限制为当前用户可读，并在提交后立即删除。程序必须输出“已进入挑战”和“已提交验证码”的阶段信息，避免把等待状态误报成密码错误。

SAP 登录必须使用同一份 Store bag 返回的三元组：`authenticateAccount`、`sign-sap-setup` 和 `sign-sap-setup-cert`。不要无条件把 bag 的 legacy MZFinance 地址改写成 native/fast；需要诊断时使用 `-signed-store -matched-bag`，让认证地址和签名配置保持匹配。该模式仍使用物理 Store GUID，不能替换账号的 iCloud Mac/ADI 身份。

Apple 已进入应用级 2FA challenge 后，同一个六位码必须在该 cookie 会话的纯路由重试中复用；204、无 `Location` 的 301/302、403/404/500/503 都没有消费验证码。验证码 provider 只读取一次，随后把该码保存在进程内，直到 Apple 返回应用响应、明确拒绝或会话结束；不能在每个路由响应后再次向用户要码。读取后的交接文件仍应立即删除，认证函数返回前清空内存副本。

默认保持直连并显式禁用环境代理。如果用户明确授权临时代理，只把它接入 Store bag、SAP setup/certificate、authenticate 与后续 Store 请求的完整同一链路，不能出现 bag/签名直连而 authenticate 走代理的混合路径。本项目可用忽略目录中的 `.cache/store-proxy-10808.enabled` 临时启用 `127.0.0.1:10808`；购买和订阅/配额核验完成后立即删除该开关。代理认证仍不等于购买成功。

`APPLESERVICES_SKILL_STORE_PROFILE=compact`、附加 `-signed-store` 或 `-appstore-profile` 是显式诊断选项，不是已验证的购买修复。当前目标曾在签名 App Store 对照收到空体 406/400，其他对照仍为通用空白/HTML 响应，均未取得商店令牌。不要把通用 HTTP 错误归因成密码或验证码错误，也不要重复同一失败实验而没有新证据。

## 关键前提

### 物理网卡 GUID

`guid` 必须是**物理网卡**的 MAC（无分隔符大写十六进制）。虚拟网卡（VMware `00:50:56`、
Hyper-V/WSL `00:15:5D`、TAP `00:FF`）会被苹果**静默丢弃**（HTTP 204）。

```powershell
Get-NetAdapter | Where-Object Status -eq Up |
  Select-Object Name,InterfaceDescription,MacAddress
```

选物理接口（如 `Intel(R) Wi-Fi`）的地址，去掉 `-`，转大写。本机实测：
`28-A4-4A-BA-C4-26` → `28A44ABAC426`。

### 账号必须是"iCloud 已激活"状态

这是最容易踩的坑。苹果对未激活账号会回 `failureType 3064`
（`MZCommerce.StoragePurchaseOnNonStorageCapableAccount`，
`"Only Apple Accounts used for iCloud can purchase an iCloud+ plan."`）。

**判定方法**（不要靠猜，跑这个探针）：

```bash
go build -o /tmp/iclouddelegate.exe ./tools/iclouddelegate/
/tmp/iclouddelegate.exe <appleid> <password> [code] [state-dir]
```

- 输出 `iCloud login OK` → 账号可用，可以购买
- 输出 `mobileme delegate has no service-data (status-message: "A server problem is
  blocking Apple ID sign in. Try signing in later.")` → **账号侧未激活**，购买必然 3064

**"在 Mac/iPhone 上登录过 iCloud" ≠ 已激活。** 实测同一账号会先报上述错误、
数天后才被苹果放行（本机日志对照：某账号 10-06 01:45 仍报
`MOBILEME_TERMS_OF_SERVICE_UPDATE`，10-07 14:56 配额才成功落盘）。
官方客户端自己的日志里也是同一条错误，可作为独立佐证：

```
%LOCALAPPDATA%\Packages\AppleInc.iCloud_*\LocalCache\Local\Logs\AOSKit\AOSKit-iCloudHome.*.log
  ERROR CreateCFErrorFromRequest  Complete error ID: MOBILEME_TERMS_OF_SERVICE_UPDATE
  _runStorageUsageThread  Storage usage request failed ... httpStatusCode=401
```

账号未激活时**不要继续尝试购买**——参数再对也会被 3064 拒绝。

## 第一步：建立商店会话

```
POST https://auth.itunes.apple.com/auth/v1/native/fast/     ← 末尾斜杠必需
Content-Type: application/x-www-form-urlencoded
User-Agent:   Configurator/2.18 (Macintosh; OS X 15.4.1; 24E263)
```

plist 请求体：

```xml
<key>appleId</key><string>…</string>
<key>attempt</key><string>1</string>
<key>guid</key><string>28A44ABAC426</string>
<key>password</key><string>…</string>       <!-- 2FA 验证码直接拼接在密码后 -->
<key>rmp</key><string>0</string>
<key>why</key><string>signIn</string>
```

从响应取 `dsPersonId`（商店 DSID）与 `passwordToken`；从响应头取
`X-Set-Apple-Store-Front` 和 `pod`。

**苹果边缘会概率性丢弃该请求**（实测 `204` / `301` / `404` / `500` / `503` 混合，
放行率约 1/10～1/20）。必须循环重试，成功标志是拿到 `dsPersonId` + `passwordToken`。
`204` 表示被静默丢弃，**重试即可，不是验证码错误**。

把 `dsid` / `token` / `storefront` / `pod` / 全部 cookie 持久化到本地
（本实现用 `session.json`），后续所有请求复用，避免重复登录消耗限流额度。

**`passwordToken` 有效期很短（数分钟）。** 过期后购买会回
`failureType 2034`（`MZCommerce.ExpiredPasswordToken`）。此时**重新登录**拿新令牌，
不要以为购买被拒绝。

## 第二步：接受商店媒体服务条款（仅首次）

若购买回 `failureType 3038` / `MZCommerce.TermsNotAccepted`，说明该账号尚未接受当前
媒体服务条款。先读状态：

```
GET https://p<pod>-buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/termsCheckSrv?guid=<MAC>
→ {"isCurrent":false,"latestTermsVersionId":"290399","termsContentText":"…"}
```

**条款页只认 AppStore 指纹。** 用 Configurator 或 iTunes 的 UA 访问同一 URL 只会
返回 1 字节空体：

```
GET …/com.apple.jingle.app.finance.DirectAction/termsPage?xToken=<token>&salableAdamId=6449562876
    &hasDoneAgeCheck=true&origPageLocation=Buy&needDiv=0&guid=<MAC>&price=6000&productType=A
    &origPage=Software-1581031638&hasAskedToFulfillPreorder=true&buyWithoutAuthorization=true
    &appExtVrsId=843583569&isBuyExcepn=true&pricingParameters=STDQ&l=en
User-Agent: AppStore/3.0 (Macintosh; OS X 15.4.1; 24E263)
→ 60KB 文档，内含同意表单
```

从返回文档解析出表单（**字段名是每次渲染生成的数字，必须原样回放**）：

```html
<form method="post"
      action="https://buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/agreeToTermsUpdate?termsId=290399">
  <input id="iagree"   name="2.0.1.1.3.0.5.1.1.7.3.9"  value="…" type="checkbox">
  <input id="continue" name="2.0.1.1.3.0.5.1.1.7.3.17" value="Agree" type="submit">
</form>
```

提交时**必须使用同一个持久 cookie jar 跨「取条款页」与「提交同意」两步**。每步新建 jar
会被回 `failureType 2002`（`MZCommerce.BadPasswordToken`，`"Your password has changed"`）
——这是误导性错误，不是密码问题。

成功后返回 `Congratulations / You've accepted the new Terms of Service` 页面，
3038 消失。

## 第三步：下单并确认

```
POST https://p<pod>-buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/buyProduct?guid=<MAC>
Content-Type: application/x-apple-plist
User-Agent:   Configurator/2.18 (Macintosh; OS X 15.4.1; 24E263)
X-Dsid / iCloud-DSID: <商店 DSID>
X-Token:              <passwordToken>
X-Apple-Store-Front:  <上一步响应头的值>
Cookie:               <会话 cookie>
```

请求体（**这一组参数缺一不可**）：

```xml
<key>appExtVrsId</key><string>843583569</string>
<key>appAdamId</key><string>1581031638</string>
<key>cc</key><string>CN</string>
<key>guid</key><string>28A44ABAC426</string>
<key>hasAskedToFulfillPreorder</key><string>true</string>
<key>hasDoneAgeCheck</key><string>true</string>
<key>buyWithoutAuthorization</key><string>true</string>
<key>needDiv</key><string>0</string>
<key>offerName</key><string>com.apple.icloud50GB_monthly</string>
<key>origPage</key><string>Software-1581031638</string>
<key>origPageLocation</key><string>Buy</string>
<key>price</key><string>6000</string>
<key>pricingParameters</key><string>STDQ</string>
<key>productType</key><string>A</string>
<key>storagePlanInBytes</key><integer>53687091200</integer>
<key>salableAdamId</key><integer>6449562876</integer>
```

> **`offerName` + `storagePlanInBytes` + `cc` 是识别储存订阅的关键。**
> 缺任一项都会得到 `MZCommerce.SystemError`（`"Unable to process your request."`），
> 它看起来像瞬时故障、可重试，实际是**确定性失败**。不要在这上面浪费时间。

成功时会返回确认弹窗（**这就是收银台**）：

```
dialogId    SRVOrderManager.ConfirmBuyingAutoRenewingSubscription
message     iCloud+ with 50 GB of storage
explanation Plan automatically renews at ¥6.00/month starting from 7 Nov 2026 until cancelled.
okButtonAction.kind      Buy
okButtonAction.buyParams cc=CN&salableAdamId=6449562876&…&hasConfirmAutoRenew=true&nextChargeDate=…
```

**确认下单 = 用苹果给的 `buyParams` 原样重发**（不要自己拼参数），并带上
`hasConfirmAutoRenew=true` 与 `hasShownExtraConfirmationNeededForBuyRecurringItem=true`。

部分 Store pod 会省略 `dialogId`，但仍返回 `okButtonAction.kind=Buy` 和有效的 `buyParams`。此时以 Buy action 和参数校验为准，继续原样提交确认；不能因为 `dialogId` 为空就返回 `confirmation-required`。

成功响应：

```xml
<key>jingleDocType</key><string>purchaseSuccess</string>
<key>jingleAction</key><string>purchaseProduct</string>
<key>status</key><integer>0</integer>
<key>metrics</key><dict>
  <key>itemIds</key><array><integer>6449562876</integer></array>
  <key>price</key><real>6.00</real>
  <key>currency</key><string>CNY</string>
```

## 核验（必做，不要只凭购买响应下结论）

```
GET https://speedysub.itunes.apple.com/subscription/v3/manage/list
GET https://speedysub.itunes.apple.com/subscription/v3/manage/detail?subscriptionId=<id>
```

`detail` 里的 `specialSubInfo` 是苹果报的**实际配额**——这是"50GB 是否真的生效"的唯一硬证据：

```json
"specialSubInfo": { "storageAmount": "50 GB", "storageUsage": "10.3 KB" }
"isAutoRenewEnabled": true,
"latestPlan": { "displayName": "iCloud+ with 50 GB of storage",
                "paidPrice": "¥6.00", "expirationDateFormattedInShortMonthDayYear": "7 Nov 2026" }
```

## 关闭自动续费

苹果的"取消订阅"效果是**关掉自动续费**，服务保留到当前周期结束。

**第 1 步**——发起取消：

```
POST https://p<pod>-buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/buyProduct?guid=<MAC>
Content-Type: application/x-www-form-urlencoded

subscriptionId=<subscriptionId>&cancelSubscription=true&salableAdamId=6449562876
```

返回确认弹窗 `MZCommerce.SubscriptionServices.ConfirmCancelSubscription`，
其 `okButtonAction.buyParams` 给出第 2 步所需参数。

**第 2 步**——确认（同样用苹果给的 `buyParams` 原样重发）：

```
showIAPSuccessDialog=false&salableAdamId=6449562876&cancelSubscription=true
&subscriptionId=<id>&guid=<MAC>&showIAPExtraDialog=false&hasConfirmedBuySubscription=true
```

返回 `purchaseSuccess`。**取消会使商店 `passwordToken` 立即失效**，之后的核验请求会得到
HTTP 401 ——需重新登录再核验，这不是取消失败。

核验应为 `"isAutoRenewEnabled": false`。

## 错误码速查

| 现象 | 含义 | 处理 |
|---|---|---|
| HTTP 204 / 301 / 404 / 500 / 503（登录） | 苹果边缘概率性丢弃 | 循环重试（放行率约 1/10–1/20） |
| `3038` `MZCommerce.TermsNotAccepted` | 未接受媒体服务条款 | 见第二步（需持久 jar + AppStore UA） |
| `2002` `MZCommerce.BadPasswordToken` | 条款两步未共用同一 cookie jar | 改用一个持久 jar 重放 |
| `2034` `MZCommerce.ExpiredPasswordToken` | 商店令牌过期 | 重新登录，非购买被拒 |
| `3064` `StoragePurchaseOnNonStorageCapableAccount` | **账号非 iCloud 激活** | 先用 iclouddelegate 探针判定；账号侧问题，改参数无用 |
| `MZCommerce.SystemError` | 缺 `offerName`/`storagePlanInBytes`/`cc` | 补齐参数，非瞬时故障 |
| `5093` `Cannot load storage plan` | `presentOffers` 对非官方客户端关闭 | 不要走网页链路，用本 skill 的原生 plist 链路 |

## 已验证无效的路径（不要重试）

这些都是实测排除的，重走只会浪费时间：

- **`presentOffers`**：非官方客户端恒返回 `5093`，无法复现网页下单链路。
- **`commerce/account/subscriptions/icloud/update`**：需要 `subscriptionId`，无它即 404。
- **MZFinance 动作名爆破**：1088 个候选名全 404，"同意"不是独立端点而是
  `agreeToTermsUpdate` 表单。
- **`buyProduct` 加同意标记位**：`termsVersionId` / `hasAcceptedTerms` / 各类似字段
  全无效，条款必须走第二步的表单。
- **用 iCloud 侧 DSID 下单**：`X-Dsid` 用移动端 DSID（`001531-…` 形态）会得到
  `failureType 2001`。必须用**商店 DSID**（`dsPersonId`，纯数字）。
- **绕过 iCloud 激活**：实测两个账号用**完全相同**的代码、
  设备身份与参数，前者 3064、后者 `purchaseSuccess`。差别只在账号侧，改客户端无用。

## 硬性要求

- **扣费前必须取得用户明确授权。** 本流程会产生真实扣款与自动续费。
- **绝不要 `Stop-Process msedge` / 批量杀浏览器进程**——会连用户自己的浏览器一起关掉。
- 核验以苹果自己的订阅记录为准，不要以购买响应的 `status: 0` 单独作为成功依据。
- 凭据与令牌属敏感信息，持久化时限制文件权限，不要写入日志或提交到版本库。
