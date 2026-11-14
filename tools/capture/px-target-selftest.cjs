// Offline validation of the PX press-target maths used by outlook-cn.cjs.
//
// It pulls the REAL functions out of outlook-cn.cjs source text (probePressText,
// locatePressTarget, checkPressPoint) and runs them against a synthetic widget
// that copies the shape of the live one:
//   360x42 pill, 可访问性挑战 badge, 按住 label NOT at the capsule centre,
//   a screen-reader copy of the whole sentence that also matches /按住/,
//   and a nested hsprotect frame so the iframe-offset translation is exercised.
//
// Run: node tools/capture/px-target-selftest.cjs
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/patchright-core');

const SOURCE = fs.readFileSync(path.join(__dirname, 'outlook-cn.cjs'), 'utf8');
function slice(from, to) {
  const a = SOURCE.indexOf(from);
  const b = SOURCE.indexOf(to, a);
  if (a < 0 || b < 0) throw new Error(`slice failed: ${from} -> ${to}`);
  return SOURCE.slice(a, b);
}

const capsuleSrc = slice('const CAPSULE_SELECTORS = [', '// Runs INSIDE one frame.');
const probeSrc = slice('function probePressText', 'async function locatePressTarget');
const locateSrc = slice('async function locatePressTarget', '// Self-check before a press is spent');
const checkSrc = slice('async function checkPressPoint', '// ONE look at the widget');

const harness = `
const LOG = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const r2 = (n) => Math.round(Number(n) * 10) / 10;
const log = (step, detail) => { LOG.push({ step, detail }); };
async function framesOf(page) { return [page.mainFrame(), ...page.frames().filter((f) => f !== page.mainFrame())]; }
${capsuleSrc}
${probeSrc}
${locateSrc}
${checkSrc}
module.exports = { locatePressTarget, checkPressPoint, LOG };
`;
const mod = { exports: {} };
new Function('module', 'exports', harness)(mod, mod.exports);
const { locatePressTarget, checkPressPoint, LOG } = mod.exports;

// ------------------------------------------------------------------ fixtures
const CASE_A = `<!doctype html><html lang="zh-CN"><body style="margin:0;background:#eef3fb">
<iframe id="host" src="/hsprotect/outer-a.html" style="position:absolute;left:338px;top:542px;width:360px;height:42px;border:0"></iframe>
</body></html>`;

const OUTER_A = `<!doctype html><html><body style="margin:0;background:#fff">
<div id="px-captcha" style="position:absolute;left:0;top:0;width:360px;height:42px;border:1px solid #cfe3f7;border-radius:21px;box-sizing:border-box">
  <span style="position:absolute;left:4px;top:4px;width:34px;height:34px;border-radius:50%;background:#5b6b7a;color:#fff;text-align:center;line-height:34px">人</span>
  <span style="position:absolute;left:46px;top:12px;font-size:14px;color:#333">可访问性挑战</span>
  <span id="lbl" style="position:absolute;left:250px;top:11px;font-size:15px;font-weight:600;color:#0b6ab0">按住</span>
</div>
<span style="position:absolute;left:-9999px;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0)">按住 人工挑战 Human Challenge需要验证。请按住按钮直到验证完成</span>
</body></html>`;

// case B: the pill lives in the outer frame, the label sits in a NESTED frame
const OUTER_B = `<!doctype html><html><body style="margin:0;background:#fff">
<div id="px-captcha" style="position:absolute;left:0;top:0;width:360px;height:42px;border:1px solid #cfe3f7;border-radius:21px;box-sizing:border-box">
  <span style="position:absolute;left:4px;top:4px;width:34px;height:34px;border-radius:50%;background:#5b6b7a;color:#fff;text-align:center;line-height:34px">人</span>
  <span style="position:absolute;left:46px;top:12px;font-size:14px;color:#333">可访问性挑战</span>
  <iframe id="inner" src="/hsprotect/inner-b.html" style="position:absolute;left:236px;top:2px;width:118px;height:36px;border:0"></iframe>
</div>
</body></html>`;

const INNER_B = `<!doctype html><html><body style="margin:0;background:transparent">
<span id="lbl" style="position:absolute;left:20px;top:8px;font-size:15px;font-weight:600;color:#0b6ab0">按住</span>
<span style="position:absolute;left:-9999px">按住 人工验证挑战 Human Challenge需要验证</span>
</body></html>`;

