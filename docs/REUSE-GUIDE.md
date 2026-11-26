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
| 邮件等待 | `flow-mechanics.emailCodeWindow` | 90 秒后继续等待 120 秒，绝不自动重发；有效租期内保留原任务等迟到验证码 |

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

```powershell
node --test tools/capture/flow-mechanics.test.cjs tools/capture/flow-mechanics.browser.test.cjs tools/capture/executor-version.test.cjs
node tools/capture/create-account.cjs --version
```

浏览器测试使用本机 Chrome 和禁止外网访问的测试表单，不调用 Apple、不租用资源、不发验证码。缺少 Chrome 时该测试会跳过，验收时必须确认实际通过而非 skipped。创建器是 Node 脚本，新任务由面板调用 Python，再启动仓库内同一入口；修改后不需要替换协议用的 `signup.exe`。已有创建进程不会热更新，切换前应确认没有旧创建任务运行。
