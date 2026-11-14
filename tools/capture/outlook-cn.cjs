// CN-NATIVE BUILD -- drive ONE Outlook signup from the operator's own Chinese
// residential line (China Unicom / Shaoxing), with NO proxy of any kind, using
// the operator's installed real Chrome, entirely in a zh-CN environment.
//
// The premise being tested: stop fighting the environment. The exit is a real
// consumer broadband line in China, so the browser speaks Chinese, the clock is
// Asia/Shanghai, and every geo signal the signup can read agrees with itself.
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
//   node tools/capture/outlook-cn.cjs [--runs .cache/outlook-cn]
//        [--hold-ms 7000] [--flow-budget-s 720]
//   No proxy flags exist in this build on purpose: the exit IS the machine.
//
// Prints progress + a JSON summary. The password is NEVER printed; it is
// written to the product file (and a private recovery file) only.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
// DISCRIMINATOR BUILD: same flow, same exit, but the browser stack is the only
// variable that changes. No Roxy/Forge profile, no connectOverCDP, no JS
// patches of any kind -- patchright drives the locally installed, real Chrome.
const PATCHRIGHT_PATH = 'D:/appleservices-family-private/.tools/js/node_modules/patchright-core';
const { chromium } = require(PATCHRIGHT_PATH);
const PATCHRIGHT_VERSION = (() => {
  try { return require(`${PATCHRIGHT_PATH}/package.json`).version; } catch { return 'unknown'; }
})();

// ------------------------------------------------------------------ config
// NO PROXY BLOCK. This build talks to Microsoft over the operator's own line.

// Route A keeps everything on Microsoft's own domain: the address is
// <username>@outlook.com, so there is no mailbox service to talk to.
const OUTLOOK_DOMAIN = 'outlook.com';
const USERNAME_MIN_LEN = 10;
const USERNAME_MAX_LEN = 12;
// "Taken?" retries: at most MAX_USERNAME_ATTEMPTS submissions of a name in the
// one session, each after a human pause. More than that is just hammering.
const MAX_USERNAME_ATTEMPTS = 3;

// CN-native product file.
const PRODUCT_FILE = 'D:/outlook-recon/产出/outlook-cn.txt';
// Route A entry. signup.live.com/signup?lic=1 is the "enter your own email"
// variant and has no "Get a new email address" link any more (verified against
// the live DOM), so it can only end in an external mailbox. The Outlook
// create-account prompt lands on the Microsoft-domain variant instead:
//   signup.live.com/signup?sru=...&fl=dob,flname,wld
//   "Enter your new email address.  New email [ ... ] @outlook.com  Next"
// with #domainDropdownId preset to @outlook.com and input aria "New email".
const SIGNUP_URL = 'https://outlook.live.com/mail/0/?prompt=create_account';
// Egress probe. China-first ordering: ip-api.com answers over plain http and is
// the most likely of the three to respond from a mainland line; the Chinese
// plain-text service is the backstop when both JSON services are unreachable.
const IP_PROBE_URLS = [
  'http://ip-api.com/json/?fields=status,country,countryCode,city,timezone,isp,query',
  'https://ipinfo.io/json',
];
const IP_PROBE_TEXT_URL = 'https://myip.ipip.net/s';
// This build follows the machine: the exit is a China Unicom residential line
// in Shaoxing, so country/zone/locale must all say China and Asia/Shanghai.
// Nothing here asserts a foreign geo any more -- the signup, the clock and the
// language now agree because they are the truth, not because they were faked.
const REQUIRED_CC = 'CN';
const REQUIRED_TZ_PREFIX = 'Asia/';
const PROFILE_TZ = 'Asia/Shanghai';
const PROFILE_LOCALE = 'zh-CN';
// A geo probe failure is not a reason to abort a one-shot run (mainland lines
// routinely cannot reach ipinfo.io); a geo *contradiction* is only recorded.
const GEO_STRICT = false;

// ------------------------------------------------------------------- args
const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}
function flag(name) { return argv.includes(`--${name}`); }

// No proxy args in this build: nothing to point anywhere but the default route.
const runRoot = path.resolve(arg('runs', path.join(__dirname, '..', '..', '.cache', 'outlook-cn')));
const runId = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
const runDir = path.join(runRoot, `run-${runId}`);
const shotDir = path.join(runDir, 'shots');
// Hold length is a knob, not a guess: 7000 ms by default, clamped to the
// 6000-8000 ms band. The previous build held 3-4 s while trembling 1-2 px every
// 150 ms, and PX rejected both holds -- a long, perfectly still press is the
// shape this widget is actually graded on.
const holdMsFixed = (() => {
  const v = Number(arg('hold-ms', '7000'));
  if (!Number.isFinite(v) || v <= 0) return 7000;
  return Math.min(8000, Math.max(6000, Math.round(v)));
})();
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
  proxy: 'none (direct, China Unicom residential line)',
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
// `label` may be a string or an array of candidates: a zh-CN document renders
// the month as "一月" while an en-US one renders "January", and this build has
// to work on whichever it gets.
function optionMatches(text, wanted) {
  if (!text || !wanted) return false;
  // zh-CN renders the day list as "1日".."31日" and the month as "6月", so the
  // unit suffix has to come off before a numeric compare can succeed.
  const norm = (s) => String(s).replace(/\s+/g, '').replace(/\.$/, '').replace(/[日号月]$/, '');
  const a = norm(text);
  const b = norm(wanted);
  if (a === b) return true;
  if (/^\d+$/.test(a) && /^\d+$/.test(b)) return Number(a) === Number(b);
  return false;
}

