import json
from pathlib import Path
import tempfile
import unittest

import numpy as np

from audit_readability import assign_splits, group_near_duplicates, human_decision, read_adjudications, recover_batches


class ReadabilityAuditTests(unittest.TestCase):
    def test_explicit_adjudication_requires_known_unique_hash_and_human_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'decisions.json'
            entry = dict(sha256='known', decision='refresh', source='user-confirmed', reason='too blurry')
            path.write_text(json.dumps([entry]), encoding='utf-8')
            self.assertEqual(read_adjudications(path, {'known'}), {'known': entry})
            with self.assertRaisesRegex(ValueError, 'invalid_human'):
                read_adjudications(path, {'other'})
            for entries in ([entry, entry], [dict(entry, source='ocr')], [dict(entry, reason='')]):
                path.write_text(json.dumps(entries), encoding='utf-8')
                with self.assertRaisesRegex(ValueError, 'invalid_human'):
                    read_adjudications(path, {'known'})

    def test_human_note_overrides_stale_snapshot_flag(self):
        row = dict(status='quarantine', label_source='unreadable', label='', label_verified='false')
        for note in ('模糊', 'MH', 'mh', 'PMN4，分不清', 'F识别不出我'):
            self.assertEqual(human_decision(row, {'note': note, 'unreadable': False}), 'refresh')
        self.assertEqual(human_decision(row, {'correction': 'j无法核对'}), 'refresh')

    def test_pseudo_label_is_not_readability_truth(self):
        row = dict(status='accepted', label_source='api', label='ABCD', label_verified='true')
        with self.assertRaisesRegex(ValueError, 'human_evidence'):
            human_decision(row, {})

    def test_arbitrary_note_is_not_refresh_truth(self):
        row = dict(status='quarantine', label_source='unreadable', label='', label_verified='false')
        with self.assertRaisesRegex(ValueError, 'human_evidence'):
            human_decision(row, {'note': 'please inspect again'})

    def test_refresh_must_not_have_character_truth(self):
        row = dict(status='quarantine', label_source='unreadable', label='GUESS', label_verified='false')
        with self.assertRaisesRegex(ValueError, 'character_truth'):
            human_decision(row, {'note': '模糊'})

    def test_character_correction_does_not_erase_ambiguous_refresh_note(self):
        row = dict(status='accepted', label_source='manual-review:human', label='ABCD', label_verified='true')
        self.assertEqual(human_decision(row, {'note': '刷新不必死磕', 'correction': 'ABCD'}), 'ambiguous')

    def test_reconstructs_acquisition_order_not_file_time(self):
        samples = [{'sha256': str(i)} for i in range(4)]
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / 'harvest-session.out.log'
            log.write_text('\n'.join(json.dumps({'at': 'harvest', 'saved': n}) for n in (3, 4)), encoding='utf-8')
            batches = recover_batches(samples, [log])
            self.assertEqual(batches, {'0': 'legacy-unknown', '1': 'legacy-unknown',
                                      '2': 'harvest-session', '3': 'harvest-session'})
            with self.assertRaisesRegex(ValueError, 'ambiguous'):
                recover_batches(samples, [log, log])

    def test_gapped_log_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / 'harvest-session.out.log'
            log.write_text('\n'.join(json.dumps({'at': 'harvest', 'saved': n}) for n in (1, 3)), encoding='utf-8')
            with self.assertRaisesRegex(ValueError, 'noncontiguous'):
                recover_batches([{'sha256': str(i)} for i in range(3)], [log])

    def test_near_duplicates_join_whole_batches_transitively(self):
        rows = [dict(sha256=str(i), source_batch=batch) for i, batch in enumerate(['a', 'b', 'b', 'c'])]
        features = [(0, np.zeros((2, 2)), (160, 70)), (1, np.zeros((2, 2)), (160, 70)),
                    (2**30 - 1, np.ones((2, 2)), (160, 70)),
                    (2**30 - 2, np.ones((2, 2)), (160, 70))]
        pairs, _ = group_near_duplicates(rows, features)
        self.assertEqual(len(pairs), 2)
        self.assertEqual({r['group'] for r in rows}, {'a'})
        with self.assertRaisesRegex(ValueError, 'crosses_splits'):
            assign_splits(rows, 'b', 'c')

    def test_split_keeps_whole_batches_and_requires_both_classes(self):
        rows = [dict(source_batch=batch, group=batch, decision=decision)
                for batch in ('a', 'b', 'c') for decision in ('readable', 'refresh')]
        assign_splits(rows, 'b', 'c')
        self.assertEqual([r['split'] for r in rows], ['train'] * 2 + ['calibration'] * 2 + ['validation'] * 2)
        rows.pop()
        with self.assertRaisesRegex(ValueError, 'both_classes'):
            assign_splits(rows, 'b', 'c')


if __name__ == '__main__':
    unittest.main()
