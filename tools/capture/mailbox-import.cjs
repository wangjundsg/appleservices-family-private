'use strict';
// Prepare an offline 8802 import; never open Store, contact Microsoft, or overwrite a file.
const fs = require('node:fs');
const path = require('node:path');
const {parseArgs} = require('node:util');
const {inventory} = require('./mailbox-inventory.cjs');

function prepare(text, existing = []) {
  const known = new Set(existing.map(row => String(row.address).trim().toLowerCase()));
  const pending = new Map();
  const skipped = new Set();
  const lines = String(text).replace(/^\uFEFF/, '').split(/\r?\n/).filter(line => line.trim());
  if (!lines.length) throw Error('import_empty');
  for (const line of lines) {
    const parts = line.split('----');
    if (parts.length !== 4 || parts.some(value => !value.trim())) throw Error('import_format_invalid');
    const [rawAddress, , clientId, refreshToken] = parts.map(value => value.trim());
    const address = rawAddress.toLowerCase();
    if (!/^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@(hotmail|outlook|live)\.com$/.test(address)) throw Error('import_address_invalid');
    if (!/^[a-f0-9]{8}-(?:[a-f0-9]{4}-){3}[a-f0-9]{12}$/i.test(clientId)) throw Error('import_client_id_invalid');
    if (/\s/.test(refreshToken)) throw Error('import_refresh_token_invalid');
    const row = {address, reader:'graph', client_id:clientId.toLowerCase(), refresh_token:refreshToken};
    const previous = pending.get(address);
    if (previous && (previous.client_id !== row.client_id || previous.refresh_token !== row.refresh_token)) throw Error('import_duplicate_conflict');
    // Even disabled/invalid/legacy records count as existing. Reauthorization is a separate operation.
    if (known.has(address)) { skipped.add(address); continue; }
    pending.set(address, row);
  }
  const rows = [...pending.values()];
  return {rows, summary:{input:lines.length, prepared:rows.length, existingSkipped:skipped.size,
    source:'purchased', authorization:'not_checked', imported:false}};
}

function writePrepared(file, rows) {
  if (!rows.length) throw Error('import_nothing_new');
  // wx prevents replacing any previous export, including one with a refreshed token.
  fs.writeFileSync(file, JSON.stringify(rows, null, 2) + '\n', {flag:'wx', mode:0o600});
}

if (require.main === module) {
  try {
    const {values} = parseArgs({options:{file:{type:'string'}, output:{type:'string'},
      'repo-root':{type:'string'}, 'outlook-root':{type:'string'}}});
    if (!values.file) throw Error('import_private_file_required');
    const repoRoot = path.resolve(values['repo-root'] || path.join(__dirname, '../..'));
    const result = prepare(fs.readFileSync(values.file, 'utf8'), inventory({repoRoot,outlookRoot:values['outlook-root']}).items);
    if (values.output) {
      const output = path.resolve(values.output);
      const privateRoot = path.join(repoRoot, '.cache');
      const relative = path.relative(privateRoot, output);
      if (!relative || relative.startsWith('..') || path.isAbsolute(relative)) throw Error('import_private_output_required');
      // A private existing directory is required; do not create or change ACLs as a side effect.
      writePrepared(output, result.rows);
    }
    console.log(JSON.stringify({...result.summary, exportWritten:Boolean(values.output)}));
  } catch (error) {
    console.error(/^import_[a-z_]+$/.test(error.message) ? error.message : 'import_prepare_failed');
    process.exitCode = 1;
  }
}
module.exports = {prepare, writePrepared};
