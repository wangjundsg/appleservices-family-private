"""Isolated PP-OCRv6 small fine-tuning; never publishes or replaces the live model."""
import argparse
import copy
import csv
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def confirmed_samples(dataset, pseudo_labels=None):
    dataset = Path(dataset).resolve()
    samples = {}
    with (dataset / 'labels.csv').open(encoding='utf-8-sig', newline='') as handle:
        for row in csv.DictReader(handle):
            if row.get('label_verified', '').lower() != 'true':
                continue
            label = row.get('label', '')
            if not re.fullmatch(r'[A-Za-z0-9]{4,6}', label):
                raise ValueError('confirmed_label_invalid')
            image = (dataset / row['file']).resolve()
            if not image.is_relative_to(dataset):
                raise ValueError('image_outside_dataset')
            digest = hashlib.sha256(image.read_bytes()).hexdigest()
            if digest != row.get('sha256'):
                raise ValueError('image_digest_mismatch')
            label = label.upper()
            if digest in samples and samples[digest]['label'] != label:
                raise ValueError('conflicting_labels')
            from PIL import Image
            with Image.open(image) as picture:
                picture.verify()
            samples[digest] = {'image': image.as_posix(), 'label': label, 'sha256': digest}
    if len(samples) < (1 if pseudo_labels else 4):
        raise ValueError('at_least_four_confirmed_unique_images_required')
    ordered = sorted(samples.values(), key=lambda row: row['sha256'])
    count = max(1, len(ordered) // 5)
    train, validation = ordered[count:], ordered[:count]
    if pseudo_labels:
        candidates, conflicts = {}, set()
        with Path(pseudo_labels).open(encoding='utf-8-sig', newline='') as handle:
            for row in csv.DictReader(handle):
                if row.get('status') != 'accepted': continue
                label = row.get('label', '')
                if not re.fullmatch(r'[A-Za-z0-9]{4,6}', label): raise ValueError('pseudo_label_invalid')
                image = (dataset / row['file']).resolve()
                if not image.is_relative_to(dataset): raise ValueError('image_outside_dataset')
                digest = hashlib.sha256(image.read_bytes()).hexdigest()
                if digest != row.get('sha256'): raise ValueError('image_digest_mismatch')
                if digest in samples: continue  # Confirmed labels and validation membership win.
                from PIL import Image
                with Image.open(image) as picture: picture.verify()
                item = {'image':image.as_posix(), 'label':label.upper(), 'sha256':digest}
                if digest in conflicts: continue
                if digest in candidates and candidates[digest]['label'] != item['label']:
                    candidates.pop(digest); conflicts.add(digest)
                else: candidates[digest] = item
        train.extend(candidates.values())
    if not train: raise ValueError('training_images_required')
    return train, validation


def case_metric_fix(source):
    file = Path(source) / 'ppocr/metrics/rec_metric.py'
    text = file.read_text(encoding='utf-8')
    if 'self.ignore_case = ' not in text:
        constructor = '        self.ignore_space = ignore_space'
        comparison = '            norm_edit_dis += Levenshtein.normalized_distance(pred, target)'
        if constructor not in text or comparison not in text: raise ValueError('unsupported_upstream_rec_metric')
        text = text.replace(constructor, constructor + '\n        self.ignore_case = kwargs.get("ignore_case", False)', 1)
        text = text.replace(comparison, '            if self.ignore_case:\n                pred, target = pred.casefold(), target.casefold()\n' + comparison, 1)
        file.write_text(text, encoding='utf-8')
    return hashlib.sha256(text.encode()).hexdigest()


def windows_loader_fix(source):
    # Upstream skips the final batch on Windows, even with zero loader workers.
    # That evaluates zero images for a one-image validation set.
    file = Path(source) / 'tools/program.py'
    original = file.read_text(encoding='utf-8')
    pattern = r'(len\((train_dataloader|valid_dataloader|eval_dataloader)\) - 1\s+if platform.system\(\) == "Windows")(?! and)'
    updated, count = re.subn(pattern, lambda match: match[1] + f' and {match[2]}.num_workers > 0', original)
    if count not in (0, 4): raise ValueError('unsupported_upstream_windows_loader_layout')
    if count: file.write_text(updated, encoding='utf-8')
    return hashlib.sha256(updated.encode()).hexdigest()


def evaluation_metrics(log):
    if isinstance(log, bytes): log = log.decode('utf-8', errors='replace')
    values = dict(re.findall(r'INFO: (acc|norm_edit_dis|fps):([0-9.eE+-]+)', log))
    if float(values.get('fps', 0)) <= 0:
        raise ValueError('validation_processed_no_images')
    return {key: float(value) for key, value in values.items()}


def prepare(dataset, source, pretrained, out, epochs, pseudo_labels=None):
    import yaml
    train, validation = confirmed_samples(dataset, pseudo_labels)
    if not 1 <= epochs <= 100:
        raise ValueError('epochs_must_be_1_to_100')
    source, pretrained, out = [Path(value).resolve() for value in (source, pretrained, out)]
    if not pretrained.is_file(): raise ValueError('pretrained_weights_missing')
    with (source / 'configs/rec/PP-OCRv6/PP-OCRv6_small_rec.yml').open(encoding='utf-8') as handle:
        config = yaml.safe_load(handle)
    out.mkdir(parents=True, exist_ok=False)
    for name, rows in [('train', train), ('validation', validation)]:
        (out / f'{name}.txt').write_text(''.join(f"{row['image']}\t{row['label']}\n" for row in rows), encoding='utf-8')
    config['Global'].update(use_gpu=False, distributed=False, epoch_num=epochs, seed=20261127,
        pretrained_model=pretrained.as_posix(), checkpoints=None, use_visualdl=False,
        save_model_dir=(out / 'weights').as_posix(), save_epoch_step=epochs,
        eval_batch_step=[0, 1], print_batch_step=1, log_smooth_window=1,
        character_dict_path=(source / config['Global']['character_dict_path']).as_posix())
    config['Optimizer']['lr'].update(learning_rate=0.00001, warmup_epoch=0)
    config['Metric'].update(is_filter=False, ignore_space=False, ignore_case=True)
    # Keep the pretrained dictionary and head shapes, including upper/lower case.
    template = copy.deepcopy(config['Eval']['dataset'])
    for mode, name in [('Train', 'train'), ('Eval', 'validation')]:
        config[mode] = {'dataset': copy.deepcopy(template), 'loader': {
            'shuffle': mode == 'Train', 'drop_last': False, 'batch_size_per_card': 1, 'num_workers': 0}}
        config[mode]['dataset'].update(data_dir=Path(dataset).resolve().as_posix(),
            label_file_list=[(out / f'{name}.txt').as_posix()])
    config_file = out / 'config.yml'
    config_file.write_text(yaml.safe_dump(config, allow_unicode=True, sort_keys=False), encoding='utf-8')
    report = {'train_images': len(train), 'validation_images': len(validation), 'epochs': epochs,
        'experimental_only': True, 'case_sensitive': False, 'training_target_case': 'upper',
        'pretrained_sha256': hashlib.sha256(pretrained.read_bytes()).hexdigest(),
        'source_commit': subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()}
    report['training_program_sha256'] = windows_loader_fix(source)
    report['training_metric_sha256'] = case_metric_fix(source)
    (out / 'summary.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    return config_file, report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['dataset', 'source', 'pretrained', 'out']:
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--epochs', type=int, default=3)
    parser.add_argument('--pseudo-labels', help='optional API candidate CSV; never used as validation truth')
    args = parser.parse_args()
    config, report = prepare(args.dataset, args.source, args.pretrained, args.out, args.epochs, args.pseudo_labels)
    out, source = Path(args.out).resolve(), Path(args.source).resolve()
    env = dict(os.environ, OMP_NUM_THREADS='2', MKL_NUM_THREADS='2', OPENBLAS_NUM_THREADS='2',
               CUDA_VISIBLE_DEVICES='', FLAGS_paddle_num_threads='2', PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK='True',
               NO_ALBUMENTATIONS_UPDATE='1', PYTHONIOENCODING='utf-8')
    stages = [('baseline', 'eval.py', []), ('train', 'train.py', []),
              ('validation', 'eval.py', ['-o', f'Global.pretrained_model={(out / "weights/latest").as_posix()}'])]
    for stage, script, overrides in stages:
        print(json.dumps({'stage': stage, **report}), flush=True)
        with (out / f'{stage}.log').open('w', encoding='utf-8') as log:
            process = subprocess.run([sys.executable, str(source / 'tools' / script), '-c', str(config), *overrides],
                cwd=source, env=env, stdout=log, stderr=subprocess.STDOUT)
        report[stage + '_exit_code'] = process.returncode
        if not process.returncode and stage != 'train':
            report[stage + '_metrics'] = evaluation_metrics((out / f'{stage}.log').read_bytes())
        (out / 'summary.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
        if process.returncode: return process.returncode
    report['state'] = 'trained_experimental'
    (out / 'summary.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps(report))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
