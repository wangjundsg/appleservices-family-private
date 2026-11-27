# 完整 A/B 2000 张：可读性模型训练与评估

2026-11-27。完整 A/B 清单替代 A-only 入口；没有先训练 A-only，也没有等待后续批次。
本轮只训练图片可读性，不输出字符标签、不修改 OCR 训练输入。
**训练已完成，但误放率与可读保留率均未达到预定目标，暂不建议接入实际流程。**

## 人工数据核验

- 新增 2,000 张：1,887 readable、113 refresh；全部原图存在、可解码、160×70，SHA-256 唯一且匹配。
- A 为 902/98，B 为 985/15（顺序 readable/refresh）。逐部分 refresh 集合与原始人工导出备份完全相符。
- 完整入口中的 A 标签与先前 A-only 清单一致。新数据与上一轮 981 张没有 SHA-256 重叠。
- 合并 2,981 张后按同一 pHash＋像素差阈值检查：21 对 pHash 候选，0 对满足双重近重复条件。
- `review_part` 原样保留；旧数据标为 `legacy`。来源目录整体作为采集组，近重复关系若跨拆分会拒绝冻结。
- `refresh` 必须 `ocr_eligible=false`，人工导出中的字符标签必须为空；不把难例或 OCR 失败自动当成 refresh。
- 冻结清单 SHA-256：`4c0662463ce7966c9b21dc2f1c9602d16bb552cf25635b65446c195f8487ed78`。

| 来源目录 | A readable/refresh | B readable/refresh | 用途 |
| --- | ---: | ---: | --- |
| expansion-2000 | 379/36 | 382/3 | 训练 |
| expansion-fixed-600 | 260/32 | 302/6 | 校准 |
| expansion-us07-600 | 263/30 | 301/6 | 全新验证 |

三个来源内 A 的 refresh 比例均高于 B，因此总体 9.8% 对 1.5% 的差异不能仅用来源占比解释。
这可能涉及样本难度或人工尺度差异，现有证据不能断定哪部分标错。保留原始人工判断，不自动改标；
`review_part` 只用于审计和分组指标，不作为图像模型输入，也不为 A/B 设置不同线上阈值。

| 集合 | readable | refresh | 总计 |
| --- | ---: | ---: | ---: |
| 训练：旧训练＋expansion-2000 | 1,197 | 114 | 1,311 |
| 校准：旧校准＋expansion-fixed-600 | 678 | 53 | 731 |
| 全新验证：expansion-us07-600 | 564 | 36 | 600 |
| 回归：上一轮验证，仍不训练 | 279 | 60 | 339 |

旧验证集仅作回归对照，不计入本轮全新验证。B 的新验证只有 6 张 refresh，百分比和置信区间必须结合计数看。

## 固定实验方案与复现

沿用首轮配方：MobileNetV3-Small 官方预训练初始化，5 epoch 分类头＋10 epoch 末端微调，
冻结 BatchNorm 运行统计；固定种子、两 CPU 线程、类别加权 BCE，无增强和合成负例。
使用合并数据从同一官方起点重训，不覆盖首轮模型。统一阈值仍只由校准集选择，要求总体校准误放率≤5%，
同时报告 A、B、legacy 各自的校准指标。最终验证与旧模型比较均在新模型和阈值固定后进行，
不根据全新验证结果选择 checkpoint 或调整阈值。

在仓库根目录执行，使用 [锁定环境](requirements-readability.txt)：

```powershell
.tools/readability/Scripts/python.exe tools/captcha/prepare_readability_review.py `
  --review-manifest .cache/captcha-readability-review-2000-20261127/readability-manifest.csv `
  --previous-manifest .cache/readability-v1/dataset-confirmed/manifest.json `
  --out .cache/readability-v1/dataset-ab-2000 `
  --calibration-source captcha-pool-20261127-expansion-fixed-600 `
  --validation-source captcha-pool-20261127-expansion-us07-600
.tools/readability/Scripts/python.exe -m unittest discover -s tools/captcha -p '*readability*.py' -v
.tools/readability/Scripts/python.exe tools/captcha/train_readability.py `
  --manifest .cache/readability-v1/dataset-ab-2000/manifest.json `
  --out .cache/readability-v1/run-ab-01 --threads 2 `
  --baseline-model .cache/readability-v1/run-01/model.pt
```

输出目录须不存在；重复实验请换新目录，保留既有冻结清单和结果。
准备器读取完整入口同目录的 `review-refresh-part-a.csv`、`review-refresh-part-b.csv`、`README.md`，
核验标签和图片后记录输入及准备源码哈希。模型沿用 [既有推理接口](READABILITY.md)，只需更换模型路径。
数据、模型、逐图预测与完整 JSON 报告仅存 `.cache`，不提交到 Git。

## 实验结果：全新验证与 A/B 差异

模型版本 `readability-mnv3s-v1-6d725e24f61c`；纯训练耗时 185.54 秒。
类别权重 refresh=5.75、readable=0.547619。Platt 参数 slope=0.4086856234、intercept=2.2441231722。
仅当 readable_probability **>0.9691470287** 时放行，阈值由校准集决定，最终验证后未再调整。

