"""Audit fixed captcha splits and compare current/candidate OCR refresh decisions."""
import argparse
import csv
import hashlib
import json
from pathlib import Path
import re
import time


VALID_LABEL = re.compile(r'[A-Za-z0-9]{4,6}')


def file_sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def load_rows(root, name, readable):
    root = Path(root).resolve(); csv_file = (root / name).resolve()
    if not csv_file.is_relative_to(root) or not csv_file.is_file():
        raise ValueError('split_csv_missing_or_outside_dataset')
    rows, seen = [], set()
    with csv_file.open(encoding='utf-8-sig', newline='') as handle:
        for row in csv.DictReader(handle):
            image = (root / row.get('file', '')).resolve()
            if not image.is_relative_to(root) or not image.is_file():
                raise ValueError('split_image_missing_or_outside_dataset')
            digest = file_sha(image)
            if digest != row.get('sha256') or digest in seen:
                raise ValueError('split_digest_invalid_or_duplicate')
            if readable:
                if (row.get('label_verified', '').lower() != 'true' or row.get('status') != 'accepted'
                        or not VALID_LABEL.fullmatch(row.get('label', ''))):
                    raise ValueError('readable_split_row_invalid')
            elif (row.get('label') or row.get('label_verified', '').lower() == 'true'
                  or row.get('status') != 'quarantine' or row.get('label_source') != 'unreadable'):
                raise ValueError('unreadable_split_row_invalid')
            seen.add(digest); rows.append({**row, 'image': str(image), 'label_upper': row.get('label', '').upper()})
    if not rows:
        raise ValueError('split_empty')
    return rows, file_sha(csv_file)


def manifest_hashes(root):
    root = Path(root).resolve(); manifest = root / 'manifest.json'
    samples = json.loads(manifest.read_text(encoding='utf-8-sig')).get('samples', [])
    hashes = set()
    for row in samples:
        image = (root / row.get('file', '')).resolve()
        if (not image.is_relative_to(root) or not image.is_file()
                or file_sha(image) != row.get('sha256') or row['sha256'] in hashes):
            raise ValueError('manifest_image_invalid_or_duplicate')
        hashes.add(row['sha256'])
    if not hashes:
        raise ValueError('manifest_empty')
    return hashes, file_sha(manifest)


def load_truth(path, roots):
    root_by_name = {Path(root).resolve().name: Path(root).resolve() for root in roots}
    rows, seen = [], set()
    with Path(path).open(encoding='utf-8-sig', newline='') as handle:
        for row in csv.DictReader(handle):
            root = root_by_name.get(row.get('dataset', ''))
            if root is None:
                raise ValueError('truth_dataset_unknown')
            image = (root / row.get('file', '')).resolve()
            if not image.is_relative_to(root) or not image.is_file():
                raise ValueError('truth_image_missing_or_outside_dataset')
            digest, status, label = file_sha(image), row.get('status'), row.get('label', '')
            if digest != row.get('sha256') or digest in seen:
                raise ValueError('truth_digest_invalid_or_duplicate')
            if status == 'clear':
                if not VALID_LABEL.fullmatch(label): raise ValueError('truth_clear_label_invalid')
            elif status == 'unreadable':
                if label: raise ValueError('truth_unreadable_has_label')
            else: raise ValueError('truth_status_invalid')
            seen.add(digest); rows.append({**row, 'image': str(image)})
    expected = set().union(*(manifest_hashes(root)[0] for root in roots))
    if seen != expected:
        raise ValueError('truth_does_not_cover_independent_manifests')
    return rows


