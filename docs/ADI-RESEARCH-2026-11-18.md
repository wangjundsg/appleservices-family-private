# 协议注册为什么老失败：ADI（真机码）实测记录

日期：2026-11-18。环境：本机 Windows，代码基线含本轮新增的 `APPLESERVICES_ADI_HEADERS` 研究开关。
本轮全部为离线或单次只读请求，**没有建号、没有买号、没有提交任何账号密码**。

## 1. 结论先行

1. **协议注册链路（`tools/signup`）一个 ADI 头都不发**：全仓搜 `X-Apple-I-MD`，命中只在 `anisette/`、`nativeadi/`、`gsa/`、`icloud/`（登录/激活侧），`tools/signup` 里 0 处。
2. **Apple 的账号端点确实区分“设备客户端”和“网页客户端”**：同一 URL、同一 UA，只加 ADI 头，返回就从 HTML 页面变成账号 API 对象。实测证据见 §3。
3. **公共 anisette 池算出来的机器身份 Apple 不认**：加了 ADI 后状态码 417（Expectation Failed），说明身份被识别为伪造/未被登记，而不是“ADI 没用”。
4. 因此“协议很难”的真实门槛不是算法，而是 **一份 Apple 认得到的机器身份**（真机 provisioning）。

## 2. ADI 的三层绑定

| 层 | 内容 | 绑定对象 | 本机状态 |
|---|---|---|---|
| ① ADI 库 | CoreADI 实现，算 `X-Apple-I-MD*` | 无 | 有：`rust_lib_bluebubbles.dll`（哈希与代码内 pin 完全一致）、Apple `CoreADI64.dll` |
| ② 机器身份 | UDID(16B) + 序列号 + provisioning 得到的 `adi_pb` | 这台机器（可多份） | `state/device.json` 只有 UDID，**`AdiPB` 为空** |
| ③ APS push token | 32 字节，来自真实设备/真实 iCloud 会话 | 某台设备 × 某个 Apple 账号 | 只有 `iCloud for Windows` 守护进程能提供，且它属于已登录的那个账号 |

关键约束（`nativeadi/client.go`）：

```go
if err != nil || strings.TrimSpace(config.Serial) == "" || len(config.PushToken) != 32 {
    return nil, errors.New("nativeadi: complete device identity and genuine APS token are required")
}
```

即：**“真机”= 一个真实的 APS token**，不能凭空生成。

## 3. 实测：ADI 头改变 Apple 的判定分支

同一台机器、同一个 `GET https://appleid.apple.com/account?countryCode=USA`、同一个 UA，唯一差异是是否带 ADI：

| 条件 | 状态码 | 响应体形态 |
|---|---|---|
| 不带 ADI | `200` | `{"metaOpenGraph":{...}}` — 网页页面 |
| 带 ADI（公共池 provisioning） | `417` | `{"success":0,"account":{"person":{...}}}` — 账号 API 对象 |

ADI 头集合（公共池给出）：

```
X-Apple-I-MD, X-Apple-I-MD-M, X-Apple-I-MD-RINFO, X-Apple-I-MD-LU,
X-Apple-I-SRL-NO, X-Mme-Device-Id, X-Mme-Client-Info,
X-Apple-I-Client-Time, X-Apple-I-TimeZone, X-Apple-Locale
```

附加 FD（`X-Apple-I-FD-Client-Info`）与 `Accept: application/json` 后仍是 417，排除“缺 FD 导致的误判”。

复现：

```powershell
go run .\.cache\adiaudit2 .cache\adi-lab      # 首次会向公共池 provisioning 并落盘
```

## 4. 为什么 417 等于“身份不被认”

公共池的做法是：别人用 CoreADI provisioning 好的机器身份，转手给你签名。Apple 侧的 `mid` 登记只在 provisioning 那一刻完成，且与 DSID/设备信息相关；共享身份会出现：

- 同一 `mid` 来自大量不同 IP/UA → 被标记；
- provisioning 会话过期 → `X-Apple-I-MD` 校验不过；
- 身份与序列号/推送身份不匹配 → 前置条件失败（417）。

## 5. 我们要的那个“真机 token”从哪来

| 来源 | 是否可用 | 说明 |
|---|---|---|
| 公共 anisette 池 | ❌ 实测 417 | 身份被标记 |
| 本机 `iCloud for Windows` 的 APS token | ⚠️ 仅限它登录的那个账号 | 跨账号使用等于伪造绑定 |
| 自己的 iPhone/Mac 做一次 provisioning | ✅ 正解 | 产出一份属于本机的 `adi_pb`，之后所有请求带 ADI |

## 6. 本轮的代码产物

`tools/signup` 增加研究开关（默认关闭，不影响生产路径）：

```
APPLESERVICES_ADI_HEADERS='{"X-Apple-I-MD":"...","X-Apple-I-MD-M":"..."}'
```

设置后该进程发出的每个注册请求都会带上这些头，用于 A/B；不设置时行为与之前完全一致。

## 7. 下一步（按依赖顺序）

1. 用一台真实设备完成一次 provisioning，得到本机 `adi_pb`；
2. 用它跑 `-stage preflight` + `-stage validate` 的 A/B，确认 `-34607001` 是否消失；
3. 只有在第 2 步成立后，才谈把 ADI 固化进注册链路的产品化。
