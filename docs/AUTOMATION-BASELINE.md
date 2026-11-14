# 浏览器自动化选型基线（2026-11-14）

> 问题：注册机要接 Roxy/Forge 的指纹浏览器，**Playwright `connectOverCDP`** 和 **Selenium `debuggerAddress`** 哪条更适合？
> 方法：同一个 profile、同两个客户端、三个公开检测页各跑一次，比可见暴露面。
> 结论：**Playwright**。不是因为"更隐身"（两边完全一样），而是因为**不绑驱动版本**、**同栈**。

---

## 1 纪律与方法

* **一个 profile 两个客户端**：`POST /browser/create` → `POST /browser/open` → 拿 `debuggingPort`，先 A 后 B 连同一个端口。不新建、不换指纹。
* **三个检测页各跑一次**（公开工具，只读）：
  * `https://bot.sannysoft.com/`（表格红绿项）
  * `https://abrahamjuliot.github.io/creepjs/`（headless/stealth/一致性）
  * `https://browserleaks.com/javascript`（`webdriver`、UA 一致性）
* **不注册任何账号、不购买任何服务**；跑完 profile 一律 `close + delete`。
* 每页除了检测页自身结论，还额外采集一份**主世界探针**（同一份 `tools/detect/probe.js` 两端共用）：`navigator.webdriver`、`window.chrome`、`permissions`、可疑全局扫描（`cdc_*` / `__playwright*` / `selenium*`）、`Function.prototype.toString` 是否被改写。

```powershell
# 一条命令跑完（create → open → A → B → close → delete）
node tools/detect/detect-clients.cjs --label my-baseline --locale zh-CN --tz Asia/Shanghai
python tools/detect/summarize.py .cache/detect-baseline/my-baseline
```

---

## 2 结果

两轮独立 profile 完全一致：`baseline-4`（Roxy 拉起 **Chrome 153**）与 `baseline-2`（**Chrome 155**）。

| 指标 | A: Playwright `connectOverCDP` | B: Selenium attach |
|---|---|---|
| 客户端版本 | playwright-core **1.65.0** | selenium **4.52.0** + chromedriver 153/155 |
| sannysoft / creepjs / browserleaks 加载 | 3/3 ok | 3/3 ok |
| `navigator.webdriver`（三页） | `false` / `false` / `false` | `false` / `false` / `false` |
| sannysoft `WebDriver (New)` | missing（passed） | missing（passed） |
| sannysoft `WebDriver Advanced` | passed | passed |
| sannysoft `Chrome (New)` | present（passed） | present（passed） |
| sannysoft `Permissions (New)` | prompt | prompt |
| **sannysoft 红项数量** | **0** | **0** |
| creepjs `chromium` | true | true |
| creepjs `like headless` / `headless` / `stealth` | **19% / 0% / 0%** | **19% / 0% / 0%** |
| creepjs `Resistance` | privacy/security/mode/extension 全 unknown | 同左 |
| browserleaks `webdriver` 行 | `webdriver false` | `webdriver false` |
| browserleaks UA 行 | 与 `navigator.userAgent` 一致 | 同左 |
| **注入痕迹**（主世界可疑全局 / DOM 属性 / toString 改写） | **0 / 0 / 未改写** | **0 / 0 / 未改写** |
| `window.chrome` | object `['loadTimes','csi','app']`，无 `runtime` | 同左 |
| UA / locale / TZ | `Windows NT 10.0; Win64; x64` / `zh-CN` / `Asia/Shanghai` | 完全相同 |
| 单轮耗时 | 32s | 33s |

> **creepjs 说明**：当前线上版本（`fp-app` 构建）**不再渲染 `trust score` 区块**，改报 headless/stealth 比例与 `Resistance` 四项。所以"信任分"这一栏用等价指标替代：`like headless 19%`、`headless 0%`、`stealth 0%`，两端**逐字一致**。

### 2.1 唯一能看出的差别：我们自己跑脚本时的调用栈形状

```
A: Error: probe | at eval (eval at evaluate (:370:30), <anonymous>:111:21)
B: Error: probe | at eval (eval at <anonymous> (:457:26), <anonymous>:113:21)
```

