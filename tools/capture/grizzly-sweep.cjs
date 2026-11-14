// Retry the cancellations the provider refused earlier.
//
// grizzly refuses setStatus=8 inside a minimum activation time (EARLY_CANCEL_
// DENIED) and accepts it for free while no SMS arrived. The country matrix ends
// each group long before that minute has passed, so the ask is parked in the
// group's session.json and collected here, once per group, before the next
// number is rented.
//
// Usage: node tools/capture/grizzly-sweep.cjs [--root .cache/cm] [--grizzly-key K]
// Output: one JSON line per activation that is still pending.
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const key = String(arg('grizzly-key', process.env.GRIZZLY_KEY || ''));
const root = path.resolve(arg('root', '.cache/cm'));
const api = 'https://api.grizzlysms.com/stubs/handler_api.php';
if (!key) {
  console.error('grizzly-sweep: --grizzly-key or GRIZZLY_KEY is required');
  process.exit(2);
}

async function call(params) {
  const url = new URL(api);
  url.searchParams.set('api_key', key);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, String(v));
  const res = await fetch(url, { timeout: 30000 });
  return (await res.text()).trim();
}

(async () => {
  let dirs = [];
  try {
    dirs = fs.readdirSync(root, { withFileTypes: true }).filter((d) => d.isDirectory()).map((d) => d.name);
  } catch {
    console.log(JSON.stringify({ ok: true, note: 'no groups yet' }));
    return;
  }
  for (const name of dirs) {
    const file = path.join(root, name, 'session.json');
    if (!fs.existsSync(file)) continue;
    let doc;
    try {
      doc = JSON.parse(fs.readFileSync(file, 'utf8'));
    } catch {
      continue;
    }
    const number = doc.number || {};
    if (!number.activation || number.sms !== 'waiting') continue;
    if (doc.refund && doc.refund.ok) continue;
    const status = await call({ action: 'getStatus', id: number.activation });
    if (!/STATUS_WAIT/.test(status)) {
      console.log(JSON.stringify({ group: name, activation: `${String(number.activation).slice(0, 4)}***`, status: status.slice(0, 40), skipped: true }));
      continue;
    }
    const response = await call({ action: 'setStatus', status: '8', id: number.activation });
    const ok = /ACCESS_CANCEL/.test(response);
    doc.refund = { ok, response: response.slice(0, 60), attempts: ((doc.refund && doc.refund.attempts) || 0) + 1, swept_at: new Date().toISOString() };
    fs.writeFileSync(file, `${JSON.stringify(doc, null, 2)}\n`, 'utf8');
    console.log(JSON.stringify({ group: name, activation: `${String(number.activation).slice(0, 4)}***`, ok, response: response.slice(0, 40) }));
  }
})().catch((e) => {
  console.error(JSON.stringify({ ok: false, error: String((e && e.message) || e).slice(0, 200) }));
  process.exit(1);
});
