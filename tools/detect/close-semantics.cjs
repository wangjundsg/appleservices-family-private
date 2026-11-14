// Does browser.close() on a connectOverCDP session kill the profile's Chrome?
// One profile, one attach, one close(), then probe the debugger port directly.
//
//   node tools/detect/close-semantics.cjs
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
  const json = await res.json().catch(() => ({}));
  return { status: res.status, json };
}

const alive = async (port) => {
  try {
    const r = await fetch(`http://127.0.0.1:${port}/json/version`, { signal: AbortSignal.timeout(3000) });
    return r.ok;
  } catch { return false; }
};

(async () => {
  const created = await roxy('/browser/create', { workspaceId: WS, projectId: PROJECT, windowName: `close-sem-${Date.now()}`, os: 'Windows' });
  const dirId = created.json?.data?.dirId;
  const opened = await roxy('/browser/open', { workspaceId: WS, dirId, args: [], forceOpen: true, headless: false });
  const port = Number(opened.json?.data?.debuggingPort || 0);
  console.log(JSON.stringify({ dirId, port, before: await alive(port) }));

  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`);
  await browser.close().catch((e) => console.log('close threw:', String(e).slice(0, 120)));
  await new Promise((r) => setTimeout(r, 2500));
  console.log(JSON.stringify({ after_browser_close: await alive(port) }));

  // leave cleanup to the operator: close/delete still has to work afterwards
  console.log(JSON.stringify({ close: (await roxy('/browser/close', { workspaceId: WS, dirId })).json?.code }));
  await new Promise((r) => setTimeout(r, 4000));
  console.log(JSON.stringify({ delete: (await roxy('/browser/delete', { workspaceId: WS, dirIds: [dirId] })).json }));
  process.exit(0);
})();
