// Diagnostic only: what does a given Microsoft signup entry URL actually render?
//
// Launches real Chrome (channel chrome) through the operator's residential proxy,
// navigates once, optionally clicks a few labels, and dumps every visible
// input/button plus the body text of every frame. It never types into a field
// and never submits a form - it exists so the real run is not guessed at.
//
// Usage:
//   node tools/capture/signup-entry-dump.cjs --port 10040 \
//        --url "https://outlook.live.com/mail/0/?prompt=create_account" \
//        [--click "Continue"] [--click "Get a new email address"] \
//        [--out .cache/entry-dump/x.json] [--shots .cache/entry-dump/x]

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

const argv = process.argv.slice(2);
function arg(name, fallback = '') {
  const all = [];
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === `--${name}` && argv[i + 1] && !argv[i + 1].startsWith('--')) all.push(argv[i + 1]);
  }
  return all.length ? all : [fallback];
}
function one(name, fallback = '') { return arg(name, fallback)[0]; }

const PROXY_HOST = 'gw.dataimpulse.com';
const PROXY_USER = '3ddce6da3add2226e7d2__cr.us;sessttl.30';
const PROXY_PASS = '2805d82cb2a633dc';

const port = one('port', '10040');
const target = one('url', 'https://signup.live.com/signup');
const clicks = arg('click', '').filter(Boolean);
const outPath = one('out', '');
const shotDir = one('shots', '');
const includeHidden = argv.includes('--all');
const waitMs = Number(one('wait', '30000')) || 30000;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function inventory(page, all = false) {
  const rows = [];
  for (const frame of page.frames()) {
    let row;
    try {
      row = await frame.evaluate((wantAll) => {
        const vis = (el) => {
          if (wantAll) return true;
          const r = el.getBoundingClientRect();
          const st = getComputedStyle(el);
          return r.width > 1 && r.height > 1 && st.visibility !== 'hidden' && st.display !== 'none';
        };
        // Does anything in the tree say "new email address", even off-screen?
        const hits = [];
        for (const el of document.querySelectorAll('a,button,[role="link"],[role="button"],span,div')) {
          const text = (el.innerText || '').replace(/\s+/g, ' ').trim();
          if (text && text.length < 90 && /new email|new address|outlook\.com|hotmail\.com/i.test(text)) {
            hits.push({
              tag: el.tagName.toLowerCase(), id: el.id || '',
              aria: (el.getAttribute('aria-label') || '').slice(0, 60),
              text: text.slice(0, 80),
              visible: Boolean(el.offsetParent) && getComputedStyle(el).visibility !== 'hidden',
            });
            if (hits.length > 20) break;
          }
        }
        return {
          url: location.href.slice(0, 200),
          title: document.title,
          text: (document.body && document.body.innerText || '').replace(/\s+/g, ' ').slice(0, 900),
          memberNameLink: Boolean(document.querySelector('#memberNameLink')),
          emailDomainEl: Boolean(document.querySelector('[aria-label*="New email" i], [aria-label*="Email domain" i], [name="MemberName"]')),
          hits,
          inputs: [...document.querySelectorAll('input, select, textarea')].filter(vis).map((el) => ({
            type: el.type || '', id: el.id || '', name: el.name || '',
            aria: (el.getAttribute('aria-label') || '').slice(0, 80),
            ph: (el.placeholder || '').slice(0, 60),
            autocomplete: (el.getAttribute('autocomplete') || '').slice(0, 40),
            hidden: !el.offsetParent,
          })),
          buttons: [...document.querySelectorAll('button, [role="button"], a, [role="link"], [role="option"]')]
            .filter(vis).slice(0, 40).map((el) => ({
              tag: el.tagName.toLowerCase(),
              id: el.id || '',
              testid: el.getAttribute('data-testid') || '',
              href: (el.getAttribute('href') || '').slice(0, 100),
              aria: (el.getAttribute('aria-label') || '').slice(0, 80),
              text: (el.innerText || el.value || '').replace(/\s+/g, ' ').trim().slice(0, 70),
            })),
        };
      }, all);
    } catch (error) {
      row = { url: frame.url().slice(0, 200), error: String(error).slice(0, 120) };
    }
    rows.push({ frameUrl: frame.url().slice(0, 200), ...row });
  }
  return rows;
}

