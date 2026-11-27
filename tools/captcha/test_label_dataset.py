import csv
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from label_dataset import Queue, DatasetLock, load_config


class QueueTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name); samples=[]
        for i in range(7):
            data = f'fixture-{i}'.encode(); file = f'{i}.png'; (self.root/file).write_bytes(data)
            samples.append({'file':file, 'sha256':hashlib.sha256(data).hexdigest()})
        (self.root/'manifest.json').write_text(json.dumps({'samples':samples}))
        self.queue = Queue(self.root); self.addCleanup(self.queue.db.close); self.queue.inventory()
        self.account={'id':'first','budget_points':12,'base_url':'https://api.yescaptcha.com'}

    def test_parallel_budget_exactly_once_and_raw_case_preserved(self):
        creates=[]
        def api(url,payload):
            if url.endswith('createTask'):
                self.assertFalse(payload['task']['case']);creates.append(payload['task']['body']);return {'taskId':len(creates)}
            return {'status':'ready','solution':{'text':'aB3D'}}
        with ThreadPoolExecutor(max_workers=3) as pool:
            tasks=[pool.submit(self.queue.worker,self.account,'TEST_KEY','secret',api,lambda _:None) for _ in range(3)]
            for task in tasks: task.result()
        self.assertEqual(len(creates),3); self.assertEqual(len(set(creates)),3)
        report=self.queue.export(); self.assertEqual(report['reserved_points'],{'first':12})
        self.assertEqual(report['states'],{'done':3,'queued':4})
        with (self.root/'pseudo-labels.csv').open() as handle: rows=list(csv.DictReader(handle))
        self.assertTrue(all(row['label']=='aB3D' and row['label_verified']=='false' for row in rows))
        self.queue.worker(self.account,'TEST_KEY','secret',api,lambda _:None)
        self.assertEqual(len(creates),3)

    def test_config_allows_five_workers_but_not_six(self):
        config = self.root / 'label-config.json'
        account = {'id':'first','base_url':'https://api.yescaptcha.com','key_envs':['TEST_KEY'],
                   'budget_points':4,'role':'primary','concurrency':5}
        config.write_text(json.dumps({'accounts':[account]}))
        self.assertEqual(load_config(config)[0]['concurrency'],5)
        account['concurrency']=6
        config.write_text(json.dumps({'accounts':[account]}))
        with self.assertRaisesRegex(ValueError,'concurrency_invalid'): load_config(config)

    def test_poll_resume_never_creates_again(self):
        job=self.queue.claim(self.account,'TEST_KEY','secret')
        self.queue.update(job['sha'],state='polling',task_id='saved-id')
        self.queue.visited.clear();self.account['budget_points']=4
        def api(url,payload):
            self.assertTrue(url.endswith('getTaskResult'));self.assertEqual(payload['taskId'],'saved-id')
            return {'status':'ready','solution':{'text':'AB3d'}}
        self.queue.worker(self.account,'TEST_KEY','secret',api,lambda _:None)
        self.assertEqual(self.queue.export()['states']['done'],1)

    def test_unknown_create_outcome_is_held_without_second_charge(self):
        self.queue.worker(self.account,'TEST_KEY','secret',lambda *_: (_ for _ in ()).throw(TimeoutError('secret')),lambda _:None)
        self.assertEqual(self.queue.export()['states']['uncertain'],1)
        self.assertEqual(self.queue.export()['reserved_points']['first'],4)
        self.assertNotIn('secret',str(self.queue.db.execute('SELECT error FROM jobs').fetchall()))

    def test_crash_intent_and_quarantine_are_not_training_truth(self):
        job=self.queue.claim(self.account,'TEST_KEY','secret')
        other=Queue(self.root)
        try:
            self.assertEqual(other.export()['states']['uncertain'],1)
        finally:other.db.close()
        self.account['budget_points']=8
        self.queue.worker(self.account,'TEST_KEY','secret',lambda url,p: {'taskId':'id'} if url.endswith('createTask') else {'status':'ready','solution':{'text':'??'}},lambda _:None)
        self.assertEqual(self.queue.export()['states']['quarantine'],1)
        with (self.root/'pseudo-labels.csv').open() as handle: self.assertEqual(len(list(csv.DictReader(handle))),0)

    def test_rejected_key_can_hand_job_to_another_budget(self):
        self.queue.worker(self.account,'TEST_KEY','secret',lambda *_:{'errorId':1},lambda _:None)
        self.assertEqual(self.queue.export()['reserved_points']['first'],0)
        second={**self.account,'id':'second','budget_points':4}
        self.queue.worker(second,'OTHER_KEY','other',lambda url,p:{'taskId':'id'} if url.endswith('createTask') else {'status':'ready','solution':{'text':'Ab3D'}},lambda _:None)
        self.assertEqual(self.queue.export()['states']['done'],1)

    def test_dataset_lock_excludes_a_second_runner(self):
        with DatasetLock(self.root):
            with self.assertRaises(ValueError):
                with DatasetLock(self.root): pass
        with DatasetLock(self.root): pass

    def test_review_agreement_ignores_case_and_conflicts_are_quarantined(self):
        labels=['aB3D','X9yZ','K7Lm'];created=0
        def primary_api(url,payload):
            nonlocal created
            if url.endswith('createTask'): created+=1;return {'taskId':created}
            return {'status':'ready','solution':{'text':labels[int(payload['taskId'])-1]}}
        self.queue.worker(self.account,'TEST_KEY','secret',primary_api,lambda _:None)
        self.queue.prepare_reviews(2)
        review={**self.account,'id':'review','budget_points':8}
        answers=iter(['Ab3d','NOPE'])
        def review_api(url,payload):
            if url.endswith('createTask'): return {'taskId':next(answers)}
            return {'status':'ready','solution':{'text':payload['taskId']}}
        self.queue.worker(review,'REVIEW_KEY','other',review_api,lambda _:None,table='reviews')
        report=self.queue.export(validation_count=1,test_count=1)
        self.assertEqual(report['review_states'],{'done':1,'quarantine':1})
        self.assertEqual(report['splits'],{'train':1,'validation':1,'test':0})
        conflict=self.queue.db.execute("SELECT error FROM reviews WHERE state='quarantine'").fetchone()[0]
        self.assertEqual(conflict,'source_conflict')


if __name__=='__main__':unittest.main()
