# 复用指南：浏览器交互与识别能力（已实现，勿重写）

本仓库里浏览器交互、拟人节奏、识别接入都已经实现并有测试覆盖。
新任务请**直接复用**，不要另写一套。

---

## 一、可直接复用的模块

| 路径 | 内容 | 复用方式 |
| --- | --- | --- |
| `tools/capture/create-account.cjs` | 完整建号流程 | 仅按 CLI 调用；直接 require 会报错，不会启动注册 |
| `tools/capture/outlook-cn.cjs` | Outlook 独立流程 | 独立 CLI，不作为创建器的函数导出模块 |
| `tools/capture/flow-mechanics.cjs` | 共享输入读回、一次纠正、键盘下拉选择、提交定位、只读取码等待、关窗原因 | `require("./flow-mechanics.cjs")` |
| `tools/capture/flow-mechanics.test.cjs` | 上述逻辑的单测 | `node --test` |
| `tools/lib/image-text.cjs` | 图片转文字 | `const { readImageText } = require("../lib/image-text.cjs")` |
| `tools/captcha/solver.py` | 识别封装（含体积上限修复） | 命令行调用，或由 create-account 的 `--captcha-cmd` 注入 |

---

## 二、拟人交互的实现位置与参数

| 能力 | 位置 | 参数 |
| --- | --- | --- |
| 逐字符输入 | `create-account.cjs` 的 `typeHuman` | 沿用现有输入函数和节奏；不以耗时证明注册会被接受 |
| 下拉选择 | `selectHuman` → `flow-mechanics.selectByKeyboard` | 点开后通过 Home / ArrowDown / Enter 选择，随后读回 |
| 读回核对 | `fillIfNeeded` / `selectIfNeeded` → 共享模块 | 已正确则跳过；填写后失焦、读回，不符仅纠正一次；持续不符立即停止 |
| 提交前核对 | `create-account.cjs` 的 `ensureField` | 再检查姓名、生日、邮箱、密码、号码和地区，防止后续页面重绘覆盖 |
| 提交按钮定位 | `create-account.cjs` 的 `clickContinue` | 只匹配主提交按钮，排除"换一张/新码" |
| 邮件等待 | `flow-mechanics.emailCodeWindow` | 默认最多 10 分钟，拆成两段各 5 分钟以兼容读取器；不超过号码租期，绝不自动重发；有效租期内保留原任务等迟到验证码 |
| 短信等待与换号 | `flow-mechanics.phoneCodeWindow`、`phoneMenuAction`、`onceAction` | 首次等待 5 分钟，菜单选择短信重发一次，再等 5 分钟；仍无新码时在同一会话打开换号表单，最多换一个同国家的新号码。到期停止，未知点击不重放。 |

**用字段读回、页面校验和提交回执验收；不能按“少于 60 秒”等耗时阈值推断 Apple 的拒绝原因或可信度。**

---

## 三、硬性要求

```
① 禁止 page.fill() / selectOption() 直接赋值
② 禁止 10 秒内跑完整个表单
③ 每个字段填完必须读回核对，不符则清空重填一次
④ 等待页面可操作与校验信号，不能把固定等待或模拟测试通过当作注册成功；站点图形校验由人完成
⑤ 后台窗口会被浏览器降频，需要：
     --disable-renderer-backgrounding
     --disable-backgrounding-occluded-windows
     --disable-background-timer-throttling
```

三个参数已同时接入独立 Chrome 启动和 `pipeline.py` 的浏览器 open 调用；它们只影响之后新开的测试浏览器，不重启已有账号 worker。

---

## 四、识别（图片转文字）接入

```js
const { readImageText } = require("./tools/lib/image-text.cjs");
const text = await readImageText(imagePath);   // 返回字符串，失败为空串
```

- 默认走远程识别；key 从配置 / `YESCAPTCHA_KEY` / `state/secrets.json` 读取
- 单张约 5 秒
- 命令行：`node tools/lib/image-text.cjs <图片路径>`
- 这是通用图片文字模块的既有接口。本轮不启用自动完成站点图形校验；注册挑战仍使用人工输入，不将等待人工判定为模块退化。

---

## 五、常见退化（看到这些说明没复用）

| 现象 | 说明 |
| --- | --- |
| 填写后值被截断或重绘覆盖 | 检查 `field-verified` 和最终读回；失败必须终止，不能继续提交 |
| 已正确字段又被重打 | 检查是否走 `fillIfNeeded`，以及实际读回是否已变化 |
| 收不到邮件后又触发发码 | 错误行为：新版没有自动重发调用；核对运行哈希和实际请求记录 |
| 停在图形校验等人输入 | 人工挑战路径的正常状态，不据此认定未复用 |
| 后台窗口不推进 | 核对实际启动参数及页面状态；不能仅凭现象认定缺某参数 |

不能仅凭输入快慢判断使用了哪个 API。`create-account.cjs --version` 输出绝对入口、Node 版本和依赖源码 SHA-256；每个实际任务的首条运行记录为 `executor-version`，可与当前文件核对，防止把旧进程当成新版。

