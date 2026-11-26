# 复用指南：浏览器交互与识别能力（已实现，勿重写）

本仓库里浏览器交互、拟人节奏、识别接入都已经实现并有测试覆盖。
新任务请**直接复用**，不要另写一套。

---

## 一、可直接复用的模块

| 路径 | 内容 | 复用方式 |
| --- | --- | --- |
| `tools/capture/create-account.cjs` | 完整建号流程 + 全套拟人交互 | 当执行器调用（CLI），或 require 其中的函数 |
| `tools/capture/outlook-cn.cjs` | 同风格流程 + 交互式校验（按住到出勾） | 同上 |
| `tools/capture/flow-mechanics.cjs` | 纯逻辑：只填一次、提交定位、取码时间窗、关窗原因 | `require("./flow-mechanics.cjs")` |
| `tools/capture/flow-mechanics.test.cjs` | 上述逻辑的单测 | `node --test` |
| `tools/lib/image-text.cjs` | 图片转文字 | `const { readImageText } = require("../lib/image-text.cjs")` |
| `tools/captcha/solver.py` | 识别封装（含体积上限修复） | 命令行调用，或由 create-account 的 `--captcha-cmd` 注入 |

---

## 二、拟人交互的实现位置与参数

| 能力 | 位置 | 参数 |
| --- | --- | --- |
| 逐字符输入 | `create-account.cjs:653` `typeHuman` | 每字符 120~350ms，12% 概率额外停 400~1100ms |
| 下拉选择 | `create-account.cjs:630` `selectHuman` | 点开 → 停 300~800ms → 选 → 停 250~700ms |
| 读回核对 | `create-account.cjs:644` `fillIfNeeded` | 值相同则跳过，重试时不重填 |
| 提交按钮定位 | `create-account.cjs:796` `clickContinue` | 只匹配主提交按钮，排除"换一张/新码" |
| 提交前漂移 | `create-account.cjs:1527` | 贝塞尔 18 步 + 停 600~1500ms |
| 交互式校验 | `outlook-cn.cjs` 按压段 | 页面内观察控件状态，出现"完成"信号才松手 |

**参考耗时：一个账号从打开页面到提交 90~150 秒。少于 60 秒视为不合格。**

---

## 三、硬性要求

```
① 禁止 page.fill() / selectOption() 直接赋值
② 禁止 10 秒内跑完整个表单
③ 每个字段填完必须读回核对，不符则清空重填一次
④ 交互式校验等控件自己出"完成"信号，不能按固定时长
⑤ 后台窗口会被浏览器降频，需要：
     --disable-renderer-backgrounding
     --disable-backgrounding-occluded-windows
     --disable-background-timer-throttling
```

---

## 四、识别（图片转文字）接入

```js
const { readImageText } = require("./tools/lib/image-text.cjs");
const text = await readImageText(imagePath);   // 返回字符串，失败为空串
```

- 默认走远程识别；key 从配置 / `YESCAPTCHA_KEY` / `state/secrets.json` 读取
- 单张约 5 秒
- 命令行：`node tools/lib/image-text.cjs <图片路径>`
- 流水线里由 `captcha_solver` 决定是否启用；未配置时默认启用，找不到 key 会明确报错而不是静默等人工

---

## 五、常见退化（看到这些说明没复用）

| 现象 | 说明 |
| --- | --- |
| 整个表单 10 秒内完成 | 用了 `fill()` 直接赋值 |
| 下拉框瞬间选中 | 用了 `selectOption()`，没有点开过程 |
| 重试时把已填字段全部重打一遍 | 没有用 `fillIfNeeded` |
| 停在图片那步等人输 | 识别没接上，或 key 没找到 |
| 多个窗口只有第一个在动 | 缺少第三节的三个启动参数 |

---

## 六、测试

```powershell
node --test tools/capture/flow-mechanics.test.cjs
node tools/lib/image-text.cjs .cache/captcha-big.png
```
