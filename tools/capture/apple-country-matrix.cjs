// One country per window: the country matrix for Apple Account creation.
//
// This is the sibling of apple-os-ab.cjs. The variable under test is the SIM
// country: locale, time zone and the DataImpulse exit all follow it, while the
// form's account region stays United States (the gift-card requirement) and the
// browser stays Windows on the pinned core. Each group gets a fresh dirId, a
// fresh exit port and one number from grizzly-sms; the number is cancelled when
// no SMS ever arrived, and only then.
//
// Usage:
//   node tools/capture/apple-country-matrix.cjs --group G1 --cc GB --country-id 16 \
//     --locale en-GB --time-zone Europe/London --proxy-country gb --proxy-port 10061 \
//     --mailbox-id mb1 --mailboxes .cache/mailboxes.json --price 0.01 \
//     --outdir .cache/cm/G1 [--grizzly-key K] [--captcha-cmd "python tools/captcha/vote.py"]
//
// No secret reaches stdout, session.json or the console: the provider key, the
// proxy password, the account password and the mailbox refresh token are all
// carried by file references or masked.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

const REPO = 'D:\\appleservices-family-private';
const ENV_FILE = 'D:\\Workbench\\apps\\gpt-free-service\\.env';
const FORGE = 'http://127.0.0.1:50100';
const GRIZZLY_API = 'https://api.grizzlysms.com/stubs/handler_api.php';

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const flag = (name) => argv.includes(`--${name}`);

const group = String(arg('group', 'G0')).toUpperCase();
const cc = String(arg('cc', 'GB')).toUpperCase();
// The subscriber part the form wants next to its dial-code select: grizzly
// reports the number in international form, so the calling code has to be
// stripped first. An empty --dial silently typed the whole international number
// into the field (G6, 2026-11-14: "+44 447759461688" — 12 digits against a +44
// select). The map is the default; --dial still wins.
const DIAL_CODES = Object.freeze({
  GB: '44', US: '1', CA: '1', AU: '61', NZ: '64', VN: '84', ID: '62', PH: '63',
  MY: '60', TH: '66', SG: '65', IN: '91', DE: '49', FR: '33', NL: '31', ES: '34',
  IT: '39', PL: '48', SE: '46', NO: '47', JP: '81', BR: '55', MX: '52', ZA: '27',
});
const dial = String(arg('dial', DIAL_CODES[cc] || ''));
const countryId = String(arg('country-id', '16'));
const locale = String(arg('locale', 'en-GB'));
const timeZone = String(arg('time-zone', 'Europe/London'));
const proxyCountry = String(arg('proxy-country', cc)).toLowerCase();
const proxyPort = Number(arg('proxy-port', '10061')) || 0;
const price = String(arg('price', '0.1'));
const service = String(arg('service', 'wx'));
const mailboxId = String(arg('mailbox-id', ''));
const mailboxesFile = path.resolve(arg('mailboxes', '.cache/mailboxes.json'));
const outdir = path.resolve(arg('outdir', `.cache/cm/${group}`));
const os = String(arg('os', 'Windows'));
const captchaCmd = String(arg('captcha-cmd', '')) || `python ${path.join(REPO, 'tools', 'captcha', 'vote.py')}`;
const egressWaitMs = Number(arg('egress-wait-ms', '0')) || 0;

const grizzlyKey = String(arg('grizzly-key', process.env.GRIZZLY_KEY || ''));
if (!grizzlyKey) {
  console.error('apple-country-matrix: --grizzly-key or GRIZZLY_KEY is required');
  process.exit(2);
}

