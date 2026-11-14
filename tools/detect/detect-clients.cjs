// Detection baseline orchestrator.
//
// One Roxy/Forge profile, two clients, one run:
//   POST /browser/create -> POST /browser/open -> debuggerAddress
//     -> client A: Playwright connectOverCDP   (tools/detect/client-playwright.cjs)
//     -> client B: Selenium  debuggerAddress   (tools/detect/client_selenium.py)
//   -> POST /browser/close -> POST /browser/delete
//
// Business codes are checked on every hop: HTTP 200 alone has already lied in
// this repository (create can answer 200 with success:false).
//
// node tools/detect/detect-clients.cjs --label baseline-1

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const ROOT = 'D:/appleservices-family-private';
const ROXY_BASE = 'http://127.0.0.1:50100';
const CREDS = require('./roxy-creds.cjs').load();
const ROXY_TOKEN = CREDS.token;
const WORKSPACE_ID = CREDS.workspaceId;
const PROJECT_ID = CREDS.projectId || '';
const CHROME_BIN_ROOT = 'D:\\Workbench\\data\\roxy-userdata\\chrome-bin';

// Roxy ships several Chrome builds (151..155) and picks one per profile, so a
// hard-coded chromedriver path only works until the service rolls another
// build. Ask the launched browser what it is, then pick that major's driver.
async function resolveDriver(port) {
  try {
    const r = await fetch(`http://127.0.0.1:${port}/json/version`, { signal: AbortSignal.timeout(5000) });
    const v = await r.json();
    const major = String(v.Browser || '').match(/(\d+)\./)?.[1] || '';
    const candidate = path.join(CHROME_BIN_ROOT, major, 'chromedriver.exe');
    if (major && fs.existsSync(candidate)) return { driver: candidate, browser: v.Browser, major };
    return { driver: '', browser: v.Browser, major };
  } catch (error) {
    return { driver: '', error: String(error).slice(0, 120) };
  }
}

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

const LABEL = String(arg('label', `baseline-${Date.now()}`));
const RUN_DIR = path.resolve(String(arg('outdir', path.join(ROOT, '.cache', 'detect-baseline', LABEL))));
const KEEP_PROFILE = Boolean(arg('keep', false));
const OUTLOOK_DIRECT = !arg('proxy', null); // no proxyInfo == local home line
const PROXY = arg('proxy', '');
const LOCALE = String(arg('locale', ''));
const TZ = String(arg('tz', ''));

fs.mkdirSync(RUN_DIR, { recursive: true });
const events = [];
const log = (step, data) => {
  const row = { at: new Date().toISOString(), step, ...(data === undefined ? {} : { data }) };
  events.push(row);
  console.log(JSON.stringify(row));
};
const persist = (extra) => fs.writeFileSync(path.join(RUN_DIR, 'run.json'), JSON.stringify({ label: LABEL, events, ...extra }, null, 2), 'utf8');

async function roxy(pathname, body) {
  const res = await fetch(ROXY_BASE + pathname, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { token: ROXY_TOKEN, Authorization: `Bearer ${ROXY_TOKEN}`, 'Content-Type': 'application/json', Accept: 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(120000),
  });
  const text = await res.text();
  let json = null;
  try { json = JSON.parse(text); } catch { json = { raw: text }; }
  const ok = res.ok && json && json.success !== false && [0, 200, '0', '200'].includes(json.code);
  return { status: res.status, ok, json, text: text.slice(0, 600) };
}

function pickPort(json) {
  const d = (json && json.data) || {};
  if (Number(d.debuggingPort)) return Number(d.debuggingPort);
  const addr = String(d.debuggerAddress || '');
  const m = addr.match(/:(\d+)$/);
  return m ? Number(m[1]) : 0;
}

