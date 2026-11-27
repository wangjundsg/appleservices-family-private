"""Fixed-recipe readability experiment with separate calibration and final validation."""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import platform
import random
import time

import numpy as np
import PIL
from scipy.optimize import minimize
from scipy.special import expit
from scipy.stats import beta
import scipy
import torch
from torch import nn
from torch.utils.data import DataLoader, TensorDataset
import torchvision
from torchvision.models import mobilenet_v3_small, MobileNet_V3_Small_Weights

from readability import PREPROCESS_VERSION, ReadabilityModel, preprocess


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def load_manifest(path):
    path = Path(path)
    audit = json.loads((path.parent / 'audit.json').read_text(encoding='utf-8'))
    if sha256(path) != audit['frozen_manifest_sha256']:
        raise ValueError('frozen_manifest_changed')
    rows = json.loads(path.read_text(encoding='utf-8'))
    hashes, groups, batches = set(), {}, {}
    for row in rows:
        if row['split'] not in ('train', 'calibration', 'validation') or row['decision'] not in ('readable', 'refresh'):
            raise ValueError('invalid_split_or_decision')
        if row['sha256'] in hashes or sha256(row['image']) != row['sha256']:
            raise ValueError('duplicate_or_changed_image')
        hashes.add(row['sha256'])
        for memberships, key in ((groups, row['group']), (batches, row['source_batch'])):
            if memberships.setdefault(key, row['split']) != row['split']:
                raise ValueError('split_group_overlap')
    for split in ('train', 'calibration', 'validation'):
        if {r['decision'] for r in rows if r['split'] == split} != {'readable', 'refresh'}:
            raise ValueError('both_classes_required_per_split')
    return rows


def tensors(rows):
    return torch.stack([preprocess(r['image']) for r in rows]), torch.tensor(
        [float(r['decision'] == 'readable') for r in rows], dtype=torch.float32)


def fit_calibration(logits, labels):
    logits, labels = np.asarray(logits, dtype=float), np.asarray(labels, dtype=float)
    if not np.isfinite(logits).all() or set(labels) != {0, 1}:
        raise ValueError('invalid_calibration_data')

    def objective(params):
        slope, intercept = params
        scores = slope * logits + intercept
        residual = expit(scores) - labels
        penalty = .001 * ((slope - 1) ** 2 + intercept ** 2)
        loss = np.mean(np.logaddexp(0, scores) - labels * scores) + penalty
        gradient = [np.mean(residual * logits) + .002 * (slope - 1), np.mean(residual) + .002 * intercept]
        return loss, np.array(gradient)

    result = minimize(objective, [1., 0.], jac=True, bounds=[(.001, 100), (-100, 100)], method='L-BFGS-B')
    if not result.success or not np.isfinite(result.x).all():
        raise ValueError('calibration_optimizer_failed')
    return result.x.tolist()


def choose_threshold(scores, labels, max_false_readable=.05):
    scores, labels = np.asarray(scores), np.asarray(labels)
    if (scores.shape != labels.shape or scores.ndim != 1 or set(labels) != {0, 1}
            or not np.isfinite(scores).all() or np.any((scores < 0) | (scores > 1))
            or not 0 <= max_false_readable <= 1):
        raise ValueError('invalid_threshold_data')
    negatives = scores[labels == 0]
    # Margin prevents floating point batching differences from accepting a threshold tie.
    candidates = sorted({0., 1., *(min(1., float(p) + 1e-6) for p in negatives)})
    return next(t for t in candidates if np.mean(negatives > t) <= max_false_readable)


def metrics(labels, scores, threshold):
    labels, scores = np.asarray(labels, dtype=int), np.asarray(scores, dtype=float)
    accepted = scores > threshold
    rr = int(np.sum((labels == 0) & ~accepted))
    ra = int(np.sum((labels == 0) & accepted))
    ar = int(np.sum((labels == 1) & ~accepted))
    aa = int(np.sum((labels == 1) & accepted))
    n, negative, positive, released = len(labels), rr + ra, ar + aa, ra + aa
    ratio = lambda a, b: a / b if b else None
    lower = float(beta.ppf(.025, ra, negative - ra + 1)) if ra else 0.
    upper = float(beta.ppf(.975, ra + 1, negative - ra)) if ra < negative else 1.
    upper_one_sided = float(beta.ppf(.95, ra + 1, negative - ra)) if ra < negative else 1.
    return {'images': n, 'confusion_matrix': [[rr, ra], [ar, aa]],
            'matrix_order': ['refresh', 'readable'], 'refresh_recall': ratio(rr, negative),
            'false_readable_rate': ratio(ra, negative), 'readable_retention': ratio(aa, positive),
            'coverage': ratio(released, n), 'accuracy': ratio(rr + aa, n),
            'refresh_fraction_among_readable_decisions': ratio(ra, released),
            'false_readable_exact_95_ci': [lower, upper] if negative else None,
            'false_readable_exact_95_upper_one_sided': upper_one_sided if negative else None,
            'brier_score': float(np.mean((scores - labels) ** 2))}


