# 验证码可读性模型：v1 数据与训练方案

2026-11-27。当前阶段按用户最新要求先确定开源模型、训练方案与数据集。
已完成数据审计与冻结；**尚未训练模型，没有模型准确率、推理耗时或可部署模型文件**。
下述训练参数为首轮预注册方案，不是实验结果。OCR 训练目标与正在运行的采集进程不在本次改动范围内。

## 模型选择

首选 **TorchVision 官方 MobileNetV3-Small + ImageNet-1K V1 权重**，只训练可读性二分类。
使用固定枚举 `MobileNet_V3_Small_Weights.IMAGENET1K_V1`，记录包版本和下载权重 SHA-256，避免浮动 `DEFAULT`。
TorchVision 实现采用 BSD-3-Clause；预训练权重的数据来源和使用条款须随模型卡保留，不能把代码许可证当成训练数据许可证。

| 方案 | 官方原始模型规模 | v1 用途 |
| --- | --- | --- |
| MobileNetV3-Small | 2,542,856 参数；权重约 9.8 MB | 首选；移除整个 ImageNet 分类头，使用 576→1 线性头与 dropout=0.2 |
| ShuffleNetV2 0.5× | 1,366,792 参数；权重约 5.282 MB | 更小的备选；先不增加架构搜索，仅在首选无法满足体积或 CPU 延迟要求时评估 |
| PyIQA 的通用图像质量模型 | 随模型变化 | 不作为可读性真值或首选分类器；自然照片质量分数没有验证本任务的字符可辨认性 |

MobileNetV3-Small 替换分类头后预计约 927,585 参数、FP32 参数净载荷约 3.54 MiB；
这些是结构估算，最终必须测量实际模型文件、加载内存和 CPU 耗时。
选择官方实现与公开预训练权重，不从零训练一个未经验证的小网络。

来源：

