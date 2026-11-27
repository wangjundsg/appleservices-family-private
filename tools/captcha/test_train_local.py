import csv
import hashlib
from pathlib import Path
import tempfile
import unittest
from PIL import Image
from train_local import (confirmed_samples, explicit_splits, evaluation_metrics,
                         windows_loader_fix, case_metric_fix)


class DatasetTests(unittest.TestCase):
    def test_explicit_splits_preserve_groups_and_reject_overlap(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); rows = []
            for i in range(3):
                image = root / f'{i}.png'; Image.new('RGB', (160, 70), (i * 40, 0, 0)).save(image)
                rows.append({'file': image.name, 'label': 'aB3D', 'label_verified': 'true',
                    'sha256': hashlib.sha256(image.read_bytes()).hexdigest(), 'status': 'accepted'})
            for name, row in zip(('train.csv', 'validation.csv', 'test.csv'), rows):
                with (root / name).open('w', newline='', encoding='utf-8') as handle:
                    writer = csv.DictWriter(handle, fieldnames=list(row)); writer.writeheader(); writer.writerow(row)
            groups = explicit_splits(root, 'train.csv', 'validation.csv', 'test.csv')
            self.assertEqual([group[0]['label'] for group in groups], ['AB3D'] * 3)
            with (root / 'test.csv').open('w', newline='', encoding='utf-8') as handle:
                writer = csv.DictWriter(handle, fieldnames=list(rows[0])); writer.writeheader(); writer.writerow(rows[0])
            with self.assertRaisesRegex(ValueError, 'split_hash_overlap'):
                explicit_splits(root, 'train.csv', 'validation.csv', 'test.csv')

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
            self.assertTrue(all(x['label'] == 'AB3D' for x in train + validation))
            self.assertFalse({x['sha256'] for x in train} & {x['sha256'] for x in validation})
            rows.append({**rows[0], 'label': 'AB3D'}); save()
            self.assertEqual(sum(map(len,confirmed_samples(root))),4)
            rows[-1]['label']='ZZ3D';save()
            with self.assertRaisesRegex(ValueError, 'conflicting_labels'): confirmed_samples(root)
            rows.pop(); rows[0]['sha256'] = 'wrong'; save()
            with self.assertRaisesRegex(ValueError, 'image_digest_mismatch'): confirmed_samples(root)

    def test_pseudo_labels_never_enter_validation_or_override_confirmed_labels(self):
        with tempfile.TemporaryDirectory() as folder:
            root=Path(folder);rows=[]
            for i in range(4):
                image=root/f'{i}.png';Image.new('RGB',(160,70),(i*30,0,0)).save(image)
                rows.append({'file':image.name,'label':'Ab3D','label_verified':'true' if i==0 else 'false',
                    'sha256':hashlib.sha256(image.read_bytes()).hexdigest(),'status':'accepted'})
            for file, data in [('labels.csv',rows[:1]),('pseudo.csv',[
                {**rows[0],'label':'ZZ3D'},rows[1],{**rows[1],'label':'AB3D'},
                rows[2],{**rows[2],'label':'XX3D'},{**rows[3],'status':'quarantine'}])]:
                with (root/file).open('w',newline='') as handle:
                    writer=csv.DictWriter(handle,fieldnames=list(rows[0]));writer.writeheader();writer.writerows(data)
            train,validation=confirmed_samples(root,root/'pseudo.csv')
            self.assertEqual(len(train),1);self.assertEqual(len(validation),1)
            self.assertEqual(validation[0]['label'],'AB3D');self.assertNotEqual(train[0]['sha256'],validation[0]['sha256'])

    def test_case_metric_ignores_only_case_and_keeps_punctuation_errors(self):
        with tempfile.TemporaryDirectory() as folder:
            root=Path(folder);(root/'ppocr/metrics').mkdir(parents=True)
            original='''class RecMetric:
    def __init__(self, ignore_space=False, **kwargs):
        self.ignore_space = ignore_space
    def __call__(self, pair):
        correct = 0
        norm_edit_dis = 0
        for (pred, _), (target, _) in zip(*pair):
            norm_edit_dis += Levenshtein.normalized_distance(pred, target)
            correct += pred == target
        return {'acc':correct / len(pair[0])}
class CNTMetric: pass
'''
            file=root/'ppocr/metrics/rec_metric.py';file.write_text(original,encoding='utf-8')
            digest=case_metric_fix(root);self.assertEqual(case_metric_fix(root),digest)
            text=file.read_text(encoding='utf-8');block=text[text.index('class RecMetric'):text.index('class CNTMetric')]
            class Distance:
                normalized_distance=staticmethod(lambda a,b: 0 if a==b else 1)
            scope={'Levenshtein':Distance};exec(block,scope)
            metric=scope['RecMetric'](ignore_case=True,ignore_space=False,is_filter=False)
            result=metric(([('ab3d',1),('AB!3D',1)],[('AB3D',1),('AB3D',1)]))
            self.assertAlmostEqual(result['acc'],.5,places=4)


if __name__ == '__main__': unittest.main()
