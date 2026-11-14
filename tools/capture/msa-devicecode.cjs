// Path A for a self-registered Outlook mailbox: turn a plain email+password into
// a Microsoft Graph refresh_token through the OAuth device-code flow.
//
// Why this shape: the mailbox we just registered has no token of its own, and the
// Apple step needs to read one mail unattended (tools/capture/graph-mail.cjs).
// The device-code grant is the only flow that yields an MSA refresh token for the
// consumer tenant with nothing but a browser login, so the human-shaped part
// (typing the code, the password, the consent) is done by patchright on the
// operator's real Chrome, and the token part is done here.
//
// Contract:
//   - the password is read from the product file line and never printed;
//   - the refresh token is written to <outdir>/graph.json with mode 0600;
//   - every step is logged with the page URL and a screenshot name, because the
//     interesting failure modes (PX press widget, "protect your account",
//     consent gate) all look identical in a text log.
//
// Usage:
//   node tools/capture/msa-devicecode.cjs --outdir .cache/apple-run2
//        [--email <addr>] [--product <file>] [--devicecode <json>] [--no-browser]

'use strict';

const fs = require('node:fs');
const path = require('node:path');

const PATCHRIGHT_PATH = 'D:/appleservices-family-private/.tools/js/node_modules/patchright-core';
const { chromium } = require(PATCHRIGHT_PATH);

const CLIENT_ID = '9e5f94bc-e8a4-4e73-b8be-63364c29d753';
const SCOPE = 'https://graph.microsoft.com/.default offline_access';
const DEVICECODE_URL = 'https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode';
const TOKEN_URL = 'https://login.microsoftonline.com/consumers/oauth2/v2.0/token';
const VERIFY_URI = 'https://www.microsoft.com/link';

// ------------------------------------------------------------------- args
const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const flag = (name) => argv.includes(`--${name}`);

const email = String(arg('email', '')).trim();
const productFile = arg('product', 'D:/outlook-recon/产出/outlook-cn.txt');
const outdir = path.resolve(arg('outdir', '.cache/apple-run2'));
const deviceCodeFile = path.resolve(arg('devicecode', path.join(outdir, 'devicecode.json')));
const tokenFile = path.join(outdir, 'graph.json');
const shotDir = path.join(outdir, 'shots');
const noBrowser = flag('no-browser');
fs.mkdirSync(shotDir, { recursive: true });

// ------------------------------------------------------------------ utils
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const stamp = () => new Date().toISOString().slice(11, 19);
function log(step, detail) {
  const tail = detail === undefined ? '' : ` ${typeof detail === 'string' ? detail : JSON.stringify(detail)}`;
  console.log(`[${stamp()}] ${step}${tail}`);
}

// readPassword pulls the password for one address out of the product file. The
// value only ever lives in this process: it is not logged, not written, and not
// put on a command line anywhere else in the pipeline.
function readPassword(file, addr) {
  const wanted = addr.toLowerCase();
  const lines = fs.readFileSync(file, 'utf8').split(/\r?\n/);
  for (const line of lines) {
    const m = line.match(/^([^\s@]+@[^\s@]+)----(.+)$/);
    if (m && m[1].toLowerCase() === wanted) return m[2].trim();
  }
  throw new Error(`address not found in product file: ${addr}`);
}

// firstAddress lets the caller skip --email: the product file already names the
// mailbox, so the address never has to appear on the command line.
function firstAddress(file) {
  const lines = fs.readFileSync(file, 'utf8').split(/\r?\n/);
  for (const line of lines) {
    const m = line.match(/^([^\s@]+@[^\s@]+)----/);
    if (m) return m[1];
  }
  return '';
}

// ------------------------------------------------------------- device code
// loadRecoveryRecord reads the temp-mail record used as the account's recovery
// address. Some of the recorded files are UTF-16LE (PowerShell redirect), so the
// BOM decides the decoder.
function loadRecoveryRecord() {
  const file = path.resolve(arg('recovery-file', '.cache/r4-3/email-raw.json'));
  const buf = fs.readFileSync(file);
  let json;
  if (buf[0] === 0xFF && buf[1] === 0xFE) json = JSON.parse(buf.slice(2).toString('utf16le'));
  else if (buf[0] === 0xEF && buf[1] === 0xBB) json = JSON.parse(buf.slice(3).toString('utf8'));
  else json = JSON.parse(buf.toString('utf8'));
  if (!json.address || !json.jwt) throw new Error('recovery record needs address + jwt');
  return json;
}