@torch.inference_mode()
def infer_logits(model, images):
    model.eval()
    return torch.cat([model(batch).flatten() for batch in images.split(32)]).numpy().astype(float)


def benchmark(predictor, images, iterations=1000):
    tensor = preprocess(images[0]).unsqueeze(0)
    for _ in range(20):
        predictor.predict(images[0])
    model_ms, end_to_end_ms = [], []
    with torch.inference_mode():
        for i in range(iterations):
            start = time.perf_counter()
            predictor.model(tensor)
            model_ms.append((time.perf_counter() - start) * 1000)
            start = time.perf_counter()
            predictor.predict(images[i % len(images)])
            end_to_end_ms.append((time.perf_counter() - start) * 1000)
    summary = lambda values: {'median_ms': float(np.median(values)), 'p95_ms': float(np.percentile(values, 95))}
    return {'iterations': iterations, 'threads': torch.get_num_threads(), 'batch_size': 1,
            'processor': platform.processor(), 'platform': platform.platform(),
            'model_only': summary(model_ms), 'decode_preprocess_infer': summary(end_to_end_ms),
            'note': 'Warm loaded model; file reads may be OS-cached; concurrent machine load is uncontrolled.'}


def run(manifest, out, threads=2):
    seed = 20261127
    if not 1 <= threads <= 8:
        raise ValueError('threads_must_be_1_to_8')
    torch.set_num_threads(threads)
    torch.use_deterministic_algorithms(True)
    random.seed(seed)
    np.random.seed(seed)
    torch.manual_seed(seed)
    rows = load_manifest(manifest)
    out = Path(out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    torch.hub.set_dir(str(out.parent / 'pretrained-cache'))
    weights = MobileNet_V3_Small_Weights.IMAGENET1K_V1
    model = mobilenet_v3_small(weights=weights)
    model.classifier = nn.Sequential(nn.Dropout(.2), nn.Linear(576, 1))
    pretrained = Path(torch.hub.get_dir()) / 'checkpoints' / weights.url.rsplit('/', 1)[1]
    groups = {split: [r for r in rows if r['split'] == split] for split in ('train', 'calibration', 'validation')}
    x_train, y_train = tensors(groups['train'])
    counts = torch.bincount(y_train.long(), minlength=2)
    class_weights = len(y_train) / (2 * counts.float())
    generator = torch.Generator().manual_seed(seed)
    loader = DataLoader(TensorDataset(x_train, y_train), batch_size=32, shuffle=True, generator=generator, num_workers=0)
    for param in model.parameters():
        param.requires_grad_(False)
    for param in model.classifier.parameters():
        param.requires_grad_(True)
    history, started = [], time.perf_counter()
    for stage, epochs, lr in [('head', 5, .001), ('tail', 10, .0001)]:
        if stage == 'tail':
            for param in model.features[-3:].parameters():
                param.requires_grad_(True)
        optimizer = torch.optim.AdamW([p for p in model.parameters() if p.requires_grad], lr=lr, weight_decay=.0001)
        for epoch in range(epochs):
            model.train()
            for layer in model.modules():
                if isinstance(layer, nn.BatchNorm2d):
                    layer.eval()
            losses = []
            for images, labels in loader:
                optimizer.zero_grad(set_to_none=True)
                logits = model(images).flatten()
                loss = (nn.functional.binary_cross_entropy_with_logits(logits, labels, reduction='none')
                        * class_weights[labels.long()]).mean()
                if not torch.isfinite(loss):
                    raise ValueError('nonfinite_training_loss')
                loss.backward()
                optimizer.step()
                losses.append(float(loss.detach()) * len(labels))
            entry = {'stage': stage, 'epoch': epoch + 1, 'weighted_train_loss': sum(losses) / len(y_train)}
            history.append(entry)
            print(json.dumps(entry), flush=True)
    training_seconds = time.perf_counter() - started
    x_calibration, y_calibration = tensors(groups['calibration'])
    calibration_logits = infer_logits(model, x_calibration)
    slope, intercept = fit_calibration(calibration_logits, y_calibration.numpy())
    calibrated = expit(slope * calibration_logits + intercept)
    threshold = choose_threshold(calibrated, y_calibration.numpy())
    versions = {'python': platform.python_version(), 'torch': str(torch.__version__),
                'torchvision': str(torchvision.__version__), 'numpy': np.__version__, 'Pillow': PIL.__version__, 'scipy': scipy.__version__}
    metadata = {'schema_version': 1, 'architecture': 'mobilenet_v3_small',
                'preprocess_version': PREPROCESS_VERSION, 'threshold': threshold,
                'calibration': [slope, intercept], 'manifest_sha256': sha256(manifest),
                'seed': seed, 'versions': versions, 'pretrained_sha256': sha256(pretrained),
                'pretrained_url': weights.url, 'experimental_only': True}
    identity = hashlib.sha256(json.dumps(metadata, sort_keys=True).encode())
    for name, tensor in sorted(model.state_dict().items()):
        identity.update(name.encode())
        identity.update(tensor.cpu().numpy().tobytes())
    metadata['model_version'] = 'readability-mnv3s-v1-' + identity.hexdigest()[:12]
    model_file = out / 'model.pt'
    torch.save({'metadata': metadata, 'state_dict': model.state_dict()}, model_file)
    # Final validation is first opened for prediction after model and threshold are fixed.
    torch.set_num_threads(1)
    predictor = ReadabilityModel(model_file)
    validation_predictions = [predictor.predict(r['image']) for r in groups['validation']]
    validation_scores = [p['readable_probability'] for p in validation_predictions]
    validation_labels = [int(r['decision'] == 'readable') for r in groups['validation']]
    validation = metrics(validation_labels, validation_scores, threshold)
    by_batch = {}
    for batch in sorted({r['source_batch'] for r in groups['validation']}):
        indices = [i for i, r in enumerate(groups['validation']) if r['source_batch'] == batch]
        by_batch[batch] = metrics([validation_labels[i] for i in indices], [validation_scores[i] for i in indices], threshold)
    report = {'model': metadata, 'model_sha256': sha256(model_file), 'model_bytes': model_file.stat().st_size,
              'source_sha256': {name: sha256(Path(__file__).with_name(name))
                                for name in ('train_readability.py', 'readability.py', 'audit_readability.py')},
              'recipe': {'seed': seed, 'head_epochs': 5, 'tail_epochs': 10, 'head_lr': .001,
                         'tail_lr': .0001, 'weight_decay': .0001, 'batch_size': 32,
                         'augmentation': 'none', 'batchnorm_running_stats': 'frozen',
                         'calibration': 'monotonic regularized Platt; penalty=0.001',
                         'max_calibration_false_readable_rate': .05},
              'parameters': sum(p.numel() for p in model.parameters()), 'training_seconds': training_seconds,
              'training_threads': threads, 'training_history': history,
              'class_weights_refresh_readable': class_weights.tolist(),
              'split_counts': {split: dict(Counter(r['decision'] for r in items)) for split, items in groups.items()},
              'calibration': metrics(y_calibration.numpy(), calibrated, threshold),
              'calibration_raw_brier': float(np.mean((expit(calibration_logits) - y_calibration.numpy()) ** 2)),
              'validation': validation, 'validation_by_source_batch': by_batch,
              'meets_experimental_targets': validation['false_readable_rate'] <= .05 and validation['readable_retention'] >= .5,
              'limitations': ['Single day and captcha style', 'Only 15 calibration refresh images',
                             'Group correlation makes image-level binomial intervals optimistic', 'No automatic production integration']}
    report['cpu_latency'] = benchmark(predictor, [r['image'] for r in groups['validation']])
    (out / 'report.json').write_text(json.dumps(report, indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (out / 'validation-predictions.json').write_text(json.dumps([
        {'sha256': r['sha256'], 'truth': r['decision'], **p}
        for r, p in zip(groups['validation'], validation_predictions)], indent=2) + '\n', encoding='utf-8')
    (out / 'calibration-predictions.json').write_text(json.dumps([
        {'sha256': r['sha256'], 'truth': r['decision'], 'logit': float(logit), 'readable_probability': float(score)}
        for r, logit, score in zip(groups['calibration'], calibration_logits, calibrated)], indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'model_version': metadata['model_version'], 'validation': validation,
                      'model_bytes': report['model_bytes'], 'cpu_latency': report['cpu_latency'],
                      'meets_experimental_targets': report['meets_experimental_targets']}), flush=True)
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--threads', type=int, default=2)
    args = parser.parse_args()
    run(args.manifest, args.out, args.threads)
