// Drive ONE Outlook signup through the operator's own Roxy/Forge fingerprint
// browser (real Chrome 155), over CDP, with human-like form filling.
//
// ROUTE A — Microsoft-hosted address only. On signup.live.com we take
// "Get a new email address", so the address is <username>@outlook.com and no
// external / temp mailbox is involved at all: no mailbox is created, nothing
// outside Microsoft is called, and there is no e-mail verification-code step.
// State machine: new-address -> password -> details -> name -> verdict, all
// inside ONE browsing session (no window/page re-open, no resume: re-opening
// on the same exit is one of the three ways the previous attempt got blocked).
//
// Reused interaction patterns (see report):
//   - workbench core/roxybrowser_client.py : /browser/create -> /browser/open
//     -> debuggerAddress -> connect_over_cdp (never launch our own browser)
//   - workbench config/humanize.py + core/roxy_registration.py : per-keystroke
//     cadence, field-level pauses, bezier mouse, click-before/after delays
//   - this repo tools/capture/px-hold-probe.cjs : frame scan for the HUMAN
//     press widget + bezier approach + hold with jitter
//   - D:/outlook-recon/OutlookRegister/controllers/base_controller.py :
//     signup step order and MS selectors (primaryButton, BirthYear/Month/Day,
//     firstNameInput/lastNameInput)
//
// Usage:
//   node tools/capture/outlook-roxy.cjs --proxy-port 10031
//        [--runs .cache/outlook-roxy] [--hold-ms 0] [--flow-budget-s 720]
//
// Prints progress + a JSON summary. The password is NEVER printed; it is
// written to the product file (and a private recovery file) only.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

// ------------------------------------------------------------------ config
const ROXY_BASE = 'http://127.0.0.1:50100';
const ROXY_TOKEN = 'dbe2bb7b7796389837527d4d90e51470';
const WORKSPACE_ID = '23340934979096576';
const PROJECT_ID = '23340935079759872';

const PROXY_HOST = 'gw.dataimpulse.com';
const PROXY_USER = '3ddce6da3add2226e7d2__cr.us;sessttl.30';
const PROXY_PASS = '2805d82cb2a633dc';

// Route A keeps everything on Microsoft's own domain: the address is
// <username>@outlook.com, so there is no mailbox service to talk to.
const OUTLOOK_DOMAIN = 'outlook.com';
const USERNAME_MIN_LEN = 10;
const USERNAME_MAX_LEN = 12;
// "Taken?" retries: at most MAX_USERNAME_ATTEMPTS submissions of a name in the
// one session, each after a human pause. More than that is just hammering.
const MAX_USERNAME_ATTEMPTS = 3;

const PRODUCT_FILE = 'D:/outlook-recon/产出/roxy-outlook.txt';
// Route A entry. signup.live.com/signup?lic=1 is the "enter your own email"
// variant and has no "Get a new email address" link any more (verified against
// the live DOM), so it can only end in an external mailbox. The Outlook
// create-account prompt lands on the Microsoft-domain variant instead:
//   signup.live.com/signup?sru=...&fl=dob,flname,wld
//   "Enter your new email address.  New email [ ... ] @outlook.com  Next"
// with #domainDropdownId preset to @outlook.com and input aria "New email".
const SIGNUP_URL = 'https://outlook.live.com/mail/0/?prompt=create_account';
const IP_PROBE_URLS = [
  'http://ip-api.com/json/?fields=status,country,countryCode,city,timezone,isp,query',
  'https://ipinfo.io/json',
];
// The Roxy profile is generated with geoSource=locale, i.e. en-US /
// America/New_York. Keep the exit in that same country+zone so the address,
// the clock and the language a signup leaks cannot disagree.
const REQUIRED_CC = 'US';
const REQUIRED_TZ_PREFIX = 'America/';
// What this Roxy build actually generates (geoSource=locale, en-US).
const PROFILE_TZ = 'America/New_York';

// ------------------------------------------------------------------- args
const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
function flag(name) { return argv.includes(`--${name}`); }

const proxyPort = String(arg('proxy-port', '10017'));
const runRoot = path.resolve(arg('runs', path.join(__dirname, '..', '..', '.cache', 'outlook-roxy')));
const runId = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
const runDir = path.join(runRoot, `run-${runId}`);
const shotDir = path.join(runDir, 'shots');
const holdMsFixed = Number(arg('hold-ms', '0')) || 0;
fs.mkdirSync(shotDir, { recursive: true });

// ------------------------------------------------------------------ utils
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const rand = (a, b) => a + Math.random() * (b - a);
const randi = (a, b) => Math.floor(rand(a, b + 1));
const chance = (p) => Math.random() < p;
const r2 = (n) => Math.round(Number(n) * 10) / 10;

const events = [];
let phase = 'init';
const t0 = Date.now();
const elapsed = () => Math.round((Date.now() - t0) / 100) / 10;

function log(step, detail) {
  const row = { at: new Date().toISOString(), t: elapsed(), phase, step, detail };
  events.push(row);
  const line = `[${String(row.t).padStart(7)}s] ${step}${detail ? ' :: ' + (typeof detail === 'string' ? detail : JSON.stringify(detail)) : ''}`;
  console.log(line.slice(0, 400));
  return row;
}

function writeJson(file, value) {
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, JSON.stringify(value, null, 2), 'utf8');
  } catch { /* best effort */ }
}

let accountPassword = '';

const state = {
  ok: false,
  stage: 'boot',
  run_id: runId,
  run_dir: runDir,
  proxy: `socks5://${PROXY_HOST}:${proxyPort}`,
  email: '',
  profile: null,
  steps: [],
  challenge: null,
  error: '',
  summary: '',
};

function persist() {
  writeJson(path.join(runDir, 'run.json'), { ...state, events });
}

