# `Dadoum/apple-crates` 评估：能借什么、不能借什么

日期：2026-11-16。来源：tangled 上的 `dadoum.zone/apple-crates`（GitHub 那份已归档，README 写明迁移）。本地副本在 `.cache/apple-crates/`（`.gitignore` 已忽略，不入库）。本轮**没有编译、没有运行、没有连接 Apple**，结论全部来自源码阅读。

## 1. 它是不是注册机

**不是。** `grandslam` 暴露的入口只有认证，没有建号：

```rust
pub async fn login(http_session, user, password) -> AuthResult
pub async fn login_with_continuation_token(http_session, user, token) -> AuthResult
pub async fn login_with_continuation_token_created(http_session, user, token, created_millis) -> AuthResult
```

`AuthOutcome` 的五种结果全部围绕登录：`Success` / `SecondaryActionRequired` / `AnisetteResyncRequired` / `AnisetteReprovisionRequired` / `UrlSwitchingRequired`。没有 `create`、没有 `/account/validate`、没有验证码提交。

## 2. 真正值得借的两块

### 2.1 真 ADI（Anisette）—— 设备身份

`library-coreadi/src/windows.rs` 在 Windows 上：

- 从已安装的 iCloud / iTunes 里找 `CoreADI64.dll` / `CoreADI.dll`（`library_candidates()`、`find_library()`）；
- 用 `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR` 加载，不改 PATH；
- 包成 `WindowsCoreADIProxy`，通过 `adi` crate 的 `CoreADIProxy` 暴露 `synchronize(dsid, sim)`。

`grandslam/src/grandslam/anisette.rs` 用它生成 Apple 认的机器头（`X-Apple-I-MD-M`、`srm`、`X-Apple-I-MD-RINFO` 等）。

**对我们意味着什么**：现在 `signup` 的设备身份是「TLS 指纹 + cookies + 设备档案」近似；ADI 是 Apple 服务端真正校验的机器凭据。这是比"养设备"更硬的一层，但它**不是**建号接口，也不保证接受率。

### 2.2 GrandSlam 登录 + 续期令牌

- SRP 密码登录，错误类型细分到 `Apple` / `Anisette` / `Network` / `SRP` / `Parsing`；
- `ServerProvidedData` 里带 `alt_dsid`、`idms_token`、`session_key`、`cookie`、`master_token`、`continuation_token`、`TokenBag`；
- 支持**用续期令牌登录**（`ckcts` 毫秒时间戳），这是面板保持会话、少打密码登录的正确做法。

## 3. 明显缺口（不能当成已有能力）

| 缺口 | 事实 |
|---|---|
| 2FA 完成动作 | `SecondaryActionType` 只有一个 `ServerDriven(String)` 变体，源码里有 `TODO: recognise the action URLs of the other types`。**短信/受信任设备提交要自己实现** |
| 建号 | 完全没有 |
| 发布形态 | 是库不是二进制；要自己写调用方并编译 |
| DLL 依赖 | 依赖用户机器上已安装的 Apple 应用，**不能随包分发 Apple 的 DLL** |
| 许可 | README 只对图标给了 Lucide 许可；crate 本身未见显式 LICENSE 文件，接入前要跟作者确认 |

## 4. 建议的接入形状：Rust sidecar + 现有适配器契约

不把 Rust 塞进现有 Go/Python 进程，而是做成**独立子进程**，复用 W3 刚建好的契约（冻结上下文 + 版本化回执 + 杀进程树）：

```text
bin/anisette-sidecar.exe
  provision                     → {"adi": {"machine_id": "...", "routing_info": ...}}
  login --user U --password-file P
                                → {"outcome":"success","tokens":{...}}
                                → {"outcome":"secondary_required","action":"...","session":"..."}
                                → {"outcome":"anisette_reprovision_required"}
  submit-code --session S --code-file C
                                → {"outcome":"success","tokens":{...}}
```

- 密码/验证码经**私密文件**传入，不进 argv（与 `read_input` 的绑定规则一致）；
- 每次调用带 `job_id/account/run_id` 绑定，回执按 `validate_outcome` 校验；
- `anisette_reprovision_required` → 走 `needs_review`，不自动重登；
- 找不到 CoreADI → 直接 `anisette_unavailable`，**零资源占用**（和 `protocol_adapter_missing` 同款 fail-closed）。

## 5. 与现有方案的关系

| 层面 | 现状 | 接入后 |
|---|---|---|
| 建号 | 纯协议 `signup.exe` | 不变（apple-crates 不参与） |
| 设备身份 | 画像 + cookies + TLS 指纹 | 可叠加 ADI（更硬的机器凭据） |
| 登录/激活 | 面板手工链路 | 可用 sidecar 替代为 SRP + 续期令牌 |
| 2FA | 面板 SMS | 仍需自己实现，crate 只给 hook |

## 6. 风险与前置条件

1. **部署依赖**：目标 Windows 机器必须装有 iCloud/iTunes（否则无 CoreADI）；
2. **不得分发 Apple 二进制**：发布包只能带我们的 sidecar，DLL 由用户环境提供；
3. **ADI ≠ 过审**：它让"设备是真的"更可信，但 Apple 仍可拒绝；
4. **重签风险**：`AnisetteResyncRequired` / `AnisetteReprovisionRequired` 必须显式处理，不能静默重试；
5. **工具链**：需要 Rust 构建产物入包，版本与哈希要进发布清单（W5 未完成项）。

## 7. 结论

- 它**不是** Apple ID 注册机，是认证/设备身份/签名库集合；
- 对当前项目唯一有实质价值的是 **ADI（CoreADI on Windows）+ GrandSlam 登录**，用途是「激活后半段」而不是「建号」；
- 建议先做 **sidecar 最小版（provision + login）**，用离线假 DLL/假回执验收，再谈接面板；真实登录仍需你明确授权。
