# 纯协议 Apple Account 注册机（无浏览器）

日期：2026-11-15。范围：`appleservices-family-private`。实现：`tools/signup/`。
替代对象：`tools/capture/create-account.cjs`（Roxy 指纹浏览器 + Playwright 驱动建号）。

---

## 0 结论先行

| 项 | 结论 | 证据 |
|---|---|---|
| 建号能否纯 HTTP 完成 | **能**，控件与端点全部在 HTTP 层 | `tools/signup/` 全链路通过离线回放测试；真实 bootstrap/preflight/captcha 已实测 |
| 旧结论「纯 HTTP 必 403」 | **是缺陷，不是服务端限制** | `docs/signup-protocol.md` §6 的 Go 探测把 `scnt` 从 HTML `app_config` 里正则出来，缺了 `\|<n>` 计数器与 144 字节尾段，也没有 `X-Apple-ID-Session-Id`。这两样都在 `GET /account?countryCode=USA` 的**响应头**里，一次拿全 |
| `fdcBrowserData` / `X-Apple-I-FD-Client-Info` | **非必需**（preflight 与 captcha 均通过） | 见 §3.5 |
| `jslog` / `v2/jslogs` 遥测 | 非必需（同样的调用不带也过） | 见 §3.5 |
| 落地页 cookie 链 | 非必需；503 也能继续 | 实测：`account.apple.com/account` 返回 503 时 config 端点仍 200 |
| 仍然必需的 | `scnt`（每响应轮换）、`X-Apple-ID-Session-Id`、`X-Apple-Widget-Key`、`X-Apple-HC`、captcha `id`+`token` | §3 |

一句话：**纯协议把「建号」从浏览器里救出来了，但没把风控救出来。** 出口国 / 号码国 / 语言三者对齐仍然是硬门槛（`docs/REGISTRAR-CONTRACT.md` A-6、`docs/RESEARCH-APPLE-SUPPLY.md` §3.3 的既有结论不变）。

---

## 1 协议面（全部取自真实抓包 + 线上 widget bundle）

抓包：`.cache/capture6/network.jsonl`（Chrome 真实一次完整建号）。
JS 依据：`https://appleid.cdn-apple.com/static/module-assets/widget-*.js`（`hashcashGenerationPlugin`、`fdcPlugin`、`getBrowserData`、`Hi(...)` 建体函数）。本地副本在 `.cache/widgetjs/`（不入库）。

### 1.1 引导

```
GET https://account.apple.com/bootstrap/portal          → {"serviceKey": "<widgetKey>", ...}
GET https://account.apple.com/account                   → 200/503，取站点 cookie（可失败）
GET https://appleid.apple.com/account?countryCode=USA   → 200，配置 JSON
```

第三个请求的**响应头**才是真引导：

```
scnt:                    AAAA-<sessionId 大写hex的base64>|<n><144B 尾段>
X-Apple-ID-Session-Id:   <240 hex>
X-Apple-HC-Bits:         10..12
X-Apple-HC-Challenge:    <32 hex>
```

`scnt` 每次响应都会换一个新的：**必须逐响应回填**，用旧值下一步就 403。

### 1.2 建号前的三步（都只读）

```
POST /account/validation/appleid   {"emailAddress": "..."}      → {"valid","used","appleOwnedDomain","isRecycledDomain"}
POST /account/validate/password    {"password","accountName","updating":false,"firstName","lastName"} → 204
POST /captcha                      {"type":"IMAGE"}             → {"id","token","type","payload":{"contentType","content(base64)"}}
```

captcha `id` 可正可负（实测两次分别 `202113227` / `-1452828184`），`token` 一次一换；**答错只换图，不作废账号**（`docs/signup-protocol.md` §8）。

### 1.3 建号本体

```
POST /account/validate
X-Apple-ID-Session-Id / scnt / X-Apple-Widget-Key / X-Apple-I-Request-Context: account
X-Apple-I-Timezone / X-Apple-HC
{
  "phoneNumberVerification": {"phoneNumber": {"id":1,"number":"(415) 555-0134","countryCode":"US",
                                "countryDialCode":"1","nonFTEU":true}, "mode":"sms"},
  "account": {"name","password",
              "person": {"name":{"firstName","lastName"},"birthday":"yyyy-mm-dd","primaryAddress":{"country":"USA"}},
              "preferences": {"preferredLanguage":"en_US","marketingPreferences":{...}},
              "verificationInfo": {"id":"","answer":""}},
  "captcha": {"id":<int>,"token":"<hex>","answer":"<5字符>"},
  "privacyPolicyChecked": false
}
```