(async () => {
  const browser = await chromium.launch({
    channel: 'chrome',
    headless: false,
    ignoreDefaultArgs: ['--enable-automation'],
    args: ['--disable-blink-features=AutomationControlled', '--no-first-run', '--no-default-browser-check'],
    viewport: { width: 1366, height: 860 },
    locale: 'en-US',
    timezoneId: 'America/New_York',
    proxy: {
      server: `http://${PROXY_HOST}:${port}`,
      username: PROXY_USER,
      password: PROXY_PASS,
    },
  });
  const context = await browser.newContext({
    viewport: { width: 1366, height: 860 },
    locale: 'en-US',
    timezoneId: 'America/New_York',
    userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36',
  });
  const page = await context.newPage();
  const report = { port, target, steps: [] };

  const snap = async (tag) => {
    // SPAs need time: wait until the document actually says something.
    const deadline = Date.now() + waitMs;
    let bodyLen = 0;
    while (Date.now() < deadline) {
      bodyLen = await page.evaluate(() => ((document.body && document.body.innerText) || '').trim().length).catch(() => 0);
      if (bodyLen > 20) break;
      await sleep(1000);
    }
    const inv = await inventory(page, includeHidden);
    report.steps.push({ tag, url: page.url(), bodyLen, inventory: inv });
    console.log(`\n### ${tag} :: ${page.url()} (bodyLen=${bodyLen})`);
    for (const f of inv) {
      console.log(`- frame ${f.frameUrl}`);
      console.log(`  text: ${String(f.text || '').slice(0, 320)}`);
      if (f.memberNameLink || f.emailDomainEl) console.log(`  memberNameLink=${f.memberNameLink} emailDomainEl=${f.emailDomainEl}`);
      if (f.hits && f.hits.length) console.log(`  hits: ${JSON.stringify(f.hits.slice(0, 8))}`);
      if (f.inputs && f.inputs.length) console.log(`  inputs: ${JSON.stringify(f.inputs)}`);
      if (f.buttons && f.buttons.length) console.log(`  buttons: ${JSON.stringify(f.buttons.slice(0, 14))}`);
    }
    if (shotDir) {
      fs.mkdirSync(shotDir, { recursive: true });
      await page.screenshot({ path: path.join(shotDir, `${report.steps.length}-${tag}.png`) }).catch(() => {});
    }
  };

  try {
    await page.goto(target, { waitUntil: 'domcontentloaded', timeout: 90000 });
    await sleep(3000);
    await snap('loaded');

    for (const label of clicks) {
      const loc = page.getByText(new RegExp(label.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i')).first();
      const seen = await loc.count().catch(() => 0);
      console.log(`\n>> click "${label}" found=${seen}`);
      if (seen) {
        await loc.click({ timeout: 8000 }).catch((e) => console.log('   click error', String(e).slice(0, 120)));
        await sleep(5000);
        await snap(`after-click-${label.replace(/\W+/g, '_')}`);
      } else {
        report.steps.push({ tag: `missing-${label}`, url: page.url() });
      }
    }
  } catch (error) {
    report.error = String(error).slice(0, 200);
    console.log('ERROR', report.error);
  } finally {
    await context.close().catch(() => {});
    await browser.close().catch(() => {});
  }

  const line = JSON.stringify(report);
  if (outPath) {
    fs.mkdirSync(path.dirname(path.resolve(outPath)), { recursive: true });
    fs.writeFileSync(path.resolve(outPath), `${line}\n`, 'utf8');
  }
  console.log(`\n${line.slice(0, 300)}...`);
})();