fs.mkdirSync(outdir, { recursive: true });
const sessionFile = path.join(outdir, 'session.json');
const logFile = path.join(outdir, 'session.jsonl');
function record(kind, data) {
  const row = { t: Date.now(), at: new Date().toISOString(), group, kind, ...data };
  try {
    fs.appendFileSync(logFile, `${JSON.stringify(row)}\n`, 'utf8');
  } catch {}
  if (['error', 'verdict', 'step', 'result'].includes(kind)) {
    console.log(kind, JSON.stringify(data).slice(0, 400));
  }
  return row;
}
const writeSession = (extra) => {
  const payload = { group, cc, locale, timeZone, countryId, outdir, ...extra };
  fs.writeFileSync(sessionFile, `${JSON.stringify(payload, null, 2)}\n`, 'utf8');
  return payload;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function loadEnv() {
  const env = {};
  if (!fs.existsSync(ENV_FILE)) return env;
  for (const line of fs.readFileSync(ENV_FILE, 'utf8').split(/\r?\n/)) {
    const m = /^\s*([A-Za-z0-9_]+)\s*=\s*(.*)\s*$/.exec(line);
    if (!m) continue;
    let v = m[2];
    if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) v = v.slice(1, -1);
    env[m[1]] = v;
  }
  return env;
}

// The exit is the only per-country variable inside the proxy: the account holds
// the DataImpulse credential and the port selects (and sticks) one IP for the
// lifetime of the run (sessttl.30 in the template).
function proxyInfoFromTemplate(template, country, portOverride) {
  const m = /^socks5h?:\/\/([^:]+):([^@]*)@([^:]+):(\d+)\s*$/i.exec(String(template || '').trim());
  if (!m) throw new Error('proxy_template_unparseable');
  const user = decodeURIComponent(m[1]);
  const pass = decodeURIComponent(m[2]);
  const host = m[3];
  const port = portOverride || Number(m[4]);
  const finalUser = /__cr\.[a-z]{2}/i.test(user) ? user.replace(/__cr\.[a-z]{2}/i, `__cr.${country}`) : user;
  return {
    proxyInfo: {
      moduleId: 0,
      proxyMethod: 'custom',
      proxyCategory: 'SOCKS5',
      ipType: 'IPV4',
      protocol: 'SOCKS5',
      host,
      port,
      proxyUserName: finalUser,
      proxyPassword: pass,
    },
    mask: `socks5://***__cr.${country}@${host}:${port}`,
  };
}

