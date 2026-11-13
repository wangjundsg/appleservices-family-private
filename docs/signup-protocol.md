# Apple Account 创建流程：抓包实证（M0）

> 证据来源：本机 Chrome（Windows）经 `tools/capture/capture.cjs` 驱动，
> 2026-11-13 实测。所有请求使用真实浏览器会话，非推测。
> 原始记录：`.cache/capture6/network.jsonl`、`.cache/capture6/form.json`（含令牌，不进 Git）。

---

## 1. 入口与 bootstrap

```
GET  https://appleid.apple.com/account          → 302/跳转 → https://account.apple.com/account
GET  https://account.apple.com/bootstrap/portal → 200
```

`bootstrap/portal` 返回：

```json
{
  "apiUrl": "https://appleid.apple.com",
  "serviceKey": "af1139274f266b22b68c2a3e7ad932cb3c0bbe854e13a79af78dcc73136882c3",
  "serviceUrl": "https://idmsa.apple.com/appleauth",
  "environment": "idms_prod_account",
  "timeOutInterval": 15,
  "moduleTimeOutInSeconds": 60
}
```

- `serviceKey` 同时是**创建表单 iframe 的 widgetKey**，也是后续请求的 `X-Apple-Widget-Key`
- `serviceUrl` = AuthKit 服务基址（`idmsa.apple.com/appleauth`），登录/2FA 走这里

页面 `<script id="app_config">` 里另有 `"scnt": "AAAA-…"`。

---

## 2. 表单在 iframe 里

```
https://appleid.apple.com/widget/account/?roleType=Agent&lv=0.4.3&widgetKey=<serviceKey>&v=3&appContext=account
```

外层页面只有导航，**所有创建控件都在这个 iframe 内**。字段名（实测 DOM）：

| 字段 | 选择器 | 说明 |
| --- | --- | --- |
| 名 / 姓 | `input[name=firstName]`, `input[name=lastName]` | 必填 |
| 国家/地区 | `select[name=countrySelect]` | ISO3，`USA` |
| 生日 | 3 个无名 `<select>`（月/日/年） | 值 `07`/`18`/`1992` |
| 邮箱 | `input[name=appleId]` | 同时是账号名 |
| 密码 / 确认 | `input[name=password]`, `input[name=confirmPassword]` | |
| 电话区号 | `select[name=phoneCode]` | `US` |
| 电话 | `input[name=phoneNumber]` | |
| 验证码 | `input[name=captcha]` | Apple 自有图片验证码 |
| 隐藏 | `input[name=fdcBrowserData]` | 浏览器指纹 |

按钮：`New Code`（换验证码）、`Vision Impaired`（语音验证码）、`Continue`（submit，校验通过前 `disabled`）。

---

## 3. 创建前的三个调用

### 3.1 取验证码

```
POST https://appleid.apple.com/captcha
{"type":"IMAGE"}
→ 201 {"payload":{"contentType":"image/jpeg","content":"<base64>"}}
```

图片 126×56，**5 个字符**，带扭曲与噪点。答案是 `captcha.answer`，`captcha.id` 与 `captcha.token` 一次一换。

### 3.2 邮箱预检

```
POST https://appleid.apple.com/account/validation/appleid
{"emailAddress":"<addr>"}
→ 200 {"valid":true,"used":false,"appleOwnedDomain":false,"isRecycledDomain":false}
```

实测结果（同日）：

| 地址 | 结果 |
| --- | --- |
| `rb4c660417@sjywj.tech` | valid=true, used=false, appleOwned=false, recycled=false |
| `q7m2xk4p@abc-temp-2026.shop` | 同上 |
| `q7m2xk4p@gmail.com` / `@qq.com` / `@163.com` | 同上 |

结论：**预检阶段免费域并不被标记**，QQ/163 的风险出现在后续投递与风控，不在这一步。
自有域名与免费域在这一步等价，选自有域名是为了收码可控与后续可恢复。

### 3.3 密码预检

```
POST https://appleid.apple.com/account/validate/password
{"password":"…","accountName":"<addr>","updating":false,"firstName":"…","lastName":"…"}
→ 204
```

密码规则（错误文案可见于 i18n）：≥8 位、不能连续 3 个相同、不能是常见弱口令、不能与账号名匹配。

---

## 4. 真正的创建调用

```
POST https://appleid.apple.com/account/validate
```

