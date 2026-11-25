'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {prepare, writePrepared} = require('./mailbox-import.cjs');
const client = '11111111-2222-3333-4444-555555555555';
const row = (email='sample@hotmail.com', token='fixture-token') => `${email}----fixture-password----${client}----${token}`;

test('four-part delivery becomes OAuth-only payload with a safe summary', () => {
  const result = prepare('\uFEFF' + row('Sample@Hotmail.com') + '\r\n\r\n');
  assert.equal(result.rows[0].address, 'sample@hotmail.com');
  assert.equal(result.rows[0].reader, 'graph');
  assert.equal(JSON.stringify(result).includes('fixture-password'), false);
  assert.equal(JSON.stringify(result.summary).includes('fixture-token'), false);
  assert.equal(result.summary.authorization, 'not_checked');
  assert.equal(result.summary.imported, false);
});
test('known addresses are skipped irrespective of status, source or reader', () => {
  const result = prepare(row(), [{address:'SAMPLE@hotmail.com',status:'disabled',reader:'web'}]);
  assert.equal(result.rows.length, 0);
  assert.equal(result.summary.existingSkipped, 1);
});
test('same batch deduplicates exactly and rejects conflicting authorization', () => {
  assert.equal(prepare(row()+'\n'+row('SAMPLE@hotmail.com')).rows.length, 1);
  assert.throws(() => prepare(row()+'\n'+row('sample@hotmail.com','changed-token')), /import_duplicate_conflict/);
});
test('invalid later row prevents returning a partial import and errors contain no inputs', () => {
  for (const invalid of ['', 'secret-input', row().replace(client,'secret-client'),row().replace('fixture-token','secret token'),row('other@example.com')]) {
    assert.throws(() => prepare(invalid === '' ? '' : row()+'\n'+invalid), error => {
      assert.match(error.message, /^import_[a-z_]+$/);
      assert.equal(error.message.includes('secret'), false);
      return true;
    });
  }
});
test('export refuses to overwrite a file containing a newer token', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(),'mailbox-import-'));
  try {
    const file = path.join(dir,'private.json');
    writePrepared(file, prepare(row()).rows);
    const original = fs.readFileSync(file,'utf8');
    assert.throws(() => writePrepared(file, prepare(row('sample@hotmail.com','newer')).rows), {code:'EEXIST'});
    assert.equal(fs.readFileSync(file,'utf8'), original);
    assert.throws(() => writePrepared(path.join(dir,'empty.json'), []), /import_nothing_new/);
    assert.equal(fs.existsSync(path.join(dir,'empty.json')), false);
  } finally { fs.rmSync(dir, {recursive:true,force:true}); }
});