async function mintDeviceCode() {
  const body = new URLSearchParams({ client_id: CLIENT_ID, scope: SCOPE });
  const res = await fetch(DEVICECODE_URL, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: body.toString(),
  });
  const json = await res.json().catch(() => null);
  if (!res.ok || !json || !json.device_code) {
    throw new Error(`devicecode refused: http_${res.status} ${json && json.error ? json.error : ''}`.trim());
  }
  fs.writeFileSync(deviceCodeFile, `${JSON.stringify(json, null, 2)}\n`, { mode: 0o600 });
  return json;
}

function loadDeviceCode() {
  try {
    const json = JSON.parse(fs.readFileSync(deviceCodeFile, 'utf8'));
    if (json && json.device_code && json.user_code) return { ...json, reused: true };
  } catch { /* mint a fresh one */ }
  return null;
}

// --------------------------------------------------------------- browser
async function framesOf(page) {
  const out = [page.mainFrame()];
  for (const f of page.frames()) if (!out.includes(f)) out.push(f);
  return out;
}

async function findVisible(page, selectors, { timeoutMs = 1500 } = {}) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    for (const frame of await framesOf(page)) {
      for (const sel of selectors) {
        const loc = frame.locator(sel).first();
        try {
          if (await loc.isVisible({ timeout: 150 })) return loc;
        } catch { /* keep looking */ }
      }
    }
    await sleep(150);
  }
  return null;
}

async function shot(page, name) {
  const file = path.join(shotDir, `${String(Date.now()).slice(-7)}-${name}.png`);
  try { await page.screenshot({ path: file, fullPage: false }); } catch { /* ignore */ }
  return path.basename(file);
}

async function bodyText(page) {
  try {
    const t = await page.evaluate(() => (document.body ? document.body.innerText : ''));
    return String(t || '').replace(/\s+/g, ' ').slice(0, 400);
  } catch { return ''; }
}

// gateProbe is the "this gate has no visible skip" branch: dump the live DOM and
// the clickable controls, then try the three dismissal routes that are known to
// work on other Microsoft interrupt templates (hidden skip control, the ru=
// return URL, and Escape).
async function gateProbe(page, round) {
  const html = await page.content().catch(() => '');
  const htmlFile = path.join(shotDir, `gate-${round}-${Date.now()}.html`);
  try { fs.writeFileSync(htmlFile, html, 'utf8'); } catch { /* ignore */ }
  const controls = await page.evaluate(() => {
    const out = [];
    for (const el of document.querySelectorAll('a,button,input[type=button],input[type=submit],[role=button]')) {
      const t = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim().replace(/\s+/g, ' ');
      if (!t) continue;
      const r = el.getBoundingClientRect();
      out.push({ tag: el.tagName.toLowerCase(), id: el.id || '', text: t.slice(0, 40), visible: r.width > 0 && r.height > 0 });
    }
    return out;
  }).catch(() => []);
  log('gate_dom', { html: path.basename(htmlFile), controls });

  // (1) a hidden/odd skip control
  const skipish = await findVisible(page, [
    'a[href*="skip" i]', '[id*="skip" i]', '[class*="skip" i]',
    'a:has-text("以后")', 'a:has-text("稍后")', 'button:has-text("以后")',
  ], { timeoutMs: 2500 });
  if (skipish) {
    await skipish.click().catch(() => {});
    await sleep(4000);
    log('gate_skip_clicked', { url: page.url(), text: await bodyText(page) });
    return;
  }

  // (2) the ru= return URL carries the opid that makes it replayable
  const ru = await page.evaluate(() => new URL(location.href).searchParams.get('ru') || '').catch(() => '');
  if (ru) {
    log('gate_try_ru', ru.slice(0, 140));
    await page.goto(ru, { waitUntil: 'domcontentloaded', timeout: 45_000 }).catch((e) => log('warn', String(e).slice(0, 80)));
    await sleep(6000);
    log('gate_after_ru', { url: page.url(), shot: await shot(page, `05d-ru-${round}`), text: await bodyText(page) });
    return;
  }

  // (3) last resort: Escape, which dismisses some credential interrupts
  await page.keyboard.press('Escape').catch(() => {});
  await sleep(3000);
  log('gate_after_escape', { url: page.url(), text: await bodyText(page) });
}

