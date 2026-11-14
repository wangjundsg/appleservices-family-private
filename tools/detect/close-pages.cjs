// Precise close() semantics: does browser.close() on a CDP attach kill the
// Chrome process, and does it close the pages that were open?
//
//   node tools/detect/close-pages.cjs
const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/playwright-core');

const BASE = 'http://127.0.0.1:50100';
const CREDS = require('./roxy-creds.cjs').load();
const TOKEN = CREDS.token;
const WS = CREDS.workspaceId;
const PROJECT = CREDS.projectId || '';

async function roxy(pathname, body) {
  const res = await fetch(BASE + pathname, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { token: TOKEN, Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(90000),
  });
  return { status: res.status, json: await res.json().catch(() => ({})) };
}

const targets = async (port) => {
  try {
    const r = await fetch(`http://127.0.0.1:${port}/json/list`, { signal: AbortSignal.timeout(3000) });
    const list = await r.json();
    return list.filter((t) => t.type === 'page').map((t) => t.url);
  } catch (e) { return [`ERR ${String(e).slice(0, 60)}`]; }
};

(async () => {
  const created = await roxy('/browser/create', { workspaceId: WS, projectId: PROJECT, windowName: `close-pages-${Date.now()}`, os: 'Windows' });
  const dirId = created.json?.data?.dirId;
  const opened = await roxy('/browser/open', { workspaceId: WS, dirId, args: [], forceOpen: true, headless: false });
  const port = Number(opened.json?.data?.debuggingPort || 0);
  console.log(JSON.stringify({ port, pages_before: await targets(port) }));

  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`);
  const ctx = browser.contexts()[0];
  const page = await ctx.newPage();
  await page.goto('about:blank');
  console.log(JSON.stringify({ pages_after_new: await targets(port) }));

  await browser.close().catch(() => {});
  await new Promise((r) => setTimeout(r, 2000));
  console.log(JSON.stringify({ pages_after_close: await targets(port) }));

  await roxy('/browser/close', { workspaceId: WS, dirId });
  await new Promise((r) => setTimeout(r, 4000));
  console.log(JSON.stringify({ delete: (await roxy('/browser/delete', { workspaceId: WS, dirIds: [dirId] })).json?.data?.deleted }));
  process.exit(0);
})();
