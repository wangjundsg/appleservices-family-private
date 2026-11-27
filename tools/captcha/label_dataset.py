"""Budgeted, resumable YesCaptcha dataset labelling. API answers remain pseudo-labels."""
import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
import csv
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import threading
import time
import urllib.request

COST = 4
HOSTS = {'https://api.yescaptcha.com', 'https://cn.yescaptcha.com'}


def load_config(path):
    accounts = json.loads(Path(path).read_text(encoding='utf-8-sig'))['accounts']
    ids, envs = set(), set()
    if not accounts: raise ValueError('accounts_required')
    for account in accounts:
        name = account['id']
        keys = account['key_envs']
        if not re.fullmatch(r'[A-Za-z0-9_-]{1,40}', name) or name in ids: raise ValueError('account_id_invalid')
        if account['base_url'] not in HOSTS: raise ValueError('unsupported_api_endpoint')
        if type(account['budget_points']) is not int or account['budget_points'] < 0: raise ValueError('budget_invalid')
        if type(account['concurrency']) is not int or not 1 <= account['concurrency'] <= 3: raise ValueError('concurrency_invalid')
        if not keys or any(not re.fullmatch(r'[A-Z][A-Z0-9_]*', key) or key in envs for key in keys): raise ValueError('key_env_invalid_or_reused')
        if len(set(keys)) != len(keys): raise ValueError('duplicate_key_env')
        if len(keys) > account['concurrency']: raise ValueError('key_count_exceeds_account_concurrency')
        account['role'] = account.get('role', 'primary')
        if account['role'] not in ('primary', 'review'): raise ValueError('account_role_invalid')
        ids.add(name); envs.update(keys)
    if sum(a['concurrency'] for a in accounts) > 8: raise ValueError('total_concurrency_exceeds_eight')
    return accounts


def load_plan(path):
    plan = json.loads(Path(path).read_text(encoding='utf-8-sig')).get('pilot', {})
    values = {name: plan.get(name, default) for name, default in
              [('review_count', 0), ('validation_count', 0), ('test_count', 0)]}
    if any(type(value) is not int or value < 0 for value in values.values()): raise ValueError('pilot_count_invalid')
    if values['validation_count'] + values['test_count'] > values['review_count']:
        raise ValueError('pilot_split_exceeds_reviews')
    return values


def post(url, payload):
    request = urllib.request.Request(url, data=json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=30) as response:
        result = json.loads(response.read(1024 * 1024))
    if not isinstance(result, dict): raise ValueError('invalid_api_response')
    return result