200 = Apple 已受理，随后**邮箱码在前、短信码在后**（`docs/signup-protocol.md` §8）。

### 1.4 两步验证（代码提交）

```
POST /account/verification              {"name":"<appleId>"}                       # 触发邮箱码
PUT  /account/verification              {"name":"<appleId>","verificationInfo":{"id":"","answer":"<6位>"}}
POST /account/verification/phone        {"phoneNumberVerification":{"phoneNumber":{...},"mode":"sms"}}   # 需要 X-Apple-HC
PUT  /account/verification/phone        {"phoneNumberVerification":{"phoneNumber":{...,"securityCode":{"code":"<6位>"}},"mode":"sms"}}  # 需要 X-Apple-HC
```

依据：widget bundle 里的 `ke / xe / Te / Fe` 四个包装函数及其调用点（`/account/verification`、`/account/verification/contactEmail`、`/account/verification/phone`）。`contactEmail` 变体用在「联系邮箱」分支，主流程走 `PUT /account/verification`。

### 1.5 判定建成

```
POST /account/validation/appleid {"emailAddress":"<同一个地址>"} → used:true
```

这是本仓一直用的建号终判（`docs/signup-protocol.md` §8），不需要登录就能查。

---

## 2 Hashcash：唯一需要自己算的东西

widget 里 `hashcashGenerationPlugin` 只对三个 URL 加头：`/account`、`/account/validate`、`/account/verification/phone`。
算法在 `window.idmsapis.hashcash.generateStamp` → 内层 `hashcashgen` UMD（MD5 库在场，但强度校验走 `createHash("sha1")`）：

```
stamp = "1:" + bits + ":" + <UTC yyyyMMddHHmmss> + ":" + challenge + "::" + 十进制计数器
ok    = SHA1(stamp) 的**前导零 bit 数** ≥ bits
```

- 计数器从 0 递增，**十进制**；服务端只对收到的字符串重算摘要，所以任何满足零位的计数都有效。
- 无挑战时有 fallback：`X-Apple-HC-Bits` 缺省视为「不需要」。
- `tools/signup/hashcash.go` 实现 + 离线单测（`TestHashcashStampSatisfiesDifficulty`）。仓里 `internal/appleweb` 的老实现用 base36 计数器，功能等价但形状与浏览器不同，纯协议这边按浏览器来。

---

## 3 关键实测记录（2026-11-15，本机家宽，本机 IP）

### 3.1 preflight 纯 HTTP 200

```
GET /account?countryCode=USA → 200，响应头 scnt/sessionId/hcBits=12/hcChallenge
POST /account/validation/appleid（只带 scnt + sessionId + widgetKey + request-context + tz）
→ 200 {"valid":true,"used":false,"appleOwnedDomain":false,"isRecycledDomain":false}
```

**没有** `X-Apple-I-FD-Client-Info`，**没有** cookie（落地页 503），**没有** jslog。

### 3.2 captcha 纯 HTTP 201

`POST /captcha {"type":"IMAGE"}` → 201，`id/token/type/payload(contentType=image/jpeg, content=base64)`，图片 160×70≈3.8KB，5 字符。

### 3.3 工具直跑

```
go run ./tools/signup -email <addr> -stage preflight -json
{"email":"...","free":true,"stage":"preflight","used":false}

go run ./tools/signup -email <addr> -stage captcha -captcha-dump .cache\signup-captcha.jpg -json
{"captchaId":-1452828184,"imageBytes":3790,"stage":"captcha"}
```

### 3.4 落地页 503 不影响

`account.apple.com/account` → 503（IP 限流，`docs/signup-protocol.md` §8 记过），config + captcha 照常 200/201。工具把落地页失败降级为警告。

### 3.5 被判为非必需的三样（逐个去掉仍过）

| 去掉的 | 结果 |
|---|---|
| `X-Apple-I-FD-Client-Info`（fdcBrowserData） | preflight 200 / captcha 201 |
| `jslog` `v2/jslogs` 遥测 | 同上 |
| 落地页 cookie 链 | 同上 |

它们仍然可能参与**风控打分**（不挡 API，但影响账号存活），所以 `tools/signup` 选择性不做：要补时按 §6 E2 单独加，不要和「能不能建」混为一谈。

---

## 4 工具用法（`tools/signup/`）