const server = http.createServer((req, res) => {
  const url = req.url.split('?')[0];
  const map = {
    '/a': ['text/html; charset=utf-8', CASE_A],
    '/hsprotect/outer-a.html': ['text/html; charset=utf-8', OUTER_A],
    '/hsprotect/outer-b.html': ['text/html; charset=utf-8', OUTER_B],
    '/hsprotect/inner-b.html': ['text/html; charset=utf-8', INNER_B],
    '/px-b': ['text/html; charset=utf-8', CASE_B_PAGE()],
  };
  const hit = map[url];
  if (!hit) { res.writeHead(404); res.end('nope'); return; }
  res.writeHead(200, { 'content-type': hit[0] });
  res.end(hit[1]);
});
function CASE_B_PAGE() {
  return `<!doctype html><html lang="zh-CN"><body style="margin:0;background:#eef3fb">
<iframe id="host" src="/hsprotect/outer-b.html" style="position:absolute;left:338px;top:542px;width:360px;height:42px;border:0"></iframe>
</body></html>`;
}

function expectedCentre() {
  // measured the same way a human eye would: iframe offset + label rect, using
  // the browser's own layout, walking nested frames by hand.
  const f = document.querySelector('iframe#host');
  const fr = f.getBoundingClientRect();
  const doc = f.contentDocument;
  let x = fr.left + (f.clientLeft || 0);
  let y = fr.top + (f.clientTop || 0);
  let label = doc.getElementById('lbl');
  if (!label) {
    const inner = doc.getElementById('inner');
    const ir = inner.getBoundingClientRect();
    x += ir.left + (inner.clientLeft || 0);
    y += ir.top + (inner.clientTop || 0);
    label = inner.contentDocument.getElementById('lbl');
  }
  const r = label.getBoundingClientRect();
  return { x: x + r.left + r.width / 2, y: y + r.top + r.height / 2 };
}

(async () => {
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const port = server.address().port;
  let failures = 0;
  const check = (name, ok, extra) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${extra ? '  ' + JSON.stringify(extra) : ''}`);
    if (!ok) failures += 1;
  };

  const browser = await chromium.launch({ channel: 'chrome', headless: true });
  const page = await browser.newPage({ viewport: { width: 1036, height: 798 } });

  for (const [label, route] of [['caseA: label in pill frame', '/a'], ['caseB: label in nested frame', '/px-b']]) {
    await page.goto(`http://127.0.0.1:${port}${route}`, { waitUntil: 'load' });
    await page.waitForTimeout(300);
    const expected = await page.evaluate(expectedCentre);
    const target = await locatePressTarget(page);
    if (!target) { check(`${label} :: target located`, false); continue; }
    const dx = Math.abs(target.point.x - expected.x);
    const dy = Math.abs(target.point.y - expected.y);
    check(`${label} :: press point == label centre`, dx <= 1.5 && dy <= 1.5, {
      point: target.point, expected: { x: +expected.x.toFixed(1), y: +expected.y.toFixed(1) }, dx: +dx.toFixed(2), dy: +dy.toFixed(2),
      label_text: target.text, exact: target.exact, frame: target.frameUrl.replace(/^https?:\/\/[^/]+/, ''), origin: target.frameOrigin,
    });
    const capCentre = target.capsulePage
      ? { x: target.capsulePage.x + target.capsulePage.w / 2, y: target.capsulePage.y + target.capsulePage.h / 2 }
      : null;
    check(`${label} :: point is NOT the capsule centre`, capCentre ? Math.hypot(target.point.x - capCentre.x, target.point.y - capCentre.y) > 20 : true, {
      point: target.point, capsule_centre: capCentre && { x: +capCentre.x.toFixed(1), y: +capCentre.y.toFixed(1) },
    });
    const self = await checkPressPoint(page, target.point);
    check(`${label} :: self-check (in view + elementFromPoint hits hsprotect)`, self.ok === true, {
      in_view: self.in_view, hit_tag: self.hit && self.hit.tag, hit_src: self.hit && self.hit.src,
    });
    const srOnly = await page.evaluate(() => {
      const f = document.querySelector('iframe#host');
      const txt = f.contentDocument.body.innerText || '';
      return txt.length;
    });
    check(`${label} :: sr-only sentence was not chosen`, target.text === '按住', { chosen: target.text, frame_text_len: srOnly });
  }

  await browser.close();
  server.close();
  console.log(`\nlog rows: ${JSON.stringify(LOG)}`);
  console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILURE(S)`);
  process.exit(failures === 0 ? 0 : 1);
})().catch((e) => { console.error('selftest crashed:', e); server.close(); process.exit(1); });