async function forge(pathname, { method = 'POST', token, body, query } = {}) {
  const url = new URL(FORGE + pathname);
  for (const [k, v] of Object.entries(query || {})) if (v !== undefined && v !== null) url.searchParams.set(k, String(v));
  const res = await fetch(url, {
    method,
    headers: { 'content-type': 'application/json', token: token || '' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {}
  return { status: res.status, json, text: text.slice(0, 400) };
}
const businessOk = (r) => r.status === 200 && r.json && r.json.success !== false && [0, 200, '0', '200'].includes(r.json.code);

async function grizzly(params) {
  const url = new URL(GRIZZLY_API);
  url.searchParams.set('api_key', grizzlyKey);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, String(v));
  const res = await fetch(url, { timeout: 30000 });
  return (await res.text()).trim();
}

async function measureInWindow(port) {
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`, { timeout: 30000 });
  try {
    const context = browser.contexts()[0] || (await browser.newContext());
    const page = await context.newPage();
    const probes = [
      { name: 'ipinfo', url: 'https://ipinfo.io/json' },
      { name: 'ipapi', url: 'https://ipapi.co/json/' },
      { name: 'ip-api', url: 'http://ip-api.com/json/?fields=status,country,countryCode,city,isp,query' },
    ];
    const attempts = [];
    let egress = null;
    const deadline = Date.now() + egressWaitMs;
    while (!egress) {
      for (const probe of probes) {
        try {
          await page.goto(probe.url, { waitUntil: 'domcontentloaded', timeout: 45000 });
          const text = ((await page.textContent('body')) || '').trim();
          const body = JSON.parse(text);
          const countryCode = String(body.country_code || body.countryCode || body.country || '').toUpperCase().slice(0, 2);
          const ip = body.ip || body.query || body.ip_addr || null;
          attempts.push({ name: probe.name, ok: Boolean(countryCode), countryCode, ip });
          if (countryCode && !egress) egress = { source: probe.name, countryCode, ip, city: body.city || '', org: body.org || body.isp || body.asn_org || '', timezone: body.timezone || '' };
        } catch (e) {
          attempts.push({ name: probe.name, ok: false, error: String((e && e.message) || e).slice(0, 100) });
        }
        if (egress) break;
      }
      if (!egress && Date.now() < deadline) await sleep(5000);
      else break;
    }
    const fp = await page.evaluate(() => ({
      ua: navigator.userAgent,
      platform: navigator.platform,
      language: navigator.language,
      languages: navigator.languages,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      webdriver: navigator.webdriver,
      screen: `${screen.width}x${screen.height}@${window.devicePixelRatio}`,
      cores: navigator.hardwareConcurrency,
    })).catch(() => null);
    await page.close().catch(() => {});
    return { egress: egress || { countryCode: '', attempts }, fp, egressAttempts: attempts };
  } finally {
    await browser.close().catch(() => {});
  }
}

function verdictOf(fp, egress) {
  const ua = String((fp && fp.ua) || '');
  const platform = String((fp && fp.platform) || '');
  const lang = String((fp && fp.language) || '');
  // Chrome reports whichever alias the platform build carries: Vietnam's zone
  // comes back as Asia/Saigon. Same zone, different spelling, not a mismatch.
  const TZ_ALIASES = { 'asia/saigon': 'asia/ho_chi_minh', 'asia/calcutta': 'asia/kolkata', 'asia/rangoon': 'asia/yangon', 'europe/kyiv': 'europe/kiev' };
  const normTz = (value) => {
    const key = String(value || '').toLowerCase();
    return TZ_ALIASES[key] || key;
  };
  const tz = normTz(fp && fp.timeZone);
  const seen = String((egress && egress.countryCode) || '').toUpperCase();
  const checks = {
    ua_os: os.toLowerCase().startsWith('mac') ? ua.includes('Macintosh; Intel Mac OS X') : ua.includes('Windows NT 10.0'),
    platform: os.toLowerCase().startsWith('mac') ? platform === 'MacIntel' : platform === 'Win32',
    locale: lang === locale,
    languages_head: Array.isArray(fp && fp.languages) && fp.languages[0] === locale,
    timezone: tz === normTz(timeZone),
    egress_country: seen === cc,
  };
  const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([k]) => k);
  return { checks, failed, ok: failed.length === 0, countryCode: seen };
}

const passwordPattern = {
  upper: 'ABCDEFGHJKLMNPQRSTUVWXYZ',
  lower: 'abcdefghijkmnopqrstuvwxyz',
  digits: '23456789',
  special: '!@#$%^&*-_=+?',
};
function ensurePassword(file) {
  try {
    const existing = fs.readFileSync(file, 'utf8').trim();
    if (existing.length >= 12) return existing;
  } catch {}
  const pick = (s) => s[Math.floor(Math.random() * s.length)];
  const chars = [pick(passwordPattern.upper), pick(passwordPattern.lower), pick(passwordPattern.digits), pick(passwordPattern.special)];
  const pool = passwordPattern.upper + passwordPattern.lower + passwordPattern.digits + passwordPattern.special;
  while (chars.length < 16) chars.push(pick(pool));
  for (let i = chars.length - 1; i > 0; i -= 1) {
    const j = Math.floor(Math.random() * (i + 1));
    [chars[i], chars[j]] = [chars[j], chars[i]];
  }
  fs.writeFileSync(file, `${chars.join('')}\n`, { mode: 0o600 });
  return chars.join('');
}

const FIRST_NAMES = ['James', 'Oliver', 'Harry', 'Jack', 'Charlie', 'Thomas', 'George', 'William', 'Henry', 'Alfie', 'Freddie', 'Arthur', 'Amelia', 'Olivia', 'Emily', 'Sophie', 'Grace', 'Chloe', 'Lucy', 'Ella', 'Megan', 'Hannah', 'Katie', 'Laura', 'Daniel', 'Michael', 'Sarah', 'Emma'];
const LAST_NAMES = ['Smith', 'Jones', 'Taylor', 'Brown', 'Wilson', 'Davies', 'Evans', 'Thomas', 'Roberts', 'Walker', 'Wright', 'Robinson', 'Thompson', 'White', 'Hughes', 'Edwards', 'Green', 'Hall', 'Wood', 'Harris', 'Clarke', 'Baker', 'Cooper', 'Ward', 'Bennett', 'Foster'];
function randomIdentity() {
  const pick = (list) => list[Math.floor(Math.random() * list.length)];
  const year = 1920 + Math.floor(Math.random() * (2000 - 1920 + 1));
  const month = 1 + Math.floor(Math.random() * 12);
  const day = 1 + Math.floor(Math.random() * new Date(Date.UTC(year, month, 0)).getUTCDate());
  return { first: pick(FIRST_NAMES), last: pick(LAST_NAMES), month: String(month).padStart(2, '0'), day: String(day).padStart(2, '0'), year: String(year) };
}

// The mailbox list is a credential file: this run copies one entry into the
// group's own token file, and writes the rotated token back to the list when
// Microsoft retires the old one (graph-mail.cjs does the rotation in place).
function prepareMailbox(entry) {
  const tokenFile = path.join(outdir, 'graph-token.json');
  fs.writeFileSync(tokenFile, `${JSON.stringify({ client_id: entry.client_id, refresh_token: entry.refresh_token, mailbox: entry.address }, null, 2)}\n`, { mode: 0o600 });
  return tokenFile;
}
function syncMailbox(id) {
  try {
    const tokenFile = path.join(outdir, 'graph-token.json');
    const rotated = JSON.parse(fs.readFileSync(tokenFile, 'utf8'));
    const list = JSON.parse(fs.readFileSync(mailboxesFile, 'utf8'));
    const row = list.find((m) => m.id === id);
    if (!row || !rotated.refresh_token) return { updated: false };
    if (row.refresh_token === rotated.refresh_token) return { updated: false };
    row.refresh_token = rotated.refresh_token;
    row.reason = `rotated by ${group} at ${new Date().toISOString()}`;
    fs.writeFileSync(mailboxesFile, `${JSON.stringify(list, null, 2)}\n`, 'utf8');
    return { updated: true };
  } catch (e) {
    return { updated: false, error: String((e && e.message) || e).slice(0, 120) };
  }
}

// grizzly reports the phone in international form without a plus; the form
// wants the subscriber part next to the dial-code select.
function localPhone(phoneFull, dialCode) {
  const digits = String(phoneFull || '').replace(/\D/g, '');
  if (dialCode && digits.startsWith(dialCode) && digits.length - dialCode.length >= 7) return digits.slice(dialCode.length);
  return digits;
}

function readLastJsonl(file) {
  try {
    const lines = fs.readFileSync(file, 'utf8').trim().split(/\r?\n/);
    return JSON.parse(lines[lines.length - 1]);
  } catch {
    return null;
  }
}

function tail(file, bytes = 4000) {
  try {
    const stat = fs.statSync(file);
    const start = Math.max(0, stat.size - bytes);
    const fd = fs.openSync(file, 'r');
    const buf = Buffer.alloc(stat.size - start);
    fs.readSync(fd, buf, 0, buf.length, start);
    fs.closeSync(fd);
    return buf.toString('utf8');
  } catch {
    return '';
  }
}

async function main() {
  const env = loadEnv();
  const token = env.ROXY_API_TOKEN || process.env.ROXY_API_TOKEN || '';
  const workspaceId = env.ROXY_WORKSPACE_ID || process.env.ROXY_WORKSPACE_ID || '';
  const projectId = env.ROXY_PROJECT_ID || process.env.ROXY_PROJECT_ID || '';
  if (!token || !workspaceId || !projectId) throw new Error('roxy_credentials_missing');
  // A number that is typed without its calling code stripped is a wasted
  // rental, so an unknown country is refused before the purchase, not after.
  if (!dial) {
    record('error', { at: 'dial_unknown', cc, hint: 'pass --dial <calling code>' });
    writeSession({ state: 'dial_unknown', cc });
    process.exit(2);
  }
  const { proxyInfo, mask } = proxyInfoFromTemplate(env.DATAIMPULSE_PROXY_TEMPLATE, proxyCountry, proxyPort);
  const password = ensurePassword(path.join(outdir, 'password.txt'));
  const identity = randomIdentity();

  let mailbox = null;
  let tokenFile = '';
  if (mailboxId) {
    const list = JSON.parse(fs.readFileSync(mailboxesFile, 'utf8'));
    mailbox = list.find((m) => m.id === mailboxId) || null;
    if (!mailbox) throw new Error(`mailbox_not_found:${mailboxId}`);
    if (mailbox.reader === 'webmail' || !mailbox.refresh_token) throw new Error(`mailbox_not_graph_readable:${mailboxId}`);
    tokenFile = prepareMailbox(mailbox);
  }

  const health = await forge('/health', { method: 'GET' });
  const core = health.json && health.json.data && health.json.data.core ? health.json.data.core : null;
  const healthOk = Boolean(health.json && health.json.data && health.json.data.ok);
  record('step', { at: 'forge-health', ok: healthOk, core: core ? core.build : null, core_pinned: core ? core.pinned : null });
  if (!core || !core.pinned) {
    writeSession({ state: 'core_unpinned', core });
    process.exit(1);
  }

  const windowName = `cm-${group.toLowerCase()}-${Date.now().toString(36)}`;
  const created = await forge('/browser/create', {
    token,
    body: { workspaceId, projectId, windowName, os, locale, timeZone, proxyInfo },
  });
  const createOk = businessOk(created) && created.json.data && created.json.data.dirId;
  record('step', {
    at: 'create', request_os: os, locale, timeZone, core: core.build,
    identity: { first: identity.first, last: identity.last, birth: `${identity.month}/${identity.day}/${identity.year}` },
    proxy: mask,
    code: created.json ? created.json.code : null,
    success: created.json ? created.json.success : null,
    dirId: createOk ? created.json.data.dirId : null,
    raw: createOk ? undefined : created.text,
  });
  if (!createOk) {
    writeSession({ state: 'create_failed', detail: created.text });
    process.exit(1);
  }
  const dirId = created.json.data.dirId;

  const opened = await forge('/browser/open', { token, body: { workspaceId, dirId, args: [], forceOpen: true, headless: false } });
  const data = opened.json && opened.json.data;
  const debugPort = Number(data && (data.debuggingPort || (data.debuggerAddress ? String(data.debuggerAddress).split(':').pop() : 0)));
  record('step', { at: 'open', code: opened.json ? opened.json.code : null, success: opened.json ? opened.json.success : null, debugPort: Number.isFinite(debugPort) ? debugPort : null });
  if (!businessOk(opened) || !debugPort) {
    writeSession({ state: 'open_failed', dirId, detail: opened.text });
    process.exit(1);
  }

  const detail = await forge('/browser/detail', { method: 'GET', token, query: { workspaceId, dirId } });
  const row = detail.json && detail.json.data && Array.isArray(detail.json.data.rows) ? detail.json.data.rows.find((r) => r.dirId === dirId) : null;
  record('step', { at: 'detail', found: Boolean(row), os: row ? row.os : null, windowName: row ? row.windowName : null });

  const measured = await measureInWindow(debugPort);
  const verdict = verdictOf(measured.fp, measured.egress);
  record('verdict', {
    request_os: os, config_os: row ? row.os : null,
    observed_platform: measured.fp ? measured.fp.platform : null,
    observed_ua: measured.fp ? String(measured.fp.ua).slice(0, 140) : null,
    language: measured.fp ? measured.fp.language : null,
    timeZone: measured.fp ? measured.fp.timeZone : null,
    egress_country: verdict.countryCode,
    egress_ip: measured.egress && measured.egress.ip ? measured.egress.ip : null,
    checks: verdict.checks, failed: verdict.failed, ok: verdict.ok,
  });

  const cleanup = async (reason) => {
    const closed = await forge('/browser/close', { token, body: { workspaceId, dirId } });
    record('step', { at: 'close', reason, code: closed.json ? closed.json.code : null, success: closed.json ? closed.json.success : null, ok: businessOk(closed) });
    let deleted = null;
    let attempts = 0;
    for (attempts = 1; attempts <= 5; attempts += 1) {
      await sleep(1500 * attempts);
      deleted = await forge('/browser/delete', { token, body: { workspaceId, dirIds: [dirId] } });
      const body = deleted.json || {};
      const rows = body.data && Array.isArray(body.data.dirIds) ? body.data.dirIds : [];
      const entry = rows.find((r) => r.dirId === dirId) || {};
      const ok = businessOk(deleted) && entry.deleted !== false;
      const eperm = /EPERM/i.test(String(body.msg || entry.error || ''));
      record('step', { at: 'delete', attempt: attempts, code: body.code === undefined ? null : body.code, success: body.success === undefined ? null : body.success, deleted: entry.deleted === undefined ? null : entry.deleted, msg: String(body.msg || entry.error || '').slice(0, 160), ok });
      if (ok) return { closed: businessOk(closed), deleted: true, attempts };
      if (!eperm) return { closed: businessOk(closed), deleted: false, attempts };
    }
    return { closed: businessOk(closed), deleted: false, attempts };
  };

  if (!verdict.ok) {
    const receipts = await cleanup('geo_mismatch');
    writeSession({ state: 'geo_mismatch', dirId, debugPort, observed: measured.fp, egress: measured.egress, egress_attempts: measured.egressAttempts, verdict, cleanup: receipts, spent: 0 });
    process.exit(2);
  }

  // --session-only stops after the fingerprint verdict: the wiring (create,
  // open, locale, time zone, exit country) is proved without renting a number.
  if (flag('session-only')) {
    const receipts = await cleanup('session-only');
    const payload = writeSession({ state: 'session_ok', dirId, debugPort, observed: measured.fp, egress: measured.egress, egress_attempts: measured.egressAttempts, verdict, cleanup: receipts, spent: 0 });
    record('result', { state: 'session_ok', spent: 0, egress: measured.egress.ip, verdict: verdict.checks });
    console.log(JSON.stringify({ group, state: 'session_ok', spent_usd: 0, egress: measured.egress, verdict: verdict.checks }));
    return payload;
  }

  const balanceBefore = await grizzly({ action: 'getBalance' });
  const bought = await grizzly({ action: 'getNumber', service, country: countryId, maxPrice: price });
  const numberMatch = /^ACCESS_NUMBER:(\d+):(\d+)/.exec(bought);
  record('step', {
    at: 'number',
    balance_before: balanceBefore.startsWith('ACCESS_BALANCE') ? balanceBefore.split(':')[1] : balanceBefore.slice(0, 40),
    bought: numberMatch ? 'ACCESS_NUMBER' : bought.slice(0, 60),
    activation_prefix: numberMatch ? `${numberMatch[1].slice(0, 4)}***` : null,
    phone_suffix: numberMatch ? `***${String(numberMatch[2]).slice(-3)}` : null,
    dial,
    phone_local_digits: phoneLocal ? phoneLocal.length : 0,
    price: price,
  });
  if (!numberMatch) {
    const receipts = await cleanup('number_failed');
    writeSession({ state: 'number_failed', dirId, detail: bought.slice(0, 80), verdict, cleanup: receipts, spent: 0 });
    process.exit(3);
  }
  const activation = numberMatch[1];
  const phoneFull = numberMatch[2];
  const phoneLocal = localPhone(phoneFull, dial);
  const boughtAt = Date.now();

  const captchaFile = path.join(outdir, 'captcha-answer.txt');
  const childOut = path.join(outdir, 'create-account.log');
  const args = [
    path.join(REPO, 'tools', 'capture', 'create-account.cjs'),
    '--email', mailbox ? mailbox.address : '',
    '--password', password,
    '--phone', phoneLocal,
    '--activation', activation,
    '--hero-key', grizzlyKey,
    '--sms-api', GRIZZLY_API,
    '--outdir', outdir,
    '--await-captcha', captchaFile,
    '--phonecc', cc,
    '--cdp-port', String(debugPort),
    '--captcha-cmd', captchaCmd,
    '--sms-code-out', path.join(outdir, 'sms-code.txt'),
    '--first', identity.first,
    '--last', identity.last,
    '--birth-month', identity.month,
    '--birth-day', identity.day,
    '--birth-year', identity.year,
  ];
  if (tokenFile) args.push('--graph-token-file', tokenFile);

  const startedAt = Date.now();
  const child = spawn(process.execPath, args, { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: false });
  const childLog = fs.createWriteStream(childOut, { flags: 'a' });
  child.stdout.pipe(childLog);
  child.stderr.pipe(childLog);

  // A grizzly number lives 20 minutes. The driver gets 18 of them; past that the
  // number is worth more as a refund than as a race.
  const capMs = 18 * 60 * 1000;
  const exitCode = await new Promise((resolve) => {
    const timer = setTimeout(() => {
      record('error', { at: 'child-timeout', ms: Date.now() - startedAt });
      try { child.kill(); } catch {}
      resolve('timeout');
    }, capMs);
    child.on('exit', (code) => {
      clearTimeout(timer);
      resolve(code);
    });
  });
  const elapsedMs = Date.now() - startedAt;

  const stateMap = { 0: 'created', 1: 'failed', 2: 'usage', 3: 'unknown', 4: 'waiting_input', 5: 'likely_success', timeout: 'timeout' };
  let state = stateMap[String(exitCode)] || `exit_${exitCode}`;

  // What the provider thinks happened decides the refund, not our exit code:
  // STATUS_OK means the SMS arrived and the number is spent.
  const statusText = await grizzly({ action: 'getStatus', id: activation }).catch(() => '');
  const smsStatus = statusText.startsWith('STATUS_OK') ? 'received' : /STATUS_WAIT/.test(statusText) ? 'waiting' : statusText.slice(0, 40);

  const balanceAfter = await grizzly({ action: 'getBalance' }).catch(() => '');
  const before = Number((balanceBefore.split(':')[1] || '0'));
  const after = Number((balanceAfter.split(':')[1] || '0'));
  const spent = Number.isFinite(before) && Number.isFinite(after) && after > 0 ? Number((before - after).toFixed(4)) : null;

  // The screens the driver walked through, read back from its own log: the
  // provider vocabulary (created / blocked / waiting_input / unknown) is only
  // honest if it is derived from the page text and not from an exit code.
  const terminal = readLastJsonl(path.join(outdir, 'run.jsonl'));
  const logTail = tail(childOut);
  const screens = [];
  let hardReject = null;
  try {
    for (const line of fs.readFileSync(path.join(outdir, 'run.jsonl'), 'utf8').split(/\r?\n/)) {
      if (!line.trim()) continue;
      try {
        const row = JSON.parse(line);
        if (!row) continue;
        if (row.kind === 'step' && /^screen-\d+$/.test(String(row.at || ''))) screens.push(String(row.text || ''));
        // The driver classifies Apple's refusal itself now, in every language it
        // meets; trusting its verdict beats re-reading a translated sentence.
        if (row.kind === 'step' && row.at === 'hard-reject') hardReject = row;
      } catch {}
    }
  } catch {}
  const lastScreen = screens.length ? screens[screens.length - 1] : '';
  const blockedText = Boolean(hardReject) || screens.some((t) => /cannot be created at this time/i.test(t));
  const blockStage = blockedText ? (/verify phone number/i.test(lastScreen) ? 'phone_step' : /verify email/i.test(lastScreen) ? 'email_step' : 'form') : '';
  if (blockedText) state = 'blocked';
  const blockText = blockedText ? 'your_account_cannot_be_created_at_this_time' : '';
  const childLogLines = logTail.trim().split(/\r?\n/);
  const dialLine = childLogLines.reverse().find((l) => l.includes('"at":"dial-code"')) || '';

  const receipts = await cleanup(`run_${state}`);

  // The exit is rented until the 20-minute mark. Cancelling is free and only
  // allowed while no SMS arrived, so the ask comes after the window is closed
  // (cleanup first) and is retried the moment the provider's two-minute
  // minimum has passed (EARLY_CANCEL_DENIED on the first ask, 2026-11-14).
  let refundResult = null;
  if (smsStatus === 'waiting') {
    const ask = async () => grizzly({ action: 'setStatus', status: '8', id: activation }).catch((e) => `ERR:${e.message}`);
    let resp = await ask();
    refundResult = { ok: /ACCESS_CANCEL/.test(resp), response: String(resp).slice(0, 60), attempts: 1 };
    if (!refundResult.ok && /EARLY_CANCEL/i.test(String(resp))) {
      const waitMs = Math.max(0, boughtAt + 150_000 - Date.now());
      await sleep(waitMs);
      resp = await ask();
      refundResult = { ok: /ACCESS_CANCEL/.test(resp), response: String(resp).slice(0, 60), attempts: 2, first: refundResult.response };
    }
    record('refund', { ...refundResult, activation: `${activation.slice(0, 4)}***` });
  } else {
    record('refund', { skipped: smsStatus, activation: `${activation.slice(0, 4)}***` });
    if (smsStatus === 'received' && state === 'failed') state = 'blocked';
  }

  const mailboxSync = mailbox ? syncMailbox(mailbox.id) : null;
  const result = {
    state,
    exit: String(exitCode),
    elapsed_ms: elapsedMs,
    spent_usd: spent,
    balance_after: after,
    dirId,
    core: core.build,
    request_os: os,
    config_os: row ? row.os : null,
    observed: measured.fp ? { platform: measured.fp.platform, ua: String(measured.fp.ua).slice(0, 120), language: measured.fp.language, timeZone: measured.fp.timeZone } : null,
    egress: measured.egress,
    verdict,
    identity: { first: identity.first, last: identity.last, birth: `${identity.month}/${identity.day}/${identity.year}` },
    mailbox: mailbox ? { id: mailbox.id, address: mailbox.address } : null,
    number: { country_id: countryId, cc, dial, activation, activation_prefix: `${activation.slice(0, 4)}***`, phone: `+${dial} ${phoneLocal}`, phone_full: phoneFull, phone_suffix: `***${phoneFull.slice(-3)}`, service, price, sms: smsStatus, bought_at: new Date(boughtAt).toISOString() },
    refund: refundResult,
    dial_code_line: dialLine.slice(0, 200),
    block_text: blockText,
    block_stage: blockStage,
    block_phrase: hardReject ? String(hardReject.phrase || '').slice(0, 160) : '',
    last_screen: lastScreen.slice(0, 300),
    terminal,
    cleanup: receipts,
    mailbox_sync: mailboxSync,
    evidence: { run_jsonl: path.join(outdir, 'run.jsonl'), create_log: childOut, session: sessionFile, captcha_dir: outdir },
  };
  writeSession(result);
  record('result', { state, exit: String(exitCode), spent, elapsed_ms: elapsedMs, sms: smsStatus, refund: refundResult, block_stage: blockStage });
  console.log(JSON.stringify({ group, state, exit: String(exitCode), spent_usd: spent, smsStatus, refund: refundResult, block_stage: blockStage, elapsed_ms: elapsedMs }));
}

main().catch((e) => {
  record('error', { at: 'main', error: String((e && e.message) || e).slice(0, 300) });
  console.error(JSON.stringify({ state: 'error', error: String((e && e.message) || e).slice(0, 200) }));
  process.exit(1);
});