这是两端注入执行器的包装差异，**只有在我们主动 `evaluate` 时**才出现在 `Error.stack` 里；检测页本身读不到。就"注册表单能不能过"而言不构成差别。

---

## 3 结果一样，为什么还是选 A

暴露面打平之后，决定因素是工程成本。这一栏是硬的：

| 维度 | A: Playwright | B: Selenium | 证据 |
|---|---|---|---|
| **驱动版本耦合** | 无，直接讲 CDP | **强耦合**：chromedriver 大版本必须等于内核大版本 | Roxy 同一装机会轮换内核（`/` 回包 `core.drift = 151/152/153/154`，当前 155）。实测 `baseline-3`：内核 **152** + 硬编码 driver **155** → `SessionNotCreatedException: This version of ChromeDriver only supports Chrome version 155 / Current browser version is 152` |
| **驱动路径** | 不需要 | 需要按实际内核挑：`chrome-bin\<major>\chromedriver.exe`（改法是查 `/json/version` 再选，见 `tools/detect/detect-clients.cjs` 的 `resolveDriver`） | `baseline-4` 拉 153 → 自动选 153 驱动 → 通过 |
| **启动参数** | attach 模式无意义 | attach 模式无意义，且 **chromedriver 155 直接拒收** `excludeSwitches` | `invalid argument: unrecognized chrome option: excludeSwitches` |
| **异步脚本** | `page.evaluate` 原生 await Promise | Execute Script **不 await**，异步探针整份返回 `None`（第一轮就踩了） | 必须改 `execute_async_script` + 手动回调包装 |
| **进程数** | 1（node） | 2（python + chromedriver 常驻） | 多一个崩溃点/端口 |
| **关闭语义** | `browser.close()` 在 CDP attach 下**只断开**：实测 Chrome 进程存活、已开页面存活（`tools/detect/close-pages.cjs`） | `driver.quit()` 只断会话 | 两者都不负责收尾，profile 生命周期仍归 `/browser/close` |
| **和现有栈的关系** | 仓库里 `tools/capture/*.cjs` 已全部是 playwright-core | 需新增 Python 依赖（`pip install selenium`） | — |

**结论：选 A（Playwright `connectOverCDP`）。** 使用纪律三条：

1. **不调 `browser.close()` 收尾**——它不会杀 Chrome，但会作废 Playwright 侧对象，后续 B/后续步骤拿到死句柄。页面用 `page.close()`，浏览器用 `/browser/close`。
2. **异步探针直接 `page.evaluate`**，需要返回对象时就返回普通对象/Promise，Playwright 会 await。
3. **每次 attach 后先做 §1.4 的四件套校验**（UA 系统标识 / locale / TZ / 出口国），别信 `create` 回包的 `fingerprint`。

---

## 4 证据文件

| 路径 | 内容 |
|---|---|
| `.cache/detect-baseline/baseline-4/run.json` | 端到端会话链（create/open/A/B/close/delete 的业务码） |
| `.cache/detect-baseline/baseline-4/A-playwright.json` / `B-selenium.json` | 两端逐页原始采集 |
| `.cache/detect-baseline/baseline-4/comparison.md` | 上面那张对比表（脚本生成） |
| `.cache/detect-baseline/baseline-4/shots/A-*.png`、`B-*.png` | 六个页面截图 |
| `.cache/detect-baseline/baseline-3/run.json` | **反例**：内核 152 + 驱动 155 挂不上的现场 |
| `tools/detect/detect-clients.cjs` | 编排器（业务码检查 / 驱动自动挑 / delete 退避重试） |
| `tools/detect/client-playwright.cjs`、`tools/detect/client_selenium.py` | 两个客户端 |
| `tools/detect/probe.js` | 两端共用的主世界探针 |
| `tools/detect/close-pages.cjs`、`close-semantics.cjs` | `browser.close()` 语义实验 |

> `.cache/` 已在 `.gitignore`：截图与采集结果不入库；需要复现就重跑第 1 节那两行命令。