请求体（实测结构，值已替换）：

```json
{
  "phoneNumberVerification": {
    "phoneNumber": {
      "id": 1,
      "number": "(415) 555-0134",
      "countryCode": "US",
      "countryDialCode": "1",
      "nonFTEU": true
    },
    "mode": "sms"
  },
  "account": {
    "name": "<email>",
    "password": "<password>",
    "person": {
      "name": { "firstName": "James", "lastName": "Carter" },
      "birthday": "1992-07-18",
      "primaryAddress": { "country": "USA" }
    },
    "preferences": {
      "preferredLanguage": "en_US",
      "marketingPreferences": { "appleNews": false, "appleUpdates": true, "iTunesUpdates": true }
    },
    "verificationInfo": { "id": "", "answer": "" }
  },
  "captcha": { "id": -838626305, "token": "<长串>", "answer": "2QZRF" },
  "privacyPolicyChecked": false
}
```

失败响应（验证码错时）：

```json
{"validationErrors":[{"code":"captchaAnswer.Invalid","message":"Please enter the characters you see or hear to continue.","path":"captcha.answer","suppressDismissal":false}]}
```

**注意**：`captcha.id` 是负数，`token` 结尾带 4 个字符的尾巴，`answer` 单独再传一次。

---

## 5. 必须携带的请求头（从真实请求抄下）

```
x-apple-widget-key: <serviceKey>
x-apple-id-session-id: <128 字节 hex，整个流程内不变>
x-apple-i-request-context: account
x-apple-i-timezone: America/New_York
x-apple-i-fd-client-info: {"U":"<UA>","L":"en-US","Z":"GMT-05:00","V":"1.1","F":"<指纹>"}
scnt: AAAA-…|<计数器>
content-type: application/json
referer: https://appleid.apple.com/
```

`scnt` 末尾的 `|3`、`|5` 是同一会话内的递增计数器，**每个请求都要取当前值**。

### Hashcash 工作量证明

`POST /account/validate` 额外带：

```
x-apple-hc: 1:11:20261112175037:97aa6cb9df724623fa74547aced502eb::1504
```

格式 `1:<bits>:<UTC yyyyMMddHHmmss>:<nonce hex>::<counter>`。
`GET /account?countryCode=USA` 里 `hashcashGenerationTimeout: 90` 说明浏览器端算这个最多允许 90 秒。
bits=11 约等于 2048 次哈希，随手可算；**但缺这个头会被拒**。

---

## 6. 纯 HTTP 复刻的现实结论

用 Go 直接打 `/account/validation/appleid`（带 widget key、scnt、session id、fd-client-info）返回 **403**；
浏览器同请求 200。差异在于浏览器还有：完整 cookie 链、`fdcBrowserData` 指纹、jslog 会话、hashcash。

因此当前路线定为：**浏览器（Playwright + 本机 Chrome）驱动创建**，
验证码人工/OCR 兜底，等 12 个账号跑通后再考虑去掉浏览器。

---

## 7. 已验证的收信接口（自有域名）

```
GET https://abc-temp-2026.shop/api/mails?limit=5&offset=0
Authorization: Bearer <address_jwt>
→ 200 {"results":[],"count":0}
```

`limit`/`offset` 都必须给，缺一个返回 400 `无效的 offset 参数`；
无凭据时 401 `无效的邮箱地址凭据`。

---

## 8. 已跑通的完整链路（2026-11-13 实测）

```
填表 → 验证码（人工读图，错则刷新重试） → POST /account/validate 200
     → 邮箱验证页（6 个 tel 框）→ 邮件码 → 继续
     → 手机验证页（6 个 tel 框）→ 短信码 → 继续
     → 账号生成
```

实测账号：`111@abc-temp-2026.shop`（密码见账号目录），
`POST /account/validation/appleid` 复查返回 **`used: true`**，证明账号确实创建成功。

关键细节：

- 顺序是**邮箱验证在前、手机验证在后**
- 邮箱码从自有域名的 `/api/mails` 取，Apple 把码写成 `<b>123456</b>`，直接正则这个标签最稳
- 短信码用接码平台 `getStatus` 轮询；用过的码要记下来，下一次只认新码
- 验证码答错不会作废账号，Apple 只刷新图片，可以无限重试

### Apple 的 IP 限流