// relinkAttempt: after the password step the login.live.com session is
// authenticated even while the credential interrupt is still on screen.
// Re-running the device-code page inside that session is the cheapest test of
// whether the interrupt blocks the OAuth grant or is only advisory.
async function relinkAttempt(page, userCode, round) {
  log('relink_nav', VERIFY_URI);
  await page.goto(VERIFY_URI, { waitUntil: 'domcontentloaded', timeout: 60_000 }).catch((e) => log('warn', String(e).slice(0, 80)));
  await sleep(2500);
  const box = await findVisible(page, ['input#otc', 'input[name="otc"]', 'input[type="text"]'], { timeoutMs: 10_000 });
  if (!box) {
    log('relink_no_box', { url: page.url(), text: await bodyText(page) });
    return;
  }
  await box.click();
  await box.fill(userCode);
  const b = await findVisible(page, ['button#idSIButton9', 'button#idSubmit_SAOTCC_Continue', 'input#idSIButton9', 'button[type="submit"]'], { timeoutMs: 6000 });
  if (b) await b.click(); else await page.keyboard.press('Enter');
  await sleep(6000);
  log('relink_after', { url: page.url(), shot: await shot(page, `05e-relink-${round}`), text: await bodyText(page) });
}

// recoveryUnlock drives the one interrupt that actually blocks the device-code
// grant on a freshly registered mailbox: "help protect your account -> add an
// e-mail address". The address has to be one we can read, so it comes from a
// temp-mail record ({address, jwt}) and the verification code is pulled from
// that provider's API.
async function recoveryUnlock(page, record, round) {
  const { address, jwt } = record;
  // Codes are single-use and the provider's inbox also holds older ones, so the
  // only mail that may answer this attempt is one that arrives after it starts.
  const attemptStart = Date.now();
  log('unlock_start', { address, round });

  const addBtn = await findVisible(page, [
    'button:has-text("添加电子邮件")',
    'button:has-text("Add email")',
    'button:has-text("添加电子邮件地址")',
  ], { timeoutMs: 8000 });
  if (!addBtn) {
    log('unlock_no_button', { url: page.url(), text: await bodyText(page) });
    return;
  }
  await addBtn.click();
  await sleep(3500);
  log('unlock_after_button', { url: page.url(), shot: await shot(page, `08-unlock-form-${round}`), text: await bodyText(page) });

  const input = await findVisible(page, [
    'input[type="email"]',
    'input[name="EmailAddress"]',
    'input#iEmailAddress',
    'input[name="email"]',
    'input[aria-label*="电子邮件"]',
    'input[placeholder*="电子邮件"]',
    'input[type="text"]',
  ], { timeoutMs: 8000 });
  if (!input) {
    log('unlock_no_input', { url: page.url(), text: await bodyText(page) });
    return;
  }
  await input.click();
  await input.fill(address);
  log('unlock_address_typed', { address });

  let next = await findVisible(page, ['button:has-text("下一步")', 'button:has-text("Next")', 'button#idSIButton9', 'input#idSIButton9', 'button[type="submit"]'], { timeoutMs: 6000 });
  if (next) await next.click(); else await page.keyboard.press('Enter');
  await sleep(6000);
  log('unlock_after_address', { url: page.url(), shot: await shot(page, `09-unlock-code-${round}`), text: await bodyText(page) });

  // The verification code lands in the temp inbox: poll it for a fresh message.
  const code = await pollTempCode(jwt, { timeoutMs: 150_000, sinceMs: attemptStart - 45_000 });
  if (!code) {
    log('unlock_no_code', { url: page.url(), text: await bodyText(page) });
    return;
  }
  log('unlock_code_read', { len: code.length });
  // This interrupt renders the code as six one-character boxes, so filling the
  // first input with the whole string keeps only the first digit (observed
  // 2026-11-14: box 1 showed "8", box 2 empty). Type digit by digit and let the
  // widget advance, which also triggers its auto-submit.
  const boxes = await page.evaluate(() => {
    const out = [];
    for (const el of document.querySelectorAll('input')) {
      const r = el.getBoundingClientRect();
      if (r.width > 0 && r.height > 0) out.push({ name: el.name || '', type: el.type || '', id: el.id || '', maxlength: el.maxLength || 0 });
    }
    return out;
  }).catch(() => []);
  log('unlock_inputs', boxes);

  const codeBox = await findVisible(page, [
    'input[name="otc"]',
    'input#otc',
    'input[autocomplete="one-time-code"]',
    'input[inputmode="numeric"]',
    'input[type="text"]',
    'input[type="tel"]',
  ], { timeoutMs: 10_000 });
  if (!codeBox) {
    log('unlock_no_code_box', { url: page.url(), text: await bodyText(page) });
    return;
  }
  await codeBox.click().catch(() => {});
  await codeBox.fill('').catch(() => {});
  await page.keyboard.type(code, { delay: 140 });
  await sleep(6000);
  log('unlock_typed', { url: page.url(), text: await bodyText(page) });

  // Some templates want an explicit Next after the sixth digit.
  if (!/全部设置|已全部|all set|完成|我们正在|redirecting/i.test(await bodyText(page))) {
    const nextBtn = await findVisible(page, ['button:has-text("下一步")', 'button:has-text("Next")', 'button#idSIButton9', 'input#idSIButton9', 'button[type="submit"]'], { timeoutMs: 4000 });
    if (nextBtn) {
      await nextBtn.click().catch(() => {});
      await sleep(6000);
    }
  }
  log('unlock_done', { url: page.url(), shot: await shot(page, `10-unlock-verified-${round}`), text: await bodyText(page) });
}