async function pickTrigger(page, triggerSelector, { index = -1, label = '' } = {}) {
  const wanted = Array.isArray(label) ? label.filter(Boolean) : (label ? [label] : []);
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
  if (wanted.length && count) {
    const seen = [];
    for (let i = 0; i < Math.min(count, 40); i += 1) {
      const item = options.nth(i);
      const text = (await item.innerText().catch(() => '') || '').trim();
      seen.push(text);
      if (!wanted.some((w) => optionMatches(text, w))) continue;
      if (await item.isVisible().catch(() => false)) {
        await humanClick(page, item, 'option');
        await sleep(rand(500, 1000));
        log('pick', { trigger: triggerSelector, mode: 'text', picked: text });
        return text;
      }
    }
    log('pick_miss', { trigger: triggerSelector, wanted: wanted.join('|'), seen: seen.slice(0, 14).join('|') });
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

// Bilingual month candidates: whichever language the document is in.
const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June', 'July',
  'August', 'September', 'October', 'November', 'December'];
const MONTH_NAMES_ZH = ['一月', '二月', '三月', '四月', '五月', '六月', '七月',
  '八月', '九月', '十月', '十一月', '十二月'];
function monthLabels(i) {
  return [MONTH_NAMES[i], MONTH_NAMES_ZH[i], `${i + 1}月`].filter(Boolean);
}

// "Add some details": Country/Region arrives prefilled from the exit IP;
// Month/Day are triggers and Year is the only real text input on the page.
async function fillDetails(page) {
  const year = String(randi(1972, 1998));
  const monthIndex = randi(0, 11);
  const day = randi(2, 27);
  // Country/Region normally arrives prefilled from the exit IP -- on this line
  // that is China, which is exactly right, so the build does not touch it. Only
  // an empty/broken prefill is repaired, and then to China, not to anything the
  // exit cannot back up.
  const countryTrigger = await anyVisible(page, ['#CountryDropdown', '[name="Country"]', 'select[name="Country"]'], 4000);
  let country = '';
  if (countryTrigger) {
    country = (await countryTrigger.locator.innerText().catch(() => '') || '').trim();
    if (!country) country = (await countryTrigger.locator.inputValue().catch(() => '') || '').trim();
    log('country_prefill', { text: country });
    if (!country && countryTrigger.selector.includes('select')) {
      await countryTrigger.locator.selectOption({ label: 'China' }).catch(() => {});
      country = 'China';
    } else if (!country) {
      await pickTrigger(page, countryTrigger.selector, { label: ['中国', 'China', '中国大陆'] });
      country = (await countryTrigger.locator.innerText().catch(() => '') || '').trim();
    }
    await sleep(rand(400, 900));
  }
  const yearField = await anyVisible(page, ['input[name="BirthYear"]', '[name="BirthYear"]'], 8000);
  if (yearField) await typeHuman(page, yearField.locator, year, 'birth_year');
  // The listbox index is a fallback only: text match on the bilingual month name
  // hits first. If it (or the day) lands on the wrong row it is repaired below.
  await pickTrigger(page, '#BirthMonthDropdown', { index: monthIndex + 1, label: monthLabels(monthIndex) });
  await sleep(rand(400, 900));
  await pickTrigger(page, '#BirthDayDropdown', { index: day - 1, label: [String(day), `${day}日`, `${day}号`, String(day).padStart(2, '0')] });
  await sleep(rand(500, 1100));
  const monthText = (await page.locator('#BirthMonthDropdown').first().innerText().catch(() => '') || '').trim();
  const dayText = (await page.locator('#BirthDayDropdown').first().innerText().catch(() => '') || '').trim();
  const yearValue = await page.locator('input[name="BirthYear"]').first().inputValue().catch(() => '');
  const monthOk = monthLabels(monthIndex).some((m) => optionMatches(monthText, m));
  const dayOk = optionMatches(dayText, String(day));
  if (!monthOk || !dayOk) {
    log('warn', `details readback mismatch :: month="${monthText}"(want ${monthLabels(monthIndex).join('/')}) day="${dayText}"(want ${day})`);
  }
  state.details = { country, month: monthText, day: dayText, year: yearValue, month_ok: monthOk, day_ok: dayOk };
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
  // ---- DISCRIMINATOR WINDOW ------------------------------------------------
  // The Roxy arm was sealed exactly 3-4s after this click: no press widget, no
  // error banner, straight to "Account creation has been blocked". Poll every
  // frame hard for 7s and record what was actually on screen, so the report can
  // state whether PX ever drew anything instead of inferring it from the verdict.
  const nameT0 = Date.now();
  const samples = [];
  while (Date.now() - nameT0 < 7000) {
    let s = null;
    try { s = await scanChallenge(page); } catch { /* mid-navigation */ }
    if (s) {
      const rows = (s.rows || []).map((r) => ({
        frame: String(r.frameUrl || '').replace(/^https?:\/\//, '').slice(0, 70),
        has_px: Boolean(r.hasPx),
        px_box: r.pxBox || null,
        press_labels: (r.labels || []).map((l) => l.box),
        draws: (r.draws || []).length,
        text_hit: Boolean(r.textHit),
      }));
      samples.push({ t: Math.round((Date.now() - nameT0) / 100) / 10, kind: s.kind, press_box: s.pressBox || null, rows });
      if (s.kind === 'press-visible') break;
    }
    await sleep(350);
  }
  const kinds = [...new Set(samples.map((x) => x.kind))];
  const pxFrames = [...new Set(samples.flatMap((x) => x.rows.filter((r) => r.has_px).map((r) => r.frame)))];
  const sizedPx = samples.some((x) => x.rows.some((r) => r.has_px && r.px_box && r.px_box.w > 8 && r.px_box.h > 8));
  state.px_window = {
    duration_s: samples.length ? samples[samples.length - 1].t : 0,
    polls: samples.length,
    kinds,
    px_frames: pxFrames,
    px_captcha_sized: sizedPx,
    press_visible: kinds.includes('press-visible'),
    press_text: kinds.includes('press-text'),
    px_idle: kinds.includes('px-idle'),
    first_samples: samples.slice(0, 6),
  };
  log('px_window', {
    dur_s: state.px_window.duration_s,
    polls: samples.length,
    kinds: kinds.join('|'),
    px_frames: pxFrames.join(','),
    px_captcha_sized: sizedPx,
  });
  if (kinds.includes('press-visible')) await screenshotNow(page, 'px-press');
  await sleep(rand(400, 900));
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
// A zh-CN PerimeterX document says 按住 (press and hold), and its accessible
// label is 可访问性挑战 ("accessibility challenge") on some builds. Both have to
// be recognised or the widget renders, gets ignored, and the run dies of a
// timeout that looks like something else. The regexes below are deliberately
// bilingual and cover the arena frame (hsprotect / px-cdn) that carries them.
const PRESS_ARIA_RE = /press|hold|按压|按住|长按|可访问|挑战/i;
const PRESS_TEXT_RE = /press\s*&?\s*hold|robot or human|verify you are human|hold the button|按住|长按|请长按|验证您是人类|确认您是人类|人类验证|可访问性挑战|机器人|人机验证/i;
const BLOCK_TEXT_RE = /access to this page has been denied|px-captcha-error|已拒绝访问此页面|访问此页面已被拒绝|您的访问已被拒绝/i;

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
  const ariaRe = /press|hold|按压|按住|长按|可访问|挑战/i;
  const labels = [];
  for (const el of document.querySelectorAll('[aria-label]')) {
    const aria = el.getAttribute('aria-label') || '';
    if (ariaRe.test(aria)) {
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
    textHit: /press\s*&?\s*hold|robot or human|verify you are human|hold the button|按住|长按|请长按|验证您是人类|确认您是人类|人类验证|可访问性挑战|人机验证/i.test(body),
    blocked: /access to this page has been denied|px-captcha-error|已拒绝访问此页面|访问此页面已被拒绝|您的访问已被拒绝/i.test(body),
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

// The HUMAN pill is 360x42: [可访问性挑战 badge][按住]. Its geometric centre is
// empty pill surface, so "press the capsule centre" (what the previous build
// did) misses the label by ~90 px horizontally. The press point has to come from
// the TEXT NODE of the label inside the pill, measured inside the iframe and
// translated into the main frame with that same capsule's boundingBox.
const CAPSULE_SELECTORS = [
  '#px-captcha',
  '[aria-label*="按住"]',
  '[aria-label*="长按"]',
  '[aria-label*="Press" i]',
  '[aria-label*="Hold" i]',
  '[aria-label*="可访问性挑战"]',
  '#px-captcha .draw',
  '.draw',
  '[role="button"]',
];

// Runs INSIDE one frame. Returns every visible pill and every visible text node
// that says 按住 / press, all in FRAME coordinates. The label does not have to
// live in the same document as the pill (on this widget the pill wrapper and the
// label sit behind different nested hsprotect frames), so the two lists are
// independent and only joined when they overlap.
function probePressText({ selectors }) {
  const rn = (n) => Math.round(n * 10) / 10;
  const vis = (el) => {
    if (!el || el.nodeType !== 1) return false;
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    if (Number(st.opacity || '1') === 0) return false;
    if (st.clip && st.clip !== 'auto') return false;
    const r = el.getBoundingClientRect();
    return r.width >= 2 && r.height >= 2;
  };
  const capsules = [];
  for (const sel of selectors) {
    let list = [];
    try { list = [...document.querySelectorAll(sel)]; } catch { continue; }
    list.forEach((el, index) => {
      if (!vis(el)) return;
      const r = el.getBoundingClientRect();
      if (r.width < 8 || r.height < 8) return;
      capsules.push({
        selector: sel, index,
        rect: { x: rn(r.x), y: rn(r.y), w: rn(r.width), h: rn(r.height) },
        tag: el.tagName, id: el.id || '', aria: (el.getAttribute('aria-label') || '').slice(0, 40),
      });
    });
  }
  const labels = [];
  const root = document.body || document.documentElement;
  if (!root) return { capsules, labels, chosen: null, frameText: '' };
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, null);
  let node;
  while ((node = walker.nextNode())) {
    const raw = (node.nodeValue || '').replace(/\u00a0/g, ' ');
    const t = raw.trim();
    if (!t) continue;
    if (!/按住|长按|press|hold/i.test(t)) continue;
    const parent = node.parentElement;
    if (!parent || !vis(parent)) continue;
    const range = document.createRange();
    range.selectNodeContents(node);
    for (const rect of range.getClientRects()) {
      if (rect.width < 6 || rect.height < 6) continue;
      const box = { x: rn(rect.x), y: rn(rect.y), w: rn(rect.width), h: rn(rect.height) };
      const center = { x: rn(rect.x + rect.width / 2), y: rn(rect.y + rect.height / 2) };
      const hit = document.elementFromPoint(center.x, center.y);
      labels.push({
        text: t.slice(0, 30), box, center,
        exact: /^(按住|长按|press|hold)$/i.test(t),
        hit_tag: hit ? hit.tagName : '',
        hit_in_capsule: Boolean(hit && hit.closest && hit.closest('#px-captcha')),
      });
    }
  }
  // Rank: a label inside a visible pill beats a naked one; an exact 按住 beats a
  // sentence that merely mentions 长按; among equals the shortest text wins (the
  // screen-reader copy of the whole sentence is the long one).
  let chosen = null;
  for (const l of labels) {
    const cap = capsules.find((c) => l.box.x >= c.rect.x - 10 && l.box.y >= c.rect.y - 10
      && l.box.x + l.box.w <= c.rect.x + c.rect.w + 10 && l.box.y + l.box.h <= c.rect.y + c.rect.h + 10);
    if (!cap && !l.exact) continue; // page copy such as 长按该按钮。 is not the button
    const score = (cap ? 0 : 100) + (l.exact ? 0 : 50) + l.text.length;
    if (!chosen || score < chosen.score) chosen = { score, label: l, capsule: cap || null };
  }
  return {
    capsules, labels, chosen,
    frameText: ((document.body && document.body.innerText) || '').replace(/\s+/g, ' ').slice(0, 160),
  };
}

async function locatePressTarget(page) {
  const notes = [];
  const found = [];
  const depthOf = (frame) => { let d = 0; let f = frame; while (f.parentFrame && f.parentFrame()) { d += 1; f = f.parentFrame(); } return d; };
  for (const frame of await framesOf(page)) {
    let probe;
    try { probe = await frame.evaluate(probePressText, { selectors: CAPSULE_SELECTORS }); } catch { continue; }
    if (!probe) continue;
    if (probe.chosen) found.push({ frame, probe, chosen: probe.chosen, depth: depthOf(frame) });
    else if ((probe.capsules || []).length || (probe.labels || []).length) {
      notes.push({ frame: frame.url().slice(0, 70), capsules: (probe.capsules || []).length, labels: (probe.labels || []).length });
    }
  }
  // Global ranking, not first-frame-wins: the pill wrapper and the label can sit
  // in different nested hsprotect frames, and the outer one also renders the
  // 可访问性挑战 badge that must never be mistaken for the button.
  found.sort((a, b) => (a.chosen.score - b.chosen.score) || (b.depth - a.depth));
  for (const f of found) {
    const frame = f.frame;
    // Frame origin in page coordinates: works at any iframe nesting depth and
    // needs no DOM mutation on the widget.
    let origin = await frame.locator('html').first().boundingBox().catch(() => null);
    if (!origin) origin = await frame.locator('body').first().boundingBox().catch(() => null);
    if (!origin) continue;
    const c = f.chosen;
    const cap = c.capsule;
    const capsulePage = cap ? {
      x: r2(cap.rect.x + origin.x), y: r2(cap.rect.y + origin.y),
      w: r2(cap.rect.w), h: r2(cap.rect.h),
    } : null;
    return {
      frame, frameUrl: frame.url(), depth: f.depth,
      selector: cap ? `${cap.selector}#${cap.index}` : 'text-node',
      capsule: cap ? cap.rect : null,
      capsulePage,
      frameOrigin: { x: r2(origin.x), y: r2(origin.y) },
      text: c.label.text, exact: Boolean(c.label.exact), score: c.score,
      textNodeBox: c.label.box, textNodeCenterFrame: c.label.center,
      point: { x: r2(c.label.center.x + origin.x), y: r2(c.label.center.y + origin.y) },
      fallbackPoint: capsulePage
        ? { x: r2(capsulePage.x + capsulePage.w / 2), y: r2(capsulePage.y + capsulePage.h / 2) }
        : { x: r2(c.label.center.x + origin.x), y: r2(c.label.center.y + origin.y) },
      labels_seen: (f.probe.labels || []).length,
      frameText: f.probe.frameText,
    };
  }
  if (notes.length) log('press_target_notes', notes);
  return null;
}

// Self-check before a press is spent: the point must be inside the viewport and
// the top document must hit an hsprotect iframe there -- i.e. the press really
// lands on the widget, not on dead page furniture.
async function checkPressPoint(page, point) {
  const vp = await page.evaluate(() => ({ w: window.innerWidth, h: window.innerHeight })).catch(() => ({ w: 0, h: 0 }));
  const inView = point.x > 4 && point.y > 4 && point.x < vp.w - 4 && point.y < vp.h - 4;
  const hit = await page.evaluate(({ x, y }) => {
    const el = document.elementFromPoint(x, y);
    if (!el) return null;
    let src = '';
    let cur = el;
    for (let i = 0; i < 8 && cur; i += 1) {
      if (cur.tagName === 'IFRAME' || cur.tagName === 'FRAME') { src = cur.getAttribute('src') || ''; break; }
      cur = cur.parentElement;
    }
    return { tag: el.tagName, id: el.id || '', cls: String(el.className || '').slice(0, 50), src: String(src).slice(0, 160) };
  }, point).catch(() => null);
  const hitHs = Boolean(hit && /hsprotect|ch_ctx|perimeterx|px-captcha/i.test(hit.src || ''));
  return { in_view: inView, viewport: vp, hit, hit_hsprotect: hitHs, ok: inView && hitHs };
}

// ONE look at the widget while the mouse is down: a single evaluate, 1500 ms
// into the hold. Nothing else touches the frame during the hold: the periodic
// screenshotter is paused and no mouse move is issued.
async function holdProbeOnce(target) {
  const t = Date.now();
  try {
    const r = await target.frame.evaluate(() => ({
      text: ((document.body && document.body.innerText) || '').replace(/\s+/g, ' ').slice(0, 140),
      has_px: Boolean(document.querySelector('#px-captcha')),
    }));
    return { took_ms: Date.now() - t, ...r };
  } catch (e) {
    return { took_ms: Date.now() - t, error: String(e).slice(0, 90) };
  }
}

// Press-and-hold, in the shape this widget is graded on:
//   approach -> hover -> mouse.down -> PERFECTLY STILL for 7 s (no jitter, no
//   moves, no frame work except ONE look at +1500 ms) -> mouse.up -> poll.
// Round 2 is only a fallback: an 800 ms tap collapses the two-segment pill to a
// single 按住, then 800 ms later an 8 s still hold on the re-located label.
// Two rounds total. If the widget survives both, the run ends as
// challenge_failed: hammering it further is exactly what turns a soft challenge
// into a hard block on this line.
async function clearChallenge(page, rounds = 2) {
  const attemptsLog = [];
  const plan = [
    { round: 1, mode: 'still', hold_ms: holdMsFixed },
    { round: 2, mode: 'tap-then-still', tap_ms: 800, gap_ms: 800, hold_ms: Math.max(8000, holdMsFixed) },
  ].slice(0, Math.max(1, rounds));

  let scan = await scanChallenge(page);
  if (scan.kind === 'none' || scan.kind === 'px-idle') {
    return { passed: true, kind: scan.kind, attempts: 0, attempts_log: attemptsLog };
  }
  if (scan.kind === 'arkose' || scan.kind === 'block-page') {
    return { passed: false, kind: scan.kind, attempts: 0, note: 'non-press challenge', attempts_log: attemptsLog };
  }

  for (const step of plan) {
    scan = await scanChallenge(page);
    if (scan.kind === 'none' || scan.kind === 'px-idle') {
      return { passed: true, kind: scan.kind, attempts: attemptsLog.length, attempts_log: attemptsLog };
    }

    let target = await locatePressTarget(page);
    for (let i = 0; i < 10 && !target; i += 1) { await sleep(700); target = await locatePressTarget(page); }
    if (!target) {
      return { passed: false, kind: scan.kind, attempts: attemptsLog.length, note: 'no press target', attempts_log: attemptsLog };
    }

    let point = target.point;
    let pointSource = 'label-text-node-center';
    const check = await checkPressPoint(page, point);
    if (!check.ok) {
      const fb = await checkPressPoint(page, target.fallbackPoint);
      if (fb.ok) {
        log('press_selfcheck_fallback', { round: step.round, primary: check, fallback: fb });
        point = target.fallbackPoint;
        pointSource = 'capsule-center-fallback';
      } else {
        log('press_selfcheck_fail', { round: step.round, point, primary: check, fallback: fb });
        attemptsLog.push({ round: step.round, mode: step.mode, note: 'selfcheck-failed', primary: check, fallback: fb });
        continue;
      }
    }

    log('press_start', {
      round: step.round, mode: step.mode, hold_ms: step.hold_ms,
      selector: target.selector,
      frame: String(target.frameUrl || '').replace(/^https?:\/\//, '').slice(0, 70),
      text: target.text, exact_label: target.exact, point, point_source: pointSource,
      capsule_page: target.capsulePage, label_box_frame: target.textNodeBox,
      label_center_frame: target.textNodeCenterFrame,
      frame_origin: target.frameOrigin, labels_seen: target.labels_seen,
      selfcheck: {
        in_view: check.in_view, viewport: check.viewport,
        hit_tag: check.hit && check.hit.tag, hit_src: check.hit && check.hit.src,
        hit_hsprotect: check.hit_hsprotect,
      },
      frame_text_before: target.frameText,
    });
    await screenshotNow(page, `press-before-r${step.round}`);

    // Hover, then press exactly once: the pointer is not touched again until up.
    await glideTo(page, point, 30);
    await sleep(rand(220, 420));
    await page.mouse.move(point.x, point.y);
    await sleep(rand(120, 300));

    let tapMs = 0;
    if (step.mode === 'tap-then-still') {
      // the short tap is what collapses the two-segment pill to a single 按住
      shotsPaused = true;
      await page.mouse.down();
      await sleep(step.tap_ms);
      await page.mouse.up();
      shotsPaused = false;
      tapMs = step.tap_ms;
      await sleep(step.gap_ms);
      const again = await locatePressTarget(page);
      if (again) {
        point = again.point;
        pointSource = `${pointSource}->relocated`;
      }
      await page.mouse.move(point.x, point.y);
      await sleep(rand(140, 280));
      log('press_tap_done', { round: step.round, tap_ms: tapMs, gap_ms: step.gap_ms, point, point_source: pointSource });
    }

    shotsPaused = true; // no periodic screenshots during the hold
    await page.mouse.down();
    const holdStart = Date.now();
    await sleep(1500);
    const mid = await holdProbeOnce(target); // the ONLY frame work during the hold
    const remain = step.hold_ms - (Date.now() - holdStart);
    if (remain > 0) await sleep(remain);
    const held = Date.now() - holdStart;
    await page.mouse.up();
    shotsPaused = false;

    // after the release the widget either fades out or repaints: poll briefly
    const settleStart = Date.now();
    let after = await scanChallenge(page);
    const polls = [{ ms: 0, kind: after.kind }];
    while ((after.kind === 'press-visible' || after.kind === 'press-text') && Date.now() - settleStart < 9000) {
      await sleep(700);
      after = await scanChallenge(page);
      polls.push({ ms: Date.now() - settleStart, kind: after.kind });
    }
    await screenshotNow(page, `press-after-r${step.round}`);
    const before = String(target.frameText || '').slice(0, 90);
    const during = String(mid.text || '').slice(0, 90);
    const row = {
      round: step.round, mode: step.mode, tap_ms: tapMs, held_ms: held,
      point, point_source: pointSource,
      mid: { at_ms: 1500, took_ms: mid.took_ms, text: mid.text || '', has_px: mid.has_px, error: mid.error },
      state_changed_during_hold: Boolean(before && during && before !== during),
      text_before: before, text_during: during,
      after: after.kind, polls,
    };
    attemptsLog.push(row);
    log('press_verdict', row);
    if (after.kind === 'none' || after.kind === 'px-idle') {
      return { passed: true, kind: after.kind, attempts: step.round, held_ms: held, attempts_log: attemptsLog };
    }
    if (step.round < plan.length) {
      // a beat of human indecision before the one fallback round
      await mouseWander(page, 1);
      await sleep(rand(1800, 3200));
    }
  }
  const finalScan = await scanChallenge(page);
  return {
    passed: false, kind: finalScan.kind, attempts: plan.length,
    note: `still challenged after ${plan.length} rounds (static hold + tap-then-hold)`,
    attempts_log: attemptsLog,
  };
}

// --------------------------------------------------------------- artefacts
let shooting = false;
let shotCount = 0;
let shotTimer = null;
// The hold must not be disturbed: no screenshot, no compositing churn and no
// frame work while the button is down (a 4 s timer would otherwise fire twice
// inside a 7 s hold).
let shotsPaused = false;
function startShots(page) {
  shotTimer = setInterval(async () => {
    if (shooting || shotsPaused) return;
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
  'input[aria-label*="新电子邮件"]',
  'input[aria-label*="新邮箱"]',
  'input[type="email"]',
  'input[name="MemberName"]',
  '#MemberName',
  'input[aria-label*="email" i]',
  'input[aria-label*="mail" i]',
  'input[aria-label*="电子邮件"]',
  'input[placeholder*="@" i]',
];

const NEXT_SELECTORS = [
  '[data-testid="primaryButton"]',
  '#iNext',
  'button[type="submit"]',
  'input[type="submit"]',
  'button:has-text("Next")',
  'button:has-text("下一步")',
  'button:has-text("创建帐户")',
  'button:has-text("创建账户")',
];

const PASSWORD_SELECTORS = [
  'input[type="password"]',
  '#Password',
  'input[name="Password"]',
  '#passwordInput',
  'input[aria-label*="password" i]',
  'input[aria-label*="密码"]',
  'input[aria-label*="新密码"]',
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
  'a:has-text("获取新的电子邮件")',
  'text=获取新的电子邮件地址',
];

// The domain picker next to the name box. Default is @outlook.com, but if the
// profile/UI ever lands on @hotmail.com we switch it back before typing.
const DOMAIN_SELECTORS = [
  '#domainDropdownId',
  '[aria-label*="domain" i]',
  '[aria-label*="域"]',
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
  '已被他人',
  '已被采用',
  '地址不可用',
  '此名称不可用',
  '请使用其他',
  '请改用其他',
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

// ---------------------------------------------- Chinese market consent gate
// A zh-CN signup (mkt=ZH-CN / lc=2052) is fronted by "个人数据导出许可": before
// any address field exists, Microsoft asks the user to agree that their personal
// data is transferred outside China. The live DOM measured on this line:
//   title  个人数据导出许可
//   button#nextButton[data-testid=primaryButton]      同意并继续
//   button#cancelButton[data-testid=secondaryButton]  拒绝并退出
// A real user clicks 同意并继续, so that is what this build does. Without this
// step the address form is never rendered and the run dies at entry_variant --
// which is exactly what the previous attempt did.
const CONSENT_TEXT_RE = /个人数据导出许可|数据导出许可|数据出境|转移到中国境外|转到中国境外|传输到中国境外|Personal data export|data export consent/i;
const CONSENT_ACCEPT_RE = /^(同意并继续|同意|接受并继续|接受|继续|我同意|下一步|Agree and continue|Agree|Accept and continue|Accept|Continue|I agree)$/i;
const CONSENT_DECLINE_RE = /拒绝|不同意|退出|Decline|Cancel|Exit/i;

async function acceptConsent(page) {
  let text = '';
  try { text = await bodyText(page); } catch { /* mid-navigation */ }
  if (!CONSENT_TEXT_RE.test(text)) return null;
  const candidates = ['#nextButton', '[data-testid="primaryButton"]', 'button[type="submit"]'];
  for (const selector of candidates) {
    const hit = await anyVisible(page, [selector], 5000);
    if (!hit) continue;
    const label = ((await hit.locator.innerText().catch(() => '')) || '').trim();
    if (CONSENT_DECLINE_RE.test(label)) continue;
    if (selector !== '#nextButton' && !CONSENT_ACCEPT_RE.test(label)) continue;
    await humanClick(page, hit.locator, 'consent_accept');
    log('click', { label: 'consent_accept', selector, text: label.slice(0, 40) });
    return { selector, label, text_ok: true };
  }
  log('warn', 'consent gate seen but no accept button matched');
  return { selector: '', label: '', text_ok: false };
}

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

// ------------------------------------------------------------ egress probe
// The egress probe runs in its own throwaway page. Doing it in the signup page
// is what killed the first CN attempt: a probe navigation that Playwright had
// already given up on was still in flight, and it aborted the goto() for the
// signup URL ("interrupted by another navigation"). A separate page cannot
// compete with the signup page for the navigation queue, and it is closed
// before the signup page is used at all.
const EGRESS_TRIES = [
  { url: 'https://ipinfo.io/json', kind: 'json' },
  { url: 'https://myip.ipip.net/', kind: 'ipip' },
  { url: 'http://ip-api.com/json/?fields=status,country,countryCode,city,timezone,isp,query', kind: 'json' },
];

async function probeEgress(context) {
  for (const probe of EGRESS_TRIES) {
    let probePage = null;
    try {
      probePage = await context.newPage();
      await probePage.goto(probe.url, { waitUntil: 'domcontentloaded', timeout: 8000 });
      const raw = ((await probePage.textContent('body')) || '').trim().replace(/\s+/g, ' ');
      const ip = (raw.match(/(\d{1,3}(?:\.\d{1,3}){3})/) || [])[1] || '';
      if (probe.kind === 'json') {
        let parsed = null;
        try { parsed = JSON.parse(raw); } catch { parsed = null; }
        if (!parsed || parsed.status === 'fail' || (!parsed.countryCode && !parsed.country)) {
          log('egress_probe_empty', { url: probe.url, sample: raw.slice(0, 80) });
          continue;
        }
        log('egress_probe_ok', { url: probe.url, countryCode: parsed.countryCode || '', ip: parsed.query || parsed.ip || '' });
        return parsed;
      }
      if (!ip) { log('egress_probe_empty', { url: probe.url, sample: raw.slice(0, 80) }); continue; }
      const isCn = /中国|China/i.test(raw) || probe.kind === 'ipip-s';
      const isp = (/联通|Unicom/i.test(raw) ? 'CHINA UNICOM' : '') || (/电信|Telecom/i.test(raw) ? 'CHINA TELECOM' : '') || (/移动|Mobile/i.test(raw) ? 'CHINA MOBILE' : '');
      log('egress_probe_ok', { url: probe.url, ip, isp, raw: raw.slice(0, 120) });
      return {
        country: isCn ? '中国' : '', countryCode: isCn ? 'CN' : '',
        city: (/绍兴/.test(raw) ? 'Shaoxing' : ''), timezone: isCn ? 'Asia/Shanghai' : '',
        isp, query: ip, raw: raw.slice(0, 160), source: probe.url,
      };
    } catch (error) {
      log('egress_probe_failed', `${probe.url} :: ${String(error).slice(0, 120)}`);
    } finally {
      if (probePage) await probePage.close().catch(() => {});
    }
  }
  return null;
}

async function runFlow(page) {
  // ---- 0. entry ---------------------------------------------------------
  phase = 'entry';
  const ipInfo = await probeEgress(page.context());
  if (ipInfo) {
    state.egress = ipInfo;
    const cc0 = String(ipInfo.countryCode || '').toUpperCase()
      || (String(ipInfo.country || '').length === 2 ? String(ipInfo.country).toUpperCase() : '');
    const cc = cc0;
    const tz = String(ipInfo.timezone || ipInfo.timeZone || '');
    state.egress_cc = cc;
    state.egress_tz = tz;
    state.egress_isp = ipInfo.isp || '';
    log('egress', {
      country: ipInfo.country || '', countryCode: cc, city: ipInfo.city || '',
      timezone: tz, isp: ipInfo.isp || '', ip: ipInfo.query || ipInfo.ip || '',
    });
    // Geo consistency, inverted from the proxy build: the environment is not
    // being faked to match an exit any more -- the exit IS the machine. A
    // mismatch here means the line is not what it is believed to be (hotspot,
    // lingering VPN), which is worth saying out loud and is not a reason to
    // abort a one-shot run. Nothing in this build throws on geo.
    if (cc && cc !== REQUIRED_CC) log('warn', `GEO_COUNTRY_MISMATCH ${cc} != ${REQUIRED_CC} - the line is not the residential CN exit expected`);
    if (tz && !tz.startsWith(REQUIRED_TZ_PREFIX)) log('warn', `GEO_TZ_MISMATCH ${tz} is not ${REQUIRED_TZ_PREFIX}*`);
    if (tz && tz !== PROFILE_TZ) log('warn', `exit zone ${tz} differs from the browser zone ${PROFILE_TZ}`);
    if (GEO_STRICT && cc && cc !== REQUIRED_CC) throw Object.assign(new Error(`GEO_COUNTRY_${cc}`), { stage: 'geo_mismatch' });
  } else {
    log('warn', 'egress geolocation unavailable - continuing without the geo assertion');
  }

  phase = 'signup-load';
  // Two tries, and only for a navigation that was interrupted by another one --
  // that is a local bookkeeping race, not a verdict about the exit, so retrying
  // it costs nothing and never hammers Microsoft (the page had not rendered).
  let signupNavError = null;
  for (let attempt = 1; attempt <= 2; attempt += 1) {
    try {
      await page.goto(SIGNUP_URL, { waitUntil: 'domcontentloaded', timeout: 90000 });
      signupNavError = null;
      break;
    } catch (error) {
      signupNavError = error;
      const msg = String(error);
      log('warn', `signup goto attempt ${attempt} failed :: ${msg.slice(0, 140)}`);
      if (!/interrupted by another navigation/i.test(msg) || attempt === 2) break;
      await page.goto('about:blank', { timeout: 15000 }).catch(() => {});
      await sleep(rand(900, 1800));
    }
  }
  if (signupNavError) throw signupNavError;
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
  // PX presence at entry, before a single keystroke: is PerimeterX loaded on
  // this document at all, which frames are live, and did #px-captcha get a
  // non-zero box yet? This is the baseline the late-window samples compare to.
  {
    let entryScan = null;
    try { entryScan = await scanChallenge(page); } catch { /* ignore */ }
    if (entryScan) {
      const frames = (entryScan.rows || []).map((r) => ({
        frame: String(r.frameUrl || '').replace(/^https?:\/\//, '').slice(0, 70),
        has_px: Boolean(r.hasPx),
        px_box: r.pxBox || null,
        press_labels: (r.labels || []).length,
      }));
      state.px_entry = { kind: entryScan.kind, frames };
      log('px_entry', {
        kind: entryScan.kind,
        interesting: frames.filter((f) => f.has_px || /hsprotect|px[-.]|perimeterx/i.test(f.frame)).slice(0, 8),
      });
    }
  }
  await dumpInventory(page, '01-entry');
  await screenshotNow(page, 'entry');

  let text = await bodyText(page);
  if (/access to this page has been denied|something went wrong|已拒绝访问此页面|出现问题/i.test(text)) {
    throw Object.assign(new Error('ENTRY_BLOCKED'), { stage: 'entry_blocked' });
  }

  // ---- 1. Route A: mint the address on Microsoft's own domain -----------
  // No external mailbox, no verification code: the account address is
  // <username>@outlook.com. Two shapes of this screen exist in the wild:
  //   (a) already the new-address form - "New email" + #domainDropdownId
  //   (b) the old "enter your email" form with a "Get a new email address" link
  phase = 'new-address';
  // The consent gate has to clear before the address form exists at all.
  const consent = await acceptConsent(page);
  if (consent) {
    state.consent = consent;
    state.steps.push('consent_accepted');
    await sleep(rand(1600, 3200));
    await dumpInventory(page, '01b-after-consent');
    await screenshotNow(page, 'after-consent');
  }
  const formReady = await anyVisible(page, [
    '#domainDropdownId',
    'input[aria-label*="New email" i]',
    'input[aria-label*="新电子邮件"]',
    // language-independent shape of the same form: a lone name field plus the
    // domain picker. Included so a zh-CN aria-label this build has not seen yet
    // cannot be mistaken for "no form at all".
    'input[name="MemberName"]',
    '#MemberName',
    'input[type="email"]',
  ], 20000);
  if (formReady) {
    log('address_form', { mode: 'new-address-form', selector: formReady.selector });
  } else {
    let newAddr = null;
    for (const selector of GET_NEW_ADDRESS_SELECTORS) {
      const hit = await anyVisible(page, [selector], 1500);
      if (!hit) continue;
      const label = (await hit.locator.innerText().catch(() => '') || '').trim();
      // guard: only the real "new address" control, not any look-alike element
      if (!/new email address|获取新的电子邮件/i.test(label)) continue;
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
    const res = await clearChallenge(page, 2);
    state.challenge = res;
    log('challenge_early', res);
    if (!res.passed) {
      phase = 'settle';
      state.verdict = 'challenge_failed';
      state.terminal = 'challenge_failed';
      state.final_url = page.url();
      state.final_text = (await bodyText(page)).slice(0, 1200);
      await screenshotNow(page, 'challenge-failed-early');
      await dumpInventory(page, 'zz-challenge-failed');
      return { created: false, verdict: 'challenge_failed' };
    }
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

    if (/account creation has been blocked|unusual activity|异常活动|已阻止创建|阻止了你的帐户创建|我们阻止了|出于安全考虑|违反.*(服务协议|使用条款)/i.test(t)) {
      verdict = 'blocked';
      state.blocked_text = t.slice(0, 500);
      log('verdict_page', { verdict, text: t.slice(0, 200) });
      break;
    }
    if (/your account has been created|account is ready|welcome to outlook|帐户已创建|账户已创建|已创建你的帐户|你的帐户已创建|你的账户已创建/i.test(t)) {
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
    // Post-creation recovery prompt: right after the HUMAN press clears,
    // Microsoft shows 帮助保护你的帐户 / 添加电子邮件 with the brand-new address
    // on top. The account already EXISTS at that point -- the screen is only
    // asking for a recovery address. The previous build had no branch for it,
    // idled until the flow budget expired and reported "timeout" for an account
    // that had already been made.
    if (nameDone
      && /帮助保护你的帐户|保护你的帐户|help protect your account|添加电子邮件|添加辅助电子邮件|recovery email|恢复电子邮件/i.test(t)
      && /@(outlook|hotmail)\.com/i.test(t)) {
      verdict = 'created';
      log('verdict_page', { verdict, reason: 'post-creation recovery-email screen', text: t.slice(0, 160), url: page.url().slice(0, 120) });
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
      const res = await clearChallenge(page, 2);
      state.challenge = res;
      log('challenge_after_name', res);
      await sleep(rand(2000, 4000));
      if (!res.passed) {
        // Two holds failed. This is the end of the run: screenshot the last
        // screen, record the original text, and stop. Re-opening and hammering
        // the widget is exactly what hard-blocks the line.
        await screenshotNow(page, 'challenge-failed');
        await dumpInventory(page, 'zz-challenge-failed');
        verdict = 'challenge_failed';
        break;
      }
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

    // --- 7. a second Chinese consent gate (per-product, can reappear) ------
    const consentAgain = await acceptConsent(page);
    if (consentAgain && consentAgain.selector) {
      state.consent_later = (state.consent_later || []).concat(consentAgain.label);
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
    // -- Route A: the address is minted on Microsoft's own form, so there is no
    //    mailbox provisioning step and nothing external is contacted.
    state.route = 'A';
    log('route', { mode: 'A', domain: OUTLOOK_DOMAIN, max_username_attempts: MAX_USERNAME_ATTEMPTS });

    // -- private recovery copy: password only until a username is accepted;
    //    runFlow() refreshes it with the address. Never printed, never in git.
    writeJson(path.join(runDir, 'secret.json'), { email: '', password: accountPassword });

    // -- launch the operator's real, locally installed Chrome with NO proxy at
    //    all: the browser uses the machine's default route, which is the China
    //    Unicom residential line. No profile emulation, no debugger endpoint,
    //    no CDP attach, no init script. The only things set are the two that
    //    make the browser tell the truth about where it is: UI language zh-CN
    //    and the Asia/Shanghai clock.
    phase = 'profile';
    state.profile = {
      engine: 'patchright-core',
      patchright: PATCHRIGHT_VERSION,
      channel: 'chrome',
      proxy: 'none',
      locale: PROFILE_LOCALE,
      timeZone: PROFILE_TZ,
      js_patches: 'none',
      cdp: false,
    };
    log('profile_geo', {
      locale: PROFILE_LOCALE, timeZone: PROFILE_TZ, proxy: 'none',
      geoSource: 'launch args + context options', js_patches: 'none', cdp: false,
    });

    phase = 'open';
    const launchArgs = ['--lang=zh-CN', '--accept-lang=zh-CN,zh;q=0.9', '--no-first-run', '--no-default-browser-check'];
    const chromeExe = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
    const launchOpts = { channel: 'chrome', headless: false, args: launchArgs, timeout: 120000 };
    try {
      browser = await chromium.launch(launchOpts);
    } catch (error) {
      log('warn', `channel=chrome failed, falling back to executablePath :: ${String(error).slice(0, 140)}`);
      browser = await chromium.launch({ ...launchOpts, channel: undefined, executablePath: chromeExe });
    }
    log('browser_launched', { channel: 'chrome', args: launchArgs.join(' '), proxy: 'none', ws: 'owned by this process (no CDP attach)' });
    const context = await browser.newContext({
      locale: PROFILE_LOCALE,
      timezoneId: PROFILE_TZ,
      viewport: null,
    });
    state.context_opts = { locale: PROFILE_LOCALE, timezoneId: PROFILE_TZ, viewport: 'window (native)' };
    log('context_ready', state.context_opts);
    page = await context.newPage();
    log('page_ready', { pages: context.pages().length });
    startShots(page);

    // What the document itself reports once attached. This is the evidence the
    // report needs for "locale/clock are real", so it is recorded verbatim.
    try {
      const seen = await page.evaluate(() => ({
        language: navigator.language,
        languages: (navigator.languages || []).slice(0, 3),
        timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        offsetMinutes: new Date().getTimezoneOffset(),
        ua: navigator.userAgent,
        webdriver: navigator.webdriver,
        platform: navigator.platform,
      }));
      state.client_geo = seen;
      log('client_geo', seen);
      if (seen.timeZone && seen.timeZone !== PROFILE_TZ) {
        log('warn', `client timezone ${seen.timeZone} != ${PROFILE_TZ}`);
      }
    } catch (error) {
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
    // This build owns its browser process outright, so there is no Forge profile
    // to hand back or delete -- just shut the browser down.
    if (browser) {
      try { await browser.close(); log('browser_closed'); } catch (error) { log('close_failed', String(error).slice(0, 160)); }
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
      elapsed_s: state.elapsed_s, egress: state.egress_cc ? { cc: state.egress_cc, tz: state.egress_tz, isp: state.egress_isp || '' } : null,
      locale: PROFILE_LOCALE, timezone: PROFILE_TZ,
      client_geo: state.client_geo ? { language: state.client_geo.language, timeZone: state.client_geo.timeZone, offsetMinutes: state.client_geo.offsetMinutes } : null,
      challenge: state.challenge, px_window: state.px_window || null, px_entry: state.px_entry || null,
      details: state.details || null,
      profile: state.profile ? { engine: state.profile.engine, channel: state.profile.channel, proxy: state.profile.proxy, js_patches: state.profile.js_patches, cdp: state.profile.cdp } : null,
      run_dir: runDir, shots: shotCount, error: state.error,
      final_url: state.final_url || '', final_text: String(state.final_text || '').slice(0, 300),
      password_written: created,
    }));
    process.exit(exitCode);
  }
})();
