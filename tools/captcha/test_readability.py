import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import numpy as np
from PIL import Image
from scipy.special import expit
import torch

from readability import PREPROCESS_VERSION, ReadabilityModel, build_model, preprocess, probability
from train_readability import choose_threshold, fit_calibration, load_manifest, metrics


class ReadabilityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        torch.set_num_threads(1)

    def test_preserves_full_image_and_pads_without_cropping(self):
        tensor = preprocess(Image.new('RGB', (160, 70), 'black'))
        self.assertEqual(tuple(tensor.shape), (3, 112, 224))
        black = torch.tensor([-.485 / .229, -.456 / .224, -.406 / .225])
        white = torch.tensor([.515 / .229, .544 / .224, .594 / .225])
        self.assertTrue(torch.allclose(tensor[:, 50, 0], black))
        self.assertTrue(torch.allclose(tensor[:, 50, -1], black))
        self.assertTrue(torch.allclose(tensor[:, 0, 100], white))

    def test_bytes_and_pil_use_identical_preprocessing(self):
        image = Image.new('RGBA', (160, 70), (0, 0, 0, 0))
        buffer = io.BytesIO()
        image.save(buffer, format='PNG')
        self.assertTrue(torch.equal(preprocess(buffer.getvalue()), preprocess(image)))
        self.assertTrue(torch.equal(preprocess(image), preprocess(Image.new('RGB', (160, 70), 'white'))))
        with self.assertRaises(Exception):
            preprocess(b'not an image')

    def test_sigmoid_is_finite_and_rejects_nonfinite_output(self):
        self.assertEqual(probability(10000, 1, 0), 1)
        self.assertEqual(probability(-10000, 1, 0), 0)
        with self.assertRaisesRegex(ValueError, 'nonfinite'):
            probability(float('nan'), 1, 0)

    def test_threshold_prioritizes_refresh_and_handles_ties(self):
        scores, labels = np.array([.1, .8, .8, .9]), np.array([0, 0, 1, 1])
        threshold = choose_threshold(scores, labels)
        result = metrics(labels, scores, threshold)
        self.assertGreater(threshold, .8)
        self.assertEqual(result['false_readable_rate'], 0)
        self.assertEqual(result['readable_retention'], .5)
        self.assertEqual(choose_threshold([1, .9], [0, 1]), 1)

    def test_all_refresh_does_not_claim_useful_coverage(self):
        result = metrics([0, 0, 1, 1], [.2, .3, .4, .5], 1)
        self.assertEqual(result['confusion_matrix'], [[2, 0], [2, 0]])
        self.assertEqual(result['readable_retention'], 0)
        self.assertEqual(result['coverage'], 0)
        self.assertIsNone(result['refresh_fraction_among_readable_decisions'])

    def test_metrics_denominators_and_exact_confidence(self):
        result = metrics([0, 0, 1, 1, 1], [.2, .8, .3, .8, .9], .5)
        self.assertEqual(result['confusion_matrix'], [[1, 1], [1, 2]])
        self.assertEqual(result['false_readable_rate'], .5)
        self.assertEqual(result['readable_retention'], 2 / 3)
        self.assertEqual(result['coverage'], .6)
        self.assertEqual(result['accuracy'], .6)
        zero = metrics([0] * 60 + [1], [0.] * 60 + [.9], .5)
        self.assertAlmostEqual(zero['false_readable_exact_95_upper_one_sided'], 1 - .05 ** (1 / 60))

    def test_calibration_is_monotonic(self):
        logits, labels = np.array([-2., -1., 1., 2.]), np.array([0, 0, 1, 1])
        slope, intercept = fit_calibration(logits, labels)
        self.assertGreater(slope, 0)
        self.assertTrue(np.all(np.diff(expit(slope * logits + intercept)) > 0))
        self.assertLess(np.mean((expit(slope * logits + intercept) - labels) ** 2),
                        np.mean((expit(logits) - labels) ** 2))
        with self.assertRaisesRegex(ValueError, 'invalid_calibration'):
            fit_calibration(logits, [1] * 4)

    def test_manifest_rejects_changed_images_and_group_leakage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            rows = []
            for i, split in enumerate(('train', 'train', 'calibration', 'calibration', 'validation', 'validation')):
                image = root / f'{i}.png'
                Image.new('RGB', (10, 10), (i * 30, 0, 0)).save(image)
                rows.append(dict(image=str(image), sha256=hashlib.sha256(image.read_bytes()).hexdigest(),
                                 split=split, group=split, source_batch=split, decision='readable' if i % 2 else 'refresh'))
            manifest = root / 'manifest.json'

            def save():
                manifest.write_text(json.dumps(rows), encoding='utf-8')
                (root / 'audit.json').write_text(json.dumps({'frozen_manifest_sha256': hashlib.sha256(manifest.read_bytes()).hexdigest()}), encoding='utf-8')

            save()
            self.assertEqual(len(load_manifest(manifest)), 6)
            rows[2]['group'] = 'train'
            save()
            with self.assertRaisesRegex(ValueError, 'group_overlap'):
                load_manifest(manifest)
            rows[2]['group'] = 'calibration'
            save()
            (root / '0.png').write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError, 'changed_image'):
                load_manifest(manifest)

    def test_model_roundtrip_offline_interface_and_bad_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            model = build_model().eval()
            for param in model.parameters():
                param.data.zero_()
            metadata = dict(schema_version=1, architecture='mobilenet_v3_small',
                            preprocess_version=PREPROCESS_VERSION, threshold=.5,
                            calibration=[1., 0.], model_version='unit-test')
            bundle = dict(state_dict=model.state_dict(), metadata=metadata)
            path = Path(directory) / 'model.pt'
            torch.save(bundle, path)
            result = ReadabilityModel(path).predict(Image.new('RGB', (160, 70), 'white'))
            self.assertEqual(result, dict(decision='refresh', readable_probability=.5, model_version='unit-test'))
            metadata['threshold'] = float('nan')
            torch.save(bundle, path)
            with self.assertRaisesRegex(ValueError, 'invalid_decision'):
                ReadabilityModel(path)


if __name__ == '__main__':
    unittest.main()
