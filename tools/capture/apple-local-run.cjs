// One local Apple Account creation, the way the operator asked for it:
//   * plain Playwright launches the system Chrome, window visible, no proxy
//   * a fresh random identity (Anglo name pool, birthday 1940..2000)
//   * image captcha through tools/captcha/solver.py (YesCaptcha via env key)
//   * email code through Graph for a mailbox taken from .cache/mailboxes.json
//   * SMS through GrizzlySMS (USA, service wx)
//
// It buys the number, writes the Graph token file, runs create-account.cjs with
// --headed and --auto-refund, then records what happened next to the run log.
// No secret is printed: the password, the Graph token and both API keys stay in
// files under .cache (gitignored).
//
//   node tools/capture/apple-local-run.cjs [--outdir .cache/apple-local-1]

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');

const REPO = 'D:\\appleservices-family-private';
const MAILBOXES = path.join(REPO, '.cache', 'mailboxes.json');
const USAGE = path.join(REPO, '.cache', 'mailbox-usage.json');
const GRIZZLY_KEY_FILE = path.join(REPO, '.cache', 'grizzly.key');
const YESCAPTCHA_KEY_FILE = path.join(REPO, '.cache', 'yescaptcha.key');
const GRIZZLY_API = 'https://api.grizzlysms.com/stubs/handler_api.php';

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const stamp = new Date().toISOString().replace(/[:.]/g, '').replace('T', '-').slice(0, 15);
const outdir = path.resolve(arg('outdir', `.cache/apple-local-${stamp}`));
fs.mkdirSync(outdir, { recursive: true });

const readKey = (file) => (fs.existsSync(file) ? fs.readFileSync(file, 'utf8').trim() : '');
const grizzlyKey = readKey(GRIZZLY_KEY_FILE);
const yesKey = readKey(YESCAPTCHA_KEY_FILE);

const FIRST = ['Oliver', 'Harry', 'Jack', 'George', 'Charlie', 'Thomas', 'Oscar', 'William', 'Amelia', 'Olivia', 'Isla', 'Emily', 'Poppy', 'Ella', 'Ruby', 'Grace'];
const LAST = ['Smith', 'Jones', 'Taylor', 'Brown', 'Williams', 'Wilson', 'Johnson', 'Davies', 'Robinson', 'Wright', 'Thompson', 'Evans', 'Walker', 'White', 'Roberts', 'Green'];

function pick(a) {
  return a[Math.floor(Math.random() * a.length)];
}
function randomIdentity() {
  const year = 1940 + Math.floor(Math.random() * 61); // 1940..2000
  const month = 1 + Math.floor(Math.random() * 12);
  const days = new Date(year, month, 0).getDate();
  const day = 1 + Math.floor(Math.random() * days);
  return {
    first: pick(FIRST),
    last: pick(LAST),
    year,
    month,
    day,
    birthMonth: String(month).padStart(2, '0'),
    birthDay: String(day).padStart(2, '0'),
    birthYear: String(year),
  };
}
function randomPassword() {
  const upper = 'ABCDEFGHJKLMNPQRSTUVWXYZ';
  const lower = 'abcdefghijkmnopqrstuvwxyz';
  const digits = '23456789';
  const special = '!@#$%^&*-_=+?';
  const all = upper + lower + digits + special;
  const base = [pick(upper), pick(lower), pick(digits), pick(special)];
  while (base.length < 16) base.push(pick(all));
  for (let i = base.length - 1; i > 0; i -= 1) {
    const j = Math.floor(Math.random() * (i + 1));
    [base[i], base[j]] = [base[j], base[i]];
  }
  return base.join('');
}

function pickMailbox() {
  const list = JSON.parse(fs.readFileSync(MAILBOXES, 'utf8')).filter((m) => m.reader === 'graph' && m.status === 'ok' && m.client_id && m.refresh_token);
  if (!list.length) throw new Error('no_graph_mailbox');
  const usage = fs.existsSync(USAGE) ? JSON.parse(fs.readFileSync(USAGE, 'utf8')) : {};
  list.sort((a, b) => String(usage[a.address] || '').localeCompare(String(usage[b.address] || '')));
  return list[0];
}

async function grizzly(params) {
  const url = new URL(GRIZZLY_API);
  url.searchParams.set('api_key', grizzlyKey);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, String(v));
  return (await (await fetch(url)).text()).trim();
}

async function egress() {
  try {
    const res = await fetch('http://ip-api.com/json/?fields=status,country,countryCode,city,isp,query', { signal: AbortSignal.timeout(15000) });
    return await res.json();
  } catch (e) {
    return { status: 'fail', error: String(e).slice(0, 100) };
  }
}

