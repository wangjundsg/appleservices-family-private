// Client A -- Playwright (playwright-core) connectOverCDP against a Roxy/Forge
// debugger port. Read-only: visits three public detection pages, records what
// each one says, screenshots, disconnects. It never launches a browser and it
// never closes the profile's Chrome (the orchestrator owns the lifecycle).
//
// node tools/detect/client-playwright.cjs --port 51234 --out .cache/x/A.json

const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

function arg(name, fallback) {
  const flag = `--${name}`;
  for (let i = 0; i < process.argv.length; i += 1) {
    const a = process.argv[i];
    if (a === flag) {
      const next = process.argv[i + 1];
      return next && !next.startsWith('--') ? next : true;
    }
    if (a.startsWith(`${flag}=`)) return a.slice(flag.length + 1);
  }
  return fallback;
}

const PORT = String(arg('port', ''));
const OUT = path.resolve(String(arg('out', '.cache/detect/A.json')));
const SHOT_DIR = path.join(path.dirname(OUT), 'shots');
fs.mkdirSync(SHOT_DIR, { recursive: true });
const PROBE = fs.readFileSync(path.join(__dirname, 'probe.js'), 'utf8');

const PAGES = [
  { id: 'sannysoft', url: 'https://bot.sannysoft.com/', settle: 6000 },
  { id: 'creepjs', url: 'https://abrahamjuliot.github.io/creepjs/', settle: 14000 },
  { id: 'browserleaks-js', url: 'https://browserleaks.com/javascript', settle: 6000 },
];

// Site-specific extraction. Everything generic lives in probe.js.
async function extract(page, id) {
  if (id === 'sannysoft') {
    return page.evaluate(() => {
      const rows = [];
      for (const tr of document.querySelectorAll('tr')) {
        const tds = [...tr.querySelectorAll('td')];
        if (tds.length < 2) continue;
        const label = (tds[0].innerText || '').trim().replace(/\s+/g, ' ');
        const cells = tds.slice(1).map((td) => ({
          text: (td.innerText || '').trim().replace(/\s+/g, ' '),
          cls: String(td.className || ''),
          id: td.id || '',
        }));
        rows.push({ label, cells });
      }
      const failed = rows.filter((r) => r.cells.some((c) => /failed/.test(c.cls) && !/passed/.test(c.cls)));
      return { rows, failed: failed.map((r) => `${r.label} :: ${r.cells.map((c) => c.text).join(' / ')}`) };
    });
  }
  if (id === 'browserleaks-js') {
    return page.evaluate(() => {
      const text = document.body.innerText || '';
      const lines = text.split('\n').map((s) => s.trim()).filter(Boolean);
      const pick = (re) => lines.filter((l) => re.test(l)).slice(0, 12);
      return {
        webdriverLines: pick(/webdriver/i),
        userAgentLines: pick(/user.?agent/i),
        lines: lines.slice(0, 160),
      };
    });
  }
  // creepjs: text only, the app renders everything into the DOM. The current
  // build has no "trust score" block; it reports headless/stealth percentages,
  // so capture the text plus whether a trust node exists at all.
  return page.evaluate(() => {
    window.scrollTo(0, 0);
    const text = document.body.innerText || '';
    return {
      text: text.slice(0, 16000),
      hasTrustNode: Boolean(document.querySelector('#fingerprint-data')),
      trustMention: /trust/i.test(document.body.innerHTML || ''),
      headlessLine: (text.match(/like headless[^\n]*/i) || [])[0] || '',
      stealthLine: (text.match(/stealth[^\n]*/i) || [])[0] || '',
      htmlHead: (document.body.innerHTML || '').slice(0, 2500),
    };
  });
}

(async () => {
  const started = Date.now();
  const result = { client: 'playwright-connectOverCDP', port: PORT, playwright: null, pages: [], errors: [] };
  try {
    const pkg = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core/package.json');
    result.client_version = pkg.version;
  } catch { result.client_version = 'unknown'; }

  let browser = null;
  try {
    browser = await chromium.connectOverCDP(`http://127.0.0.1:${PORT}`, { timeout: 30000 });
    result.playwright = browser.version();
    const context = browser.contexts()[0];
    if (!context) throw new Error('CDP_NO_CONTEXT');

    for (const spec of PAGES) {
      const rec = { id: spec.id, url: spec.url, ok: false };
      let page = null;
      try {
        page = await context.newPage();
        const t0 = Date.now();
        await page.goto(spec.url, { waitUntil: 'domcontentloaded', timeout: 60000 });
        await page.waitForTimeout(spec.settle);
        rec.load_ms = Date.now() - t0;
        rec.title = await page.title();
        rec.probe = await page.evaluate(PROBE);
        rec.extract = await extract(page, spec.id);
        const shot = path.join(SHOT_DIR, `A-${spec.id}.png`);
        await page.screenshot({ path: shot, fullPage: false, timeout: 30000 });
        rec.screenshot = path.relative(path.dirname(OUT), shot);
        rec.ok = true;
      } catch (error) {
        rec.error = String(error && error.message ? error.message : error).slice(0, 400);
        result.errors.push(`${spec.id}: ${rec.error}`);
      } finally {
        if (page) await page.close().catch(() => {});
      }
      result.pages.push(rec);
    }
  } catch (error) {
    result.errors.push(`attach: ${String(error && error.message ? error.message : error).slice(0, 400)}`);
    result.attach_failed = true;
  } finally {
    // close() on a CDP attach does NOT kill the profile Chrome: verified on
    // playwright-core 1.65 -- the debug port and the open pages survive, only
    // the Playwright objects go away (tools/detect/close-pages.cjs). The
    // browser process still belongs to the orchestrator's /browser/close.
    if (browser) result.note = 'CDP close() disconnects only; profile Chrome and pages outlive it';
    result.elapsed_ms = Date.now() - started;
    fs.mkdirSync(path.dirname(OUT), { recursive: true });
    fs.writeFileSync(OUT, JSON.stringify(result, null, 2), 'utf8');
    console.log(JSON.stringify({
      client: result.client, ok: result.pages.filter((p) => p.ok).length, errors: result.errors,
      out: OUT, elapsed_ms: result.elapsed_ms,
    }));
    process.exit(result.errors.length ? 3 : 0);
  }
})();
