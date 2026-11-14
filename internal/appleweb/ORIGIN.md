# 来源与边界

本包从本机现有 `iCloud-Privacy-Mail-v2/internal/protocol` 的网页认证实现迁入，
使 Apple 产线可以独立构建、运行和评审，不再依赖工作台目录或外部二进制。
迁入范围为 SRP 网页登录、手机号验证、条款和网页会话校验；保留相应本地 HTTP 测试。
Cookie/请求头辅助函数从同一实现提取。未引入邮箱收信、IMAP、数据库、WebUI 或家庭组模块。

迁入后的改动：

- 仅开放 iCloud web flow；不开放 Apple Account management flow。
- 无需 2FA 的条款分支由 `cmd/appleweb` 处理。
- `probe` 只认证账号，不显式请求短信/设备码、不提交条款；Apple 自己可能产生登录通知。
- 登录和保活使用账户浏览器记录的 UA/语言；删除原始响应调试输出。
- `/accountLogin` 传输失败不自动重放。
- 成功校验后保存服务器轮换的 Cookie；失败不覆盖原会话。

协议来源是本地已有实现，离线测试通过不代表当前 Apple 端接受所有账号。
