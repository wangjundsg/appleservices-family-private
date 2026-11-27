import unittest

from prepare_readability_review import ensure_isolation, validate_review
from train_readability import metrics_by_field


class HumanReviewPreparationTests(unittest.TestCase):
    def setUp(self):
        self.rows = [dict(sha256='a' * 64, review_part='A', readability_label='refresh', ocr_eligible='false'),
                     dict(sha256='b' * 64, review_part='B', readability_label='readable', ocr_eligible='true')]
        self.backups = {'A': [dict(sha256='a' * 64, label='', label_verified='false', label_source='unreadable', status='quarantine')], 'B': []}

    def test_accepts_explicit_binary_labels_without_character_truth(self):
        validate_review(self.rows, self.backups)

    def test_refresh_cannot_be_ocr_eligible(self):
        self.rows[0]['ocr_eligible'] = 'true'
        with self.assertRaisesRegex(ValueError, 'eligibility_conflict'):
            validate_review(self.rows, self.backups)

    def test_duplicate_hashes_and_wrong_review_part_fail(self):
        with self.assertRaisesRegex(ValueError, 'duplicate_review_hash'):
            validate_review(self.rows + [self.rows[0]], self.backups)
        self.rows[0]['review_part'] = 'B'
        with self.assertRaisesRegex(ValueError, 'backup_mismatch'):
            validate_review(self.rows, self.backups)

    def test_backup_cannot_supply_guessed_characters(self):
        self.backups['A'][0]['label'] = 'GUESS'
        with self.assertRaisesRegex(ValueError, 'invalid_refresh_backup'):
            validate_review(self.rows, self.backups)

    def test_near_duplicate_cannot_move_old_holdout_into_training(self):
        rows = [dict(group=split, split=split, decision=decision)
                for split in ('train', 'calibration', 'validation', 'regression')
                for decision in ('readable', 'refresh')]
        ensure_isolation(rows)
        rows[-1]['group'] = 'train'
        with self.assertRaisesRegex(ValueError, 'crosses_splits'):
            ensure_isolation(rows)

    def test_part_metrics_keep_their_own_denominators(self):
        rows = [{'review_part': part} for part in ('A', 'A', 'B', 'B', 'B')]
        result = metrics_by_field(rows, [0, 1, 0, 0, 1], [.9, .9, .1, .9, .2], .5, 'review_part')
        self.assertEqual(result['A']['confusion_matrix'], [[0, 1], [0, 1]])
        self.assertEqual(result['B']['confusion_matrix'], [[1, 1], [1, 0]])
        self.assertEqual(result['A']['false_readable_rate'], 1)
        self.assertEqual(result['B']['false_readable_rate'], .5)
        readable_only = metrics_by_field([{'review_part': 'B'}], [1], [.9], .5, 'review_part')
        self.assertIsNone(readable_only['B']['false_readable_rate'])


if __name__ == '__main__':
    unittest.main()