- [TorchVision MobileNetV3-Small 模型及权重说明](https://docs.pytorch.org/vision/stable/models/generated/torchvision.models.mobilenet_v3_small.html)
- [TorchVision ShuffleNetV2 官方实现及权重元数据](https://github.com/pytorch/vision/blob/main/torchvision/models/shufflenetv2.py)
- [TorchVision BSD-3-Clause 许可证](https://github.com/pytorch/vision/blob/main/LICENSE)
- [PyIQA 官方项目](https://github.com/chaofengc/IQA-PyTorch)

## 数据审计结论

目标来源为 `.cache/captcha-pool-20261127-validation`，人工证据是
`manual-reviewed-labels.csv`、`manual-unreadable.csv` 和 `.cache/captcha-review-workbook/review-snapshot*.json`。
三份复核快照包含 246、456、279 条，共 981 条，SHA-256 集合与最终复核 CSV 一致。

- 原始 manifest 共 1,000 张，已复核 981 张；19 张没有人工复核，排除。
- 已复核图片全部存在、可解码，均为 160×70；981 个 SHA-256 唯一且与文件字节一致。
- CSV 为 832 条 accepted 和 149 条 quarantine，与交接数量相同。
- 5 条快照 `unreadable=false` 与明确的“无法核对／识别不出／分不清”备注不一致。
  依原始人类备注归为 refresh，与最终 quarantine CSV 一致；不修改原始快照。
- 另有 1 条 accepted 同时包含字符纠正和“刷新不必死磕”备注，不能断定是可靠可读或明确不可读。
  暂时排除，等待后续重新人工判断。排除清单及哈希只写本地审计文件。
- **最终冻结 980 张：831 readable、149 refresh。** 相比交接，可读训练候选减少 1 张。
- 人工输入的 `MH/mh` 是本批模糊备注的缩写；严格匹配这一约定，不把任意备注自动当成 refresh。
- API 字符猜测、OCR 置信度、识别成功与否均不参与可读性标签判断。

旁路数据：另一个独立目录有 4 条 user_confirmed 和 1 条 unreadable，暂不并入本批，
因为其人工证据格式不同；其他扩展目录和独立测试目录的 `labels.csv` 尚无已验证标签。
不把 ongoing 采集目录的实时数量当成冻结数据量。

公开数据检索尚未找到可直接替代本批人工二分类真值的可靠数据集。
[Hansda/captcha-dataset 的发布说明](https://huggingface.co/datasets/Hansda/captcha-dataset/blob/main/README.md)
列出 12,000 张合成图与字符字符串标签，许可证标为 Apache-2.0，但没有人工 readable/refresh 标签。
因此首版不使用它来补充正负例，也不以“有字符串标签”推断图片可读。
通用 IQA 数据集的图像质量评分同样不能直接转成此任务标签。

## 防泄漏分组与冻结拆分

`manifest.json` 未保存采集会话 ID，但当前采集器按保存顺序追加 samples，并输出累计 `saved=N`。
将完整日志中的 N 与 `samples[N-1]` 对齐，可恢复五次采集运行的图片来源：

| 来源组 | manifest 中 1-based 序号 | 可读 | 刷新 | 拆分 |
| --- | --- | ---: | ---: | --- |
| legacy-unknown | 1–468 | 394 | 67 | train |
| harvest-20261127-153325 | 469–471 | 2 | 1 | train |
| harvest-headless-20261127-154037 | 472–603 | 116 | 15 | calibration |
| harvest-headless-20261127-161329 | 604–952 | 279 | 60 | validation |
| harvest-headless-20261127-170033 | 953–962 | 10 | 0 | train |
| harvest-headless-20261127-172737 | 963–1000 | 30 | 6 | train |
| **合计** | 已排除未复核与歧义样本 | **831** | **149** | |

同一次运行的多窗口、连续刷新和窗口重建一律留在同组；早期缺失日志的样本整体作为一个组。
没有使用文件时间、人工复核表批次、字符内容或随机逐图抽样划分来源。
恢复依据依赖原始 manifest 的追加顺序及对应累计日志；不是服务器提供的会话证明。
审计脚本记录所有输入文件哈希，拒绝重复累计序号、不连续日志、输入审计期间发生改变等情况。

近重复使用 63-bit DCT pHash（不含 DC），Hamming≤6 后再要求灰度缩略图平均绝对误差≤0.035。
满足条件的图片通过并查集合并其**整个来源组**；若合并后跨拆分，审计失败，必须重新安排整组归属。
这属于近重复启发式检查，不能证明没有任何经过裁剪或复杂变形的近重复。
当前冻结集没有满足上述双重条件的近重复对。

| 集合 | readable | refresh | 总量 | 用途 |
| --- | ---: | ---: | ---: | --- |
| train | 436 | 74 | 510 | 训练参数，类别权重仅由此集计算 |
| calibration | 116 | 15 | 131 | 概率与判定阈值校准 |
| validation | 279 | 60 | 339 | 独立真实验证；训练、模型选择和阈值选择均不可使用 |

为了保留完整采集组及足够的真实 refresh 验证样本，没有强行凑 80/20 比例。
全部来自同一天、同一验证码样式，验证能衡量跨采集运行表现，不能证明跨日期、跨样式泛化。

## 首轮训练与推理约定

1. 固定随机种子 20261127。创建隔离虚拟环境，安装官方 CPU PyTorch/TorchVision 配套版本并锁定版本；不修改 OCR 环境。
2. RGB 输入，等比缩放至 224×98，白边填充到 224×112，使用 ImageNet mean/std。
   不使用默认中心裁剪，以免切掉字符。推理与训练使用完全相同的基础预处理。
3. 第一轮不使用合成负例，也不模糊、遮挡、裁剪、翻转或进行 MixUp/CutMix。
   从无增强基线开始；后续若增加增强，只允许经过核验的轻微亮度变化，保留原始图片和标签。
4. 冻结骨干训练线性头 5 epoch，AdamW，lr=1e-3，weight_decay=1e-4，batch_size=32；
   再解冻最后 3 个 feature block 与分类头训练 10 epoch，lr=1e-4，固定其余骨干。
   小样本阶段固定 BatchNorm 运行统计。冻结所有 epoch 数，不通过最终 validation 选择 checkpoint。
5. readable 为 1、refresh 为 0。逐样本加权 BCE：
   `w_readable=510/(2*436)≈0.585`，`w_refresh=510/(2*74)≈3.446`。
   不再同时过采样，避免重复放大类别权重。
6. 在 calibration 上拟合受约束的单调 Platt 校准（正斜率、正则化），记录原始与校准后的 Brier score。
   这是小样本经验概率估计，`readable_probability` 不能解释为经过保证的识别成功率。
7. calibration 上选择满足 false-readable rate≤5% 的最低阈值，以尽可能保留可读图。
   15 个真实 refresh 意味着必须 0 次误放才满足该点估计目标。
   若无法保留可读图，明确报告全部刷新或低覆盖失败，不用提高总体准确率掩盖它。
8. 保存模型、预处理、阈值、校准参数、版本、输入清单哈希及包版本；正式推理不再下载预训练权重。
   稳定 Python 接口计划为 `ReadabilityModel(model_path).predict(image)`，返回
   `decision`（`readable`/`refresh`）、`readable_probability`（0–1）、`model_version`。
   UI 映射为“可读”/“刷新”；损坏输入应显式报错，由调用方安全处理，不产生假概率。

首轮不做超参数搜索或多随机种子择优，不读取最终 validation 结果再选择架构。
ShuffleNet 若需启用，应在新的实验中明确记录其选择依据，不能在同一最终验证集上反复挑冠军并称其为独立效果。

## 实验验收与风险

一次性独立验证必须同时输出混淆矩阵（真值行、预测列，顺序 refresh/readable）与原始计数：

- refresh recall = 正确刷新的 refresh / 全部真实 refresh。
- false-readable rate = 被放行的 refresh / 全部真实 refresh；与 refresh recall 互补。
- readable retention = 被放行的 readable / 全部真实 readable。
- coverage = 全部放行图片 / 全部图片；不能与 readable retention 混用。
- overall accuracy、放行结果中 refresh 的比例；分母为零时报告 null。
- false-readable rate 的精确二项置信区间及各来源组计数，避免只报百分比。
- 实际模型文件字节数；CPU 型号、包版本、线程数；预热后 batch=1、单线程 1,000 次耗时的 median/p95。
  单独报告解码+预处理+推理端到端耗时与纯模型耗时，模型加载时间不混入热调用耗时。

候选实验目标为最终验证 false-readable rate≤5%、readable retention≥50%；未达到即不建议集成。
这些是事先约定的评估目标，不是已经达到的效果，也不是自动部署授权。
最终验证 60 个 refresh 即便 0 误放，单侧 95% 精确上界仍约为 4.87%；
calibration 的 15 个 refresh 即便 0 误放，上界仍约为 18.1%。若要求证明误放率低于 1%，
需要至少 299 个足够独立的真实 refresh 且 0 误放；采集组内相关性还可能使简单二项区间过于乐观。

## 已执行的冻结与复现命令

以下从仓库根目录执行；审计仅需 Python、NumPy、Pillow，不加载模型、不发网络请求、不修改来源文件。
本次环境：Python 3.12、NumPy 2.5.4、Pillow 13.0.0。

```powershell
python -m unittest discover -s tools/captcha -p test_audit_readability.py -v
python tools/captcha/audit_readability.py `
  --dataset .cache/captcha-pool-20261127-validation `
  --reviews .cache/captcha-review-workbook `
  --out .cache/readability-v1/dataset `
  --calibration-batch harvest-headless-20261127-154037 `
  --validation-batch harvest-headless-20261127-161329
```

输出目录必须不存在，重复审计请换新目录，不覆盖已冻结清单。
本次冻结 manifest SHA-256：`7d2288317187d5999a27c94ddad6b05de66923f50c4250c009890e12e2e18b68`。
`audit.json` 包含输入哈希、来源组、排除项和计数；`manifest.json` 仅包含图像路径、哈希、可读性标签与拆分，
不包含字符标签、API 预测或账号状态。后续训练仍须逐图重验哈希，拒绝文件变化。
训练命令与实际模型文件将在实现训练器并完成首轮实验后提供，本阶段不列出尚不存在的命令。

仅提交审计源码、单元测试及本方案。数据、冻结清单、日志、模型二进制全部留在 git 忽略的 `.cache` 内。
