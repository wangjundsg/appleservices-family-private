// One variable, two windows: an OS A/B for Apple Account creation on Roxy/Forge.
//
// Group A: os=macOS   Group B: os=Windows
// Everything else is pinned by both groups: locale en-GB, timeZone Europe/London,
// a UK DataImpulse SOCKS5 exit, a UK hero-sms number, a fresh dirId and a fresh
// window name. The session layer follows docs/REGISTRAR-CONTRACT.md: every step
// is judged on the business code (code==0 && success), the proxy is an explicit
// nested SOCKS5 proxyInfo, close and delete are separate and their receipts are
// kept, and the fingerprint is measured inside the window instead of trusted
// from the create reply.
//
// Usage:
//   node tools/capture/apple-os-ab.cjs --group A --os macOS \
//     --email rouugyylzw@outlook.com --email-mode webmail \
//     --password-file .cache/apple-os-ab/A/password.txt \
//     [--hero-key K] [--yescaptcha-key K] [--no-number] [--session-only]
//
// --session-only stops after the fingerprint verdict, before a number is bought.
// No secret is printed: the token, the proxy password, the account password and
// the mailbox password never reach stdout, session.json or the console.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

const REPO = 'D:\\appleservices-family-private';
const ENV_FILE = 'D:\\Workbench\\apps\\gpt-free-service\\.env';
const FORGE = 'http://127.0.0.1:50100';
const HERO_API = 'https://hero-sms.com/stubs/handler_api.php';

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const flag = (name) => argv.includes(`--${name}`);

const group = String(arg('group', 'A')).toUpperCase();
const os = String(arg('os', group === 'A' ? 'macOS' : 'Windows'));
const outdir = path.resolve(arg('outdir', `.cache/apple-os-ab/${group}`));
const email = String(arg('email', ''));
const emailMode = String(arg('email-mode', 'webmail'));
const passwordFile = String(arg('password-file', path.join(outdir, 'password.txt')));
const heroKeyFile = String(arg('hero-key-file', ''));
const heroKey = String(arg('hero-key', process.env.HERO_KEY || '')) || (heroKeyFile && fs.existsSync(heroKeyFile) ? fs.readFileSync(heroKeyFile, 'utf8').trim() : '');
const yescaptchaKey = String(arg('yescaptcha-key', process.env.YESCAPTCHA_KEY || ''));
const numberCountry = String(arg('number-country', '16'));
const numberService = String(arg('number-service', 'wx'));
const numberMaxPrice = String(arg('number-max-price', '0.1'));
const locale = 'en-GB';
const timeZone = 'Europe/London';
const proxyCountry = String(arg('proxy-country', 'gb')).toLowerCase();
const proxyPortOverride = Number(arg('proxy-port', '0')) || 0;
const DIRTY_PORTS = new Set([10013, 10030, 10033]);
const graphTokenFile = String(arg('graph-token', ''));

// Identity is the one thing that is supposed to differ between two runs even
// when the OS is the variable under test: a form filled with the same name and
// birthday twice is a different experiment. Names come from a common Anglo pool,
// the birthday is uniform over 1920..2000 with a real day count for its month.
const FIRST_NAMES = [
  'James', 'Oliver', 'Harry', 'Jack', 'Charlie', 'Thomas', 'George', 'William',
  'Henry', 'Alfie', 'Freddie', 'Arthur', 'Amelia', 'Olivia', 'Emily', 'Sophie',
  'Grace', 'Chloe', 'Lucy', 'Ella', 'Megan', 'Hannah', 'Katie', 'Laura',
];
const LAST_NAMES = [
  'Smith', 'Jones', 'Taylor', 'Brown', 'Wilson', 'Davies', 'Evans', 'Thomas',
  'Roberts', 'Walker', 'Wright', 'Robinson', 'Thompson', 'White', 'Hughes',
  'Edwards', 'Green', 'Hall', 'Wood', 'Harris', 'Clarke', 'Baker', 'Cooper', 'Ward',
];
function randomIdentity() {
  const pick = (list) => list[Math.floor(Math.random() * list.length)];
  const first = String(arg('first', '')) || pick(FIRST_NAMES);
  const last = String(arg('last', '')) || pick(LAST_NAMES);
  const year = Number(arg('birth-year', '0')) || 1920 + Math.floor(Math.random() * (2000 - 1920 + 1));
  const month = Number(arg('birth-month', '0')) || 1 + Math.floor(Math.random() * 12);
  const daysInMonth = new Date(Date.UTC(year, month, 0)).getUTCDate();
  const day = Number(arg('birth-day', '0')) || 1 + Math.floor(Math.random() * daysInMonth);
  return {
    first,
    last,
    // Zero-padded in the same shape the form's selects carry.
    month: String(month).padStart(2, '0'),
    day: String(day).padStart(2, '0'),
    year: String(year),
  };
}