(async () => {
  const started = Date.now();
  const state = { label: LABEL, started_at: new Date().toISOString(), profile: null, clients: {}, ok: false, errors: [] };
  let dirId = '';
  let closed = false;

  try {
    const root = await roxy('/');
    log('roxy_root', { service: root.json?.data?.service, version: root.json?.data?.version, driver: root.json?.data?.driver });

    // ---- create ---------------------------------------------------------
    const createBody = {
      workspaceId: WORKSPACE_ID,
      projectId: PROJECT_ID,
      windowName: `detect-${LABEL}`.slice(0, 40),
      os: 'Windows',
    };
    // Pin the fingerprint instead of accepting whatever the service rolls: an
    // unpinned create answers with a random locale (baseline-1 got da-DK).
    if (LOCALE) createBody.locale = LOCALE;
    if (TZ) createBody.timeZone = TZ;
    if (!OUTLOOK_DIRECT) {
      createBody.proxyInfo = {
        moduleId: 0, proxyMethod: 'custom', proxyCategory: 'SOCKS5', ipType: 'IPV4', protocol: 'SOCKS5',
        host: PROXY.host, port: PROXY.port, proxyUserName: PROXY.user, proxyPassword: PROXY.pass,
      };
    }
    const created = await roxy('/browser/create', createBody);
    log('create', { status: created.status, ok: created.ok, code: created.json?.code, success: created.json?.success, dirId: created.json?.data?.dirId || created.json?.data?.id });
    if (!created.ok) throw new Error(`CREATE_FAILED ${created.text}`);
    dirId = String(created.json?.data?.dirId || created.json?.data?.id || '');
    if (!dirId) throw new Error('CREATE_NO_DIRID');
    const fp = created.json?.data?.fingerprint || null;
    state.profile = { dirId, windowName: createBody.windowName, proxy: OUTLOOK_DIRECT ? 'direct(local)' : 'socks5', fingerprint: fp };
    log('profile_geo', fp ? { locale: fp.locale, timeZone: fp.timeZone, ua: String(fp.ua || '').slice(0, 90) } : null);

    // ---- open -----------------------------------------------------------
    const opened = await roxy('/browser/open', { workspaceId: WORKSPACE_ID, dirId, args: [], forceOpen: true, headless: false });
    log('open', { status: opened.status, ok: opened.ok, code: opened.json?.code, success: opened.json?.success });
    if (!opened.ok) throw new Error(`OPEN_FAILED ${opened.text}`);
    const port = pickPort(opened.json);
    if (!port) throw new Error(`OPEN_NO_DEBUG_PORT ${opened.text}`);
    state.debug_port = port;

    // ---- client A -------------------------------------------------------
    const aOut = path.join(RUN_DIR, 'A-playwright.json');
    try {
      const a = execFileSync(process.execPath, [path.join(ROOT, 'tools/detect/client-playwright.cjs'), '--port', String(port), '--out', aOut], { encoding: 'utf8', timeout: 480000 });
      log('client_A', { done: true, stdout: a.trim().slice(0, 400) });
    } catch (error) {
      log('client_A', { done: false, error: String(error.stdout || error.message).slice(0, 600) });
      state.errors.push(`A: ${String(error.message).slice(0, 300)}`);
    }

    // ---- client B -------------------------------------------------------
    const bOut = path.join(RUN_DIR, 'B-selenium.json');
    const drv = await resolveDriver(port);
    log('driver_pick', drv);
    try {
      const b = execFileSync('python', [path.join(ROOT, 'tools/detect/client_selenium.py'), '--port', String(port), '--out', bOut, '--driver', drv.driver || ''], { encoding: 'utf8', timeout: 600000 });
      log('client_B', { done: true, stdout: b.trim().slice(0, 400) });
    } catch (error) {
      log('client_B', { done: false, error: String(error.stdout || error.message).slice(0, 800) });
      state.errors.push(`B: ${String(error.message).slice(0, 300)}`);
    }

    state.ok = true;
  } catch (error) {
    state.errors.push(String(error && error.message ? error.message : error).slice(0, 500));
    log('exception', state.errors[state.errors.length - 1]);
  } finally {
    // ---- close + delete (business code checked, HTTP 200 is not enough) --
    if (dirId) {
      try {
        const cl = await roxy('/browser/close', { workspaceId: WORKSPACE_ID, dirId });
        closed = cl.ok;
        log('close', { status: cl.status, ok: cl.ok, code: cl.json?.code, success: cl.json?.success, text: cl.ok ? undefined : cl.text });
      } catch (error) { log('close_failed', String(error).slice(0, 200)); }
      if (!KEEP_PROFILE) {
        // close -> delete can answer EPERM while the Chrome process is still
        // releasing the profile dir. Retrying is the documented behaviour; a
        // single failed delete leaves the fingerprint cache on disk.
        let del = null;
        for (let attempt = 1; attempt <= 5; attempt += 1) {
          try {
            del = await roxy('/browser/delete', { workspaceId: WORKSPACE_ID, dirIds: [dirId] });
            log('delete_attempt', { attempt, ok: del.ok, code: del.json?.code, text: del.ok ? undefined : String(del.text).slice(0, 160) });
            if (del.ok) break;
          } catch (error) {
            log('delete_attempt', { attempt, error: String(error).slice(0, 160) });
          }
          await new Promise((r) => setTimeout(r, 1500 * attempt));
        }
        if (del && !del.ok) state.errors.push(`delete: ${String(del.text).slice(0, 200)}`);
      }
    }
    state.closed = closed;
    state.finished_at = new Date().toISOString();
    state.elapsed_ms = Date.now() - started;
    persist(state);
    console.log(JSON.stringify({ label: LABEL, run_dir: RUN_DIR, ok: state.ok, errors: state.errors, elapsed_ms: state.elapsed_ms }));
  }
})();
