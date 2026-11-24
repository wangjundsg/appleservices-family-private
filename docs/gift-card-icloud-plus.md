# 礼品卡兑换后开通 50GB

当前新增的是后端命令行适配器，复用 `tools/icloudpurchase` 的原生 Store 会话与 50GB 购买入口。首次支持美国商店的账户余额礼品码；没有自动改区，也没有接入面板按钮。

## 已实现与待联调

- 从 Apple 实时 Store bag 取得账户查询、兑换预检和兑换地址，并限制域名、路径及重定向。
- 核对 Store/iCloud 账号绑定、美国 storefront、账户查询返回的账号及地区；不会把公共 bag 的默认地区当成账号地区。
- 读取现有账单资料与美元余额。已有姓名和地址由 Apple 原样保留，不发送资料修改请求。缺少 `fullName`、`street1`、`cityStateZip` 时输出缺项并停止兑换；这些是本适配器的准备条件，不代表 Apple 完整的地址校验规则。
- 先预检，再单次兑换。将账户与卡码的摘要作为持久提交标记，换一个运行日志也不能自动再次兑换同一张卡。
- 兑换成功必须同时满足：明确成功回执、回执账号匹配、明确美元入账数、重新查询的余额与回执一致，而且余额增量等于本次入账额。空响应、错误、要求重放、验证弹窗和余额变化不一致都停止后续购买。
- 已到账且余额至少覆盖基础月价后，重新核对实时 50GB 报价，调用原有购买流程。完成判定同时要求成功回执、匹配的订阅和真实付费容量。

**尚未真实兑换/购买验收。** 用户将另行提供地区匹配的账号和礼品卡。请求形状来自 Apple 公开前端；测试为合成响应，不能证明私有会话一定被服务器接受。未知响应结构保持停止状态，不猜测金额、货币、地区或成功状态。

缺少资料时的模板写入、自动改区、兑换阶段的额外条款/身份证/礼品卡附加验证均未实现。后续拿到测试账号与真实必填字段后，再接入缺项补全；不要拿模板覆盖已有资料。已有登录/设备激活自动条款处理及购买条款处理不受影响。

## 运行

先通过既有激活流程取得有效 iCloud checkpoint；通过同账号 Store 认证取得私有 Store checkpoint。所有输入和日志都放在当前用户独占、Git 忽略的目录中。示例仅使用占位路径；卡码不放到参数、环境变量、普通日志或仓库。

```powershell
go build -o bin/icloudpurchase.exe ./tools/icloudpurchase

# APPLESERVICES_APPLEID 通过当前进程环境提供；Store checkpoint 必须属于同账号及本机 GUID。
# 只查询准备情况，不兑换、不下单，也不需要购买确认标志。
.\bin\icloudpurchase.exe -claude-skill -native-store `
  -dir <private-account-dir> -accepted <private-icloud-checkpoint> `
  -store-session-in <private-store-checkpoint> -journal <new-private-preflight.jsonl> `
  -gift-card-file <private-card.txt> -gift-card-country US -gift-card-check-only

# 仅在卡码兑换及 USD 0.99/月（另加适用税费、自动续费）均已得到授权后运行。
.\bin\icloudpurchase.exe -claude-skill -native-store -confirm-us-50gb `
  -dir <private-account-dir> -accepted <private-icloud-checkpoint> `
  -store-session-in <private-store-checkpoint> -journal <new-private-attempt.jsonl> `
  -gift-card-file <private-card.txt> -gift-card-country US -confirm-gift-card
```

准备查询仍需有效 iCloud/Store 会话；不会建立网页 iCloud 登录来兑换。新建 Store 会话也可以复用原入口的验证码选项。新输入卡码目前限定为以 X 开头的 16 位字母数字兑换码。

余额检查只覆盖基础月价，**不计算账单地址的税额，也不保证只从礼品卡余额扣款**。Apple 可能要求补足余额或使用现有付款方式。不要把 `$1` 当成含税购买保证；真实下单前说明金额、税费和续费。现有有效订阅会阻止该组合流程兑换及再次购买。

如果兑换完成但购买失败：保留兑换提交标记与私有日志，重新查询余额、订阅和容量后，使用原有“不带 gift-card 参数”的购买入口续办，不能重新兑换。结果不明确时，提交标记必须保留；不要删除标记来试错。卡码文件由用户管理，程序不会自动删除原文件。

## 依据和验证边界

2026-11-24 实际读取公开 bag，三个地址均存在：`accountSummaryGraphQL` → `/account/v1/summary/graphql`；`codeDetails` → `/commerce/codeDetails`；`redeemCodeSrv` → `/commerce/redeemCodeSrv`。

请求及回执字段参考 [Apple 公开账户前端](https://apps.mzstatic.com/content/1ae2998afc5d4d6cabf5effc124a94f6/account/settings) 中的 account summary 查询、兑换服务与回执转换代码。公开前端不是 Apple 承诺稳定的第三方 API；原生容器可能附加本适配器尚未验证的认证行为。真实账号返回形状不同须先定位，不放宽成功判定来“通过”。

[Apple 说明礼品卡的地区限制](https://support.apple.com/en-md/108285)，并说明 [Apple 账户余额可以用于 iCloud+](https://support.apple.com/en-gb/118245)。以上规则不代表本实现已获真实购买成功。

模拟测试覆盖正常入账、账户/地区不匹配、缺失账单字段、恶意接口地址、空响应、HTML、重定向、超时后查询、要求重放、入账数不一致、持久防重复标记和付费容量缺失。