---

## 六、测试

首次加载从打开注册页开始，共享 90 秒截止时间，包含主页面、注册 iframe、frame 和邮箱输入框可见；浏览器启动及最多 3 秒的关闭清理不计入此预算。后续状态切换仍使用原有 45 秒等待。加载超时或连接失败记录 `page_load_timeout` / `page_load_failed`，面板显示“页面加载失败”，保留原资源，修复后继续原任务。每个任务继续使用已保存的本机 / Forge 模式、出口和语言配置。

退出码 1 本身不能证明 Apple 拒绝。明确拒绝文案才标记 `registration_rejected`；未提交的脚本故障显示执行失败；已经提交或缺少提交证据的异常保留为待核实，不能直接重开注册。长期会话维护另行处理。

```powershell
node --test tools/capture/flow-mechanics.test.cjs tools/capture/flow-mechanics.browser.test.cjs tools/capture/executor-version.test.cjs
node --test tools/capture/initial-load.test.cjs tools/capture/initial-load.browser.test.cjs tools/capture/creation-outcome.test.cjs
node tools/capture/create-account.cjs --version
```

浏览器测试使用本机 Chrome 和禁止外网访问的测试表单，不调用 Apple、不租用资源、不发验证码。缺少 Chrome 时该测试会跳过，验收时必须确认实际通过而非 skipped。创建器是 Node 脚本，新任务由面板调用 Python，再启动仓库内同一入口；修改后不需要替换协议用的 `signup.exe`。已有创建进程不会热更新，切换前应确认没有旧创建任务运行。

## 七、短信恢复约定

- 初次提交表单之前记录供应商旧码；只接受 `STATUS_OK` 六位新码。已提交过的验证码不再提交。供应商无法提供新旧消息标识时，相同数字保守视为旧码。
- 重发、换号及验证码提交在私有任务目录先落操作记录。异常或重启不自动重放结果未知的动作；菜单不包含语音呼叫路径。支持当前英语、简繁中文和葡语文案，未识别的文案安全停止，不猜按钮位置。
- 创建器通过 `--replacement-number-file` 接收私有 JSON，字段为 `previous_activation`、`activation`、`phone`（本地号码）、`phonecc`、`expires_at`，以及可选 `api`、`key`。必须匹配旧订单、同号码国家、不同号码、同供应商且租期有效。
- Python 收到 `replacement_number_required` 后沿用原购号预算和供应商的持久化采购记录，最多两个号码；没有付费权限或预算不足则停止并显示需要处理。换号不会取消或退款旧号码。新号码租期同步到浏览器与父进程截止时间。
- 出口国家、页面语言和账号地区使用任务原有配置，号码区号独立；美国出口可以搭配葡萄牙号码。预检不能拿号码国家当作出口国家。
- `--version` 的 `automaticResend` 分别报告邮件与短信策略。新任务首条 `executor-version` 的源码哈希必须对应部署版本；本地表单测试不代表 Apple 真实接受换号。

## 八、首次购邮与任务恢复

自动任务支持“创建任务 → 购买邮箱并保存回执 → 购号 → 所选浏览器注册 → 面板自动激活”。在注册资源中填写 OTPGmail 凭据、仓库和 VND 预算，已有订单路径留空即按任务购邮；填写订单路径仍沿用原绑定方式。默认仓库 2、单邮箱上限 700 VND，可调低预算或选择仓库 1/4。保存设置和查询不扣费，任务启动和继续购买仍受服务端 `--allow-purchase` 控制。本机 / Forge 和出口配置均随任务冻结。

`mail-purchase.json` 在付费请求前持久化原购买标识；`otpgmail-order.json` 保存已购订单与价格，`input.json` 绑定邮箱并取得账号占用锁后才允许购号。重复执行 `run --input` 只读取原任务。购邮中断、排队或缺货时，面板的“继续原购邮任务”（`POST /api/register/{id}/continue-mailbox`）或 `pipeline.py resume --job ID --allow-purchase` 继续原任务，不另建订单。

已知订单号只通过 GET 查询；响应丢失时复用原购买标识和请求体。供应商声明幂等键保留 24 小时，代码在 23 小时后禁止重发未知购买，要求核实原订单，避免键失效后重复扣费。排队、取消、已有验证码、回执不匹配或超预算都不能进入购号阶段；不自动切仓库、换国家或轮询抢库存。[供应商恢复约定](https://otpgmail.net/en/guides/otp-api-retries-timeouts-idempotency)

预算在购买前核对报价，并在返回后核对实付回执；接口未提供已验证的原子锁价参数，因此报价与下单之间涨价仍可能扣费，遇到超预算回执会保留原订单并停止后续购号，不重新购买。真实付费和 Apple 注册/激活验收不由这些离线测试替代。

验证命令：

```powershell
node --test tools/capture/otpgmail-acquire.test.cjs internal/dashboard/registration_mailbox.browser.test.cjs
python -m unittest discover -s tools/apple-pipeline -p test_mailbox_acquisition.py
go test ./internal/dashboard
```