fs.mkdirSync(outdir, { recursive: true });

const sessionFile = path.join(outdir, 'session.json');
const logFile = path.join(outdir, 'session.jsonl');
const log = [];
function record(kind, data) {
  const row = { t: Date.now(), at: new Date().toISOString(), group, kind, ...data };
  log.push(row);
  try {
    fs.appendFileSync(logFile, `${JSON.stringify(row)}\n`, 'utf8');
  } catch {}
  if (['error', 'verdict', 'step'].includes(kind)) {
    console.log(kind, JSON.stringify(data).slice(0, 300));
  }
  return row;
}
const writeSession = (extra) => {
  const payload = { group, os, locale, timeZone, outdir, ...extra };
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

// proxyInfoFromTemplate turns the workspace's DataImpulse template into the
// nested object Forge expects. Only the country segment is rewritten; the
// session part (sessttl) and the port are preserved, because the port is what
// makes the exit sticky for one run. HTTP is not an option here: Forge does not
// send Proxy-Authorization for it and every request comes back 407.
function proxyInfoFromTemplate(template, country, portOverride) {
  const m = /^socks5h?:\/\/([^:]+):([^@]*)@([^:]+):(\d+)\s*$/i.exec(String(template || '').trim());
  if (!m) throw new Error('proxy_template_unparseable');
  const user = decodeURIComponent(m[1]);
  const pass = decodeURIComponent(m[2]);
  const host = m[3];
  let port = portOverride || Number(m[4]);
  let finalUser;
  if (/__cr\.[a-z]{2}/i.test(user)) finalUser = user.replace(/__cr\.[a-z]{2}/i, `__cr.${country}`);
  else if (/__cr\.[a-z]{2}/i.test(decodeURIComponent(m[1]))) finalUser = user;
  else finalUser = `${user}__cr.${country}`;
  if (DIRTY_PORTS.has(port)) port = 10001;
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
    // A mask for the log: the user (which carries the provider account) and the
    // password are both cut down to their country segment / length.
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

const businessOk = (r) =>
  r.status === 200 && r.json && r.json.success !== false && [0, 200, '0', '200'].includes(r.json.code);

async function hero(params) {
  const url = new URL(HERO_API);
  url.searchParams.set('api_key', heroKey);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, String(v));
  const res = await fetch(url, { timeout: 30000 });
  return (await res.text()).trim();
}

// measureInWindow attaches over CDP, reads what Apple would see from the same
// page the job will use, and disconnects. browser.close() on a CDP attach only
// drops this client (docs/AUTOMATION-BASELINE.md 搂3): the window stays up for
// the creation driver.
async function measureInWindow(port) {
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`, { timeout: 30000 });
  try {
    const context = browser.contexts()[0] || (await browser.newContext());
    const page = await context.newPage();
    // ip-api answers `{"status":"fail"}` on a shared residential exit often
    // enough (quota or reserved range) that one endpoint is not a measurement.
    // Each endpoint is asked in turn and the first that reports a country wins;
    // every raw answer is kept so a failure is diagnosable, not just red.
    const probes = [
      { name: 'ipinfo', url: 'https://ipinfo.io/json' },
      { name: 'ipapi', url: 'https://ipapi.co/json/' },
      { name: 'ip-api', url: 'http://ip-api.com/json/?fields=status,country,countryCode,city,isp,query' },
      { name: 'ifconfig', url: 'https://ifconfig.co/json' },
    ];
    const attempts = [];
    let egress = null;
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

function osVerdict(fp, egress) {
  const ua = String((fp && fp.ua) || '');
  const platform = String((fp && fp.platform) || '');
  const lang = String((fp && fp.language) || '');
  const tz = String((fp && fp.timeZone) || '');
  const cc = String((egress && egress.countryCode) || '').toUpperCase();
  const wantPlatform = os.toLowerCase().startsWith('mac') ? 'MacIntel' : 'Win32';
  const wantUa = os.toLowerCase().startsWith('mac') ? 'Macintosh; Intel Mac OS X' : 'Windows NT 10.0';
  const checks = {
    ua_os: ua.includes(wantUa),
    platform: platform === wantPlatform,
    locale: lang === locale,
    languages_head: Array.isArray(fp && fp.languages) && fp.languages[0] === locale,
    timezone: tz === timeZone,
    egress_country: cc === proxyCountry.toUpperCase(),
  };
  const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([k]) => k);
  return { checks, failed, ok: failed.length === 0, countryCode: cc };
}

function readPassword(file) {
  try {
    const raw = fs.readFileSync(file, 'utf8').trim();
    return raw;
  } catch {
    return '';
  }
}
// A password Apple accepts must carry all four classes; one missing special
// character silently disables the submit button (run-20261114-run2 搂2, A-1).
function ensurePassword(file) {
  const existing = readPassword(file);
  if (existing && existing.length >= 12) return existing;
  const upper = 'ABCDEFGHJKLMNPQRSTUVWXYZ';
  const lower = 'abcdefghijkmnopqrstuvwxyz';
  const digits = '23456789';
  const special = '!@#$%^&*-_=+?';
  const pick = (s) => s[Math.floor(Math.random() * s.length)];
  const base = [pick(upper), pick(lower), pick(digits), pick(special)];
  const pool = upper + lower + digits + special;
  while (base.length < 16) base.push(pick(pool));
  for (let i = base.length - 1; i > 0; i -= 1) {
    const j = Math.floor(Math.random() * (i + 1));
    [base[i], base[j]] = [base[j], base[i]];
  }
  const password = base.join('');
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, `${password}\n`, { mode: 0o600 });
  return password;
}

async function main() {
  const env = loadEnv();
  const token = env.ROXY_API_TOKEN || process.env.ROXY_API_TOKEN || '';
  const workspaceId = env.ROXY_WORKSPACE_ID || process.env.ROXY_WORKSPACE_ID || '';
  const projectId = env.ROXY_PROJECT_ID || process.env.ROXY_PROJECT_ID || '';
  if (!token || !workspaceId || !projectId) throw new Error('roxy_credentials_missing');
  const { proxyInfo, mask } = proxyInfoFromTemplate(env.DATAIMPULSE_PROXY_TEMPLATE, proxyCountry, proxyPortOverride);
  const password = ensurePassword(path.resolve(passwordFile));

  const health = await forge('/health', { method: 'GET' });
  const core = health.json && health.json.data && health.json.data.core ? health.json.data.core : null;
  record('step', {
    at: 'forge-health',
    ok: Boolean(health.json && health.json.data && health.json.data.ok),
    auth: health.json && health.json.data ? health.json.data.authEnabled : null,
    core: core ? core.build : null,
    core_pinned: core ? core.pinned : null,
    core_drift: core && Array.isArray(core.drift) ? core.drift.map((d) => d.build) : null,
  });
  // A run whose core is not pinned cannot be compared with its twin: the drift
  // list means the box holds several builds and would otherwise hand each window
  // whatever the last install left behind.
  if (!core || !core.pinned) {
    writeSession({ state: 'core_unpinned', core });
    throw new Error('core_not_pinned: set FORGE_CORE_PIN on the forge-api process before an A/B run');
  }
  const identity = randomIdentity();

  const windowName = `apple-ab-${group.toLowerCase()}-${Date.now().toString(36)}`;
  const created = await forge('/browser/create', {
    token,
    body: {
      workspaceId,
      projectId,
      windowName,
      os,
      locale,
      timeZone,
      proxyInfo,
    },
  });
  const createOk = businessOk(created) && created.json.data && created.json.data.dirId;
  record('step', {
    at: 'create',
    request_os: os,
    locale,
    timeZone,
    core: core ? core.build : null,
    core_pinned: core ? core.pinned : null,
    identity: { first: identity.first, last: identity.last, birth: `${identity.month}/${identity.day}/${identity.year}` },
    proxy: mask,
    code: created.json ? created.json.code : null,
    success: created.json ? created.json.success : null,
    dirId: createOk ? created.json.data.dirId : null,
    fingerprint: createOk ? created.json.data.fingerprint || null : null,
    raw: createOk ? undefined : created.text,
  });
  if (!createOk) {
    writeSession({ state: 'create_failed', detail: created.text });
    process.exit(1);
  }
  const dirId = created.json.data.dirId;

  const opened = await forge('/browser/open', {
    token,
    body: { workspaceId, dirId, args: [], forceOpen: true, headless: false },
  });
  const data = opened.json && opened.json.data;
  const debugPort = Number(data && (data.debuggingPort || (data.debuggerAddress ? String(data.debuggerAddress).split(':').pop() : 0)));
  record('step', {
    at: 'open',
    code: opened.json ? opened.json.code : null,
    success: opened.json ? opened.json.success : null,
    debugPort: Number.isFinite(debugPort) ? debugPort : null,
    alreadyRunning: data ? Boolean(data.alreadyRunning) : null,
    raw: businessOk(opened) && debugPort ? undefined : opened.text,
  });
  if (!businessOk(opened) || !debugPort) {
    writeSession({ state: 'open_failed', dirId, detail: opened.text });
    process.exit(1);
  }

  // Configured OS, read back from the service rather than from the create reply.
  const detail = await forge('/browser/detail', {
    method: 'GET',
    token,
    query: { workspaceId, dirId },
  });
  const row = detail.json && detail.json.data && Array.isArray(detail.json.data.rows) ? detail.json.data.rows.find((r) => r.dirId === dirId) : null;
  record('step', {
    at: 'detail',
    found: Boolean(row),
    os: row ? row.os : null,
    windowName: row ? row.windowName : null,
    running: row ? row.running : null,
  });

  const measured = await measureInWindow(debugPort);
  const verdict = osVerdict(measured.fp, measured.egress);
  record('verdict', {
    request_os: os,
    config_os: row ? row.os : null,
    observed_platform: measured.fp ? measured.fp.platform : null,
    observed_ua: measured.fp ? String(measured.fp.ua).slice(0, 140) : null,
    language: measured.fp ? measured.fp.language : null,
    timeZone: measured.fp ? measured.fp.timeZone : null,
    egress_country: verdict.countryCode,
    egress_ip: measured.egress && measured.egress.ip ? measured.egress.ip : null,
    checks: verdict.checks,
    failed: verdict.failed,
    ok: verdict.ok,
  });

  // A window whose fingerprint or exit does not match the two pinned values is
  // closed and deleted before a number is bought: spending here only dirties a
  // number (REGISTRAR-CONTRACT 搂1.4).
  const cleanup = async (reason) => {
    const closed = await forge('/browser/close', { token, body: { workspaceId, dirId } });
    record('step', {
      at: 'close',
      reason,
      code: closed.json ? closed.json.code : null,
      success: closed.json ? closed.json.success : null,
      ok: businessOk(closed),
    });
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
      record('step', {
        at: 'delete',
        attempt: attempts,
        code: body.code === undefined ? null : body.code,
        success: body.success === undefined ? null : body.success,
        deleted: entry.deleted === undefined ? null : entry.deleted,
        msg: String(body.msg || entry.error || '').slice(0, 160),
        ok,
      });
      if (ok) return { closed: businessOk(closed), deleted: true, attempts };
      if (!eperm) return { closed: businessOk(closed), deleted: false, attempts };
    }
    return { closed: businessOk(closed), deleted: false, attempts };
  };

  if (!verdict.ok) {
    const receipts = await cleanup('geo_mismatch');
    writeSession({
      state: 'geo_mismatch',
      dirId,
      debugPort,
      request_os: os,
      config_os: row ? row.os : null,
      observed: measured.fp,
      egress: measured.egress,
      egress_attempts: measured.egressAttempts,
      verdict,
      cleanup: receipts,
      spent: 0,
    });
    process.exit(2);
  }

  if (flag('session-only') || flag('no-number')) {
    const receipts = await cleanup('session-only');
    writeSession({
      state: 'session_ok',
      dirId,
      debugPort,
      request_os: os,
      config_os: row ? row.os : null,
      observed: measured.fp,
      egress: measured.egress,
      egress_attempts: measured.egressAttempts,
      verdict,
      cleanup: receipts,
      spent: 0,
    });
    process.exit(0);
  }

  if (!heroKey) throw new Error('hero_key_missing');

  // Cost accounting: hero reports the balance, so the spend of this run is the
  // difference around the purchase rather than a price list we assumed.
  const balanceBefore = await hero({ action: 'getBalance' });
  const bought = await hero({
    action: 'getNumber',
    service: numberService,
    country: numberCountry,
    maxPrice: numberMaxPrice,
  });
  const numberMatch = /^ACCESS_NUMBER:(\d+):(\d+)/.exec(bought);
  record('step', {
    at: 'number',
    balance_before: balanceBefore.startsWith('ACCESS_BALANCE') ? balanceBefore.split(':')[1] : balanceBefore.slice(0, 40),
    bought: numberMatch ? 'ACCESS_NUMBER' : bought.slice(0, 60),
    activation_prefix: numberMatch ? `${numberMatch[1].slice(0, 4)}***` : null,
    phone_suffix: numberMatch ? `***${String(numberMatch[2]).slice(-3)}` : null,
  });
  if (!numberMatch) {
    const receipts = await cleanup('number_failed');
    writeSession({ state: 'number_failed', dirId, detail: bought.slice(0, 80), verdict, cleanup: receipts, spent: 0 });
    process.exit(3);
  }
  const activation = numberMatch[1];
  const phoneFull = numberMatch[2];
  const phoneLocal = phoneFull.startsWith('44') && phoneFull.length >= 11 ? phoneFull.slice(2) : phoneFull;

  const captchaFile = path.join(outdir, 'captcha-answer.txt');
  const emailCodeFile = path.join(outdir, 'apple-code.txt');
  const childOut = path.join(outdir, 'create-account.log');
  const args = [
    path.join(REPO, 'tools', 'capture', 'create-account.cjs'),
    '--email', email,
    '--password', password,
    '--phone', phoneLocal,
    '--activation', activation,
    '--hero-key', heroKey,
    '--outdir', outdir,
    '--await-captcha', captchaFile,
    '--phonecc', 'GB',
    '--cdp-port', String(debugPort),
    '--auto-refund',
    // Identity: randomised per group, recorded here and in the child's run.jsonl.
    '--first', identity.first,
    '--last', identity.last,
    '--birth-month', identity.month,
    '--birth-day', identity.day,
    '--birth-year', identity.year,
  ];
  if (emailMode === 'manual') args.push('--await-email-code', emailCodeFile);
  if (emailMode === 'webmail') args.push('--await-email-code', emailCodeFile);
  // graph: the driver reads the code itself through Graph, so neither a sidecar
  // reader nor a human is in the loop.
  if (emailMode === 'graph') {
    if (!graphTokenFile) throw new Error('graph_token_missing (--graph-token)');
    args.push('--graph-token-file', path.resolve(graphTokenFile));
  }

  // The mail reader keeps the logged-in Outlook session that already exists in
  // .cache/apple-run2; a new outdir would ask for a password we do not type.
  let mailReader = null;
  if (emailMode === 'webmail') {
    mailReader = spawn(process.execPath, [
      path.join(REPO, 'tools', 'capture', 'outlook-web-mail.cjs'),
      '--outdir', '.cache/apple-run2',
      '--write', emailCodeFile,
      '--minutes', '30',
    ], { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: false });
    const mailLog = fs.createWriteStream(path.join(outdir, 'mail-reader.log'), { flags: 'a' });
    mailReader.stdout.pipe(mailLog);
    mailReader.stderr.pipe(mailLog);
  }

  const startedAt = Date.now();
  const child = spawn(process.execPath, args, { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: false });
  const childLog = fs.createWriteStream(childOut, { flags: 'a' });
  child.stdout.pipe(childLog);
  child.stderr.pipe(childLog);

  const exitCode = await new Promise((resolve) => {
    const timer = setTimeout(() => {
      record('error', { at: 'child-timeout', ms: Date.now() - startedAt });
      try {
        child.kill();
      } catch {}
      resolve('timeout');
    }, 27 * 60 * 1000);
    child.on('exit', (code) => {
      clearTimeout(timer);
      resolve(code);
    });
  });
  const elapsedMs = Date.now() - startedAt;
  if (mailReader) {
    try {
      mailReader.kill();
    } catch {}
  }

  const stateMap = { 0: 'created', 1: 'failed', 2: 'usage', 3: 'unknown', 4: 'waiting_input', 5: 'likely_success', timeout: 'timeout' };
  const state = stateMap[String(exitCode)] || `exit_${exitCode}`;
  const balanceAfter = await hero({ action: 'getBalance' }).catch(() => '');
  const before = Number((balanceBefore.split(':')[1] || '0'));
  const after = Number((balanceAfter.split(':')[1] || '0'));
  const spent = Number.isFinite(before) && Number.isFinite(after) && after > 0 ? Number((before - after).toFixed(4)) : null;

  record('step', { at: 'run-end', exit: String(exitCode), state, elapsed_ms: elapsedMs, spent_usd: spent });

  const receipts = await cleanup(`run_${state}`);
  writeSession({
    state,
    exit: String(exitCode),
    dirId,
    debugPort,
    request_os: os,
    config_os: row ? row.os : null,
    observed: measured.fp,
    egress: measured.egress,
      egress_attempts: measured.egressAttempts,
    verdict,
    core: core ? { build: core.build, pinned: core.pinned, drift: core.drift } : null,
    identity: { first: identity.first, last: identity.last, birth: `${identity.month}/${identity.day}/${identity.year}` },
    email: { address: email, mode: emailMode },
    number: { service: numberService, country: numberCountry, activation_prefix: `${activation.slice(0, 4)}***`, phone_suffix: `***${phoneFull.slice(-3)}` },
    elapsed_ms: elapsedMs,
    spent_usd: spent,
    cleanup: receipts,
    evidence: {
      run_jsonl: path.join(outdir, 'run.jsonl'),
      create_log: childOut,
      roxy_fingerprint: path.join(outdir, 'roxy-fingerprint.json'),
      captcha_dir: outdir,
    },
  });
  console.log(JSON.stringify({ state, exit: String(exitCode), spent_usd: spent, dirId, cleanup: receipts }));
}

main().catch((e) => {
  record('error', { at: 'main', error: String((e && e.message) || e).slice(0, 300) });
  console.error(JSON.stringify({ state: 'error', error: String((e && e.message) || e).slice(0, 200) }));
  process.exit(1);
});