def audit(primary, independent):
    names = [('train', 'human-train.csv', True), ('validation', 'human-validation.csv', True),
             ('test', 'human-test.csv', True), ('unreadable', 'manual-unreadable.csv', False)]
    groups, csv_hashes, used = {}, {}, set()
    for key, name, readable in names:
        groups[key], csv_hashes[name] = load_rows(primary, name, readable)
        hashes = {row['sha256'] for row in groups[key]}
        if used & hashes:
            raise ValueError('primary_split_overlap')
        used.update(hashes)
    primary_hashes, primary_manifest = manifest_hashes(primary)
    if not used <= primary_hashes:
        raise ValueError('split_not_in_primary_manifest')
    independent_report, all_hashes = [], set(primary_hashes)
    for root in independent:
        hashes, manifest_digest = manifest_hashes(root)
        if all_hashes & hashes:
            raise ValueError('dataset_hash_overlap')
        all_hashes.update(hashes)
        labels = Path(root) / 'labels.csv'
        confirmed = 0
        if labels.is_file():
            with labels.open(encoding='utf-8-sig', newline='') as handle:
                confirmed = sum(row.get('label_verified', '').lower() == 'true' for row in csv.DictReader(handle))
        independent_report.append({'root': str(Path(root).resolve()), 'images': len(hashes),
            'manifest_sha256': manifest_digest, 'confirmed_labels': confirmed})
    return groups, {'primary_root': str(Path(primary).resolve()), 'primary_manifest_sha256': primary_manifest,
        'csv_sha256': csv_hashes, 'counts': {key: len(value) for key, value in groups.items()},
        'case_sensitive_source_labels_preserved': True, 'training_and_metrics_case_sensitive': False,
        'group_overlap_count': 0, 'unreviewed_primary_images': len(primary_hashes - used),
        'independent': independent_report, 'independent_overlap_count': 0}


def predict(model_dir, rows, batch_size):
    from paddleocr import TextRecognition
    model = TextRecognition(model_name='PP-OCRv6_small_rec', model_dir=str(Path(model_dir).resolve()), device='cpu')
    started = time.monotonic()
    outputs = list(model.predict(input=[row['image'] for row in rows], batch_size=batch_size))
    elapsed = time.monotonic() - started
    if len(outputs) != len(rows):
        raise ValueError('prediction_count_mismatch')
    predictions = []
    for output in outputs:
        item = output.json
        if isinstance(item, str): item = json.loads(item)
        item = item.get('res', item)
        predictions.append({'text': item.get('rec_text', ''), 'score': float(item.get('rec_score', 0))})
    return predictions, elapsed


def accepted(prediction, threshold):
    return bool(VALID_LABEL.fullmatch(prediction['text'])) and prediction['score'] >= threshold


def threshold_for(clear, unreadable, max_clear_false_refresh=None):
    candidates = sorted({0.0, 1.0, *[item['score'] for item in clear + unreadable]})
    best = None
    limit = None
    if max_clear_false_refresh is not None:
        minimum = 1 - sum(accepted(item, 0) for item in clear) / len(clear)
        limit = max(max_clear_false_refresh, minimum)
    for threshold in candidates:
        clear_keep = sum(accepted(item, threshold) for item in clear) / len(clear)
        refresh_recall = 1 - sum(accepted(item, threshold) for item in unreadable) / len(unreadable)
        if limit is not None and 1 - clear_keep > limit + 1e-12:
            continue
        key = ((clear_keep + refresh_recall) / 2, clear_keep, refresh_recall, threshold)
        if limit is not None:
            key = (refresh_recall, clear_keep, threshold)
        if best is None or key > best[0]: best = (key, threshold)
    return best[1], limit


def metrics(clear_rows, clear_predictions, unreadable_predictions, threshold, seconds):
    keep = [accepted(item, threshold) for item in clear_predictions]
    exact = [item['text'].casefold() == row['label'].casefold()
             for row, item in zip(clear_rows, clear_predictions)]
    refresh = [not accepted(item, threshold) for item in unreadable_predictions]
    correct_outcomes = sum(a and b for a, b in zip(keep, exact)) + sum(refresh)
    total = len(clear_rows) + len(unreadable_predictions)
    return {'threshold': threshold, 'ocr_full_string_correct': sum(exact), 'ocr_clear_images': len(exact),
        'ocr_full_string_accuracy': sum(exact) / len(exact), 'refresh_recall': sum(refresh) / len(refresh),
        'clear_false_refresh_rate': 1 - sum(keep) / len(keep),
        'accepted_clear_correct': sum(a and b for a, b in zip(keep, exact)),
        'handled_correctly': correct_outcomes, 'decision_images': total,
        'handleable_proportion': correct_outcomes / total, 'seconds': seconds,
        'milliseconds_per_image': seconds * 1000 / total}


def truth_metrics(rows, predictions, threshold, seconds):
    clear = [(row, prediction) for row, prediction in zip(rows, predictions) if row['status'] == 'clear']
    unreadable = [prediction for row, prediction in zip(rows, predictions) if row['status'] == 'unreadable']
    result = metrics([row for row, _ in clear], [prediction for _, prediction in clear],
                     unreadable, threshold, seconds)
    result['truth_counts'] = {'clear': len(clear), 'unreadable': len(unreadable)}
    return result


