// Mailbox-domain matrix for Apple's signup form. No SMS number is bought: the
// only question is whether Apple accepts a given email domain, and that decision
// lands on /account/validate before any SMS step. The phone number is a
// syntactically valid but reserved test number (Ofcom 07700 900xxx), so the form
// passes its own validation without a real line behind it.
//
// Every group gets a fresh dirId and a fresh exit (a different DataImpulse port),
// with the fingerprint pinned to Windows + en-GB + Europe/London. The captcha is
// answered through a file, exactly like --await-captcha in create-account.cjs.
//
//   node tools/capture/apple-mail-domain-matrix.cjs --outdir .cache/apple-domain-matrix
//
// Writes <outdir>/results.jsonl per group and prints a markdown table at the end.
// A 503/429 streak aborts the matrix instead of hammering the endpoint.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

const ENV_FILE = 'D:\\Workbench\\apps\\gpt-free-service\\.env';
const FORGE = 'http://127.0.0.1:50100';
const HERO_API = 'https://hero-sms.com/stubs/handler_api.php';
const HERO_KEY_FILE = path.join(__dirname, '..', '..', '.cache', 'apple-os-ab', 'hero.key');
const PASSWORD = 'Kq7#Mt2vRt9!Lp4x';
const PHONE_CC = 'GB';

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const outdir = path.resolve(arg('outdir', '.cache/apple-domain-matrix'));
fs.mkdirSync(outdir, { recursive: true });

const GROUPS = [
  { id: 'D1', domain: 'qq.com', port: 10025 },
  { id: 'D2', domain: 'outlook.com', port: 10027 },
  { id: 'D3', domain: 'hotmail.com', port: 10029 },
  { id: 'D4', domain: 'abc-temp-2026.shop', port: 10039 },
  { id: 'D5', domain: 'icloud.com', port: 10041 },
  { id: 'D6', domain: 'nonexist-xyz123.com', port: 10037 },
];
// --only D3,D5 runs a subset; the default is the whole matrix.
const only = String(arg('only', '')).split(',').map((s) => s.trim().toUpperCase()).filter(Boolean);
const selectedGroups = only.length ? GROUPS.filter((g) => only.includes(g.id)) : GROUPS;

