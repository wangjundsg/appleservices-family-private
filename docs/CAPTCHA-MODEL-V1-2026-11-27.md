# 验证码决策模型第一版训练与验收记录（2026-11-27）

## 结论

已完成两次真实 PP-OCRv6 small 微调、模型导出、内部评估和 200 张独立人工真值验收。第一版候选的清晰图整串识别明显优于现用模型，但“刷新不可读图”和“保留清晰图”仍不能同时达到可靠水平；继续微调一轮得到的第二版在独立集上退化。因此保留第一版作为实验检查点，淘汰第二版，**没有替换现用模型**。

第一版在独立 183 张清晰图上整串完全正确 135 张（73.8%），现用模型为 84 张（45.9%）。但第一版的平衡阈值会误刷新 46.4% 的清晰图；限制清晰图误刷新后，仍会漏掉 41.2% 的不可读图。该取舍不满足可靠上线条件。

## 数据清单与隔离

主数据集为 `.cache/captcha-pool-20261127-validation`。所有图片在读取时重新计算 SHA-256；四个分组按图片哈希零交集，且均属于主 `manifest.json`。原始字符标签保留原大小写，训练目标转为大写，推理和指标统一忽略大小写。

| 分组 | 数量 | CSV SHA-256 | 用途 |
| --- | ---: | --- | --- |
| OCR 训练 | 782 | `375e547da0e48e3575d52d284e3e9bb3fd2f7dd31ebbcc1ef05ecb5796891217` | 仅训练 |
| OCR 验证 | 30 | `c25e5ba39955525555688c0892eed608db8785d35fe5a1aa1fdd06eac28bcf1e` | 阈值校准/内部验证 |
| OCR 测试 | 20 | `6ee7bfc724e920ebb76ed8b8e65b4b301d36da742c76bdcb4c50f05c5701e42b` | 内部测试 |
| 人工不可读 | 149 | `edf42ec6671e7a31c8e3d61fbeab25d6e424278ab5d03e8bb742024e57207dfa` | 124 张校准刷新阈值、25 张内部测试；从未赋字符标签 |

主清单 SHA-256 为 `83f6e3aa1cc12b4fa83f680692aada7a0f442359a5efc0c89dca8361a637f22b`。另有 19 张未完成复核的主集图片，未进入训练或评估。

独立集 `.cache/captcha-test-20261127-independent` 与 `.cache/captcha-validation-20261127-refresh` 各 100 张；清单 SHA-256 分别为 `69141858b559251c72ecfa2e7bc488c729f60ec863ee4e3d5e8a98d9e1a089dc`、`4bef253fd38c7cdb01955896450fd39f6f74048c4251f880c617de419ffcb7f8`。它们与主集及彼此均无哈希交集，也从未进入训练或阈值选择。

人工复核工作簿 SHA-256 为 `048418f092638119a011f7efecbb929b3172d37aa85663a02cf230ace3c6cb9a`。按“空白表示参考标签正确、填写修正表示采用修正、标记或备注看不清表示刷新”的约定，得到 183 张清晰图和 17 张不可读图：22 张使用人工修正，161 张由空白确认参考标签。两个独立集分别为 95/5 和 88/12（清晰/不可读）。导出的固定真值 CSV SHA-256 为 `02a04f75e97e3d50b4fddf00bbf7f57c6038354b51d390ebdb3a56e37df7a9ae`；评估脚本会验证其覆盖、路径、标签格式和每张图片的 SHA-256。

## 训练配置

- 官方 PaddleOCR 训练源码固定在提交 `dab3fe35379033fdcb2d0e9572fac0b36c9a9ebf`。
- 第一版从官方 PP-OCRv6 small 预训练参数微调 1 轮；第二版从第一版 `latest.pdparams` 再微调 1 轮。批大小均为 8，随机种子 `20261127`，学习率 `1e-4`、余弦衰减。
- 保留官方 `RecConAug` 和 `RecAug`；不可读图片只用于刷新阈值，绝不伪造字符标签训练 OCR。
- 782 张训练图共 99 批；训练、验证、测试分组由显式 CSV 固定，不自动重分组。
- 第一版推理权重 SHA-256：`644567c74a0693907eb632792916caee0cbcc7ecc34af0217075f0cec715dba8`。
- 第二版推理权重 SHA-256：`b93888a71d8c1a61b1628b1ea87c5eb5b9f596f10d33e710b82d97e4f355bb88`。
- 刷新决策使用同一 OCR 的格式与整串置信度，不另训低层图像分类器。阈值只用 30 张清晰验证图和 124 张不可读校准图选择；20 张清晰测试图、25 张不可读测试图及两个独立集均不参与阈值选择。

