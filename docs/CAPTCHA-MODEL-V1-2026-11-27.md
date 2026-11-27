# 验证码决策模型第一版训练记录（2026-11-27）

## 结论

已完成一次真实 PP-OCRv6 small 预训练模型微调、候选模型导出及现用模型同数据对比。候选的内部整串识别准确率明显提高，但“刷新召回/清晰图误刷新”仍没有同时达到可靠水平；两个独立 100 张数据集也尚无人工真值。因此候选只保留在本机 `.cache`，**没有替换现用模型**。

## 数据清单与隔离

主数据集为 `.cache/captcha-pool-20261127-validation`。所有图片在读取时重新计算 SHA-256；四个分组按图片哈希零交集，且均属于主 `manifest.json`。原始字符标签保留原大小写，训练目标转为大写，推理和指标统一忽略大小写。

| 分组 | 数量 | CSV SHA-256 | 用途 |
| --- | ---: | --- | --- |
| OCR 训练 | 782 | `375e547da0e48e3575d52d284e3e9bb3fd2f7dd31ebbcc1ef05ecb5796891217` | 仅训练 |
| OCR 验证 | 30 | `c25e5ba39955525555688c0892eed608db8785d35fe5a1aa1fdd06eac28bcf1e` | 阈值校准/内部验证 |
| OCR 测试 | 20 | `6ee7bfc724e920ebb76ed8b8e65b4b301d36da742c76bdcb4c50f05c5701e42b` | 内部测试 |
| 人工不可读 | 149 | `edf42ec6671e7a31c8e3d61fbeab25d6e424278ab5d03e8bb742024e57207dfa` | 124 张校准刷新阈值、25 张内部测试；从未赋字符标签 |

主清单 SHA-256 为 `83f6e3aa1cc12b4fa83f680692aada7a0f442359a5efc0c89dca8361a637f22b`。另有 19 张未完成复核的主集图片，未进入训练或评估。

独立集 `.cache/captcha-test-20261127-independent` 与 `.cache/captcha-validation-20261127-refresh` 各 100 张；清单 SHA-256 分别为 `69141858b559251c72ecfa2e7bc488c729f60ec863ee4e3d5e8a98d9e1a089dc`、`4bef253fd38c7cdb01955896450fd39f6f74048c4251f880c617de419ffcb7f8`。它们与主集及彼此均无哈希交集，人工确认标签均为 0，因此只报告接受数量，不报告准确率。

## 训练配置

- 官方 PaddleOCR 训练源码固定在提交 `dab3fe35379033fdcb2d0e9572fac0b36c9a9ebf`。
- PP-OCRv6 small 预训练参数 SHA-256：`25c9bd54b0e5900916e8bb6ada938abeffb1eac1baedac0ca54a45b1c9310825`。
- CPU 训练 1 轮，批大小 8，随机种子 `20261127`，学习率 `1e-4`、余弦衰减；保留官方 `RecConAug` 和 `RecAug`，不对不可读图片伪造字符标签。
- 782 张训练图共 99 批；训练、验证、测试分组由显式 CSV 固定，不再自动重分组。
- 候选推理权重 SHA-256：`644567c74a0693907eb632792916caee0cbcc7ecc34af0217075f0cec715dba8`。
- 刷新决策使用同一 OCR 的格式与整串置信度，不另训低层图像分类器。刷新阈值只用 30 张清晰验证图和 124 张不可读校准图选择；20 张清晰测试图和 25 张不可读测试图只做最终内部对比。

本地运行目录为 `.cache/ocr-training-20261127/run-first-v1-e1`，其中 `summary.json`、`config.yml`、阶段日志、检查点、导出模型和 `decision-evaluation.json` 均为运行产物，不进入 Git。

## 内部结果

OCR 整串完全正确率（忽略大小写）：

| 模型 | 验证集 | 测试集 |
| --- | ---: | ---: |
| 现用 PP-OCRv6 small | 13/30（43.3%） | 8/20（40.0%） |
| 第一版候选 | 22/30（73.3%） | 14/20（70.0%） |

平衡“保留清晰图/刷新不可读图”的阈值结果：

| 模型 | 阈值 | 刷新召回率 | 清晰图误刷新率 | 正确处理比例 | CPU 单图耗时 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 现用 | 0.8923 | 84% | 50% | 29/45（64.4%） | 32.7 ms |
| 候选 | 0.9624 | 88% | 60% | 30/45（66.7%） | 30.9 ms |

把验证集清晰图误刷新限制在 25%（现用模型因格式拒绝最低只能达到 26.7%）时：现用测试集刷新召回 40%、清晰图误刷新 40%、正确处理 18/45；候选刷新召回 56%、清晰图误刷新 35%、正确处理 25/45。候选存在真实 OCR 提升，但两种阈值取舍都暴露明显缺口：高召回会大量误刷新清晰图，低误刷新又漏掉近半不可读图。

在无人工真值的两个独立集上，平衡阈值仅能报告覆盖：现用分别接受 40/100、53/100；候选分别接受 51/100、49/100。任何一项都不能解释为准确率。

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
  --current-model $HOME/.paddlex/official_models/PP-OCRv6_small_rec `
  --candidate-model .cache/ocr-training-20261127/run-first-v1-e1/inference `
  --out .cache/ocr-training-20261127/run-first-v1-e1/decision-evaluation.json
```

下一道验收门槛是为两套独立集补齐人工真值，再按预先固定的阈值评估；在此之前不得发布候选或宣称最终准确率。
