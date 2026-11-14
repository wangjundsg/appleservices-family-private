# 交接文档：产号工作台（Outlook 邮箱供给 + Apple 建号）

写给接手完善 UI 与流程的工程师（或 GPT）。目标是把现有的"脚本 + 控制台骨架"变成
**两块独立面板**：① Outlook 注册（并发）② 账号/邮箱库（含流量与取码）。

---

## 一、当前已有的东西（都在这台机器上）

### 1.1 两个注册机（已完成，命令行）

| 用途 | 入口 | 说明 |
| --- | --- | --- |
| Outlook 邮箱注册 | `tools/capture/outlook-cn.cjs` | 本机家宽 + 中文环境（zh-CN / Asia/Shanghai）+ 系统 Chrome。**必须中文环境**，英文/美国出口反而被微软封 |
| Apple 账号注册 | `tools/capture/create-account.cjs` | 通过 Roxy/Forge 或 Playwright 直开 Chrome；表单/验证码/邮箱码/短信码全流程 |
| Roxy 会话层 | `tools/capture/` 内各脚本 | Forge 在 `127.0.0.1:50100`，create→open→connectOverCDP |

### 1.2 取码模块（已完成）

| 模块 | 场景 | 状态 |
| --- | --- | --- |
| `tools/capture/graph-mail.cjs` | 有 refresh_token 的邮箱，走 Graph API | ✅ 支持多语言主题（英/中/日/越/德/西/土） |
| `tools/capture/outlook-web-mail.cjs` | 无 token 的自建邮箱，复用持久化 Chrome profile 读网页收件箱 | ✅ 可用 |

### 1.3 辅助工具

| 工具 | 作用 |
| --- | --- |
| `tools/capture/solver.py` | YesCaptcha 图形码（本地 OCR 兜底） |
| `tools/capture/refund.cjs` / `refund-sweep.cjs` | 退号 / 回收挂账退号 |
| `tools/capture/preflight-browser.cjs` | 浏览器级出口预检（比裸 HTTP 探针准） |
| `tools/console/` | 现有控制台骨架（单页，需按本文件第 3 节重做） |

---

## 二、要做的两块面板（用户明确要求）

### 面板 A：Outlook 注册（并发）

**核心要求**
- 执行方式：**Roxy/Forge**（不接开源那两套的浏览器栈）
- 系统：**只用 Windows**（与另一条产品线比，这里限制单一系统以降低变量）
- 并发：可配置并发数（建议上限 4；家宽单 IP 建议 ≤2）
- 代理三选一：
  1. **直连**（本机家宽，默认；Outlook 必须中文环境 + 国内出口才容易过）
  2. **711**（`socks5h://…-zone-custom-region-<CC>-session-<sid>@global.rotgb.trustsource.live:10000`）
  3. **DataImpulse**（`…__cr.<cc>;sessttl.30@gw.dataimpulse.com:<port>`，端口 10000~20000 粘性）
- 域名选择：**hotmail.com**（推荐，已实测苹果放行）/ outlook.com（**不要用于 Apple**）
- 每账号产出：`地址 + 密码 + Chrome profile 目录`（取码要用）

**流程（脚本已实现，UI 只需串起来）**
```
建 dirId/窗口 → 打开 outlook.live.com 注册页 → 中文同意页 → 新建邮箱地址
→ 密码 → 生日/姓名 → PX 长按 → 建号成功 → 添加保护邮箱（我们用 abc-temp）→ 落盘
```

**PX 长按要点（踩过坑）**
```
短按 800ms → 等 800ms → 静止按住 8000ms（禁止抖动、按住期间禁止扫帧）
```

### 面板 B：账号/邮箱库

- 列表字段：地址、来源（自建/购买）、reader 类型（graph/webmail）、最近一封邮件时间、**流量消耗**、状态
- 操作：取码（调取码模块）、复制地址、标记作废、查看该账号的 profile 目录
- 流量统计：目前只能从代理侧/浏览器侧粗算（`playwright` 无内置流量统计），建议先记"每次任务耗时 + 代理侧账单"两项

### 取码：建议独立成"取码池"服务
理由：Apple 建号、Outlook 保护邮箱、以后的 OpenAI 注册都要取码，耦合在面板里会重复实现。

```
取码池 API（建议）
POST /code {mailbox, since} → {code, messageId, received, from, subject}
GET  /mailboxes → 列表（不含密码/token）
```

---

## 三、现有控制台骨架（`tools/console/`）现状与差距