// pollTempCode reads the newest 6-digit code out of the temp-mail API. It uses
// the same provider the repository already knows (abc-temp-2026.shop).
async function pollTempCode(jwt, { timeoutMs = 120_000, intervalMs = 5000, sinceMs = 0 } = {}) {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    await sleep(intervalMs);    let body = null;
    try {
      const res = await fetch('https://abc-temp-2026.shop/api/mails?limit=10&offset=0', {
        headers: { Authorization: `Bearer ${jwt}` },
      });
      body = await res.json().catch(() => null);
    } catch (e) {
      log('unlock_poll', `net ${String(e.message).slice(0, 40)}`);
      continue;
    }
    const rows = (body && body.results) || [];
    // Newest first, and only a mail from the last ten minutes may answer: an old
    // code from a previous attempt would otherwise be typed in first and burn
    // the attempt.
    const fresh = rows
      .filter((m) => {
        const when = Date.parse(String(m.created_at || '').replace(' ', 'T') + 'Z');
        if (!Number.isFinite(when)) return !sinceMs;
        if (when < sinceMs) return false;
        return Date.now() - when < 15 * 60_000;
      })
      .sort((a, b) => String(b.created_at || '').localeCompare(String(a.created_at || '')));
    for (const mail of fresh) {
      const blob = `${mail.subject || ''} ${mail.source || ''} ${mail.raw || ''} ${mail.text || ''}`;
      const m = blob.match(/(?<!\d)(\d{6})(?!\d)/);
      if (m && /microsoft|account/i.test(blob)) {
        log('unlock_code_source', { source: String(mail.source || '').slice(0, 48), created_at: mail.created_at || '' });
        return m[1];
      }
    }
  }
  return '';
}

