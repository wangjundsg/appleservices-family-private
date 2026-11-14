// Path B mail reader: the Apple verification code, read straight out of the
// Outlook web inbox in a persistent patchright/Chrome session.
//
// Why this exists: the device-code grant for a freshly registered MSA is blocked
// by the "help protect your account" interrupt, but the web session itself is
// perfectly usable (outlook.live.com/mail/ opens). So the code is read the same
// way a human would, and written to the file that create-account.cjs polls via
// --await-email-code.
//
// Usage:
//   node tools/capture/outlook-web-mail.cjs --outdir .cache/apple-run2
//        [--write .cache/apple-run2/apple-code.txt] [--minutes 25] [--since-min 30]
//
// Prints only the code length and the mail subject; the code itself goes to the
// output file (mode 0600), never to stdout.

'use strict';

const fs = require('node:fs');
const path = require('node:path');

const PATCHRIGHT_PATH = 'D:/appleservices-family-private/.tools/js/node_modules/patchright-core';
const { chromium } = require(PATCHRIGHT_PATH);

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const i = argv.indexOf(`--${name}`);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith(`--${name}=`));
  return eq ? eq.slice(name.length + 3) : fallback;
}

const outdir = path.resolve(arg('outdir', '.cache/apple-run2'));
const outFile = path.resolve(arg('write', path.join(outdir, 'apple-code.txt')));
const minutes = Number(arg('minutes', '25'));
const sinceMin = Number(arg('since-min', '30'));
const shotDir = path.join(outdir, 'shots');
fs.mkdirSync(shotDir, { recursive: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const stamp = () => new Date().toISOString().slice(11, 19);
const log = (s, d) => console.log(`[${stamp()}] ${s}${d === undefined ? '' : ' ' + JSON.stringify(d)}`);

// pullAppleCode finds the newest Apple row in the mail list, opens it and reads
// the 6-digit code out of the reading pane.
//
// The modern Outlook web client renders the list as div[role="option"] items but
// also ships a "focused / other" split, so three lookups are tried in order and
// the first one that resolves wins. Element handle + locator are used instead of
// a hand-rolled querySelector walk because the row markup changes per build.
async function pullAppleCode(page) {
  const candidates = [
    page.locator('[role="option"]').filter({ hasText: /Apple/i }),
    page.locator('[role="listbox"] > div').filter({ hasText: /Apple/i }),
    page.locator('div[data-convid]').filter({ hasText: /Apple/i }),
  ];
  let row = null;
  for (const c of candidates) {
    const n = await c.count().catch(() => 0);
    if (n > 0) { row = c.first(); break; }
  }
  const pageText = await page.evaluate(() => (document.body ? document.body.innerText : '')).catch(() => '');
  if (!row) {
    return { ok: false, rows: [String(pageText).replace(/\s+/g, ' ').slice(0, 120)] };
  }

  const subject = (await row.innerText().catch(() => '')).replace(/\s+/g, ' ').slice(0, 90);
  await row.click().catch(() => {});
  await sleep(4000);

  // The reading pane text, then the code inside it.
  const paneText = await page.evaluate(() => {
    const pane = document.querySelector('[role="main"]') || document.body;
    return (pane.innerText || '').replace(/\s+/g, ' ');
  }).catch(() => '');
  const m = paneText.match(/(?<!\d)(\d{6})(?!\d)/);
  return { ok: Boolean(m), code: m ? m[1] : '', subject, text: paneText.slice(0, 200) };
}

(async () => {
  const context = await chromium.launchPersistentContext(path.join(outdir, 'chrome-profile'), {
    channel: 'chrome',
    headless: false,
    viewport: { width: 1360, height: 940 },
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    args: ['--disable-blink-features=AutomationControlled', '--no-first-run', '--no-default-browser-check'],
  });
  const page = context.pages()[0] || await context.newPage();
  page.setDefaultTimeout(30000);

  log('nav', 'https://outlook.live.com/mail/0/');
  await page.goto('https://outlook.live.com/mail/0/', { waitUntil: 'domcontentloaded', timeout: 60000 })
    .catch((e) => log('warn', String(e).slice(0, 80)));
  await sleep(12000);
  log('inbox', { url: page.url() });
  await page.screenshot({ path: path.join(shotDir, `mailbox-${Date.now()}.png`) }).catch(() => {});

  const deadline = Date.now() + minutes * 60_000;
  let lastLogged = '';
  while (Date.now() < deadline) {
    const got = await pullAppleCode(page).catch((e) => ({ ok: false, error: String(e).slice(0, 80) }));
    if (got.ok && got.code) {
      fs.writeFileSync(outFile, `${got.code}\n`, { mode: 0o600 });
      log('code_written', { file: path.basename(outFile), len: got.code.length, subject: got.subject });
      fs.writeFileSync(`${outFile}.meta.json`, JSON.stringify({ at: new Date().toISOString(), subject: got.subject }, null, 2));
      await context.close().catch(() => {});
      process.exit(0);
    }
    const marker = JSON.stringify(got.rows || []);
    if (marker !== lastLogged) { log('scan', { rows: got.rows ? got.rows.slice(0, 2) : [], error: got.error }); lastLogged = marker; }
    await sleep(7000);
  }
  log('timeout', { after_min: minutes });
  await context.close().catch(() => {});
  process.exit(4);
})().catch((e) => { log('fatal', String(e).slice(0, 200)); process.exit(1); });