def evaluate_model(model_dir, groups, independent_roots, batch_size, truth_rows=None):
    unreadable = sorted(groups['unreadable'], key=lambda row: row['sha256'])
    calibration_bad, test_bad = unreadable[:-25], unreadable[-25:]
    work = groups['validation'] + groups['test'] + calibration_bad + test_bad
    predictions, elapsed = predict(model_dir, work, batch_size)
    sizes = [len(groups['validation']), len(groups['test']), len(calibration_bad), len(test_bad)]
    offsets, chunks = 0, []
    for size in sizes:
        chunks.append(predictions[offsets:offsets + size]); offsets += size
    threshold, _ = threshold_for(chunks[0], chunks[2])
    low_false_refresh_threshold, limit = threshold_for(chunks[0], chunks[2], .25)
    result = {'calibration': metrics(groups['validation'], chunks[0], chunks[2], threshold,
                                     elapsed * (sizes[0] + sizes[2]) / len(work)),
              'test': metrics(groups['test'], chunks[1], chunks[3], threshold,
                              elapsed * (sizes[1] + sizes[3]) / len(work))}
    result['low_false_refresh'] = {'validation_limit': limit,
        'calibration': metrics(groups['validation'], chunks[0], chunks[2], low_false_refresh_threshold,
                               elapsed * (sizes[0] + sizes[2]) / len(work)),
        'test': metrics(groups['test'], chunks[1], chunks[3], low_false_refresh_threshold,
                        elapsed * (sizes[1] + sizes[3]) / len(work))}
    result['model_dir'] = str(Path(model_dir).resolve())
    result['model_files_sha256'] = {path.name: file_sha(path) for path in Path(model_dir).glob('*') if path.is_file()}
    result['independent'] = []
    for root in independent_roots:
        hashes, _ = manifest_hashes(root)
        rows = [{'image': str((Path(root) / (digest + '.jpg')).resolve()), 'sha256': digest} for digest in sorted(hashes)]
        preds, independent_seconds = predict(model_dir, rows, batch_size)
        result['independent'].append({'root': str(Path(root).resolve()), 'images': len(rows),
            'accepted': sum(accepted(item, threshold) for item in preds),
            'seconds': independent_seconds, 'accuracy': None,
            'truth_status': 'human_truth_available' if truth_rows else 'not_human_confirmed'})
    if truth_rows:
        predictions, truth_seconds = predict(model_dir, truth_rows, batch_size)
        result['independent_truth'] = {
            'balanced': truth_metrics(truth_rows, predictions, threshold, truth_seconds),
            'low_false_refresh': truth_metrics(truth_rows, predictions, low_false_refresh_threshold, truth_seconds)}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dataset', required=True)
    parser.add_argument('--independent', action='append', default=[])
    parser.add_argument('--current-model', required=True)
    parser.add_argument('--candidate-model', required=True)
    parser.add_argument('--human-truth', help='confirmed independent truth CSV; must cover every independent image')
    parser.add_argument('--out', required=True)
    parser.add_argument('--batch-size', type=int, default=32)
    args = parser.parse_args()
    groups, inventory = audit(args.dataset, args.independent)
    truth = load_truth(args.human_truth, args.independent) if args.human_truth else None
    if truth: inventory['human_truth_sha256'] = file_sha(args.human_truth)
    current = evaluate_model(args.current_model, groups, args.independent, args.batch_size, truth)
    candidate = evaluate_model(args.candidate_model, groups, args.independent, args.batch_size, truth)
    improved = (candidate['test']['ocr_full_string_accuracy'] > current['test']['ocr_full_string_accuracy']
                and candidate['test']['handleable_proportion'] > current['test']['handleable_proportion'])
    report = {'inventory': inventory, 'refresh_split': {'calibration': 124, 'test': 25},
        'current': current, 'candidate': candidate, 'candidate_internal_improvement': improved,
        'production_model_replaced': False,
        'recommendation': ('retain_current_model_independent_tradeoff_not_qualified' if truth
                           else ('manual_label_independent_sets_before_considering_replacement' if improved
                                 else 'retain_current_model_no_reliable_internal_improvement'))}
    output = Path(args.out).resolve(); output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps(report, ensure_ascii=False))


if __name__ == '__main__':
    main()