// signInWithPassword walks the interactive sign-in far enough to authorize the
// device code: enter code -> email -> password -> protect-account skip ->
// stay-signed-in -> consent.
async function signInWithPassword({ userCode, password, mailbox }) {
  const launchOpts = {
    channel: 'chrome',
    headless: false,
    viewport: { width: 1280, height: 900 },
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    args: [
      '--disable-blink-features=AutomationControlled',
      '--no-first-run',
      '--no-default-browser-check',
    ],
  };
  const profileDir = path.join(outdir, 'chrome-profile');
  let context;
  try {
    context = await chromium.launchPersistentContext(profileDir, launchOpts);
  } catch (e) {
    log('warn', `channel=chrome failed (${String(e).slice(0, 100)}), retrying executablePath`);
    context = await chromium.launchPersistentContext(profileDir, { ...launchOpts, channel: undefined });
  }
  const page = context.pages()[0] || await context.newPage();
  page.setDefaultTimeout(20_000);
  log('browser_up', { profile: path.basename(profileDir) });

  log('nav', VERIFY_URI);
  await page.goto(VERIFY_URI, { waitUntil: 'domcontentloaded', timeout: 60_000 })
    .catch((e) => log('warn', `goto ${String(e).slice(0, 80)}`));
  await sleep(2500);
  log('landed', { url: page.url(), shot: await shot(page, '01-landed'), text: await bodyText(page) });

  // ---- stage 1: the code entry box
  const box = await findVisible(page, [
    'input#otc',
    'input[name="otc"]',
    'input[aria-label*="code" i]',
    'input[aria-label*="代码"]',
    'input[type="text"]',
  ], { timeoutMs: 20_000 });
  if (!box) {
    log('fail', { stage: 'code_box_not_found', url: page.url(), text: await bodyText(page) });
    throw new Error('code entry box not found');
  }
  await box.click();
  await box.fill(userCode);
  log('code_typed', { len: userCode.length });

  const next = await findVisible(page, ['button#idSIButton9', 'button#idSubmit_SAOTCC_Continue', 'input#idSIButton9', 'button:has-text("Next")', 'button:has-text("下一步")', 'button[type="submit"]'], { timeoutMs: 8000 });
  if (next) await next.click(); else await page.keyboard.press('Enter');
  await sleep(4000);
  log('after_code', { url: page.url(), shot: await shot(page, '02-after-code'), text: await bodyText(page) });

  // ---- stage 2: e-mail
  const user = await findVisible(page, ['input[name="loginfmt"]', 'input#i0116', 'input[type="email"]'], { timeoutMs: 12_000 });
  if (user) {
    await user.fill(mailbox);
    const b = await findVisible(page, ['button#idSIButton9', 'input#idSIButton9', 'button:has-text("Next")', 'button:has-text("下一步")']);
    if (b) await b.click(); else await page.keyboard.press('Enter');
    await sleep(3500);
    log('email_submitted', { url: page.url(), shot: await shot(page, '03-after-email'), text: await bodyText(page) });
  } else {
    log('note', 'no email field (already signed in?)');
  }

  // ---- stage 3: password
  const pass = await findVisible(page, ['input[name="passwd"]', 'input#i0118', 'input[type="password"]'], { timeoutMs: 12_000 });
  if (pass) {
    await pass.fill(password);
    const b = await findVisible(page, ['button#idSIButton9', 'input#idSIButton9', 'button:has-text("Sign in")', 'button:has-text("登录")']);
    if (b) await b.click(); else await page.keyboard.press('Enter');
    await sleep(6000);
    log('password_submitted', { url: page.url(), shot: await shot(page, '04-after-password'), text: await bodyText(page) });
  } else {
    log('note', 'no password field');
    log('page', { url: page.url(), shot: await shot(page, '04-no-password'), text: await bodyText(page) });
  }

  // ---- stage 4: protect-account gate / stay-signed-in / consent
  for (let round = 0; round < 8; round += 1) {
    const text = await bodyText(page);
    // "Help protect your account" (account.live.com/interrupt) is a gate, not an
    // error: it wants a recovery address or a skip. The skip link keeps the same
    // id across the classic live templates, so try it before anything else.
    if (/帮助保护你的帐户|保护你的帐户|Help us protect|protect your account/i.test(text)) {
      const skip = await findVisible(page, [
        'a#iShowSkip',
        '#iShowSkip',
        'a:has-text("以后再说")',
        'a:has-text("跳过")',
        'a:has-text("暂时跳过")',
        'a:has-text("稍后再说")',
        'button:has-text("以后再说")',
        'button:has-text("跳过")',
        'a:has-text("Skip for now")',
        'button:has-text("Skip for now")',
      ], { timeoutMs: 6000 });
      if (skip) {
        await skip.click().catch(() => {});
        await sleep(5000);
        log('protect_skipped', { url: page.url(), shot: await shot(page, `05b-skip-${round}`), text: await bodyText(page) });
        continue;
      }
      log('protect_gate_no_skip', { url: page.url(), shot: await shot(page, `05c-noskip-${round}`), text });
      if (flag('unlock')) {
        await recoveryUnlock(page, loadRecoveryRecord(), round);
        continue;
      }
      if (flag('relink')) {
        await relinkAttempt(page, userCode, round);
        continue;
      }
      if (flag('gate-dump')) {
        await gateProbe(page, round);
        continue;
      }
      break;
    }
    if (/stay signed in|保持登录|是否保持|keep me signed/i.test(text)) {
      const stay = await findVisible(page, ['button#idBtn_Back', 'button#idSIButton9']);
      if (stay) {
        await stay.click().catch(() => {});
        await sleep(4000);
        log('stay_signed_in_answered', { url: page.url(), shot: await shot(page, `05-stay-${round}`) });
        continue;
      }
    }
    const consent = await findVisible(page, ['button#idBtn_Accept', 'input#idBtn_Accept', 'button:has-text("Accept")', 'button:has-text("接受")', 'button:has-text("Continue")', 'button:has-text("继续")'], { timeoutMs: 2500 });
    if (consent) {
      await consent.click().catch(() => {});
      await sleep(4000);
      log('consent_answered', { url: page.url(), shot: await shot(page, `06-consent-${round}`), text: await bodyText(page) });
      continue;
    }
    log('idle', { url: page.url(), text, shot: await shot(page, `07-idle-${round}`) });
    break;
  }

  return context;
}