class DatasetLock:
    def __init__(self, root): self.path = Path(root) / '.label.lock'
    def __enter__(self):
        self.file = self.path.open('a+b')
        try:
            if os.fstat(self.file.fileno()).st_size == 0: self.file.write(b'0'); self.file.flush()
            self.file.seek(0)
            if os.name == 'nt':
                import msvcrt
                msvcrt.locking(self.file.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(self.file.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.file.close(); raise ValueError('dataset_label_queue_locked') from None
        return self
    def __exit__(self, *args): self.file.close()


class Queue:
    def __init__(self, root):
        self.root = Path(root).resolve()
        self.db = sqlite3.connect(self.root / 'label-jobs.sqlite', check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.lock = threading.RLock(); self.visited = set(); self.disabled = set()
        self.db.execute('''CREATE TABLE IF NOT EXISTS jobs (
            sha TEXT PRIMARY KEY, file TEXT, state TEXT DEFAULT 'queued', account TEXT, key_env TEXT,
            key_hash TEXT, base_url TEXT, task_id TEXT, created REAL, charge INTEGER DEFAULT 0,
            label TEXT DEFAULT '', error TEXT DEFAULT '')''')
        self.db.execute('''CREATE TABLE IF NOT EXISTS reviews (
            sha TEXT PRIMARY KEY, file TEXT, state TEXT DEFAULT 'queued', account TEXT, key_env TEXT,
            key_hash TEXT, base_url TEXT, task_id TEXT, created REAL, charge INTEGER DEFAULT 0,
            label TEXT DEFAULT '', error TEXT DEFAULT '')''')
        # A create request may have landed before a crash; never pay for it again automatically.
        self.db.execute("UPDATE jobs SET state='uncertain',error='create_outcome_unknown' WHERE state='submitting'")
        self.db.execute("UPDATE reviews SET state='uncertain',error='create_outcome_unknown' WHERE state='submitting'")
        self.db.commit()

    def image(self, file, digest):
        path = (self.root / file).resolve()
        if not path.is_relative_to(self.root) or path.stat().st_size > 10 * 1024 * 1024: raise ValueError('image_path_or_size_invalid')
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != digest: raise ValueError('image_digest_mismatch')
        return data

    def inventory(self):
        confirmed, excluded = set(), set()
        labels = self.root / 'labels.csv'
        if labels.exists():
            with labels.open(encoding='utf-8-sig', newline='') as handle:
                for row in csv.DictReader(handle):
                    if row.get('label_verified', '').lower() == 'true': confirmed.add(row['sha256'])
                    elif row.get('label_source') == 'unreadable' or row.get('status') == 'quarantine': excluded.add(row['sha256'])
        for row in json.loads((self.root / 'manifest.json').read_text(encoding='utf-8-sig'))['samples']:
            self.image(row['file'], row['sha256'])
            if row['sha256'] not in confirmed:
                self.db.execute('INSERT OR IGNORE INTO jobs(sha,file) VALUES (?,?)', (row['sha256'], row['file']))
                if row['sha256'] in excluded:
                    self.db.execute("UPDATE jobs SET state='quarantine',error='manual_exclusion' WHERE sha=?", (row['sha256'],))
            else:
                self.db.execute("UPDATE jobs SET state='confirmed' WHERE sha=?", (row['sha256'],))
        self.db.commit()

    def update(self, sha, table='jobs', **fields):
        if table not in ('jobs', 'reviews'): raise ValueError('queue_table_invalid')
        with self.lock:
            self.db.execute('UPDATE ' + table + ' SET ' + ','.join(key + '=?' for key in fields) + ' WHERE sha=?', (*fields.values(), sha))
            self.db.commit()

    def claim(self, account, key_env, key, table='jobs'):
        if table not in ('jobs', 'reviews'): raise ValueError('queue_table_invalid')
        with self.lock:
            if account['id'] in self.disabled: return None
            pending = self.db.execute(f"SELECT * FROM {table} WHERE state='polling' AND account=? AND key_env=?", (account['id'], key_env)).fetchall()
            for row in pending:
                visit = (table, row['sha'])
                if visit in self.visited: continue
                self.visited.add(visit)
                if row['key_hash'] != hashlib.sha256(key.encode()).hexdigest() or row['base_url'] != account['base_url']:
                    self.update(row['sha'], table=table, error='original_api_key_required'); continue
                return dict(row)
            spent = self.db.execute('''SELECT
                COALESCE((SELECT SUM(charge) FROM jobs WHERE account=?),0) +
                COALESCE((SELECT SUM(charge) FROM reviews WHERE account=?),0)''', (account['id'], account['id'])).fetchone()[0]
            if spent + COST > account['budget_points']: return None
            row = self.db.execute(f"SELECT * FROM {table} WHERE state='queued' ORDER BY sha LIMIT 1").fetchone()
            if not row: return None
            self.visited.add((table, row['sha']))
            self.update(row['sha'], table=table, state='submitting', account=account['id'], key_env=key_env,
                        key_hash=hashlib.sha256(key.encode()).hexdigest(), base_url=account['base_url'], created=time.time(), charge=COST)
            return dict(self.db.execute(f'SELECT * FROM {table} WHERE sha=?', (row['sha'],)).fetchone())

    def worker(self, account, key_env, key, transport=post, wait=time.sleep, now=time.monotonic, table='jobs'):
        while (job := self.claim(account, key_env, key, table)) is not None:
            sha = job['sha']; task_id = job['task_id']
            try:
                if not task_id:
                    image = self.image(job['file'], sha)
                    result = transport(account['base_url'] + '/createTask', {'clientKey': key, 'task': {
                        'type': 'ImageToTextTask', 'body': base64.b64encode(image).decode(), 'case': False,
                        'numeric': 0, 'minLength': 4, 'maxLength': 6}})
                    if result.get('errorId'):
                        self.update(sha, table=table, state='queued', charge=0, error='api_create_rejected')
                        with self.lock: self.disabled.add(account['id'])
                        return
                    task_id = result.get('taskId')
                    if not isinstance(task_id, (str, int)) or isinstance(task_id, bool) or not str(task_id):
                        raise ValueError('missing_task_id')
                    self.update(sha, table=table, state='polling', task_id=str(task_id))
                end = now() + 180
                while now() < end:
                    wait(min(5, max(0, end - now())))
                    result = transport(account['base_url'] + '/getTaskResult', {'clientKey': key, 'taskId': task_id})
                    if result.get('errorId'):
                        self.update(sha, table=table, state='quarantine', error='api_task_failed'); break
                    if result.get('status') == 'ready':
                        label = (result.get('solution') or {}).get('text', '')
                        if isinstance(label, str) and re.fullmatch(r'[A-Za-z0-9]{4,6}', label.strip()):
                            label = label.strip()
                            if table == 'reviews':
                                primary = self.db.execute('SELECT label FROM jobs WHERE sha=? AND state=\'done\'', (sha,)).fetchone()
                                if primary and primary['label'].casefold() == label.casefold():
                                    self.update(sha, table=table, state='done', label=label, error='')
                                else: self.update(sha, table=table, state='quarantine', label=label, error='source_conflict')
                            else: self.update(sha, table=table, state='done', label=label, error='')
                        else: self.update(sha, table=table, state='quarantine', error='unreliable_label')
                        break
                else:
                    self.update(sha, table=table, error='poll_timeout'); return
            except Exception:
                self.update(sha, table=table, state='polling' if task_id else 'uncertain', error='transport_or_response_error')
                with self.lock: self.disabled.add(account['id'])
                return

    def prepare_reviews(self, count):
        existing = self.db.execute('SELECT COUNT(*) FROM reviews').fetchone()[0]
        remaining = max(0, count - existing)
        if remaining:
            self.db.execute('''INSERT OR IGNORE INTO reviews(sha,file)
                SELECT j.sha,j.file FROM jobs j WHERE j.state='done'
                AND NOT EXISTS(SELECT 1 FROM reviews r WHERE r.sha=j.sha)
                ORDER BY j.sha LIMIT ?''', (remaining,))
            self.db.commit()

    def export(self, validation_count=0, test_count=0):
        file = self.root / 'pseudo-labels.csv'; temporary = file.with_suffix('.tmp')
        train_count = 0
        with temporary.open('w', newline='', encoding='utf-8') as handle:
            writer = csv.DictWriter(handle, fieldnames=['file','label','label_verified','sha256','label_source','status'])
            writer.writeheader()
            for row in self.db.execute("""SELECT j.* FROM jobs j WHERE j.state='done'
                    AND NOT EXISTS(SELECT 1 FROM reviews r WHERE r.sha=j.sha) ORDER BY j.sha"""):
                writer.writerow(dict(file=row['file'], label=row['label'], label_verified='false', sha256=row['sha'],
                                     label_source='yescaptcha:' + row['account'], status='accepted'))
                train_count += 1
        os.replace(temporary, file)
        agreed = list(self.db.execute("""SELECT j.file,j.label,j.sha,j.account AS primary_account,r.account AS review_account
            FROM reviews r JOIN jobs j ON j.sha=r.sha WHERE r.state='done' ORDER BY j.sha"""))
        split_rows = {'validation': agreed[:validation_count],
                      'test': agreed[validation_count:validation_count + test_count]}
        for name, rows in split_rows.items():
            target = self.root / f'pseudo-{name}.csv'; temp = target.with_suffix('.tmp')
            with temp.open('w', newline='', encoding='utf-8') as handle:
                writer = csv.DictWriter(handle, fieldnames=['file','label','label_verified','sha256','label_source','status'])
                writer.writeheader()
                for row in rows:
                    writer.writerow(dict(file=row['file'],label=row['label'],label_verified='false',sha256=row['sha'],
                        label_source='consensus:' + row['primary_account'] + '+' + row['review_account'],status='accepted'))
            os.replace(temp, target)
        reserved = dict(self.db.execute('''SELECT account,SUM(charge) FROM (
            SELECT account,charge FROM jobs UNION ALL SELECT account,charge FROM reviews)
            WHERE account IS NOT NULL GROUP BY account''').fetchall())
        return {'states': dict(self.db.execute('SELECT state,COUNT(*) FROM jobs GROUP BY state').fetchall()),
                'review_states': dict(self.db.execute('SELECT state,COUNT(*) FROM reviews GROUP BY state').fetchall()),
                'splits': {'train': train_count,
                           'validation': len(split_rows['validation']), 'test': len(split_rows['test'])},
                'reserved_points': reserved}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dataset', required=True); parser.add_argument('--config', required=True)
    parser.add_argument('--run', action='store_true', help='submit paid tasks within configured cumulative budgets')
    args = parser.parse_args(); accounts = load_config(args.config); plan = load_plan(args.config)
    workers = {'primary': [], 'review': []}
    if args.run:
        fingerprints = {}
        for account in accounts:
            keys = [(name, os.environ.get(name, '')) for name in account['key_envs']]
            for name, key in keys:
                if not key: raise ValueError('configured_api_key_missing')
                digest = hashlib.sha256(key.encode()).hexdigest()
                if digest in fingerprints and fingerprints[digest] != account['id']: raise ValueError('same_key_in_multiple_budgets')
                fingerprints[digest] = account['id']
            for index in range(account['concurrency']): workers[account['role']].append((account, *keys[index % len(keys)]))
    with DatasetLock(args.dataset):
        queue = Queue(args.dataset)
        try:
            queue.inventory()
            # Known create rejection costs nothing; let another remaining account claim it.
            for _ in range(max(1, len(accounts))):
                disabled_before = len(queue.disabled)
                with ThreadPoolExecutor(max_workers=max(1, len(workers['primary']))) as pool:
                    futures = [pool.submit(queue.worker, *worker) for worker in workers['primary']]
                    for future in futures: future.result()
                if len(queue.disabled) == disabled_before: break
            queue.prepare_reviews(plan['review_count'])
            if args.run:
                with ThreadPoolExecutor(max_workers=max(1, len(workers['review']))) as pool:
                    futures = [pool.submit(queue.worker, *worker, table='reviews') for worker in workers['review']]
                    for future in futures: future.result()
            report = queue.export(plan['validation_count'], plan['test_count'])
            report['plan'] = {**plan, 'primary_budget_points':sum(a['budget_points'] for a in accounts if a['role']=='primary'),
                              'review_budget_points':sum(a['budget_points'] for a in accounts if a['role']=='review')}
            print(json.dumps({'paid_mode': args.run, **report}))
        finally: queue.db.close()


if __name__ == '__main__':
    try: main()
    except Exception as error:
        # Never print HTTP bodies, request objects, credentials or provider diagnostics.
        print(json.dumps({'state': 'failed', 'error_type': type(error).__name__}), file=__import__('sys').stderr)
        raise SystemExit(1)
