"""Freeze human readability labels and acquisition groups without touching collectors."""
import argparse
from collections import Counter
import csv
import hashlib
import json
from pathlib import Path
import re

import numpy as np
from PIL import Image


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def read_csv(path):
    with Path(path).open(encoding='utf-8-sig', newline='') as handle:
        return list(csv.DictReader(handle))


def human_decision(row, review):
    """Use explicit human evidence; never use the API's character prediction."""
    evidence = ' '.join(str(review.get(key, '')).strip()
                        for key in ('note', 'correction', 'treatment'))
    unclear = bool(re.search(
        r'模糊|看不清|无法辨认|无法识别|识别不出|不清楚|分不清|无法核对|不能核对|难以辨认|\bmh\b',
        evidence, re.IGNORECASE))
    if unclear:
        if row['status'] != 'quarantine' or row['label_source'] != 'unreadable':
            raise ValueError('human_unreadable_disagrees_with_csv')
        if row['label'] or row['label_verified'].lower() != 'false':
            raise ValueError('unreadable_has_character_truth')
        return 'refresh'
    if (row['status'] != 'accepted' or row['label_verified'].lower() != 'true'
            or not row['label_source'].startswith('manual-review:')):
        raise ValueError('missing_explicit_human_evidence')
    if review.get('note', '').strip() or review.get('treatment', '').strip():
        return 'ambiguous'
    if review.get('unreadable'):
        raise ValueError('review_flag_conflict')
    if review.get('correction', '').strip() and not re.fullmatch(
            r'[A-Za-z0-9]{4,6}', review['correction'].strip()):
        raise ValueError('unclassified_human_correction')
    return 'readable'


def read_adjudications(path, known_hashes):
    decisions = {}
    if path is None:
        return decisions
    for entry in json.loads(Path(path).read_text(encoding='utf-8')):
        sha = entry.get('sha256')
        if (sha not in known_hashes or sha in decisions
                or entry.get('decision') not in ('readable', 'refresh')
                or entry.get('source') != 'user-confirmed'
                or not isinstance(entry.get('reason'), str) or not entry['reason'].strip()):
            raise ValueError('invalid_human_adjudication')
        decisions[sha] = entry
    return decisions


def recover_batches(samples, logs):
    """Collector saved=N refers to samples[N-1]; unknown history stays one group."""
    if len({row['sha256'] for row in samples}) != len(samples):
        raise ValueError('duplicate_manifest_hash')
    batches = {row['sha256']: 'legacy-unknown' for row in samples}
    seen = set()
    for path in sorted(logs):
        indices = []
        for line in path.read_text(encoding='utf-8', errors='strict').splitlines():
            if not line.strip():
                continue
            event = json.loads(line)
            if event.get('at') != 'harvest':
                continue
            index = event.get('saved')
            if type(index) is not int or not 1 <= index <= len(samples):
                raise ValueError('invalid_saved_index')
            if index in seen:
                raise ValueError('ambiguous_acquisition_batch')
            seen.add(index)
            indices.append(index)
            batches[samples[index - 1]['sha256']] = path.name.removesuffix('.out.log')
        if indices and indices != list(range(min(indices), max(indices) + 1)):
            raise ValueError('noncontiguous_acquisition_log')
    return batches


def image_features(path):
    with Image.open(path) as image:
        image.load()
        gray = image.convert('L')
        small = np.asarray(gray.resize((32, 32), Image.Resampling.BILINEAR), dtype=float) / 255
        pixels = np.asarray(gray.resize((64, 28), Image.Resampling.BILINEAR), dtype=float) / 255
        size = image.size
    # 63 low-frequency DCT bits, excluding brightness (DC).
    basis = np.cos(np.pi * (np.arange(32)[None, :] + .5) * np.arange(8)[:, None] / 32)
    coefficients = (basis @ small @ basis.T).ravel()[1:]
    phash = sum(int(bit) << i for i, bit in enumerate(coefficients > np.median(coefficients)))
    return phash, pixels, size


def group_near_duplicates(rows, features):
    parents = {row['source_batch']: row['source_batch'] for row in rows}

    def find(key):
        while parents[key] != key:
            parents[key] = parents[parents[key]]
            key = parents[key]
        return key

    pairs, candidates = [], 0
    for i, (phash, pixels, _) in enumerate(features):
        for j in range(i):
            other, other_pixels, _ = features[j]
            if (phash ^ other).bit_count() > 6:
                continue
            candidates += 1
            if np.abs(pixels - other_pixels).mean() > .035:
                continue
            pairs.append([rows[j]['sha256'], rows[i]['sha256']])
            left, right = find(rows[j]['source_batch']), find(rows[i]['source_batch'])
            parents[max(left, right)] = min(left, right)
    for row in rows:
        row['group'] = find(row['source_batch'])
    return pairs, candidates


def assign_splits(rows, calibration_batch, validation_batch):
    available = {row['source_batch'] for row in rows}
    if calibration_batch == validation_batch or not {calibration_batch, validation_batch} <= available:
        raise ValueError('distinct_existing_holdout_batches_required')
    for row in rows:
        row['split'] = ('calibration' if row['source_batch'] == calibration_batch else
                        'validation' if row['source_batch'] == validation_batch else 'train')
    membership = {}
    for row in rows:
        previous = membership.setdefault(row['group'], row['split'])
        if previous != row['split']:
            raise ValueError('near_duplicate_group_crosses_splits')
    for split in ('train', 'calibration', 'validation'):
        if {r['decision'] for r in rows if r['split'] == split} != {'refresh', 'readable'}:
            raise ValueError('both_classes_required_per_split')