**现在**：单页把任务列表、发起、邮箱池、截图全塞一起（用户反馈"塞到一个面板"）。

**必须改成**：两个独立页面/路由（`/outlook`、`/accounts`），再加一个 `/code-pool`。

**保留可用的部分**：
- 任务扫描（读 `.cache/**/*.jsonl`，已能读 86 条历史）
- 截图查看（含路径穿越拦截）
- 邮箱列表（敏感字段已 `scrub()`，不输出密码/token）

**要加的**：
- 并发队列（N 个 worker，任务态：queued/running/done/failed）
- 代理选择器（直连 / 711 / DataImpulse + 国家下拉）
- 真跑开关（**默认关闭**，开启需显式确认，防止手滑烧钱）

---

## 四、两个注册机的参数契约

### Outlook（`outlook-cn.cjs`）
```
--proxy <url>            可选；省略=直连家宽
--phonecc / --phone      不用于 Outlook
--headless false         用户要看窗口
输出：.cache/<run>/run.jsonl、run-<...>/shots/*.png、账号写 outlook-cn.txt
```

### Apple（`create-account.cjs`）
```
--email --password --phone --phonecc --activation --hero-key
--proxy / --proxy-user / --proxy-pass
--captcha-cmd 'python tools/captcha/solver.py --provider yes --skip-local'
--graph-token-file <json>   或   --await-email-code <file>
--auto-refund
--profile <profile.json>
退出码：0 成功 / 1 failed / 2 usage / 3 unknown / 4 waiting_input / 5 likely_success
```

---

## 五、必须遵守的坑（全是实测踩出来的）

### Apple 侧
1. 密码**必须含特殊字符**，否则 Continue 灰着（日志会伪装成 click-timeout）
2. 图形码**一张图只有一次机会**，被拒后站点自己换图——**绝不点"新验证码"按钮**
3. 短信码提交后 iframe 被销毁是**成功**（`likely_success`），不是失败
4. 手机国家码会被控件水合重置 → 填完再读一次 `phoneCode`
5. 2FA 取码有频率上限（`tooManyCodesSent` / 423）→ 每激活最多 2 次
6. 退号有 120 秒冷却 → 挂账 + `refund-sweep`
7. `run.jsonl` 必须逐条 append，进程被杀才不丢证据
8. **邮箱域名差异极大**：`qq.com` / `hotmail.com` 放行；`outlook.com` 会被硬拒；`icloud.com` 不能当 Apple ID

### Outlook 侧
9. 必须**本机中文环境**；英文 + 美国住宅反而直接封
10. PX 两步式长按，抖动会重置进度
11. Forge 的 HTTP 代理必 407 → **只能 SOCKS5**
12. 建完要过"添加保护邮箱"，码发到 abc-temp，可自动读
13. 家宽单 IP 每天 2~3 个，多了封

### 自动化通用
14. 不要用 `.fill()`（0 毫秒填完是最大特征）→ 用逐字符 `typeHuman`
15. 下拉框要"点开→等→选→等"，不能 `selectOption` 一把梭
16. 任何字段填完都要**读回核对**（自动填充/水合会偷改）
17. 失败必须存证据：整页截图 + 验证码图 + 我们填的答案 + 页面红字

---

## 六、当前卡点（给接手人）

```
① Apple 建号在国家/号源/行为三层仍不稳定：连续两次图形码被拒后会触发硬拒
② Outlook 家宽产能只有每天 2~3 个，要批量必须换网络或加代理
③ 流量统计尚无实现，只能靠代理账单
④ 面板 B 需要的 profile 目录管理还没写
```

---

## 七、建议的推进顺序

1. 先按第 2 节把面板拆成 `/outlook`、`/accounts`、`/code-pool` 三个路由
2. 接并发队列（先跑 dry-run，确认调度正确）
3. 接真跑（加二次确认）
4. 补 profile 目录与流量字段
5. 最后再回到 Apple 成功率优化（那需要大量真号试错）

---

## 八、仓库与运行

```
仓库：D:\appleservices-family-private
控制台：http://127.0.0.1:8801（PID 文件 .cache/console/server.pid）
启动：powershell -ExecutionPolicy Bypass -File tools\console\start.ps1
停止：同上 -Stop
证据：.cache/<run>/ 下（已 gitignore）
凭据：.cache/mailboxes.json（0600）、.cache/console/tokens/*.json（0600）
```

**纪律**：不把密码/token/账号状态提交进 git；`.cache/` 全目录不入库。