// ------------------------------------------------------------- roxy client
async function roxy(pathname, body) {
  const res = await fetch(ROXY_BASE + pathname, {
    method: body === undefined ? 'GET' : 'POST',
    headers: {
      token: ROXY_TOKEN,
      Authorization: `Bearer ${ROXY_TOKEN}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(120000),
  });
  const text = await res.text();
  let json;
  try { json = JSON.parse(text); } catch { json = { raw: text }; }
  if (!res.ok) throw new Error(`ROXY_HTTP_${res.status} ${pathname}: ${text.slice(0, 300)}`);
  if (json && json.success === false) throw new Error(`ROXY_FAIL ${pathname}: ${text.slice(0, 300)}`);
  if (json && json.code !== undefined && ![0, 200, '0', '200'].includes(json.code)) {
    throw new Error(`ROXY_CODE ${pathname}: ${text.slice(0, 300)}`);
  }
  return json;
}

// --------------------------------------------------------------- username
// 10..12 lowercase letters. No digits at all (so no runs of digits), no
// tripled letters, no vowel pile-ups, and it must contain at least one rare
// letter so the shape cannot be a plain dictionary word. A short blocklist
// keeps the obviously-unwanted and the obviously-dictionary remnants out.
const NAME_BLOCKLIST = [
  'ass', 'anal', 'bitch', 'cum', 'cunt', 'dick', 'die', 'drug', 'fag', 'fuck',
  'gay', 'kill', 'kkk', 'nazi', 'nigg', 'piss', 'porn', 'rape', 'sex', 'slut',
  'tit', 'whore',
  'admin', 'email', 'hotmail', 'login', 'mail', 'micro', 'office', 'outlook',
  'password', 'support', 'test', 'user', 'windows',
];

function genUsername() {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    const len = randi(USERNAME_MIN_LEN, USERNAME_MAX_LEN);
    let out = '';
    for (let i = 0; i < len; i += 1) out += String.fromCharCode(97 + randi(0, 25));
    if (/([a-z])\1\1/.test(out)) continue;       // no triple letters
    if (/[aeiouy]{4}/.test(out)) continue;       // no syllable pile-ups
    if (!/[qxjzvkwy]/.test(out)) continue;       // keeps the word shape non-English
    if (NAME_BLOCKLIST.some((w) => out.includes(w))) continue;
    return out;
  }
  // Deterministic fallback with the same shape guarantees.
  return `z${Array.from({ length: USERNAME_MIN_LEN - 1 }, () => String.fromCharCode(97 + randi(0, 25))).join('')}`;
}

function genPassword() {
  // Strong, MS-acceptable, no ambiguous chars, 4 char classes.
  const lower = 'abcdefghijkmnopqrstuvwxyz';
  const upper = 'ABCDEFGHJKLMNPQRSTUVWXYZ';
  const digit = '23456789';
  const sym = '!@#$%^&*-_=+?';
  const pick = (s) => s[randi(0, s.length - 1)];
  const chars = [pick(lower), pick(upper), pick(digit), pick(sym)];
  const all = lower + upper + digit + sym;
  while (chars.length < 16) chars.push(pick(all));
  for (let i = chars.length - 1; i > 0; i -= 1) {
    const j = randi(0, i);
    [chars[i], chars[j]] = [chars[j], chars[i]];
  }
  return chars.join('');
}

// MS renders Country/Month/Day as buttons + a role=listbox popup, not <select>.
async function pickTrigger(page, triggerSelector, { index = -1, label = '' } = {}) {
  const trigger = await findVisible(page, [triggerSelector], 15000);
  const before = (await trigger.locator.innerText().catch(() => '') || '').trim();
  await humanClick(page, trigger.locator, 'trigger');
  await sleep(rand(600, 1200));
  for (let i = 0; i < 15; i += 1) {
    if ((await page.locator('[role="option"]').count().catch(() => 0)) > 0) break;
    await sleep(400);
  }
  const options = page.locator('[role="option"]');
  const count = await options.count().catch(() => 0);
  if (label && count) {
    for (let i = 0; i < Math.min(count, 40); i += 1) {
      const item = options.nth(i);
      const text = (await item.innerText().catch(() => '') || '').trim();
      if (text !== label) continue;
      if (await item.isVisible().catch(() => false)) {
        await humanClick(page, item, 'option');
        await sleep(rand(500, 1000));
        log('pick', { trigger: triggerSelector, mode: 'text', picked: text });
        return text;
      }
    }
  }
  if (index >= 0 && count > index) {
    const item = options.nth(index);
    const text = (await item.innerText().catch(() => '') || '').trim();
    if (await item.isVisible().catch(() => false)) {
      await humanClick(page, item, 'option');
      await sleep(rand(500, 1000));
      log('pick', { trigger: triggerSelector, mode: 'index', picked: text });
      return text;
    }
  }
  await page.keyboard.press('ArrowDown');
  for (let i = 0; i < Math.max(index, 0); i += 1) {
    await sleep(rand(60, 140));
    await page.keyboard.press('ArrowDown');
  }
  await page.keyboard.press('Enter');
  await sleep(rand(500, 1000));
  const after = (await trigger.locator.innerText().catch(() => '') || '').trim();
  log('pick', { trigger: triggerSelector, mode: 'keyboard', before, after });
  return after;
}

const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June', 'July',
  'August', 'September', 'October', 'November', 'December'];

// "Add some details": Country/Region arrives prefilled from the exit IP;
// Month/Day are triggers and Year is the only real text input on the page.
async function fillDetails(page) {
  const year = String(randi(1972, 1998));
  const monthIndex = randi(0, 11);
  const day = randi(2, 27);
  // Country/Region normally arrives prefilled from the exit IP. If it is empty
  // or sitting on another country, choose the one the exit actually is in -
  // the form's own country must agree with the IP and the profile clock.
  const countryTrigger = await anyVisible(page, ['#CountryDropdown', '[name="Country"]', 'select[name="Country"]'], 4000);
  let country = '';
  if (countryTrigger) {
    country = (await countryTrigger.locator.innerText().catch(() => '') || '').trim();
    log('country_prefill', { text: country });
    if (!country && countryTrigger.selector.includes('select')) {
      await countryTrigger.locator.selectOption({ label: 'United States' }).catch(() => {});
      country = 'United States';
    } else if (!country) {
      await pickTrigger(page, countryTrigger.selector, { label: 'United States' });
      country = (await countryTrigger.locator.innerText().catch(() => '') || '').trim();
    }
    await sleep(rand(400, 900));
  }
  const yearField = await anyVisible(page, ['input[name="BirthYear"]', '[name="BirthYear"]'], 8000);
  if (yearField) await typeHuman(page, yearField.locator, year, 'birth_year');
  await pickTrigger(page, '#BirthMonthDropdown', { index: monthIndex, label: MONTH_NAMES[monthIndex] });
  await sleep(rand(400, 900));
  await pickTrigger(page, '#BirthDayDropdown', { label: String(day) });
  await sleep(rand(500, 1100));
  const monthText = (await page.locator('#BirthMonthDropdown').first().innerText().catch(() => '') || '').trim();
  const dayText = (await page.locator('#BirthDayDropdown').first().innerText().catch(() => '') || '').trim();
  const yearValue = await page.locator('input[name="BirthYear"]').first().inputValue().catch(() => '');
  state.details = { country, month: monthText, day: dayText, year: yearValue };
  log('details_filled', state.details);
  await sleep(rand(700, 1500));
  await clickNext(page, 'details_next');
  await sleep(rand(1800, 3200));
  await dumpInventory(page, '04-after-details');
  await screenshotNow(page, 'after-details');
}

async function fillName(page) {
  const last = LAST_NAMES[randi(0, LAST_NAMES.length - 1)];
  const first = FIRST_NAMES[randi(0, FIRST_NAMES.length - 1)];
  const lastField = await findVisible(page, ['#lastNameInput', 'input[name="LastName"]', 'input[aria-label*="last name" i]'], 15000);
  await typeHuman(page, lastField.locator, last, 'last_name');
  const firstField = await anyVisible(page, ['#firstNameInput', 'input[name="FirstName"]', 'input[aria-label*="first name" i]'], 8000);
  if (firstField) await typeHuman(page, firstField.locator, first, 'first_name');
  state.name = { first, last };
  log('name', state.name);
  await sleep(rand(700, 1500));
  await clickNext(page, 'name_next');
  await sleep(rand(1800, 3200));
  await dumpInventory(page, '08-after-name');
  await screenshotNow(page, 'after-name');
}

// ------------------------------------------------------------ mouse/typing
function bezierPath(from, to, samples = 26) {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const dist = Math.hypot(dx, dy) || 1;
  const nx = -dy / dist;
  const ny = dx / dist;
  const bow = Math.min(160, Math.max(30, dist * 0.32)) * (chance(0.5) ? -1 : 1);
  const p1 = { x: from.x + dx * 0.3 + nx * bow, y: from.y + dy * 0.3 + ny * bow };
  const p2 = { x: from.x + dx * 0.68 - nx * bow * 0.42, y: from.y + dy * 0.68 - ny * bow * 0.42 };
  const points = [];
  for (let i = 1; i <= samples; i += 1) {
    const raw = i / samples;
    const t = 1 - Math.pow(1 - raw, 4); // ease-out
    const mt = 1 - t;
    const x = mt * mt * mt * from.x + 3 * mt * mt * t * p1.x + 3 * mt * t * t * p2.x + t * t * t * to.x;
    const y = mt * mt * mt * from.y + 3 * mt * mt * t * p1.y + 3 * mt * t * t * p2.y + t * t * t * to.y;
    const wobble = i < samples ? rand(-0.9, 0.9) : 0;
    points.push({ x: x + wobble, y: y + wobble });
  }
  return points;
}

async function glideTo(page, to, samples = 26) {
  const from = page.__lastPointer || { x: rand(200, 900), y: rand(180, 520) };
  for (const p of bezierPath(from, to, samples)) {
    await page.mouse.move(p.x, p.y);
    await sleep(rand(8, 22));
  }
  page.__lastPointer = to;
}

async function mouseWander(page, moves = 2) {
  for (let i = 0; i < moves; i += 1) {
    await glideTo(page, { x: rand(220, 1180), y: rand(160, 700) }, 14);
    await sleep(rand(220, 700));
  }
}

async function humanClick(page, target, label = '') {
  const box = await target.boundingBox().catch(() => null);
  if (!box || box.width < 2 || box.height < 2) throw new Error(`NO_BOX ${label}`);
  const point = {
    x: box.x + box.width * rand(0.32, 0.68),
    y: box.y + box.height * rand(0.34, 0.66),
  };
  await glideTo(page, point);
  await sleep(rand(200, 500)); // hover before click
  await page.mouse.down();
  await sleep(rand(45, 130));
  await page.mouse.up();
  await sleep(rand(120, 320));
  return point;
}

async function typeHuman(page, target, text, label = '') {
  await humanClick(page, target, label || 'focus');
  const current = await target.inputValue().catch(() => '');
  if (current) {
    await page.keyboard.press('Control+A');
    await sleep(rand(60, 160));
    await page.keyboard.press('Backspace');
    await sleep(rand(120, 260));
  }
  const tStart = Date.now();
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i];
    await page.keyboard.type(ch);
    if (i < text.length - 1) {
      await sleep(rand(80, 220)); // per-character cadence
      if (chance(0.11)) await sleep(rand(400, 900)); // occasional burst pause
      if (chance(0.04)) await sleep(rand(120, 340));
    }
  }
  const typedMs = Date.now() - tStart;
  await sleep(rand(600, 1500)); // settle before the next field
  log('typed', { label, chars: text.length, ms: typedMs });
  return typedMs;
}

// --------------------------------------------------------- element finding
async function framesOf(page) {
  const frames = [page.mainFrame(), ...page.frames().filter((f) => f !== page.mainFrame())];
  return frames;
}

async function findVisible(page, selectors, opts = {}) {
  const timeoutMs = opts.timeoutMs || 25000;
  const deadline = Date.now() + timeoutMs;
  let lastErr = null;
  while (Date.now() < deadline) {
    for (const frame of await framesOf(page)) {
      for (const selector of selectors) {
        try {
          const loc = frame.locator(selector).first();
          if ((await loc.count()) === 0) continue;
          if (!(await loc.isVisible().catch(() => false))) continue;
          const box = await loc.boundingBox().catch(() => null);
          if (!box || box.width < 2 || box.height < 2) continue;
          return { locator: loc, selector, frameUrl: frame.url() };
        } catch (error) {
          lastErr = error;
        }
      }
    }
    await sleep(450);
  }
  throw new Error(`NOT_FOUND ${selectors.join('|')} ${lastErr ? String(lastErr).slice(0, 80) : ''}`);
}

async function anyVisible(page, selectors, timeoutMs = 1200) {
  try {
    return await findVisible(page, selectors, { timeoutMs });
  } catch {
    return null;
  }
}

async function dumpInventory(page, tag) {
  const out = [];
  for (const frame of await framesOf(page)) {
    let row;
    try {
      row = await frame.evaluate(() => {
        const vis = (el) => {
          const r = el.getBoundingClientRect();
          const st = getComputedStyle(el);
          return r.width > 1 && r.height > 1 && st.visibility !== 'hidden' && st.display !== 'none';
        };
        const inputs = [...document.querySelectorAll('input,select,textarea')].filter(vis).map((el) => ({
          tag: el.tagName.toLowerCase(), type: el.type || '', id: el.id || '', name: el.name || '',
          aria: (el.getAttribute('aria-label') || '').slice(0, 60),
          ph: (el.placeholder || '').slice(0, 60),
          role: el.getAttribute('role') || '', maxlength: el.maxLength > 0 ? el.maxLength : 0,
        }));
        const buttons = [...document.querySelectorAll('button, [role="button"], input[type="submit"]')]
          .filter(vis).slice(0, 14).map((el) => ({
            tag: el.tagName.toLowerCase(), id: el.id || '',
            testid: el.getAttribute('data-testid') || '',
            aria: (el.getAttribute('aria-label') || '').slice(0, 60),
            text: (el.innerText || el.value || '').trim().slice(0, 50),
          }));
        return {
          url: location.href.slice(0, 160), title: document.title,
          text: (document.body && document.body.innerText || '').replace(/\s+/g, ' ').slice(0, 500),
          inputs, buttons,
        };
      });
    } catch (error) {
      row = { url: frame.url().slice(0, 160), error: String(error).slice(0, 90) };
    }
    out.push(row);
  }
  writeJson(path.join(runDir, `dom-${tag}.json`), out);
  log('dom', { tag, frames: out.length, main_text: (out[0] && out[0].text || '').slice(0, 180) });
  return out;
}

async function bodyText(page) {
  try {
    return await page.evaluate(() => (document.body && document.body.innerText || '').replace(/\s+/g, ' '));
  } catch {
    return '';
  }
}

// ------------------------------------------------------- challenge (HUMAN)
function probeFrame() {
  const rn = (n) => Math.round(n * 10) / 10;
  const box = (el) => {
    if (!el) return null;
    const r = el.getBoundingClientRect();
    return { x: rn(r.x), y: rn(r.y), w: rn(r.width), h: rn(r.height) };
  };
  const px = document.querySelector('#px-captcha');
  const enforcement = document.querySelector('iframe#enforcementFrame, #enforcementFrame');
  const body = (document.body && document.body.innerText) || '';
  const labels = [];
  for (const el of document.querySelectorAll('[aria-label]')) {
    const aria = el.getAttribute('aria-label') || '';
    if (/press|hold/i.test(aria)) {
      labels.push({ aria: aria.slice(0, 100), box: box(el) });
    }
  }
  const draws = [...document.querySelectorAll('.draw')].map(box);
  return {
    url: location.href,
    hasPx: Boolean(px),
    pxBox: box(px),
    hasEnforcement: Boolean(enforcement),
    labels,
    draws,
    textHit: /press\s*&?\s*hold|robot or human|verify you are human|hold the button/i.test(body),
    blocked: /access to this page has been denied|px-captcha-error/i.test(body),
    text: body.replace(/\s+/g, ' ').slice(0, 260),
  };
}

async function scanChallenge(page) {
  const rows = [];
  for (const frame of await framesOf(page)) {
    let row;
    try { row = await frame.evaluate(probeFrame); } catch (e) { row = { url: frame.url(), error: String(e).slice(0, 80) }; }
    rows.push({ frameUrl: frame.url(), ...row });
  }
  const sized = (b) => Boolean(b && b.w > 8 && b.h > 8);
  const visible = rows.find((r) => (r.hasPx && sized(r.pxBox))
    || (r.labels || []).some((l) => sized(l.box))
    || (r.draws || []).some((b) => sized(b)));
  let kind = 'none';
  if (visible) kind = 'press-visible';
  else if (rows.some((r) => r.hasEnforcement)) kind = 'arkose';
  else if (rows.some((r) => r.blocked)) kind = 'block-page';
  else if (rows.some((r) => r.textHit)) kind = 'press-text';
  else if (rows.some((r) => r.hasPx)) kind = 'px-idle';
  return { kind, rows, pressFrame: visible ? visible.frameUrl : '', pressBox: visible ? (visible.pxBox || (visible.labels || [])[0]?.box || (visible.draws || [])[0]) : null };
}

async function locatePressTarget(page) {
  const selectors = [
    '#px-captcha',
    '[aria-label*="Press" i]',
    '[aria-label*="Hold" i]',
    '[aria-label*="press" i]',
    '[aria-label*="hold" i]',
    '.draw',
    'div[role="button"]:has(svg)',
  ];
  for (const frame of await framesOf(page)) {
    for (const selector of selectors) {
      try {
        const loc = frame.locator(selector).first();
        if ((await loc.count()) === 0) continue;
        if (!(await loc.isVisible().catch(() => false))) continue;
        const box = await loc.boundingBox();
        if (box && box.width > 8 && box.height > 8) {
          return { selector, frameUrl: frame.url(), box, center: { x: box.x + box.width / 2, y: box.y + box.height / 2 } };
        }
      } catch { /* next */ }
    }
  }
  return null;
}

async function clearChallenge(page, holds = 3) {
  for (let attempt = 1; attempt <= holds; attempt += 1) {
    let scan = await scanChallenge(page);
    if (scan.kind === 'none' || scan.kind === 'px-idle') return { passed: true, kind: scan.kind, attempts: attempt - 1 };
    if (scan.kind === 'arkose' || scan.kind === 'block-page') {
      return { passed: false, kind: scan.kind, attempts: attempt - 1, note: 'non-press challenge' };
    }
    // give the widget a moment to draw, then look for a pressable target
    let target = await locatePressTarget(page);
    if (!target) {
      for (let i = 0; i < 12 && !target; i += 1) { await sleep(700); target = await locatePressTarget(page); }
    }
    if (!target) return { passed: false, kind: scan.kind, attempts: attempt - 1, note: 'no press target' };
    log('press_start', { attempt, selector: target.selector, center: { x: r2(target.center.x), y: r2(target.center.y) } });
    await glideTo(page, target.center, 30);
    await sleep(rand(180, 420));
    const want = holdMsFixed > 0 ? holdMsFixed : randi(2000, 4000);
    await page.mouse.down();
    const start = Date.now();
    let clearedDuring = false;
    while (Date.now() - start < want) {
      await sleep(rand(100, 190));
      await page.mouse.move(target.center.x + rand(-1.6, 1.6), target.center.y + rand(-1.6, 1.6));
      if (Date.now() - start > 900) {
        const now = await scanChallenge(page);
        if (now.kind === 'none' || now.kind === 'px-idle') { clearedDuring = true; break; }
      }
    }
    const held = Date.now() - start;
    await page.mouse.up();
    log('press_done', { attempt, held_ms: held, cleared_during_hold: clearedDuring });
    await sleep(rand(900, 1800));
    scan = await scanChallenge(page);
    if (scan.kind === 'none' || scan.kind === 'px-idle') return { passed: true, kind: scan.kind, attempts: attempt, held_ms: held };
  }
  const final = await scanChallenge(page);
  return { passed: false, kind: final.kind, attempts: holds, note: 'still challenged' };
}

// --------------------------------------------------------------- artefacts
let shooting = false;
let shotCount = 0;
let shotTimer = null;
function startShots(page) {
  shotTimer = setInterval(async () => {
    if (shooting) return;
    shooting = true;
    try {
      shotCount += 1;
      const name = `f${String(shotCount).padStart(4, '0')}-${phase}.png`;
      await page.screenshot({ path: path.join(shotDir, name), timeout: 8000 });
    } catch { /* ignore */ } finally {
      shooting = false;
    }
  }, 4000);
}
async function stopShots() {
  if (shotTimer) clearInterval(shotTimer);
  shotTimer = null;
  for (let i = 0; i < 20 && shooting; i += 1) await sleep(200);
}

async function screenshotNow(page, name) {
  const file = path.join(shotDir, `k-${String(++shotCount).padStart(4, '0')}-${name}.png`);
  await page.screenshot({ path: file, timeout: 10000 }).catch(() => {});
  return file;
}

// ------------------------------------------------------------------- flow
const EMAIL_SELECTORS = [
  'input[aria-label*="New email" i]',
  'input[type="email"]',
  'input[name="MemberName"]',
  '#MemberName',
  'input[aria-label*="email" i]',
  'input[aria-label*="mail" i]',
  'input[placeholder*="@" i]',
];

const NEXT_SELECTORS = [
  '[data-testid="primaryButton"]',
  '#iNext',
  'button[type="submit"]',
  'input[type="submit"]',
  'button:has-text("Next")',
  'button:has-text("下一步")',
];

const PASSWORD_SELECTORS = [
  'input[type="password"]',
  '#Password',
  'input[name="Password"]',
  '#passwordInput',
  'input[aria-label*="password" i]',
];

// Route A entry: the link that switches the signup form from "use my own email"
// to "create a new Outlook address".
const GET_NEW_ADDRESS_SELECTORS = [
  '#memberNameLink',
  'a:has-text("Get a new email address")',
  '[role="link"]:has-text("Get a new email address")',
  'button:has-text("Get a new email address")',
  'text=Get a new email address',
  'a:has-text("获取新的电子邮件地址")',
  'text=获取新的电子邮件地址',
];

// The domain picker next to the name box. Default is @outlook.com, but if the
// profile/UI ever lands on @hotmail.com we switch it back before typing.
const DOMAIN_SELECTORS = [
  '#domainDropdownId',
  '[aria-label*="domain" i]',
  'button:has-text("@outlook.com")',
  'button:has-text("@hotmail.com")',
  '[role="combobox"]',
];

// "That name is gone" -- phrased so it cannot be confused with the entry page's
// unrelated "Already have an account? Sign in".
const USERNAME_TAKEN_RE = new RegExp([
  'someone already has',
  'that email address is already',
  'email address is (already )?in use',
  'name is not available',
  "isn't available",
  'is not available',
  'already taken',
  '已被占用',
  '已被使用',
  '已被注册',
  '已经有人',
  '请使用其他',
].join('|'), 'i');

async function readDomain(page) {
  for (const selector of DOMAIN_SELECTORS) {
    const hit = await anyVisible(page, [selector], 1200);
    if (!hit) continue;
    const value = (await hit.locator.innerText().catch(() => '') || '').trim();
    if (value.includes('@')) return { value, hit };
    const aria = await hit.locator.getAttribute('aria-label').catch(() => '');
    if (aria && /@/.test(value || aria)) return { value: `${value}${aria}`, hit };
  }
  return { value: '', hit: null };
}

async function ensureOutlookDomain(page) {
  const current = await readDomain(page);
  log('domain_probe', { value: current.value });
  if (current.value.toLowerCase().includes(`@${OUTLOOK_DOMAIN}`)) return current.value;
  if (current.hit) {
    await humanClick(page, current.hit.locator, 'domain_picker');
    await sleep(rand(700, 1400));
    const option = await anyVisible(page, [
      `[role="option"]:has-text("@${OUTLOOK_DOMAIN}")`,
      `div[role="option"]:text-is("@${OUTLOOK_DOMAIN}")`,
      `text=@${OUTLOOK_DOMAIN}`,
    ], 6000);
    if (option) {
      await humanClick(page, option.locator, 'domain_option');
      await sleep(rand(700, 1500));
    }
    log('domain_picked', { selector: option ? option.selector : '' });
  }
  const after = await readDomain(page);
  log('domain', { value: after.value });
  return after.value || `@${OUTLOOK_DOMAIN}`;
}

async function clickNext(page, label) {
  const target = await findVisible(page, NEXT_SELECTORS, { timeoutMs: 20000 });
  await humanClick(page, target.locator, label || 'next');
  log('click', { label: label || 'next', selector: target.selector });
  await sleep(rand(900, 2100));
}

const FIRST_NAMES = ['James', 'Michael', 'Daniel', 'Chris', 'Ryan', 'Kevin', 'Brian', 'Eric', 'Adam', 'Jason'];
const LAST_NAMES = ['Miller', 'Walker', 'Bennett', 'Carter', 'Hayes', 'Brooks', 'Reed', 'Foster', 'Grant', 'Parker'];

async function pickDropdown(page, names, index) {
  const selectors = names.flatMap((n) => [`select[name="${n}"]`, `[name="${n}"]`, `#${n}`]);
  const found = await findVisible(page, selectors, { timeoutMs: 15000 });
  const tag = await found.locator.evaluate((el) => el.tagName.toLowerCase()).catch(() => '');
  if (tag === 'select') {
    if (await found.locator.selectOption({ index }).then(() => true).catch(() => false)) {
      log('select', { selector: found.selector, tag, index });
    } else {
      await found.locator.selectOption({ label: String(index) }).catch(() => {});
    }
    await sleep(rand(400, 900));
    return;
  }
  await humanClick(page, found.locator, 'dropdown');
  await sleep(rand(500, 1100));
  const items = page.locator('[role="option"], li[role="option"], [role="listbox"] [role="option"]');
  const count = await items.count().catch(() => 0);
  if (count > index) {
    const item = items.nth(index);
    if (await item.isVisible().catch(() => false)) {
      await humanClick(page, item, 'option');
      log('select', { selector: found.selector, tag, mode: 'option', count, index });
      await sleep(rand(400, 900));
      return;
    }
  }
  // keyboard fallback: open list, walk down, commit
  await page.keyboard.press('ArrowDown');
  for (let i = 0; i < index; i += 1) { await sleep(rand(60, 140)); await page.keyboard.press('ArrowDown'); }
  await sleep(rand(130, 300));
  await page.keyboard.press('Enter');
  log('select', { selector: found.selector, tag, mode: 'keyboard', index });
  await sleep(rand(400, 900));
}

async function runFlow(page) {
  // ---- 0. entry ---------------------------------------------------------
  phase = 'entry';
  let ipInfo = null;
  for (const probeUrl of IP_PROBE_URLS) {
    try {
      await page.goto(probeUrl, { waitUntil: 'domcontentloaded', timeout: 45000 });
      const body = (await page.textContent('body') || '').trim();
      const parsed = JSON.parse(body);
      if (parsed && (parsed.countryCode || parsed.country)) { ipInfo = parsed; break; }
      log('egress_probe_empty', { url: probeUrl, sample: body.slice(0, 80) });
    } catch (error) {
      log('egress_probe_failed', String(error).slice(0, 140));
    }
  }
  if (ipInfo) {
    state.egress = ipInfo;
    const cc0 = String(ipInfo.countryCode || '').toUpperCase()
      || (String(ipInfo.country || '').length === 2 ? String(ipInfo.country).toUpperCase() : '');
    const cc = cc0;
    const tz = String(ipInfo.timezone || ipInfo.timeZone || '');
    state.egress_cc = cc;
    state.egress_tz = tz;
    log('egress', {
      country: ipInfo.country || '', countryCode: cc, city: ipInfo.city || '',
      timezone: tz, ip: ipInfo.query || ipInfo.ip || '',
    });
    // Geo self-consistency: this Roxy build derives the profile from the locale
    // (en-US -> America/New_York). If the exit sits in another country, or in a
    // zone the profile cannot honestly match, stop *before* typing anything:
    // a signup whose IP, clock and language disagree is a free risk signal.
    if (cc && cc !== REQUIRED_CC) throw Object.assign(new Error(`GEO_COUNTRY_${cc}`), { stage: 'geo_mismatch' });
    if (tz && !tz.startsWith(REQUIRED_TZ_PREFIX)) throw Object.assign(new Error(`GEO_TZ_${tz}`), { stage: 'geo_mismatch' });
    if (tz && tz !== PROFILE_TZ) log('warn', `exit zone ${tz} differs from the profile zone ${PROFILE_TZ} (same country, minor skew)`);
  } else {
    log('warn', 'egress geolocation unavailable - continuing without the geo assertion');
  }

  phase = 'signup-load';
  await page.goto(SIGNUP_URL, { waitUntil: 'domcontentloaded', timeout: 90000 });
  state.stage = 'signup_loaded';
  // outlook.live.com -> login.live.com -> signup.live.com is a redirect chain
  // and the last hop renders client-side; wait for a document that says
  // something before counting the entry as loaded.
  const loadDeadline = Date.now() + 45000;
  let bodyLen = 0;
  while (Date.now() < loadDeadline) {
    bodyLen = (await bodyText(page)).trim().length;
    if (bodyLen > 20) break;
    await sleep(1200);
  }
  // read a while before touching anything
  const readMs = randi(3000, 6000);
  await mouseWander(page, 2);
  await sleep(Math.max(600, readMs - 900));
  log('loaded', { url: page.url().slice(0, 160), read_ms: readMs, body_len: bodyLen });
  await dumpInventory(page, '01-entry');
  await screenshotNow(page, 'entry');

  let text = await bodyText(page);
  if (/access to this page has been denied|something went wrong/i.test(text)) {
    throw Object.assign(new Error('ENTRY_BLOCKED'), { stage: 'entry_blocked' });
  }

  // ---- 1. Route A: mint the address on Microsoft's own domain -----------
  // No external mailbox, no verification code: the account address is
  // <username>@outlook.com. Two shapes of this screen exist in the wild:
  //   (a) already the new-address form - "New email" + #domainDropdownId
  //   (b) the old "enter your email" form with a "Get a new email address" link
  phase = 'new-address';
  const formReady = await anyVisible(page, [
    '#domainDropdownId',
    'input[aria-label*="New email" i]',
  ], 12000);
  if (formReady) {
    log('address_form', { mode: 'new-address-form', selector: formReady.selector });
  } else {
    let newAddr = null;
    for (const selector of GET_NEW_ADDRESS_SELECTORS) {
      const hit = await anyVisible(page, [selector], 2500);
      if (!hit) continue;
      const label = (await hit.locator.innerText().catch(() => '') || '').trim();
      // guard: only the real "new address" control, not any look-alike element
      if (!/new email address|获取新的电子邮件地址/i.test(label)) continue;
      newAddr = hit;
      log('new_address_link', { selector, label: label.slice(0, 60) });
      break;
    }
    if (!newAddr) throw Object.assign(new Error('NO_NEW_ADDRESS_FORM'), { stage: 'entry_variant' });
    await humanClick(page, newAddr.locator, 'get_new_email');
    log('click', { label: 'get_new_email', selector: newAddr.selector });
    await sleep(rand(1300, 2700));
  }
  await dumpInventory(page, '02-new-address');
  await screenshotNow(page, 'new-address');

  const domain = await ensureOutlookDomain(page);
  state.domain = domain;
  if (!String(domain).toLowerCase().includes(`@${OUTLOOK_DOMAIN}`)) {
    log('warn', `domain shows ${domain || '(unknown)'} - expected @${OUTLOOK_DOMAIN}`);
  }

  const emailField = await findVisible(page, EMAIL_SELECTORS, { timeoutMs: 25000 });
  log('field', { name: 'address', selector: emailField.selector, frame: emailField.frameUrl.slice(0, 80) });

  let accepted = false;
  for (let attempt = 1; attempt <= MAX_USERNAME_ATTEMPTS; attempt += 1) {
    state.username = genUsername();
    state.email = `${state.username}@${OUTLOOK_DOMAIN}`;
    await typeHuman(page, emailField.locator, state.username, `username#${attempt}of${MAX_USERNAME_ATTEMPTS}`);
    log('username', { attempt, address: state.email });
    // recovery copy: the address may change on a "taken" retry, so refresh it
    writeJson(path.join(runDir, 'secret.json'), { email: state.email, password: accountPassword });
    await clickNext(page, `address_next#${attempt}`);
    await sleep(rand(1600, 3000));
    await dumpInventory(page, `03-after-address-${attempt}`);
    await screenshotNow(page, `after-address-${attempt}`);

    const afterText = await bodyText(page);
    if (USERNAME_TAKEN_RE.test(afterText)) {
      log('username_taken', { attempt, address: state.email, text: afterText.slice(0, 180) });
      state.taken = (state.taken || []).concat(state.email);
      if (attempt === MAX_USERNAME_ATTEMPTS) {
        throw Object.assign(new Error(`USERNAME_TAKEN_${MAX_USERNAME_ATTEMPTS}X`), { stage: 'username_taken' });
      }
      // human pause + a little movement before re-keying the field
      await sleep(rand(2600, 5400));
      await mouseWander(page, 1);
      continue;
    }
    accepted = true;
    break;
  }
  if (!accepted) throw Object.assign(new Error('USERNAME_NOT_ACCEPTED'), { stage: 'username_rejected' });
  state.steps.push('address_submitted');

  // early challenge (some exits gate right after the address step)
  if ((await scanChallenge(page)).kind === 'press-visible') {
    const res = await clearChallenge(page);
    state.challenge = res;
    log('challenge_early', res);
  }

  // After the address step Microsoft's own order is data-driven, so poll for
  // whatever the next screen is instead of hard-coding one sequence. Route A
  // observed order (Microsoft-hosted address, no external mailbox):
  //   address -> password -> add details (country/birthdate) -> name
  //   -> HUMAN press (only if the exit is flagged) -> verdict
  // The whole thing runs in the one session that is already open: nothing here
  // ever re-navigates back to signup, reopens a window or resumes a run.
  phase = 'advance';
  const SKIP_SELECTORS = [
    'button:has-text("Skip for now")',
    'button:has-text("Not now")',
    'button:has-text("No, thanks")',
    'button:has-text("Maybe later")',
    'button:has-text("Later")',
  ];

  const budget = Date.now() + Number(arg('flow-budget-s', '720')) * 1000;
  let verdict = '';
  let codeDone = false;
  let detailsDone = false;
  let passwordDone = false;
  let nameDone = false;
  let challengeDone = false;

  while (Date.now() < budget) {
    if (page.isClosed()) {
      verdict = 'session_lost';
      log('verdict_page', { verdict, reason: 'page closed - no re-open allowed in a one-shot run' });
      break;
    }
    const t = await bodyText(page);

    if (/account creation has been blocked|unusual activity|异常活动/i.test(t)) {
      verdict = 'blocked';
      state.blocked_text = t.slice(0, 500);
      log('verdict_page', { verdict, text: t.slice(0, 200) });
      break;
    }
    if (/your account has been created|account is ready|welcome to/i.test(t)) {
      verdict = 'created';
      log('verdict_page', { verdict, text: t.slice(0, 160) });
      break;
    }
    // The created flow sometimes lands straight in the mailbox/account portal
    // without a "welcome" sentence. Only trusted after the name step, so a
    // mid-signup hop through those hosts cannot be mistaken for success.
    if (nameDone && /^https:\/\/(outlook|www)\.live\.com\/(mail|owa|0)|^https:\/\/account\.microsoft\.com/i.test(String(page.url() || ''))) {
      verdict = 'created';
      log('verdict_page', { verdict, reason: 'post-signup destination', url: page.url().slice(0, 120) });
      break;
    }

    // --- 1. e-mail verification code: Route A never has one -----------------
    if (!codeDone && await anyVisible(page, ['#codeEntry-0'], 700)) {
      phase = 'code';
      codeDone = true;
      await dumpInventory(page, '06-code-page');
      await screenshotNow(page, 'code-page-unexpected');
      // Nothing outside Microsoft is reachable from here, so a code mailed to
      // the brand-new @outlook.com mailbox cannot be read. Record and stop
      // instead of guessing a digit string.
      state.code_page_seen = true;
      verdict = 'no_code';
      log('verdict_page', { verdict, reason: 'unexpected code page on a Microsoft-hosted address' });
      break;
    }

    // --- 2. "Add some details": country prefilled, month/day triggers, year ---
    if (!detailsDone && await anyVisible(page, ['#BirthMonthDropdown'], 700)) {
      phase = 'details';
      detailsDone = true;
      await fillDetails(page);
      state.steps.push('details_submitted');
      continue;
    }

    // --- 3. password (only some entry points ask for one) -------------------
    if (!passwordDone && await anyVisible(page, PASSWORD_SELECTORS, 700)) {
      phase = 'password';
      passwordDone = true;
      const passField = await findVisible(page, PASSWORD_SELECTORS, 15000);
      await typeHuman(page, passField.locator, accountPassword, 'password');
      await clickNext(page, 'password_next');
      state.steps.push('password_submitted');
      await sleep(rand(1500, 2800));
      await dumpInventory(page, '05-after-password');
      continue;
    }

    // --- 4. name (only some entry points ask for one) -----------------------
    if (!nameDone && await anyVisible(page, ['#lastNameInput', '#firstNameInput'], 700)) {
      phase = 'name';
      nameDone = true;
      await fillName(page);
      state.steps.push('name_submitted');
      continue;
    }

    // --- 5. HUMAN press-and-hold -------------------------------------------
    const scanNow = await scanChallenge(page);
    if (!challengeDone && (scanNow.kind === 'press-visible' || scanNow.kind === 'press-text')) {
      phase = 'challenge';
      challengeDone = true;
      const res = await clearChallenge(page, 3);
      state.challenge = res;
      log('challenge_after_name', res);
      await sleep(rand(2000, 4000));
      if (!res.passed) { verdict = 'challenge_failed'; break; }
      continue;
    }
    if (scanNow.kind === 'arkose') { verdict = 'arkose'; break; }

    // --- 6. optional "skip" screens ----------------------------------------
    const skipNow = await anyVisible(page, SKIP_SELECTORS, 600);
    if (skipNow) {
      await humanClick(page, skipNow.locator, 'skip');
      log('click', { label: 'skip', selector: skipNow.selector });
      await sleep(rand(1600, 3000));
      continue;
    }

    await sleep(1200);
  }

  if (!verdict) verdict = 'timeout';

  phase = 'settle';
  state.verdict = verdict;
  // Failure taxonomy the operator asked for: blocked / no_code /
  // challenge_failed / other. Created is its own terminal state.
  const TERMINAL = {
    created: 'created',
    blocked: 'blocked',
    no_code: 'no_code',
    challenge_failed: 'challenge_failed',
    arkose: 'challenge_failed',
    timeout: 'other',
    session_lost: 'other',
  };
  state.terminal = TERMINAL[verdict] || 'other';
  state.final_url = page.url();
  state.final_text = (await bodyText(page)).slice(0, 1200);
  await screenshotNow(page, 'final');
  await dumpInventory(page, '09-final');
  log('verdict', { verdict, terminal: state.terminal, url: String(state.final_url).slice(0, 100) });

  const created = verdict === 'created';
  return { created, verdict };
}

// ------------------------------------------------------------------- main
(async () => {
  accountPassword = genPassword();
  state.started_at = new Date().toISOString();
  let browser = null;
  let page = null;
  let opened = null;
  let created = false;

  const fail = (stage, error) => {
    state.stage = stage;
    state.error = String(error && error.message ? error.message : error).slice(0, 300);
    state.ok = false;
  };

  try {
    // -- roxy reachability
    const root = await roxy('/');
    log('roxy', { service: root.data && root.data.service, version: root.data && root.data.version, running: root.data && root.data.running });

    // -- Route A: the address is minted on Microsoft's own form, so there is no
    //    mailbox provisioning step and nothing external is contacted.
    state.route = 'A';
    log('route', { mode: 'A', domain: OUTLOOK_DOMAIN, max_username_attempts: MAX_USERNAME_ATTEMPTS });

    // -- private recovery copy: password only until a username is accepted;
    //    runFlow() refreshes it with the address. Never printed, never in git.
    writeJson(path.join(runDir, 'secret.json'), { email: '', password: accountPassword });

    // -- create the fingerprint profile with the residential proxy
    phase = 'profile';
    const proxyInfo = {
      moduleId: 0,
      proxyMethod: 'custom',
      proxyCategory: 'SOCKS5',
      ipType: 'IPV4',
      protocol: 'SOCKS5',
      host: PROXY_HOST,
      port: proxyPort,
      proxyUserName: PROXY_USER,
      proxyPassword: PROXY_PASS,
    };
    const createBody = {
      workspaceId: WORKSPACE_ID,
      projectId: PROJECT_ID,
      windowName: `ol-${runId.slice(11, 19).replace(/-/g, '')}`,
      os: 'Windows',
      proxyInfo,
    };
    const createdRes = await roxy('/browser/create', createBody);
    const dirId = String((createdRes.data && (createdRes.data.dirId || createdRes.data.id)) || '');
    if (!dirId) throw new Error('CREATE_NO_DIRID');
    const fingerprint = (createdRes.data && createdRes.data.fingerprint) || null;
    state.profile = { dirId, windowName: createBody.windowName, fingerprint };
    // Geo self-consistency, part 1: the profile itself must be en-US on a US
    // clock, which is what the operator's spec asks for (US exit ->
    // America/New_York, language en-US). Refuse to spend the exit otherwise.
    if (fingerprint) {
      const fpLocale = String(fingerprint.locale || '');
      const fpTz = String(fingerprint.timeZone || '');
      log('profile_geo', { locale: fpLocale, timeZone: fpTz, geoSource: fingerprint.geoSource || '' });
      if (fpLocale && fpLocale !== 'en-US') throw Object.assign(new Error(`PROFILE_LOCALE_${fpLocale}`), { stage: 'geo_mismatch' });
      if (fpTz && !fpTz.startsWith('America/')) throw Object.assign(new Error(`PROFILE_TZ_${fpTz}`), { stage: 'geo_mismatch' });
    }
    log('profile_created', { dirId, windowName: createBody.windowName });
    persist();

    // -- open it and attach over CDP
    phase = 'open';
    const openRes = await roxy('/browser/open', { workspaceId: WORKSPACE_ID, dirId, args: [], forceOpen: true, headless: false });
    const od = openRes.data || {};
    const debugPort = od.debuggingPort || (od.debuggerAddress ? Number(String(od.debuggerAddress).split(':').pop()) : 0);
    if (!debugPort) throw new Error('OPEN_NO_DEBUG_PORT');
    opened = { dirId, port: debugPort };
    state.debug_port = debugPort;
    log('profile_open', { port: debugPort, pid: od.pid || null, boot: Boolean(od.alreadyRunning) ? 'reused' : 'launched' });

    browser = await chromium.connectOverCDP(`http://127.0.0.1:${debugPort}`, { timeout: 30000 });
    const context = browser.contexts()[0];
    if (!context) throw new Error('CDP_NO_CONTEXT');
    page = context.pages()[0] || await context.newPage();
    log('cdp_attached', { contexts: browser.contexts().length, pages: context.pages().length, url: page.url().slice(0, 80) });
    startShots(page);

    // Geo self-consistency, part 2: what the page itself reports once attached.
    try {
      const seen = await page.evaluate(() => ({
        language: navigator.language,
        languages: (navigator.languages || []).slice(0, 3),
        timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        ua: navigator.userAgent,
        webdriver: navigator.webdriver,
      }));
      state.client_geo = seen;
      log('client_geo', seen);
      if (seen.timeZone && !String(seen.timeZone).startsWith('America/')) {
        throw Object.assign(new Error(`CLIENT_TZ_${seen.timeZone}`), { stage: 'geo_mismatch' });
      }
    } catch (error) {
      if (error && error.stage === 'geo_mismatch') throw error;
      log('warn', `client_geo_unavailable ${String(error).slice(0, 100)}`);
    }

    // -- the actual signup
    const outcome = await runFlow(page);
    created = Boolean(outcome.created);
    state.stage = created ? 'created' : (state.verdict || 'unconfirmed');
    state.ok = created;

    // -- persist the account
    if (created) {
      fs.mkdirSync(path.dirname(PRODUCT_FILE), { recursive: true });
      fs.appendFileSync(PRODUCT_FILE, `${state.email}----${accountPassword}\n`, 'utf8');
      log('product_written', PRODUCT_FILE);
    }

    // -- traffic estimate from the page's own resource timings
    try {
      const traffic = await page.evaluate(() => {
        const rows = performance.getEntriesByType('resource');
        let bytes = 0; let n = 0;
        for (const r of rows) { if (r.transferSize > 0) { bytes += r.transferSize; n += 1; } }
        return { resources: n, bytes };
      });
      state.traffic = traffic;
      log('traffic_page_estimate', traffic);
    } catch { /* ignore */ }
  } catch (error) {
    fail(state.stage === 'boot' ? 'flow' : state.stage, error);
    if (error && error.stage) state.stage = error.stage;
    log('failed', { stage: state.stage, error: state.error });
    if (page) await screenshotNow(page, 'failure').catch(() => {});
    if (page) await dumpInventory(page, 'zz-failure').catch(() => {});
  } finally {
    await stopShots();
    if (page) {
      try { state.final_url = state.final_url || page.url(); } catch { /* ignore */ }
      try { state.final_text = state.final_text || (await bodyText(page)).slice(0, 800); } catch { /* ignore */ }
    }
    if (browser) await browser.close().catch(() => {});
    if (opened) {
      try {
        await roxy('/browser/close', { workspaceId: WORKSPACE_ID, dirId: opened.dirId });
        log('profile_closed', opened.dirId);
      } catch (error) { log('close_failed', String(error).slice(0, 160)); }
      if (created) {
        try {
          await roxy('/browser/delete', { workspaceId: WORKSPACE_ID, dirIds: [opened.dirId] });
          log('profile_deleted', opened.dirId);
        } catch (error) { log('delete_failed', String(error).slice(0, 160)); }
      }
    }
    state.finished_at = new Date().toISOString();
    state.elapsed_s = elapsed();
    if (!state.terminal) state.terminal = created ? 'created' : 'other';
    state.summary = created
      ? `CREATED ${state.email} in ${state.elapsed_s}s`
      : `STOPPED terminal=${state.terminal} verdict=${state.verdict || 'n/a'} stage=${state.stage} ${state.error}`;
    log('done', state.summary);

    persist();
    const exitCode = created ? 0 : 2;
    console.log(JSON.stringify({
      ok: state.ok, stage: state.stage, terminal: state.terminal, verdict: state.verdict || '',
      email: state.email, domain: state.domain || '', taken: state.taken || [],
      elapsed_s: state.elapsed_s, egress: state.egress_cc ? { cc: state.egress_cc, tz: state.egress_tz } : null,
      challenge: state.challenge, profile: state.profile ? { dirId: state.profile.dirId } : null,
      run_dir: runDir, shots: shotCount, error: state.error,
      final_url: state.final_url || '', final_text: String(state.final_text || '').slice(0, 300),
      password_written: created,
    }));
    process.exit(exitCode);
  }
})();