连续提交几次创建后，`/account/validation/appleid` 会直接返回 **503**（同一 IP）。
换住宅代理后立刻恢复 200。所以批量时必须一账号一出口，且出口要能换。

### 用过的资源

- Hero-SMS：`service=wx`（就是 Apple），`country=187`（美国），单价 **$0.13**，库存 66 万+
- 号码 `+1 518-246-0055`，activation `1112400649`，短信码 `810914`
- 限时 20 分钟的号，**创建完成后必须立刻保存浏览器会话或登记受信任设备**，
  否则号一释放，账号就再也登录不进去（2FA 只能发到那个号）

## 9. 建号之后的真实链路（第二轮实测）

第二个账号 `uinr1p2e864@sjywj.tech` 走完了：建号 → 邮箱码 591487 → 短信码 282184 → GSA 密码认证
→ 2FA 短信 453810 → **session.json 落盘**（12 个 app token + DeviceIdentityToken）。
然后卡在 iCloud delegate：

```
login: appleservices: iCloud login: icloud: mobileme delegate refused status=1
```

`status=1` 表示这个新账号还没完成 iCloud 侧的开通（条款/设备资格），不是密码问题。
仓库里的 `tools/accountsetupcheck` 能处理这一步，但它要求 native ADI 硬件：

```
accountsetup: hardware unavailable
```

所以建号链路的最后一公里依赖 **iCloud for Windows（CoreADI64.dll）** 装在这台机器上。

### 关键经验

1. **anisette 默认池是坏的**：默认服务器报 `EndProvisioningError` / `init rejected (ec=-80045)`。
   可用的是 `https://mac-ani.jkcoxson.com` 和 `https://new.ani.sidestore.io`（用假账号探到 `ec=-20970`，
   用真账号+错密码探到 `ec=-22406`，说明 init 与账号查找都正常）。
2. **建号后必须在同一个小时窗口里走完**：号码一旦释放，2FA 短信就收不到；
   但 GSA 会话落盘后 cookie 仍在，后续可复用。
3. 邮箱不需要向他要：临时邮箱后台用 `x-admin-auth: <admin密码>` 就能批量建地址
   （`POST /admin/new_address`，返回 `address` + `jwt`），库里已有 1202 个地址。

## 10. 下一步（需要外部输入）

1. 接码 API：拿到美国号 → 填 `phoneNumber` → 提交 → 抓短信验证码步骤与请求体
2. 代理：美国住宅粘性出口，注册全程固定
3. 验证码：先人工读图（12 个号够用），后续再接 OCR

## 11. 登录协议搬运结果（2026-11-13）

借用 iCloud-Privacy-Mail-v2 的 internal/protocol 实现，在
D:\Workbench\sources\iCloud-Privacy-Mail-v2\cmd\applelogin 加了一个 CLI，构建为
in/applelogin.exe。它走完整链路：

    AuthKit start -> SRP 密码 -> 2FA 短信 -> accountLogin -> getTerms -> repairDone -> iCloudSession

实测（us-04 / uf3xqe12ibt@sjywj.tech）：

    terms required: version=630101
    ok apple_id=... dsid=23087649... host=www.icloud.com cookies=18
    cookie names: dslang,site,aasp,acn01,DES...,X-APPLE-WEBAUTH-HSA-TRUST,
                  X-APPLE-UNIQUE-CLIENT-ID,X-APPLE-WEBAUTH-USER,X_APPLE_WEB_KB-...,
                  X-APPLE-WEBAUTH-VALIDATE,X-APPLE-WEBAUTH-PCS-*,X-APPLE-WEBAUTH-TOKEN,
                  X-APPLE-DS-WEB-SESSION-TOKEN
    icloud_plus=false can_create_hme=true

结论：

- **iCloud 网页会话与条款已经协议化**，不再需要浏览器点页面
- 产物落 state/accounts/<dir>/icloud-web-session.json（owner-only）
- can_create_hme=true 说明隐藏地址能力已开通

遗留：setup.icloud.com/setup/iosbuddy/loginDelegates 仍返回 status=1
（带不带网页 cookie 都一样）。这是**设备 Setup Assistant** 路径，新网页账号大概率不走它。
后续要么改用网页会话直接调 HME/家庭组接口，要么从 ccountLogin 的 webservices
里取服务令牌，而不是继续攻 iosbuddy delegate。