(async () => {
  if (!grizzlyKey) throw new Error('grizzly_key_missing');
  const identity = randomIdentity();
  const password = randomPassword();
  const passwordFile = path.join(outdir, 'password.txt');
  fs.writeFileSync(passwordFile, `${password}\n`, { mode: 0o600 });

  const mailbox = pickMailbox();
  const graphFile = path.join(outdir, 'graph.json');
  fs.writeFileSync(graphFile, `${JSON.stringify({ client_id: mailbox.client_id, refresh_token: mailbox.refresh_token, mailbox: mailbox.address }, null, 2)}\n`, { mode: 0o600 });

  const net = await egress();
  const balanceBefore = await grizzly({ action: 'getBalance' });
  const boughtRaw = await grizzly({ action: 'getNumber', service: 'wx', country: 12, maxPrice: 0.06 });
  const m = /^ACCESS_NUMBER:(\d+):(\d+)/.exec(boughtRaw);
  const summary = {
    outdir,
    identity: { first: identity.first, last: identity.last, dob: `${identity.birthYear}-${identity.birthMonth}-${identity.birthDay}` },
    mailbox: mailbox.address,
    number: m ? { activation_prefix: `${m[1].slice(0, 4)}***`, phone_suffix: `***${m[2].slice(-3)}`, country: 'US' } : { error: boughtRaw.slice(0, 60) },
    sms_provider: 'grizzly',
    sms_api: GRIZZLY_API,
    egress: { country: net.countryCode || '', ip: net.query || '', isp: net.isp || '' },
    balance_before: (balanceBefore.split(':')[1] || '').trim(),
    started: new Date().toISOString(),
  };
  fs.writeFileSync(path.join(outdir, 'summary.json'), `${JSON.stringify(summary, null, 2)}\n`, 'utf8');
  if (!m) {
    console.log(JSON.stringify({ state: 'number_failed', error: boughtRaw.slice(0, 60) }));
    process.exit(3);
  }
  const activation = m[1];
  const phoneFull = m[2];
  const phoneLocal = phoneFull.startsWith('1') && phoneFull.length === 11 ? phoneFull.slice(1) : phoneFull;

  // Remember which mailbox this address used, so the next run takes a different
  // one instead of burning the same inbox twice.
  const usage = fs.existsSync(USAGE) ? JSON.parse(fs.readFileSync(USAGE, 'utf8')) : {};
  usage[mailbox.address] = new Date().toISOString();
  fs.writeFileSync(USAGE, JSON.stringify(usage, null, 2), 'utf8');

  const args = [
    path.join(REPO, 'tools', 'capture', 'create-account.cjs'),
    '--email', mailbox.address,
    '--password', password,
    '--phone', phoneLocal,
    '--activation', activation,
    '--hero-key', grizzlyKey,
    '--phonecc', 'US',
    '--outdir', outdir,
    '--graph-token-file', graphFile,
    '--first', identity.first,
    '--last', identity.last,
    '--birth-month', identity.birthMonth,
    '--birth-day', identity.birthDay,
    '--birth-year', identity.birthYear,
    '--sms-api', GRIZZLY_API,
    '--captcha-cmd', 'python tools/captcha/solver.py --provider yes --upscale 4',
    '--headed',
    '--auto-refund',
  ];

  const child = spawn(process.execPath, args, {
    cwd: REPO,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, YESCAPTCHA_KEY: yesKey },
    windowsHide: false,
  });
  const logFile = fs.createWriteStream(path.join(outdir, 'run.log'), { flags: 'a' });
  child.stdout.pipe(process.stdout);
  child.stderr.pipe(process.stdout);
  child.stdout.pipe(logFile);
  child.stderr.pipe(logFile);

  const exit = await new Promise((resolve) => child.on('exit', (code) => resolve(code)));
  const elapsedMs = Date.now() - Date.parse(summary.started);
  const balanceAfter = await grizzly({ action: 'getBalance' }).catch(() => '');
  const spent = Number((Number(summary.balance_before) - Number((balanceAfter.split(':')[1] || '0')).toFixed(4)).toFixed(4));

  const lines = fs.existsSync(path.join(outdir, 'run.jsonl'))
    ? fs.readFileSync(path.join(outdir, 'run.jsonl'), 'utf8').trim().split(/\r?\n/).map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter(Boolean)
    : [];
  const finalRec = [...lines].reverse().find((l) => l.kind === 'final') || {};
  const identityRec = lines.find((l) => l.at === 'identity') || null;
  const smsRec = lines.find((l) => l.at === 'phone_step' || l.at === 'sms' || l.at === 'phone-code') || null;

  const result = {
    ...summary,
    exit_code: String(exit),
    final_state: finalRec.state || 'unknown',
    final_evidence: finalRec.evidence || '',
    elapsed_ms: elapsedMs,
    sms_spend_usd: Number.isFinite(spent) ? spent : null,
    identity_recorded: identityRec,
    sms_record: smsRec,
    finished: new Date().toISOString(),
  };
  fs.writeFileSync(path.join(outdir, 'result.json'), `${JSON.stringify(result, null, 2)}\n`, 'utf8');
  console.log(JSON.stringify(result, null, 2));
})().catch((e) => {
  console.error(JSON.stringify({ state: 'runner_error', error: String((e && e.message) || e).slice(0, 200) }));
  process.exit(1);
});