```powershell
go build -o .\bin\signup.exe .\tools\signup

# 1) 只读预检
.\bin\signup.exe -email a@domain -stage preflight -json

# 2) 取图（人工读图 或 YesCaptcha）
.\bin\signup.exe -email a@domain -stage captcha -captcha-dump .cache\cap.jpg -json

# 3) 建号（需要号码；-captcha-answer 人工，或设 YESCAPTCHA_KEY 自动）
$env:YESCAPTCHA_KEY="<key>"
.\bin\signup.exe -email a@domain -password 'Qq7#mZp2vLt9' -first James -last Carter `
  -phone 4155550134 -phonecc US -tz America/New_York -locale en-US `
  -proxy 'socks5://...' -stage create -json

# 4) 邮箱码 → 短信码
.\bin\signup.exe -email a@domain -stage email            # 触发发码
.\bin\signup.exe -email a@domain -stage email -code 123456
.\bin\signup.exe -email a@domain -stage phone            # 触发短信
.\bin\signup.exe -email a@domain -stage phone -code 654321

# 5) 终判
.\bin\signup.exe -email a@domain -stage verify -json
```

状态落 `.cache/signup/<email>.json`（0600，含密码，不入库）。每次 `-stage` 都是一次独立进程，重跑同一 stage 幂等。

**纪律**：`-stage create` 之前全部只读；`create` 之后号码一旦释放就再也进不去账号（`docs/signup-protocol.md` §8 关键经验 3），所以建完必须**同一个小时窗口内**跑完 §1.4。

---

## 5 与浏览器路线的关系

| 维度 | 浏览器（现状） | 纯协议（本工具） |
|---|---|---|
| 建号耗时 | 90~150s（拟人节奏） | 秒级（DDL 之外无等待） |
| 资源 | 一个 Roxy profile + Chrome | 一个 HTTP 连接 |
| 指纹一致性 | 靠 profile 冻结 + 三项校验 | **没有浏览器指纹面**；只剩 IP/locale/tz/号码国 |
| 失败面 | 交互控件、iframe 销毁、水合重排 | 只剩状态码与 JSON 错误码 |
| 验证码 | YesCaptcha / 人工读图 | 同（可复用 `tools/captcha/solver.py` 或内置 YesCaptcha 客户端） |
| 风控面 | 大（headless、canvas、UA 漂移） | 小但**没有掩护**：出口与号码不一致会被直接判 |

**保留浏览器路线的场景**：`Your account cannot be created at this time` 这类判定出现时，浏览器可用「拟人节奏 + 真实指纹」当对照；纯协议更适合当**主力产能**，浏览器当**兜底与诊断**。

---

## 6 未验证项与实验阶梯（按顺序做，别跳）

| # | 实验 | 判据 | 成本 |
|---|---|---|---|
| E1 | `-stage create` 端到端跑通 1 个号 | `POST /account/validate` 200 且 `verification` 两步都能过 | 1 个号码（hero-sms $0.13） |
| E2 | 加/不加 `X-Apple-I-FD-Client-Info` 各建 5 个号，看 T+1h 存活 | 存活率差 > 20% 则 FD 参与风控，必须补 | 10 个号 |
| E3 | `scnt` 复用窗口：同一 scnt 连发 N 次 preflight | 何时开始 403，决定是否需要每账号新 session | $0 |
| E4 | 邮箱码/短信码的真实体（`POST /account/verification` 是否吃 `{name}`） | 用 `-stage email` 实测回包；错就按 bundle 的 `contactEmail` 分支改 | 1 个号 |
| E5 | 出口国 = 号码国 = `-locale/-tz` 对齐的最小组合 | 三对齐下 create 200 率；不对齐时的错误码样本 | 3~5 个号 |
| E6 | IP 配额：同 IP 连建第几个开始 503 | 与 `REGISTRAR-CONTRACT.md` 的「一账号一出口」对照 | $0 |

E1 之前不要谈产能：`docs/RESEARCH-APPLE-SUPPLY.md` P2（账号存活）仍然是整条链路的真问题，纯协议只解决 P1（能不能建）。

---

## 7 代码位置

| 文件 | 内容 |
|---|---|
| `tools/signup/client.go` | 会话、头部装配、`scnt` 轮换吸收、六个端点、口令生成 |
| `tools/signup/hashcash.go` | SHA1 前导零位工作量证明（含 `hashcashSatisfies` 自检） |
| `tools/signup/captcha.go` | 取图、YesCaptcha（ImageToTextTask）、本地落图 |
| `tools/signup/main.go` | stage 机（preflight/captcha/create/email/phone/verify/all）、状态文件 |
| `tools/signup/client_test.go` | 假服务端回放：scnt 轮换、hashcash 必校验、全链路、used=true 终判 |

离线验收：

```powershell
go test ./tools/signup/ -count=1
go vet ./tools/signup/
```
