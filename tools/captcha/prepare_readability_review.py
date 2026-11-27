"""Merge explicit A/B human readability decisions while keeping acquisition sources intact."""
import argparse
from collections import Counter
import json
from pathlib import Path
import re

from audit_readability import digest, group_near_duplicates, image_features, read_csv
from train_readability import load_manifest


def validate_review(rows, backups):
    seen = set()
    for row in rows:
        sha = row['sha256']
        if not re.fullmatch('[0-9a-f]{64}', sha) or sha in seen:
            raise ValueError('invalid_or_duplicate_review_hash')
        seen.add(sha)
        if row['review_part'] not in backups or row['readability_label'] not in ('readable', 'refresh'):
            raise ValueError('unknown_review_part_or_decision')
        expected_eligible = 'true' if row['readability_label'] == 'readable' else 'false'
        if row['ocr_eligible'].lower() != expected_eligible:
            raise ValueError('readability_ocr_eligibility_conflict')
    for part, backup in backups.items():
        hashes = set()
        for row in backup:
            if (row['sha256'] in hashes or row['label'] or row['label_verified'].lower() != 'false'
                    or row['label_source'] != 'unreadable' or row['status'] != 'quarantine'):
                raise ValueError('invalid_refresh_backup')
            hashes.add(row['sha256'])
        expected = {r['sha256'] for r in rows if r['review_part'] == part and r['readability_label'] == 'refresh'}
        if hashes != expected:
            raise ValueError('refresh_backup_mismatch')


def ensure_isolation(rows):
    memberships = {}
    for row in rows:
        if memberships.setdefault(row['group'], row['split']) != row['split']:
            raise ValueError('near_duplicate_group_crosses_splits')
    for split in ('train', 'calibration', 'validation'):
        if {r['decision'] for r in rows if r['split'] == split} != {'readable', 'refresh'}:
            raise ValueError('both_classes_required_per_split')


def prepare(review_manifest, previous_manifest, out, calibration_source, validation_source):
    review_manifest, previous_manifest, out = map(lambda p: Path(p).resolve(), (review_manifest, previous_manifest, out))
    if out.exists():
        raise ValueError('output_must_be_new')
    backup_files = {part: review_manifest.parent / f'review-refresh-part-{part.lower()}.csv' for part in ('A', 'B')}
    inputs = [review_manifest, previous_manifest, previous_manifest.parent / 'audit.json',
              review_manifest.parent / 'README.md', *backup_files.values()]
    before = {str(p): digest(p) for p in inputs}
    reviewed = read_csv(review_manifest)
    validate_review(reviewed, {part: read_csv(path) for part, path in backup_files.items()})
    sources = {r['source_dataset'] for r in reviewed}
    if calibration_source == validation_source or not {calibration_source, validation_source} < sources:
        raise ValueError('distinct_train_calibration_validation_sources_required')
    previous = load_manifest(previous_manifest)
    if {r['sha256'] for r in reviewed} & {r['sha256'] for r in previous}:
        raise ValueError('review_overlaps_previous_requires_explicit_reconciliation')
    rows = []
    for row in previous:
        rows.append({**row, 'review_part': row.get('review_part', 'legacy'),
                     'split': 'regression' if row['split'] == 'validation' else row['split']})
    for row in reviewed:
        image = Path(row['image_path'])
        if (not image.is_absolute() or image.name != row['file'] or image.parent.name != row['source_dataset']
                or digest(image) != row['sha256']):
            raise ValueError('review_image_path_or_hash_mismatch')
        split = ('calibration' if row['source_dataset'] == calibration_source else
                 'validation' if row['source_dataset'] == validation_source else 'train')
        rows.append({'sha256': row['sha256'], 'image': image.resolve().as_posix(),
                     'decision': row['readability_label'], 'source_batch': 'human-review:' + row['source_dataset'],
                     'split': split, 'review_part': row['review_part'], 'review_reason': row['review_reason']})
    features = [image_features(r['image']) for r in rows]
    pairs, candidates = group_near_duplicates(rows, features)
    ensure_isolation(rows)
    if before != {str(p): digest(p) for p in inputs}:
        raise ValueError('review_inputs_changed_during_audit')
    report = {
        'schema_version': 1, 'reviewed': len(reviewed), 'combined_images': len(rows),
        'new_classes': dict(Counter(r['readability_label'] for r in reviewed)),
        'new_review_part_counts': {part: dict(Counter(r['readability_label'] for r in reviewed if r['review_part'] == part))
                                   for part in ('A', 'B')},
        'source_part_counts': {source: {part: dict(Counter(r['readability_label'] for r in reviewed
                                                           if r['review_part'] == part and r['source_dataset'] == source))
                                       for part in ('A', 'B')} for source in sorted(sources)},
        'split_counts': {split: dict(Counter(r['decision'] for r in rows if r['split'] == split))
                         for split in ('train', 'calibration', 'validation', 'regression')},
        'near_duplicate_candidates': candidates, 'near_duplicate_pairs': pairs,
        'image_sizes': dict(Counter(f'{f[2][0]}x{f[2][1]}' for f in features)),
        'exact_overlap_with_previous': 0, 'input_sha256': before,
        'preparation_source_sha256': digest(__file__),
        'provenance': 'Entire source_dataset directories remain together; A/B is review metadata, never a model input.',
        'evaluation_notes': 'New validation is untouched A/B source data; previous validation is regression only. No OCR labels exported.',
    }
    out.mkdir(parents=True, exist_ok=False)
    (out / 'manifest.json').write_text(json.dumps(sorted(rows, key=lambda r: r['sha256']), ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    report['frozen_manifest_sha256'] = digest(out / 'manifest.json')
    (out / 'audit.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--review-manifest', required=True)
    parser.add_argument('--previous-manifest', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--calibration-source', required=True)
    parser.add_argument('--validation-source', required=True)
    args = parser.parse_args()
    report = prepare(args.review_manifest, args.previous_manifest, args.out, args.calibration_source, args.validation_source)
    print(json.dumps({k: report[k] for k in ('reviewed', 'new_classes', 'split_counts', 'near_duplicate_candidates', 'near_duplicate_pairs', 'frozen_manifest_sha256')}))
