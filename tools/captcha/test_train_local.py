import csv
import hashlib
from pathlib import Path
import tempfile
import unittest
from PIL import Image
from train_local import confirmed_samples, evaluation_metrics, windows_loader_fix


class DatasetTests(unittest.TestCase):
    def test_empty_evaluation_is_not_a_valid_baseline(self):
        with self.assertRaisesRegex(ValueError, 'processed_no_images'):
            evaluation_metrics('INFO: acc:0.0\nINFO: fps:0')
        self.assertEqual(evaluation_metrics('INFO: acc:0.0\nINFO: fps:1.2')['acc'], 0)
        self.assertEqual(evaluation_metrics(b'\xa8 progress\nINFO: acc:0.0\nINFO: fps:1.2')['fps'], 1.2)

    def test_windows_single_worker_fix_is_scoped_and_idempotent(self):
        with tempfile.TemporaryDirectory() as folder:
            source = Path(folder); (source / 'tools').mkdir()
            file = source / 'tools/program.py'
            file.write_text('\n'.join(f'len({name}) - 1\n if platform.system() == "Windows"\n else len({name})'
                for name in ['train_dataloader','train_dataloader','valid_dataloader','eval_dataloader']), encoding='utf-8')
            digest = windows_loader_fix(source)
            self.assertEqual(digest, windows_loader_fix(source))
            self.assertIn('if platform.system() == "Windows" and valid_dataloader.num_workers > 0', file.read_text())

    def test_confirmation_hash_case_and_disjoint_split(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            rows = []
            for i in range(5):
                file = root / f'{i}.png'
                Image.new('RGB', (160, 70), (i * 40, 0, 0)).save(file)
                rows.append({'file': file.name, 'label': 'aB3D', 'label_verified': 'true' if i < 4 else 'false',
                             'sha256': hashlib.sha256(file.read_bytes()).hexdigest()})
            def save():
                with (root / 'labels.csv').open('w', newline='', encoding='utf-8') as handle:
                    writer = csv.DictWriter(handle, fieldnames=list(rows[0])); writer.writeheader(); writer.writerows(rows)
            save()
            train, validation = confirmed_samples(root)
            self.assertEqual((len(train), len(validation)), (3, 1))
            self.assertTrue(all(x['label'] == 'aB3D' for x in train + validation))
            self.assertFalse({x['sha256'] for x in train} & {x['sha256'] for x in validation})
            rows.append({**rows[0], 'label': 'AB3D'}); save()
            with self.assertRaisesRegex(ValueError, 'conflicting_labels'): confirmed_samples(root)
            rows.pop(); rows[0]['sha256'] = 'wrong'; save()
            with self.assertRaisesRegex(ValueError, 'image_digest_mismatch'): confirmed_samples(root)


if __name__ == '__main__': unittest.main()