def audit(dataset, reviews, out, calibration_batch, validation_batch, adjudications=None):
    dataset, reviews, out = map(lambda p: Path(p).resolve(), (dataset, reviews, out))
    if out.exists():
        raise ValueError('output_must_be_new_to_preserve_frozen_snapshot')
    audit_file = dataset / 'manual-reviewed-labels.csv'
    manifest_file = dataset / 'manifest.json'
    inputs = [audit_file, manifest_file, dataset / 'manual-unreadable.csv']
    review_files = sorted(reviews.glob('review-snapshot*.json'))
    logs = sorted(dataset.glob('harvest*.out.log'))
    inputs += review_files + logs
    if adjudications:
        inputs.append(Path(adjudications).resolve())
    before = {str(p): digest(p) for p in inputs}
    reviews_by_hash = {}
    for path in review_files:
        for row in json.loads(path.read_text(encoding='utf-8'))['rows']:
            if row['sha256'] in reviews_by_hash:
                raise ValueError('duplicate_human_review')
            reviews_by_hash[row['sha256']] = row
    samples = json.loads(manifest_file.read_text(encoding='utf-8'))['samples']
    batches = recover_batches(samples, logs)
    manifest = {row['sha256']: row for row in samples}
    source_rows = read_csv(audit_file)
    confirmed = read_adjudications(adjudications, {r['sha256'] for r in source_rows})
    if {r['sha256'] for r in source_rows} != set(reviews_by_hash):
        raise ValueError('review_coverage_mismatch')
    rows, features, seen, excluded = [], [], set(), []
    flag_disagreements, original_refresh = 0, set()
    for row in source_rows:
        sha = row['sha256']
        if sha in seen:
            raise ValueError('duplicate_reviewed_image')
        seen.add(sha)
        if sha not in manifest or row['file'] != manifest[sha]['file']:
            raise ValueError('review_manifest_mismatch')
        image = (dataset / row['file']).resolve()
        if not image.is_relative_to(dataset) or digest(image) != sha:
            raise ValueError('image_path_or_digest_invalid')
        decision = human_decision(row, reviews_by_hash[sha])
        if decision == 'refresh':
            original_refresh.add(sha)
        if decision != 'ambiguous':
            flag_disagreements += (decision == 'refresh') != bool(reviews_by_hash[sha].get('unreadable'))
        if sha in confirmed:
            if decision != 'ambiguous':
                raise ValueError('adjudication_requires_ambiguous_original')
            decision = confirmed[sha]['decision']
        feature = image_features(image)
        if decision == 'ambiguous':
            excluded.append({'sha256': sha, 'reason': 'accepted_character_correction_with_nonempty_readability_note'})
            continue
        rows.append({'sha256': sha, 'image': image.as_posix(), 'decision': decision,
                     'source_batch': batches[sha]})
        features.append(feature)
    if {r['sha256'] for r in read_csv(inputs[2])} != original_refresh:
        raise ValueError('refresh_csv_mismatch')
    pairs, candidates = group_near_duplicates(rows, features)
    assign_splits(rows, calibration_batch, validation_batch)
    after = {str(p): digest(p) for p in inputs}
    if before != after:
        raise ValueError('source_changed_during_audit_retry_with_stable_input')
    split_counts = {split: dict(Counter(r['decision'] for r in rows if r['split'] == split))
                    for split in ('train', 'calibration', 'validation')}
    report = {
        'schema_version': 1, 'reviewed': len(source_rows), 'eligible': len(rows),
        'manifest_images': len(samples), 'excluded_ambiguous': excluded,
        'human_adjudications': list(confirmed.values()),
        'unreviewed_excluded': len(samples) - len(source_rows),
        'source_csv_status_counts': dict(Counter(r['status'] for r in source_rows)),
        'classes': dict(Counter(r['decision'] for r in rows)),
        'handoff_delta': {key: sum(r['decision'] == key for r in rows) - expected
                          for key, expected in [('readable', 832), ('refresh', 149)]},
        'unique_sha256': len(seen), 'snapshot_flag_disagreements': flag_disagreements,
        'image_sizes': dict(Counter(f'{f[2][0]}x{f[2][1]}' for f in features)),
        'near_duplicate_candidates': candidates, 'near_duplicate_pairs': pairs,
        'near_duplicate_method': '63-bit DCT pHash Hamming <=6 AND grayscale MAE <=0.035; heuristic, not a proof',
        'source_batches': {batch: dict(Counter(r['decision'] for r in rows if r['source_batch'] == batch))
                           for batch in sorted({r['source_batch'] for r in rows})},
        'split_counts': split_counts,
        'provenance': 'manifest append order joined to collector cumulative saved indices; unknown prefix is one batch',
        'input_sha256': before,
    }
    out.mkdir(parents=True, exist_ok=False)
    frozen = json.dumps(sorted(rows, key=lambda r: r['sha256']), ensure_ascii=False, indent=2) + '\n'
    (out / 'manifest.json').write_text(frozen, encoding='utf-8')
    report['frozen_manifest_sha256'] = digest(out / 'manifest.json')
    (out / 'audit.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dataset', required=True)
    parser.add_argument('--reviews', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--calibration-batch', required=True)
    parser.add_argument('--validation-batch', required=True)
    parser.add_argument('--adjudications', help='Local JSON array of explicit human decisions for ambiguous rows')
    args = parser.parse_args()
    report = audit(args.dataset, args.reviews, args.out, args.calibration_batch, args.validation_batch, args.adjudications)
    print(json.dumps({k: report[k] for k in ('reviewed', 'classes', 'split_counts', 'frozen_manifest_sha256')}))