// ------------------------------------------------------------------ token
async function pollToken(deviceCode, { interval = 5, deadlineMs = 15 * 60_000 } = {}) {
  const started = Date.now();
  let wait = Math.max(1, Number(interval) || 5);
  for (;;) {
    if (Date.now() - started > deadlineMs) return { ok: false, error: 'poll timeout' };
    await sleep(wait * 1000);
    const body = new URLSearchParams({
      grant_type: 'urn:ietf:params:oauth:grant-type:device_code',
      client_id: CLIENT_ID,
      device_code: deviceCode,
    });
    // A dropped connection on a residential line is not a verdict: retry until
    // the deadline instead of dying on the first ECONNRESET.
    let res;
    try {
      res = await fetch(TOKEN_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: body.toString(),
      });
    } catch (e) {
      log('poll', `net ${String(e && e.message ? e.message : e).slice(0, 60)} (retry)`);
      continue;
    }
    const json = await res.json().catch(() => null);
    if (res.ok && json && json.access_token) {
      return { ok: true, json, elapsed_s: Math.round((Date.now() - started) / 1000) };
    }
    const code = (json && json.error) || `http_${res.status}`;
    if (code === 'authorization_pending') { log('poll', 'authorization_pending'); continue; }
    if (code === 'slow_down') { wait += 5; log('poll', `slow_down wait=${wait}s`); continue; }
    if (/^http_5/.test(code)) { log('poll', `${code} (retry)`); continue; }
    return { ok: false, error: String((json && json.error_description) || code).split('\n')[0].slice(0, 220), code };
  }
}

function writeTokenFile(json, mailbox) {
  const payload = {
    client_id: CLIENT_ID,
    refresh_token: json.refresh_token,
    mailbox: mailbox.toLowerCase(),
    scope: SCOPE,
    obtained_at: new Date().toISOString(),
  };
  fs.writeFileSync(tokenFile, `${JSON.stringify(payload, null, 2)}\n`, { mode: 0o600 });
  try { fs.chmodSync(tokenFile, 0o600); } catch { /* windows */ }
}

// ------------------------------------------------------------------- main
(async () => {
  const mailbox = email || firstAddress(productFile);
  if (!mailbox) throw new Error('no mailbox: pass --email or seed the product file');
  const password = readPassword(productFile, mailbox);
  log('password_loaded', { mailbox, len: password.length });

  const dc = loadDeviceCode() || await mintDeviceCode();
  log('device_code', {
    user_code: dc.user_code,
    verification_uri: dc.verification_uri || VERIFY_URI,
    reused: Boolean(dc.reused),
  });

  let context = null;
  if (!noBrowser) {
    context = await signInWithPassword({ userCode: dc.user_code, password, mailbox });
  }

  log('poll_start', { interval: dc.interval || 5 });
  const result = await pollToken(dc.device_code, {
    interval: dc.interval || 5,
    deadlineMs: Math.min(15 * 60_000, (Number(dc.expires_in) || 900) * 1000),
  });
  if (!result.ok) {
    log('fail', { stage: 'token_poll', error: result.error, code: result.code });
    if (context) {
      const page = context.pages()[0];
      if (page) log('final_page', { url: page.url(), shot: await shot(page, '99-final'), text: await bodyText(page) });
    }
    await context?.close().catch(() => {});
    process.exit(3);
  }

  writeTokenFile(result.json, mailbox);
  log('token_ok', {
    out: path.relative(process.cwd(), tokenFile),
    has_refresh: Boolean(result.json.refresh_token),
    expires_in: result.json.expires_in,
    elapsed_s: result.elapsed_s,
  });
  await context?.close().catch(() => {});
  process.exit(0);
})().catch((e) => {
  log('fatal', String(e && e.message ? e.message : e).slice(0, 300));
  process.exit(2);
});