矩阵均为真值行、预测列，类别顺序 `[refresh, readable]`。

| 全新验证范围 | 混淆矩阵 | refresh 召回率 | 不可读误放率 | 可读保留率 | 放行覆盖率 | 准确率 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| A，293 张 | `[[26,4],[218,45]]` | 86.67% | **4/30 = 13.33%** | 45/263 = 17.11% | 16.72% | 24.23% |
| B，307 张 | `[[6,0],[261,40]]` | 100.00% | **0/6 = 0.00%** | 40/301 = 13.29% | 13.03% | 14.98% |
| 合计，600 张 | `[[32,4],[479,85]]` | **88.89%** | **4/36 = 11.11%** | **85/564 = 15.07%** | **14.83%** | **19.50%** |

全部四次误放来自 A；放行的 89 张中有 4 张真实 refresh（4.49%），不要与 4/36 的误放率混淆。
全新验证总体误放率的双侧 95% 精确二项区间为 **3.11%–26.06%**。
B 的 0/6 对应单侧 95% 上界 **39.30%**，不能当成“B 已可靠”。
所有样本来自单日同一样式，并且来源组内相关性可能使逐图二项区间偏乐观。
总体与分组结果均不支持将当前模型当成可靠筛选器；该实验也不能确定失败应归因于模型表征还是人工尺度差异。

## 校准和旧验证回归

| 校准范围 | 混淆矩阵 | 不可读误放率 | 可读保留率 |
| --- | --- | ---: | ---: |
| A，292 张 | `[[31,1],[215,45]]` | 1/32 = 3.13% | 45/260 = 17.31% |
| B，308 张 | `[[6,0],[246,56]]` | 0/6 = 0.00% | 56/302 = 18.54% |
| legacy，131 张 | `[[14,1],[94,22]]` | 1/15 = 6.67% | 22/116 = 18.97% |
| 合计，731 张 | `[[51,2],[555,123]]` | 2/53 = 3.77% | 123/678 = 18.14% |

统一阈值满足预注册的总体校准误放率≤5%，不代表每个子集都满足该条件；legacy 校准子集超过 5%。
校准表现不能冒充全新验证结果。

旧验证 339 张仅作回归对照：新模型矩阵 `[[57,3],[238,41]]`，误放率 3/60=5.00%，
可读保留率 41/279=14.70%。旧模型在这批旧验证上为 1/60=1.67%、29/279=10.39%。
这同样是保留率上升伴随误放增加，并非全面改善。

## 同一全新验证集上的旧模型对照

旧模型采用其原始校准和阈值，没有针对 A/B 调参；新旧模型都在同一 600 张上评估：

| 模型 | A 误放率／可读保留率 | B 误放率／可读保留率 | 总体误放率／可读保留率 |
| --- | ---: | ---: | ---: |
| 首轮 `6fc088e046a2` | 3/30=10.00% ／ 29/263=11.03% | 0/6=0.00% ／ 21/301=6.98% | **3/36=8.33% ／ 50/564=8.87%** |
| 本轮 `6d725e24f61c` | 4/30=13.33% ／ 45/263=17.11% | 0/6=0.00% ／ 40/301=13.29% | **4/36=11.11% ／ 85/564=15.07%** |

不能用首轮旧验证上的 1.67% 与本轮新验证上的 11.11% 直接衡量模型退化；同集对照才是上表。
新增数据后的模型并未达到“误放率≤5% 且可读保留率≥50%”的实验目标，两版均保留。

## 文件、CPU 耗时与验证

- 新模型：`.cache/readability-v1/run-ab-01/model.pt`，**3,830,635 字节（3.65 MiB）**，927,585 参数。
- 模型 SHA-256：`8cca6c42fe730cd01d9ab04045d4fad5fe2817ac5517fa470d5be8179b3816a5`。
- 完整报告：`.cache/readability-v1/run-ab-01/report.json`，包含逐 A/B 指标、校准和回归指标、旧模型同集对照。
- 逐图预测分别保存为 `validation-predictions.json`、`baseline-validation-predictions.json`、
  `regression-predictions.json`、`calibration-predictions.json`，均保留 `review_part`。

CPU 为 AMD Ryzen 7 4800H；单线程、batch=1，预热 20 次后测量 1,000 次：

| 测量范围 | median | p95 |
| --- | ---: | ---: |
| 纯模型 | 7.89 ms | 10.73 ms |
| 文件读取＋解码＋预处理＋推理 | **9.80 ms** | **12.98 ms** |

模型已加载，文件读取可能命中系统缓存；其他工作负载未隔离，因此不与上轮耗时直接作性能优劣结论。
25 项针对性测试通过，覆盖 A/B 备份一致性、refresh 的 OCR eligibility 校验、原验证隔离、分组指标分母和既有推理契约。
另从保存的逐图预测核验 A/B 标签、集合归属和混淆矩阵，重验模型及实验源码哈希，并检查真实图片字节推理接口。
推理接口继续返回 `decision`、`readable_probability`、`model_version`，仅模型路径变化。