本地运行目录为 `.cache/ocr-training-20261127/run-first-v1-e1` 和 `.cache/ocr-training-20261127/run-first-v2-e1`。配置、日志、检查点、导出模型、真值 CSV 和评估 JSON 都是运行产物，不进入 Git。

## 内部结果

OCR 整串完全正确率（忽略大小写）：

| 模型 | 验证集 | 测试集 |
| --- | ---: | ---: |
| 现用 PP-OCRv6 small | 13/30（43.3%） | 8/20（40.0%） |
| 第一版候选 | 22/30（73.3%） | 14/20（70.0%） |
| 第二版候选 | 22/30（73.3%） | 11/20（55.0%） |

平衡阈值的内部测试结果：

| 模型 | 阈值 | 刷新召回率 | 清晰图误刷新率 | 正确处理比例 |
| --- | ---: | ---: | ---: | ---: |
| 现用 | 0.8923 | 84% | 50% | 29/45（64.4%） |
| 第一版 | 0.9624 | 88% | 60% | 30/45（66.7%） |
| 第二版 | 0.9525 | 96% | 50% | 32/45（71.1%） |

小型内部测试看似偏向第二版，但独立集显示第二版 OCR 明显退化，说明不能用 45 张内部决策样本选模型。

## 独立人工真值结果

两种阈值均在内部校准集上固定后直接用于 200 张独立图片。原始 OCR 准确率只统计 183 张清晰图；决策正确处理比例把“接受且整串正确”与“正确刷新不可读图”相加。

| 模型 | 原始 OCR | 平衡阈值：刷新召回 / 误刷新 / 正确处理 | 低误刷新阈值：刷新召回 / 误刷新 / 正确处理 |
| --- | ---: | ---: | ---: |
| 现用 | 84/183（45.9%） | 76.5% / 51.4% / 81/200（40.5%） | 41.2% / 24.6% / 90/200（45.0%） |
| 第一版 | 135/183（73.8%） | 88.2% / 46.4% / 101/200（50.5%） | 58.8% / 22.4% / 128/200（64.0%） |
| 第二版 | 117/183（63.9%） | 94.1% / 51.9% / 95/200（47.5%） | 58.8% / 24.6% / 114/200（57.0%） |

CPU 批量推理的观测耗时约为：现用模型 29.5–42.0 ms/张、第一版 30.1 ms/张、第二版 29.8 ms/张。速度差异不是当前瓶颈，阻塞上线的是刷新判断的可靠性。

第一版确有可复现的 OCR 提升，也是两个候选中更好的一个；但平衡阈值误刷新接近一半清晰图，低误刷新阈值又只能刷新 10/17 张不可读图。因此独立验收结论为“不合格，不替换生产模型”。

## 复现

```powershell
.tools/ocr-training/Scripts/python.exe tools/captcha/train_local.py `
  --dataset .cache/captcha-pool-20261127-validation `
  --source .tools/PaddleOCR-training `
  --pretrained .cache/ocr-training-20261127/pretrained.pdparams `
  --out .cache/ocr-training-20261127/run-first-v1-e1 `
  --epochs 1 --batch-size 8 `
  --train-labels human-train.csv `
  --validation-labels human-validation.csv `
  --test-labels human-test.csv

.tools/ocr-paddle/Scripts/python.exe tools/captcha/decision_eval.py `
  --dataset .cache/captcha-pool-20261127-validation `
  --independent .cache/captcha-test-20261127-independent `
  --independent .cache/captcha-validation-20261127-refresh `
  --human-truth .cache/ocr-training-20261127/run-first-v1-e1/independent-human-truth.csv `
  --current-model $HOME/.paddlex/official_models/PP-OCRv6_small_rec `
  --candidate-model .cache/ocr-training-20261127/run-first-v1-e1/inference `
  --out .cache/ocr-training-20261127/run-first-v1-e1/decision-evaluation-final.json
```

第二版用第一版 `weights/latest.pdparams` 作为 `--pretrained`，并把输出目录和评估候选目录替换为 `run-first-v2-e1` 即可复现。下一步应增加专门覆盖“清晰但低置信度”和“不可读但高置信度”的刷新训练数据，或训练独立的轻量可读性分类器；在新的独立验收合格前，继续使用现用模型。
