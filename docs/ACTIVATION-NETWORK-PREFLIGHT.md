# 激活网络预检与持久部署

面板在停止现有 worker、创建 Mac APS 身份、启动登录之前检查网络与原生运行环境。GSA 的实际发码入口还会重新检查该客户端使用的传输；包括可能自动发码的 `/auth` 发现请求、显式指定号码发码、短信重发和受信任设备发码。探测不携带账号凭据、cookie、设备令牌或短信内容，不自动轮换代理，不关闭 TLS 校验。

## 实际路由

| 阶段 | 配置与检查 |
| --- | --- |
| 原生 ADI helper | `nativeadi.Environment()` 将显式 `APPLESERVICES_NATIVE_HTTP_PROXY` 映射到 helper 的 HTTP(S) 代理，并删除 SOCKS、ALL_PROXY、NO_PROXY 干扰。先探测该路由，再验证固定 DLL 哈希、Python 64 位和编译器；worker 的真实 native 初始化仍在 `BeginLogin` 前运行。 |
| GSA、验证码 | `gsa.AppleRootTransport()` 使用 HTTP(S)_PROXY / NO_PROXY，专用 APPLESERVICES_SOCKS_PROXY 优先。发码预检复用该客户端实际 `twoFactorTransport()`，不使用面板环境代替已有 worker 的环境。 |
| Go APS | 使用现有 courier dialer，连接同一 courier 和端口，仅完成验证服务器证书的 TLS 握手，不发送 APS 认证帧。 |
| Mac APS 初始化 | 现有初始化器清空代理、强制直连。缺少本账号推送状态时另做直连 courier TLS 探测；原生初始化结果仍是最终依据。 |
| worker 自动 NAC 激活 | 继承 worker 路由，分别检查证书与会话端点的无凭据 HEAD。 |
| 面板手动 NAC 激活 | 现有 `filteredActivationEnv` 强制直连，与自动激活不同；不能把自动激活预检视作这条路径的验收。 |

预检的 `connect`、`tls`、`http` 阶段进入面板消息；本地依赖失败为 `native-library` / `native-runtime`。407、429、5xx 阻止继续。403、404、405、401 仅证明连接和 TLS 到达 HTTP 层，不表示登录、发码、NAC 操作或激活得到授权。探测不跟随重定向、不保存响应内容。

ADI 路由探测使用 Go 和项目已有的 Apple 根证书，并非运行 native 库的网络栈；DLL 内实际初始化仍可能失败，错误继续按 native TLS/连接阶段显示。预检不能保证探测结束后网络永不变化。

## 持久配置

使用 `tools/start-activation-dashboard.ps1`，明确传入 `-StateDir`、`-NetworkConfig`、`-DashboardBinary`、`-WorkerBinary`、`-Library`、`-Python`。启动器要求 PowerShell 7.4+。

本地 JSON 必须包含六个字符串字段：`native_http_proxy`、`http_proxy`、`https_proxy`、`all_proxy`、`socks_proxy`、`no_proxy`。空字符串表示清空该项，而不是继承旧配置。SOCKS 值为 `host:port`；原生代理只接受带显式端口的 HTTP/HTTPS URL。启动器拒绝凭据嵌入 URL，不读历史 `native-proxy.dpapi`，不自动发现或轮换端口。应先确认当前代理监听端口，再写本地配置；仓库不提供固定历史端口。

所有账号密码环境变量在子进程启动前清空；`APPLESERVICES_REMOTE_ANISETTE` 同时清空，确保本部署使用本地 ADI。配置文件与运行日志保留在忽略的本地目录，禁止提交。

`-ValidateOnly` 检查配置并给出二进制、DLL、配置 SHA-256，不启动进程。`-PreflightOnly` 执行上述无账号网络检查后退出。正常启动使用 `-manual-start`：不自动恢复其他账号，也不启动历史注册/家庭维护任务。用户显式操作仍可执行。

## 部署验收

1. 运行 Go 相关包测试、`tools/test-activation-launcher.ps1` 和 `git diff --check`。
2. 构建新的面板与未来 worker 文件名；不要覆盖仍在运行的 worker 映像。构建来源若含已有未提交改动，要在本地保存源码哈希清单，不能把它宣称为纯提交构建。
3. 使用持久配置执行 `-PreflightOnly`。保留阶段结果，不能把 HTTP 可达写成登录验收。
4. 记录成功 worker 的 PID、启动时间、映像哈希及设备/会话文件哈希。只停止核实路径与 PID 后的旧面板，不发送 worker 停止请求。
5. 启动器校验监听端口属于新 PID、进程路径和面板 SHA-256，写本地 `deployment-receipt.json`。通过面板受保护的 `GET /api/network-config` 核对运行进程的配置来源和 SHA-256；此接口不返回代理 URL/凭据。
6. 核验旧 worker PID、启动时间、映像和设备/会话文件未变，仅读取本地状态确认会话仍在线。核对实际 HTTP 页面包含预期的新界面。
7. 只提交本任务源码、测试和脱敏文档；推送后比较远端分支与本地提交。

`-manual-start` 是保留已成功会话的部署模式；其启动行为不恢复历史后台任务。已有在线 worker 继续运行原二进制，新 worker 的发码预检要在以后显式启动该版本时才生效，不能为验证新功能重启成功账号或真实发码。