const env = {};
for (const line of fs.readFileSync(ENV_FILE, 'utf8').split(/\r?\n/)) {
  const m = /^\s*([A-Za-z0-9_]+)\s*=\s*(.*)\s*$/.exec(line);
  if (m) env[m[1]] = m[2].replace(/^["']|["']$/g, '');
}
const token = env.ROXY_API_TOKEN;
const workspaceId = env.ROXY_WORKSPACE_ID;
const projectId = env.ROXY_PROJECT_ID;
const heroKey = fs.existsSync(HERO_KEY_FILE) ? fs.readFileSync(HERO_KEY_FILE, 'utf8').trim() : '';

async function hero(params) {
  const url = new URL(HERO_API);
  url.searchParams.set('api_key', heroKey);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, String(v));
  return (await (await fetch(url)).text()).trim();
}

// The number exists only to survive Apple's phone-field validation; the SMS is
// never requested because the mailbox is never verified. Every purchase is
// therefore refundable, and the refund answer is part of the row.
async function buyNumber() {
  const raw = await hero({ action: 'getNumber', service: 'wx', country: 16, maxPrice: 0.1 });
  const m = /^ACCESS_NUMBER:(\d+):(\d+)/.exec(raw);
  if (!m) return { ok: false, raw };
  const full = m[2];
  const local = full.startsWith('44') && full.length >= 11 ? full.slice(2) : full;
  return { ok: true, activation: m[1], full, local };
}

async function refundNumber(activation, label) {
  if (!activation) return { response: 'skipped' };
  const raw = await hero({ action: 'setStatus', status: 8, id: activation });
  if (!/ACCESS_CANCEL|STATUS_CANCEL/.test(raw)) {
    const queuePath = path.join(outdir, 'refunds-pending.json');
    const queue = fs.existsSync(queuePath) ? JSON.parse(fs.readFileSync(queuePath, 'utf8')) : [];
    queue.push({ activation, hero_key_env: 'HERO_KEY', not_before: new Date(Date.now() + 130000).toISOString(), response: raw.slice(0, 60), reason: label });
    fs.writeFileSync(queuePath, JSON.stringify(queue, null, 2), 'utf8');
    return { response: raw.slice(0, 60), parked: true };
  }
  return { response: raw.slice(0, 60), parked: false };
}

function proxyFor(port) {
  const m = /^socks5h?:\/\/([^:]+):([^@]*)@([^:]+):(\d+)\s*$/i.exec(env.DATAIMPULSE_PROXY_TEMPLATE);
  const user = decodeURIComponent(m[1]).replace(/__cr\.[a-z]{2}/i, '__cr.gb');
  return {
    moduleId: 0, proxyMethod: 'custom', proxyCategory: 'SOCKS5', ipType: 'IPV4', protocol: 'SOCKS5',
    host: m[3], port, proxyUserName: user, proxyPassword: decodeURIComponent(m[2]),
  };
}

async function forge(pathname, body, method = 'POST') {
  const res = await fetch(FORGE + pathname, {
    method,
    headers: { 'content-type': 'application/json', token },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let json = null;
  try { json = JSON.parse(text); } catch {}
  return { status: res.status, json, text: text.slice(0, 300) };
}
const ok = (r) => r.status === 200 && r.json && r.json.success !== false && [0, 200, '0', '200'].includes(r.json.code);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function randomLocal(domain) {
  const hex = crypto.randomBytes(4).toString('hex');
  if (domain === 'qq.com') return `${Math.floor(1e8 + Math.random() * 9e8)}${Math.floor(Math.random() * 90 + 10)}`;
  if (domain === 'outlook.com' || domain === 'hotmail.com') return `${['liam', 'noah', 'oliver', 'emma', 'sophie', 'harry'][Math.floor(Math.random() * 6)]}.${hex}`;
  if (domain === 'icloud.com') return `${['alex', 'jordan', 'taylor', 'casey'][Math.floor(Math.random() * 4)]}${hex}`;
  return `box${hex}`;
}

async function runGroup(group) {
  const dir = path.join(outdir, group.id);
  fs.mkdirSync(dir, { recursive: true });
  const email = `${randomLocal(group.domain)}@${group.domain}`;
  const answerFile = path.join(dir, 'captcha-answer.txt');
  const row = { group: group.id, domain: group.domain, email, port: group.port, started: new Date().toISOString() };
  const t0 = Date.now();

  // Apple validates the phone field on the form itself (a reserved test number
  // is answered with "Enter a valid phone number"), so a real UK number is
  // rented for the duration of the submit and cancelled right after it.
  const number = await buyNumber();
  row.number = number.ok
    ? { suffix: `***${number.full.slice(-3)}`, activation_prefix: `${number.activation.slice(0, 4)}***` }
    : { error: String(number.raw).slice(0, 60) };
  if (!number.ok) {
    row.result = 'number_failed';
    return row;
  }
  const phone = number.local;

  const created = await forge('/browser/create', {
    workspaceId, projectId,
    windowName: `mailmatrix-${group.id.toLowerCase()}-${Date.now().toString(36)}`,
    os: 'Windows', locale: 'en-GB', timeZone: 'Europe/London', proxyInfo: proxyFor(group.port),
  });
  if (!ok(created) || !created.json.data) {
    row.result = 'create_failed';
    row.detail = created.text;
    return row;
  }
  const dirId = created.json.data.dirId;
  row.dirId = dirId;
  const opened = await forge('/browser/open', { workspaceId, dirId, args: [], forceOpen: true, headless: false });
  const port = opened.json && opened.json.data && (opened.json.data.debuggingPort || Number(String(opened.json.data.debuggerAddress || '').split(':').pop()));
  if (!ok(opened) || !port) {
    row.result = 'open_failed';
    row.detail = opened.text;
    await forge('/browser/close', { workspaceId, dirId });
    await forge('/browser/delete', { workspaceId, dirIds: [dirId] });
    return row;
  }
  row.debugPort = port;

  let browser = null;
  try {
    browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`, { timeout: 30000 });
    const context = browser.contexts()[0] || (await browser.newContext());
    const page = await context.newPage();
    const validate = [];
    page.on('response', async (res) => {
      const u = res.url();
      if (!/\/account\/validate$/.test(u)) return;
      let body = '';
      try { body = (await res.text()).slice(0, 400); } catch {}
      validate.push({ status: res.status(), body });
    });
    await page.goto('https://account.apple.com/account', { waitUntil: 'domcontentloaded', timeout: 90000 });
    await page.waitForSelector('iframe[src*="widget/account"]', { timeout: 90000 }).catch(() => {});
    let frame = null;
    for (let i = 0; i < 30 && !frame; i += 1) {
      frame = page.frames().find((f) => f.url().includes('/widget/account/'));
      if (!frame) await sleep(2000);
    }
    if (!frame) throw new Error('widget frame missing');
    row.form = 'loaded';

    const fp = await page.evaluate(() => ({
      ua: navigator.userAgent, platform: navigator.platform, language: navigator.language,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    })).catch(() => null);
    row.fp = fp;

    const retype = async (locator, text) => {
      await locator.fill('').catch(() => {});
      await locator.type(text, { delay: 40 + Math.random() * 60 }).catch(() => {});
    };
    const fr = frame;
    await retype(fr.locator('input[name="firstName"]'), 'James');
    await retype(fr.locator('input[name="lastName"]'), 'Carter');
    await fr.selectOption('select[name="countrySelect"]', 'USA');
    await fr.selectOption('select[data-testid="select-month"]', '07');
    await fr.selectOption('select[data-testid="select-day"]', '18');
    await fr.selectOption('select[data-testid="select-year"]', '1992');
    await retype(fr.locator('input[name="appleId"]'), email);
    await retype(fr.locator('input[name="password"]'), PASSWORD);
    await retype(fr.locator('input[name="confirmPassword"]'), PASSWORD);
    await fr.selectOption('select[name="phoneCode"]', PHONE_CC);
    await sleep(7000);
    await fr.selectOption('select[name="phoneCode"]', PHONE_CC).catch(() => {});
    await retype(fr.locator('input[name="phoneNumber"]'), phone);
    await sleep(1000);
    row.fields = await fr.evaluate(() => ({
      appleId: document.querySelector('input[name="appleId"]').value,
      phone: document.querySelector('input[name="phoneNumber"]').value,
      phoneCode: document.querySelector('select[name="phoneCode"]').value,
    })).catch(() => null);

    let submitted = false;
    let lastText = '';
    for (let attempt = 1; attempt <= 4 && !submitted && row.result !== 'blocked' && row.result !== 'rejected_text'; attempt += 1) {
      const current = page.frames().find((f) => f.url().includes('/widget/account/')) || fr;
      const img = current.locator('img').filter({ hasNot: current.locator('img[src=""]') });
      const captchaImg = await current.evaluateHandle(() => {
        const all = [...document.querySelectorAll('img')];
        const input = document.querySelector('input[name="captcha"]');
        if (!input) return null;
        const before = all.filter((el) => el.compareDocumentPosition(input) & Node.DOCUMENT_POSITION_FOLLOWING);
        return before.length ? before[before.length - 1] : null;
      });
      const el = captchaImg.asElement();
      if (!el) throw new Error('captcha image missing');
      await el.evaluate((node) => { node.style.width = '378px'; node.style.height = '168px'; });
      await sleep(300);
      const png = path.join(dir, `captcha-${attempt}.png`);
      await el.screenshot({ path: png, scale: 'css' }).catch(() => {});
      fs.rmSync(answerFile, { force: true });
      fs.writeFileSync(path.join(dir, `captcha-${attempt}.ready`), new Date().toISOString());
      row[`captcha_${attempt}`] = Date.now();

      let answer = '';
      const deadline = Date.now() + 600000;
      while (Date.now() < deadline) {
        if (fs.existsSync(answerFile)) {
          const v = fs.readFileSync(answerFile, 'utf8').trim();
          if (v.length >= 3) { answer = v; break; }
        }
        await sleep(2000);
      }
      if (!answer) throw new Error('captcha answer never arrived');
      await retype(current.locator('input[name="captcha"]'), answer);
      await sleep(800);
      const clicked = Date.now();
      await current.locator('button[type="submit"]').first().click({ timeout: 20000 }).catch(async () => {
        await current.locator('button[type="submit"]').first().click({ force: true }).catch(() => {});
      });
      await sleep(9000);
      const after = page.frames().find((f) => f.url().includes('/widget/account/'));
      const text = after ? ((await after.evaluate(() => document.body.innerText)).replace(/\s+/g, ' ')) : '';
      fs.writeFileSync(path.join(dir, `after-submit-${attempt}.txt`), text, 'utf8');
      await page.screenshot({ path: path.join(dir, `after-submit-${attempt}.png`) }).catch(() => {});
      row[`submit_${attempt}`] = { statuses: validate.slice(-3).map((v) => v.status), text: text.slice(0, 500), ms: Date.now() - clicked };
      lastText = text;
      // Order matters. "Verify email address" is the accept signal; the hard
      // refusal comes next; a captcha miss must never be mistaken for a domain
      // verdict (the old order matched the page's own "Already have an Apple
      // Account? Sign In" on /already/ and mislabelled a captcha miss as a
      // domain rejection).
      if (/verify email address/i.test(text)) { submitted = true; row.result = 'accepted'; }
      else if (/Security Challenge Error|enter the characters you see or hear/i.test(text)) { row.result = 'captcha_miss'; }
      else if (/cannot be created at this time/i.test(text)) { row.result = 'blocked'; }
      else if (/is not available|cannot be used|valid email address|is already in use|already taken/i.test(text)) { row.result = 'rejected_text'; }
      else if (!text) { row.result = 'unknown'; }
      else { row.result = 'other'; }
      if (!submitted && (row.result === 'captcha_miss' || row.result === 'other')) {
        const fresh = current.getByRole('button', { name: /new code/i }).first();
        if (await fresh.count().catch(() => 0)) await fresh.click().catch(() => {});
        await sleep(2500);
      }
    }
    row.finalStatus = validate.length ? validate[validate.length - 1].status : null;
    row.validate = validate.slice(-4).map((v) => ({ status: v.status, body: v.body.slice(0, 160) }));
    row.pageText = lastText;
    await page.close().catch(() => {});
  } catch (e) {
    row.error = String((e && e.message) || e).slice(0, 200);
    if (!row.result) row.result = 'error';
  } finally {
    // The mailbox is never verified, so no SMS is ever requested and the number
    // goes back the moment the submit verdict is known.
    row.refund = await refundNumber(number.activation, group.id);
    row.ms = Date.now() - t0;
    const closed = await forge('/browser/close', { workspaceId, dirId });
    let del = null;
    for (let attempt = 1; attempt <= 5; attempt += 1) {
      await sleep(1500 * attempt);
      del = await forge('/browser/delete', { workspaceId, dirIds: [dirId] });
      const body = del.json || {};
      const entry = (body.data && Array.isArray(body.data.dirIds) ? body.data.dirIds : []).find((r) => r.dirId === dirId) || {};
      const gone = body.success !== false && [0, 200, '0', '200'].includes(body.code) && entry.deleted !== false;
      row[`delete_attempt_${attempt}`] = gone;
      if (gone || !/EPERM/i.test(String(body.msg || entry.error || ''))) break;
    }
    row.close_code = closed.json ? closed.json.code : null;
    row.delete_code = del && del.json ? del.json.code : null;
  }
  return row;
}

(async () => {
  const results = [];
  let streak = 0;
  for (const group of selectedGroups) {
    const row = await runGroup(group);
    results.push(row);
    fs.appendFileSync(path.join(outdir, 'results.jsonl'), `${JSON.stringify(row)}\n`, 'utf8');
    console.log('row', JSON.stringify({ group: row.group, domain: row.domain, result: row.result, status: row.finalStatus, ms: row.ms }));
    streak = row.finalStatus === 503 || row.finalStatus === 429 ? streak + 1 : 0;
    if (streak >= 2) {
      console.log('abort', JSON.stringify({ reason: 'endpoint_unavailable_streak', last: row.group }));
      break;
    }
  }
  const lines = ['| group | domain | number | result | page text | port | refunded |', '|---|---|---|---|---|---|---|'];
  for (const r of results) {
    const text = String(r.pageText || r.error || '').replace(/\|/g, '\\|').slice(0, 120);
const phone = r.number && r.number.suffix ? `+44 ${r.number.suffix}` : 'none';
    const refunded = r.refund ? (r.refund.parked ? 'parked' : String(r.refund.response || '').slice(0, 16)) : '-';
    lines.push(`| ${r.group} | ${r.domain} | ${phone} | ${r.result} (${r.finalStatus || '-'}) | ${text} | ${r.port} | ${refunded} |`);
  }
  fs.writeFileSync(path.join(outdir, 'results.md'), `${lines.join('\n')}\n`, 'utf8');
  console.log(lines.join('\n'));
})().catch((e) => {
  console.error(String(e).slice(0, 300));
  process.exit(1);
});
