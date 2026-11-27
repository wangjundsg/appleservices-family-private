import csv
import hashlib
from pathlib import Path
import tempfile
import unittest
from PIL import Image
from decision_eval import accepted, audit, load_truth, metrics, threshold_for, truth_metrics


class DecisionEvaluationTests(unittest.TestCase):
    def test_human_truth_must_cover_manifests_and_scores_case_insensitively(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / 'independent'; root.mkdir(); rows = []
            for i, status in enumerate(('clear', 'unreadable')):
                image = root / f'{i}.jpg'; Image.new('RGB', (160, 70), (i * 40, 0, 0)).save(image)
                rows.append({'dataset': root.name, 'file': image.name,
                    'sha256': hashlib.sha256(image.read_bytes()).hexdigest(), 'status': status,
                    'label': 'Ab3D' if status == 'clear' else ''})
            (root / 'manifest.json').write_text(__import__('json').dumps({'samples': rows}))
            truth = Path(folder) / 'truth.csv'
            with truth.open('w', newline='', encoding='utf-8') as handle:
                writer = csv.DictWriter(handle, fieldnames=list(rows[0])); writer.writeheader(); writer.writerows(rows)
            loaded = load_truth(truth, [root])
            result = truth_metrics(loaded, [{'text': 'AB3D', 'score': .9}, {'text': 'XXXX', 'score': .2}], .5, 1)
            self.assertEqual(result['ocr_full_string_accuracy'], 1)
            self.assertEqual(result['refresh_recall'], 1)
            self.assertEqual(result['handleable_proportion'], 1)

    def test_threshold_and_case_insensitive_metrics(self):
        clear = [{'text': 'aB3D', 'score': .9}, {'text': 'wrong', 'score': .8}]
        bad = [{'text': 'AB3D', 'score': .2}, {'text': 'ZZZZ', 'score': .3}]
        threshold, _ = threshold_for(clear, bad)
        self.assertTrue(accepted(clear[0], threshold)); self.assertFalse(accepted(bad[0], threshold))
        result = metrics([{'label': 'AB3D'}, {'label': 'RIGHT'}], clear, bad, threshold, 1)
        self.assertEqual(result['ocr_full_string_correct'], 1)
        self.assertEqual(result['handled_correctly'], 3)
        constrained, limit = threshold_for(clear, bad, .25)
        self.assertEqual(limit, .25)
        self.assertTrue(accepted(clear[0], constrained))

    def test_audit_rejects_cross_dataset_hash_overlap(self):
        with tempfile.TemporaryDirectory() as folder:
            primary = Path(folder) / 'primary'; independent = Path(folder) / 'independent'
            primary.mkdir(); independent.mkdir(); samples = []
            for i, (name, readable) in enumerate((('human-train.csv', True), ('human-validation.csv', True),
                    ('human-test.csv', True), ('manual-unreadable.csv', False))):
                image = primary / f'{i}.jpg'; Image.new('RGB', (160, 70), (i * 40, 0, 0)).save(image)
                digest = hashlib.sha256(image.read_bytes()).hexdigest(); samples.append({'file': image.name, 'sha256': digest})
                row = {'file': image.name, 'label': 'Ab3D' if readable else '',
                    'label_verified': 'true' if readable else 'false', 'sha256': digest,
                    'label_source': 'manual' if readable else 'unreadable',
                    'status': 'accepted' if readable else 'quarantine'}
                with (primary / name).open('w', newline='', encoding='utf-8') as handle:
                    writer = csv.DictWriter(handle, fieldnames=list(row)); writer.writeheader(); writer.writerow(row)
            (primary / 'manifest.json').write_text(__import__('json').dumps({'samples': samples}))
            source = primary / samples[0]['file']; target = independent / samples[0]['file']; target.write_bytes(source.read_bytes())
            (independent / 'manifest.json').write_text(__import__('json').dumps({'samples': [samples[0]]}))
            with self.assertRaisesRegex(ValueError, 'dataset_hash_overlap'):
                audit(primary, [independent])


if __name__ == '__main__': unittest.main()
