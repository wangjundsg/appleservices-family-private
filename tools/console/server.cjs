#!/usr/bin/env node
// 薄控制台 — 驱动本仓库 tools/capture/* 的可视化壳子。
//
// 设计约束（见 README）：
//   * 单进程、无数据库、零 npm 依赖（只用 node 内置模块）
//   * 只监听 127.0.0.1
//   * 只读 .cache/：任务状态、日志、截图、邮箱池全部从现有文件读取
//   * 不打印 / 不返回任何 password / refresh_token / client_id
//   * 「发起」按钮默认只做 dry-run 演练；真跑需要 CONSOLE_ALLOW_REAL=1 且请求显式确认
//
// 启动：node tools/console/server.cjs   （或 tools/console/start.ps1）
// 端口：CONSOLE_PORT，默认 8801

'use strict';

const http = require('node:http');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const path = require('node:path');
const { spawn } = require('node:child_process');

// ----------------------------------------------------------------- 常量 / 路径
const ROOT = path.resolve(__dirname, '..', '..');
const CACHE = path.join(ROOT, '.cache');
const PUBLIC_DIR = path.join(__dirname, 'public');
const CONSOLE_DIR = path.join(CACHE, 'console');
const JOBS_DIR = path.join(CONSOLE_DIR, 'jobs');
const TOKENS_DIR = path.join(CONSOLE_DIR, 'tokens');
const MAIL_STATE = path.join(CONSOLE_DIR, 'mail-state.json');
const MAILBOXES = path.join(CACHE, 'mailboxes.json');

const PORT = Number(process.env.CONSOLE_PORT || process.env.PORT || 8801);
const HOST = '127.0.0.1';
const ALLOW_REAL = process.env.CONSOLE_ALLOW_REAL === '1';
const VERSION = '0.1.0';
const MAX_SCAN_DEPTH = 4;
const TAIL_DEFAULT = 40;
const TAIL_DETAIL = 300;

for (const d of [CONSOLE_DIR, JOBS_DIR, TOKENS_DIR]) {
  fs.mkdirSync(d, { recursive: true });
}

// ------------------------------------------------------------------ 小工具
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function json(res, code, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(code, {
    'Content-Type': 'application/json; charset=utf-8',
    'Cache-Control': 'no-store',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
}

function readBody(req, limit = 1 << 20) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on('data', (c) => {
      size += c.length;
      if (size > limit) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8');
      if (!raw.trim()) return resolve({});
      try {
        resolve(JSON.parse(raw));
      } catch (e) {
        reject(new Error('invalid JSON body'));
      }
    });
    req.on('error', reject);
  });
}

function relFromRoot(abs) {
  return path.relative(ROOT, abs).split(path.sep).join('/');
}

function relFromCache(abs) {
  return path.relative(CACHE, abs).split(path.sep).join('/');
}

function insideCache(abs) {
  const r = path.resolve(abs);
  return r === CACHE || r.startsWith(CACHE + path.sep);
}

function nowIso(ms) {
  return new Date(ms).toISOString().replace('T', ' ').slice(0, 19);
}

// 去掉可能混进日志的敏感字段值（防御性：现有 run.jsonl 只记字段名与长度）。
const SECRET_KEY_RE = /^(password|passwd|pwd|refresh_token|access_token|client_id|client_secret|secret|token|jwt)$/i;
function scrub(obj, depth = 0) {
  if (depth > 4 || obj === null || typeof obj !== 'object') return obj;
  if (Array.isArray(obj)) return obj.map((v) => scrub(v, depth + 1));
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    if (SECRET_KEY_RE.test(k) && typeof v === 'string' && v.length > 0) {
      out[k] = '***';
    } else if (k === 'field' || k === 'at') {
      out[k] = v;
    } else {
      out[k] = scrub(v, depth + 1);
    }
  }
  return out;
}

// ------------------------------------------------------------- 任务扫描 / 汇总
/** @type {Map<string, {mtimeMs:number,size:number,sum:any}>} */
const summaryCache = new Map();

function walkJsonl(dir, depth, out) {
  if (depth > MAX_SCAN_DEPTH) return;
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return;
  }
  for (const e of entries) {
    const abs = path.join(dir, e.name);
    if (e.isDirectory()) {
      if (e.name === 'node_modules' || e.name === 'tokens') continue;
      walkJsonl(abs, depth + 1, out);
    } else if (e.isFile() && e.name.endsWith('.jsonl')) {
      out.push(abs);
    }
  }
}

function scanJsonlFiles() {
  const out = [];
  walkJsonl(CACHE, 0, out);
  return out;
}

function readTextLimited(file, size) {
  const LIMIT = 8 * 1024 * 1024;
  if (size <= LIMIT) return fs.readFileSync(file, 'utf8');
  // 超大文件：只取尾部，避免控制台把内存吃光。
  const fd = fs.openSync(file, 'r');
  try {
    const len = Math.min(size, LIMIT);
    const buf = Buffer.alloc(len);
    fs.readSync(fd, buf, 0, len, size - len);
    return buf.toString('utf8');
  } finally {
    fs.closeSync(fd);
  }
}

const STATUS_MAP = {
  success: '成功',
  ok: '成功',
  failed: '失败',
  failure: '失败',
  error: '失败',
  unknown: '未知',
  waiting_input: '等输入',
  blocked: '被风控',
  'hard-reject': '硬拒',
  canceled: '已取消',
  cancelled: '已取消',
  dryrun: '演练(未真跑)',
};

function detectType(records, dirName) {
  const name = dirName.toLowerCase();
  if (/apple|icloud|gsa|hero|idmsa/.test(name)) return 'apple';
  if (/outlook|msa|hotmail|live-?cn/.test(name)) return 'outlook';
  let apple = 0;
  let outlook = 0;
  for (const r of records) {
    const s = JSON.stringify(r).slice(0, 600);
    if (/appleid\.apple\.com|gsa\.apple\.com|idmsa\.apple|hero_status|jwt_ignored/.test(s)) apple += 1;
    if (/login\.live\.com|signup\.live\.com|outlook\.live\.com|new-address|createAccount/i.test(s)) outlook += 1;
  }
  if (apple === 0 && outlook === 0) return 'unknown';
  return apple >= outlook ? 'apple' : 'outlook';
}

function fmtRecord(r) {
  const t = typeof r.t === 'number' ? new Date(r.t).toISOString().slice(11, 19) : '--:--:--';
  let msg = r.msg || r.message || '';
  if (!msg) {
    const parts = [];
    if (r.kind) parts.push(r.kind);
    if (r.at) parts.push('@' + r.at);
    if (r.field) parts.push('field=' + r.field);
    if (r.url) parts.push(String(r.url).replace(/^https?:\/\//, '').slice(0, 120));
    if (r.status !== undefined) parts.push('status=' + r.status);
    if (r.state) parts.push('state=' + r.state);
    if (r.reason) parts.push('reason=' + r.reason);
    if (r.error) parts.push('error=' + r.error);
    if (r.evidence) parts.push('evidence=' + r.evidence);
    if (r.code) parts.push('code=' + r.code);
    if (r.length !== undefined && r.field) parts.push('len=' + r.length);
    if (r.text) parts.push('text=' + String(r.text).replace(/\s+/g, ' ').slice(0, 120));
    msg = parts.join(' ');
  }
  return `[${t}] ${String(msg).replace(/\s+/g, ' ').slice(0, 320)}`;
}

// 列表页只要一个数字：浅扫两层目录里的图片数（detail 页才做完整索引）。
function countShotsShallow(dirAbs) {
  let n = 0;
  const one = (d) => {
    let entries;
    try {
      entries = fs.readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      if (e.isFile() && /\.(png|jpg|jpeg|webp)$/i.test(e.name)) n += 1;
      else if (e.isDirectory() && /shot|screen|img/i.test(e.name)) one(path.join(d, e.name));
    }
  };
  one(dirAbs);
  return n;
}

function summarizeFile(file) {  let st;
  try {
    st = fs.statSync(file);
  } catch {
    return null;
  }
  const cached = summaryCache.get(file);
  if (cached && cached.mtimeMs === st.mtimeMs && cached.size === st.size) return cached.sum;

  let raw = '';
  try {
    raw = readTextLimited(file, st.size);
  } catch {
    return null;
  }
  const lines = raw.split(/\r?\n/).filter((l) => l.trim());
  const records = [];
  const CAP = 6000;
  const from = Math.max(0, lines.length - CAP);
  for (let i = from; i < lines.length; i += 1) {
    try {
      records.push(JSON.parse(lines[i]));
    } catch {
      /* 坏行跳过 */
    }
  }

  const dirName = path.basename(path.dirname(file));
  const ts = records.map((r) => r.t).filter((n) => typeof n === 'number');
  const startedMs = ts.length ? ts[0] : Math.round(st.birthtimeMs || st.mtimeMs);
  const lastMs = ts.length ? ts[ts.length - 1] : Math.round(st.mtimeMs);

  let terminal = null;
  for (let i = records.length - 1; i >= 0; i -= 1) {
    const r = records[i];
    if (r.kind === 'final' || r.kind === 'close') {
      terminal = r;
      break;
    }
    if (!terminal && (r.state || r.kind === 'summary')) terminal = r;
  }

  const rawState = String((terminal && (terminal.state || terminal.reason)) || '').trim();
  let status;
  if (rawState) status = STATUS_MAP[rawState] || rawState;
  else if (Date.now() - st.mtimeMs < 5 * 60 * 1000) status = '进行中?';
  else status = '无终态';

  const errorCount = records.filter(
    (r) => r.kind === 'error' || r.level === 'ERROR' || r.ok === false,
  ).length;

  const resultBits = [];
  if (terminal) {
    if (terminal.state) resultBits.push('state=' + terminal.state);
    if (terminal.reason) resultBits.push('reason=' + terminal.reason);
    if (terminal.evidence) resultBits.push('evidence=' + terminal.evidence);
    if (terminal.error) resultBits.push('error=' + terminal.error);
  }
  const result = resultBits.join(' ') || (records.length ? '—' : '空文件');

  const sum = {
    // id 用「文件相对路径去掉 .jsonl」，同一个目录下多个 jsonl 也能各占一行
    id: (relFromCache(file) || '').replace(/\.jsonl$/i, ''),
    name: path.basename(file),
    file: relFromRoot(file),
    dir: relFromRoot(path.dirname(file)),    type: detectType(records.slice(-400), dirName),
    status,
    rawState,
    startedMs,
    startedAt: nowIso(startedMs),
    lastMs,
    durationMs: Math.max(0, lastMs - startedMs),
    lineCount: lines.length,
    recordCount: records.length,
    errorCount,
    result,
    mtimeMs: Math.round(st.mtimeMs),
    size: st.size,
    shotsCount: countShotsShallow(path.dirname(file)),
    tail: records.slice(-TAIL_DEFAULT).map(fmtRecord),
  };
  summaryCache.set(file, { mtimeMs: st.mtimeMs, size: st.size, sum });
  return sum;
}

function listTasks() {
  const files = scanJsonlFiles();
  const tasks = [];
  for (const f of files) {
    const s = summarizeFile(f);
    if (s) tasks.push(s);
  }
  tasks.sort((a, b) => b.mtimeMs - a.mtimeMs);
  return tasks;
}

// ------------------------------------------------------------------ 截图索引
function listShots(dirAbs) {
  const out = [];
  const walk = (d, depth) => {
    if (depth > 3 || out.length > 400) return;
    let entries;
    try {
      entries = fs.readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      const abs = path.join(d, e.name);
      if (e.isDirectory()) walk(abs, depth + 1);
      else if (e.isFile() && /\.(png|jpg|jpeg|webp)$/i.test(e.name)) {
        try {
          const st = fs.statSync(abs);
          out.push({ name: e.name, path: relFromRoot(abs), size: st.size, mtimeMs: Math.round(st.mtimeMs) });
        } catch {
          /* ignore */
        }
      }
    }
  };
  walk(dirAbs, 0);
  out.sort((a, b) => b.mtimeMs - a.mtimeMs);
  return out;
}

// ------------------------------------------------------------------- 邮箱面板
async function loadMailboxes() {
  try {
    const raw = await fsp.readFile(MAILBOXES, 'utf8');
    const arr = JSON.parse(raw);
    return Array.isArray(arr) ? arr : [];
  } catch {
    return [];
  }
}

async function loadMailState() {
  try {
    const raw = await fsp.readFile(MAIL_STATE, 'utf8');
    return JSON.parse(raw) || {};
  } catch {
    return {};
  }
}

async function saveMailState(state) {
  await fsp.writeFile(MAIL_STATE, JSON.stringify(state, null, 2), 'utf8');
}

// 只暴露不含凭据的字段：password / refresh_token / client_id 一律不出网。
function sanitizeMailbox(mb, state) {
  const s = state[mb.id] || {};
  return {
    id: mb.id || '',
    address: mb.address || '',
    reader: mb.reader || '',
    status: mb.status || '',
    reason: mb.reason || '',
    source: mb.source || '',
    hasToken: Boolean(mb.refresh_token),
    lastCheckAt: s.at || '',
    lastCheckOk: s.ok === undefined ? null : Boolean(s.ok),
    lastMailAt: s.lastMailAt || '',
    lastMailSubject: s.lastMailSubject || '',
    lastCheckNote: s.note || '',
  };
}

const CODE_RE = /(?<!\d)(\d{6})(?!\d)/;

// graph-mail.cjs 换 access_token 时会把新的 refresh_token 写回它拿到的那个文件；
// 我们用的是 .cache 下的副本，所以这里把轮换后的 token 同步回 mailboxes.json，
// 否则池里那条记录会因为微软 retire 旧 token 而作废。写之前先备份。
async function syncRotatedToken(mb, tokenFile) {
  let fresh = null;
  try {
    fresh = JSON.parse(await fsp.readFile(tokenFile, 'utf8'));
  } catch {
    return false;
  }
  if (!fresh || !fresh.refresh_token || fresh.refresh_token === mb.refresh_token) return false;

  const boxes = await loadMailboxes();
  const idx = boxes.findIndex((x) => String(x.id) === String(mb.id));
  if (idx < 0) return false;

  const stamp = new Date().toISOString().replace(/[-:.TZ]/g, '').slice(0, 14);
  try {
    await fsp.copyFile(MAILBOXES, path.join(CONSOLE_DIR, 'mailboxes.backup-' + stamp + '.json'));
  } catch {
    /* 备份失败不阻断 */
  }
  boxes[idx].refresh_token = fresh.refresh_token;
  const tmp = MAILBOXES + '.tmp';
  await fsp.writeFile(tmp, JSON.stringify(boxes, null, 2), 'utf8');
  await fsp.rename(tmp, MAILBOXES);
  return true;
}

function runCapture(scriptRel, args, timeoutMs) {
  return new Promise((resolve) => {
    const started = Date.now();
    const child = spawn(process.execPath, [path.join(ROOT, scriptRel), ...args], {
      cwd: ROOT,
      env: { ...process.env },
      windowsHide: true,
    });
    let out = '';
    let err = '';
    const timer = setTimeout(() => {
      try {
        child.kill();
      } catch {
        /* ignore */
      }
    }, timeoutMs);
    child.stdout.on('data', (d) => {
      out += d.toString();
      if (out.length > 4 * 1024 * 1024) out = out.slice(-4 * 1024 * 1024);
    });
    child.stderr.on('data', (d) => {
      err += d.toString();
      if (err.length > 256 * 1024) err = err.slice(-256 * 1024);
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      resolve({ code, stdout: out, stderr: err, elapsedMs: Date.now() - started });
    });
    child.on('error', (e) => {
      clearTimeout(timer);
      resolve({ code: -1, stdout: out, stderr: String(e.message || e), elapsedMs: Date.now() - started });
    });
  });
}

function parseLastJson(text) {
  const a = text.indexOf('{');
  const b = text.lastIndexOf('}');
  if (a < 0 || b <= a) return null;
  try {
    return JSON.parse(text.slice(a, b + 1));
  } catch {
    return null;
  }
}

async function fetchMailboxCode(id) {
  const boxes = await loadMailboxes();
  const mb = boxes.find((x) => String(x.id) === String(id));
  if (!mb) return { code: 404, body: { ok: false, error: '邮箱不存在: ' + id } };

  const reader = String(mb.reader || '');
  const note = 'reader=' + (reader || '未标注');

  if (reader === 'webmail') {
    return {
      code: 200,
      body: {
        ok: false,
        address: mb.address,
        reader,
        note: note + '：验证码走已登录的 Outlook 网页会话，需要浏览器，本控制台未接线（见 README「真跑 / 占位」）。',
        hint: 'node tools/capture/outlook-web-mail.cjs --outdir <任务目录>',
      },
    };
  }
  if (!mb.refresh_token) {
    return {
      code: 200,
      body: { ok: false, address: mb.address, reader, note: note + '：该邮箱没有 refresh_token，无法走 Graph 读信。' },
    };
  }

  // graph-mail.cjs 需要 {client_id, refresh_token, mailbox} 文件；落到 .cache 下（已被 .gitignore 忽略）。
  const tokenFile = path.join(TOKENS_DIR, String(mb.id || 'mb') + '.json');
  await fsp.writeFile(
    tokenFile,
    JSON.stringify(
      { client_id: mb.client_id || '', refresh_token: mb.refresh_token || '', mailbox: mb.address || '' },
      null,
      2,
    ),
    { encoding: 'utf8', mode: 0o600 },
  );

  const r = await runCapture('tools/console/mail-code.cjs', ['--file', tokenFile, '--top', '8'], 90000);
  const parsed = parseLastJson(r.stdout);
  const rotated = await syncRotatedToken(mb, tokenFile);

  const state = await loadMailState();
  const at = nowIso(Date.now());

  if (!parsed || parsed.ok !== true) {
    const errText = String((parsed && parsed.error) || r.stderr || r.stdout || '').slice(0, 400);
    state[mb.id] = { at, ok: false, note: errText.slice(0, 200) };
    await saveMailState(state);
    return {
      code: 200,
      body: {
        ok: false,
        address: mb.address,
        reader,
        note: note + '：读信失败',
        error: errText,
        elapsedMs: r.elapsedMs,
      },
    };
  }

  const messages = (parsed.messages || []).map((m) => ({
    received: m.received || '',
    from: String(m.from || ''),
    subject: String(m.subject || ''),
    toSelf: true,
    code: String(m.code || '') || ((String(m.subject || '').match(CODE_RE) || [])[1] || ''),
  }));
  const newest = messages[0] || null;
  state[mb.id] = {
    at,
    ok: true,
    note: messages.length ? 'messages=' + messages.length : 'inbox empty',
    lastMailAt: (newest && newest.received) || '',
    lastMailSubject: (newest && newest.subject) || '',
  };
  await saveMailState(state);

  return {
    code: 200,
    body: {
      ok: true,
      address: mb.address,
      reader,
      mailbox: parsed.mailbox || mb.address,
      count: messages.length,
      code: String(parsed.code || '') || (newest && newest.code) || '',
      hit: parsed.hit || null,
      messages,
      rotated,
      elapsedMs: r.elapsedMs,
      note: note + '：读信成功（只读，不触发任何注册动作）',
    },
  };
}

// ------------------------------------------------------------------- 发起任务
const MODE_ROUTES = {
  outlook: [
    { id: 'outlook-cn', script: 'tools/capture/outlook-cn.cjs', label: 'outlook-cn（中文环境直连）' },
    { id: 'outlook-roxy', script: 'tools/capture/outlook-roxy.cjs', label: 'outlook-roxy（Roxy 指纹）' },
    { id: 'outlook-native', script: 'tools/capture/outlook-native.cjs', label: 'outlook-native（本机 Chrome）' },
  ],
  apple: [{ id: 'create-account', script: 'tools/capture/create-account.cjs', label: 'create-account（Apple 端到端）' }],
};

function buildPlan(body) {
  const mode = String(body.mode || '').toLowerCase();
  if (!MODES_OK.has(mode)) throw new Error('未知任务类型: ' + (body.mode || ''));
  const routes = MODE_ROUTES[mode];
  const routeId = String(body.route || routes[0].id);
  const route = routes.find((r) => r.id === routeId);
  if (!route) throw new Error('未知执行路线: ' + routeId);

  const jobId = new Date().toISOString().replace(/[-:.TZ]/g, '').slice(0, 14) + '-' + mode;
  const outdir = path.join(JOBS_DIR, jobId);
  const p = body.params || {};
  const args =
    mode === 'outlook'
      ? ['--runs', outdir]
      : [
          '--email',
          String(p.email || '<EMAIL>'),
          '--phone',
          String(p.phone || '<PHONE>'),
          '--activation',
          String(p.activation || '<ACTIVATION_ID>'),
          '--outdir',
          outdir,
        ];
  if (p.useRoxy === true && mode === 'apple') args.push('--use-roxy');
  if (p.proxyPort) args.push('--proxy-port', String(p.proxyPort));

  return { mode, route, jobId, outdir, args };
}

const MODES_OK = new Set(['outlook', 'apple']);

function displayCommand(plan) {
  const parts = ['node', plan.route.script, ...plan.args];
  return parts.map((x) => (/[\s]/.test(x) ? JSON.stringify(x) : x)).join(' ');
}

async function writeDryRun(plan) {
  await fsp.mkdir(plan.outdir, { recursive: true });
  const file = path.join(plan.outdir, 'run.jsonl');
  const t0 = Date.now();
  const rec = (obj) => JSON.stringify({ t: Date.now(), ...obj }) + '\n';

  const steps = [
    { kind: 'step', at: 'console', msg: 'DRY-RUN：控制台演练，未启动浏览器、未发起任何真实注册' },
    { kind: 'step', at: 'plan', mode: plan.mode, route: plan.route.id, msg: '计划命令: ' + displayCommand(plan) },
    { kind: 'step', at: 'plan', msg: '密码/token 不在命令行出现（Apple 走 APPLESERVICES_PASSWORD 环境变量）' },
    { kind: 'step', at: 'preflight', msg: '（演练）预检：Roxy/Chrome 可达性检查跳过' },
    { kind: 'step', at: 'flow', msg: '（演练）状态机：第一屏 → 表单 → 提交 → 终态' },
    {
      kind: 'final',
      state: 'dryrun',
      evidence: 'console-dry-run',
      elapsed_ms: Date.now() - t0,
      msg: '演练结束：未创建任何账号',
    },
  ];

  await fsp.writeFile(file, '', 'utf8');
  for (const s of steps) {
    await fsp.appendFile(file, rec(s), 'utf8');
    await sleep(120);
  }
  return file;
}

// ---------------------------------------------------------------- HTTP handler
async function handleApi(req, res, url) {
  const p = url.pathname;

  if (p === '/api/health') {
    const tasks = listTasks();
    return json(res, 200, {
      ok: true,
      version: VERSION,
      port: PORT,
      root: ROOT,
      cache: relFromRoot(CACHE),
      tasks: tasks.length,
      allowReal: ALLOW_REAL,
      now: nowIso(Date.now()),
    });
  }

  if (p === '/api/tasks') {
    const q = (url.searchParams.get('q') || '').trim().toLowerCase();
    const limit = Math.min(500, Number(url.searchParams.get('limit') || 200));
    let tasks = listTasks();
    if (q) {
      tasks = tasks.filter((t) =>
        [t.id, t.type, t.status, t.result].join(' ').toLowerCase().includes(q),
      );
    }
    return json(res, 200, { ok: true, total: tasks.length, tasks: tasks.slice(0, limit) });
  }

  if (p === '/api/task') {
    const id = url.searchParams.get('id') || '';
    const tasks = listTasks();
    const hit = tasks.find((t) => t.id === id);
    if (!hit) return json(res, 404, { ok: false, error: '任务不存在: ' + id });
    const dirAbs = path.resolve(ROOT, hit.dir);
    const shots = insideCache(dirAbs) ? listShots(dirAbs) : [];
    // 详情页要更长的日志尾巴
    let detailTail = hit.tail;
    try {
      const raw = await fsp.readFile(path.resolve(ROOT, hit.file), 'utf8');
      const lines = raw.split(/\r?\n/).filter((l) => l.trim()).slice(-TAIL_DETAIL);
      detailTail = lines.map((l) => {
        try {
          return fmtRecord(scrub(JSON.parse(l)));
        } catch {
          return l.slice(0, 320);
        }
      });
    } catch {
      /* 用默认尾巴 */
    }
    return json(res, 200, { ok: true, task: { ...hit, tail: detailTail }, shots });
  }

  if (p === '/api/mailboxes') {
    const boxes = await loadMailboxes();
    const state = await loadMailState();
    return json(res, 200, {
      ok: true,
      total: boxes.length,
      source: relFromRoot(MAILBOXES),
      mailboxes: boxes.map((mb) => sanitizeMailbox(mb, state)),
    });
  }

  if (p === '/api/mail/code' && req.method === 'POST') {
    const body = await readBody(req);
    const out = await fetchMailboxCode(String(body.id || ''));
    return json(res, out.code, out.body);
  }

  if (p === '/api/routes') {
    return json(res, 200, {
      ok: true,
      allowReal: ALLOW_REAL,
      modes: Object.entries(MODE_ROUTES).map(([mode, routes]) => ({
        mode,
        routes: routes.map((r) => ({ id: r.id, label: r.label, script: r.script })),
      })),
    });
  }

  if (p === '/api/launch' && req.method === 'POST') {
    const body = await readBody(req);
    let plan;
    try {
      plan = buildPlan(body);
    } catch (e) {
      return json(res, 400, { ok: false, error: String(e.message || e) });
    }
    const dry = body.dryRun !== false;
    if (dry) {
      const file = await writeDryRun(plan);
      return json(res, 200, {
        ok: true,
        dryRun: true,
        jobId: plan.jobId,
        taskId: relFromCache(file).replace(/\.jsonl$/i, ''),
        mode: plan.mode,
        route: plan.route.id,
        command: displayCommand(plan),
        file: relFromRoot(file),
        note: '演练已写入 .cache/console/jobs，可在任务列表里看到；未创建任何账号。',
      });
    }
    if (!ALLOW_REAL) {
      return json(res, 409, {
        ok: false,
        dryRun: false,
        error: '真跑未接线：本控制台默认只做 dry-run 演练。',
        command: displayCommand(plan),
        note: '要真跑请手动执行上面的命令；或设 CONSOLE_ALLOW_REAL=1 并重启控制台（默认仍不建议）。',
      });
    }
    return json(res, 501, {
      ok: false,
      error: 'CONSOLE_ALLOW_REAL=1 已打开，但本版本仍未接线真跑执行器（防止误触真实注册）。',
      command: displayCommand(plan),
    });
  }

  if (p === '/api/shot') {
    const rel = url.searchParams.get('path') || '';
    const abs = path.resolve(ROOT, rel);
    if (!insideCache(abs) || !/\.(png|jpg|jpeg|webp)$/i.test(abs)) {
      return json(res, 400, { ok: false, error: '只允许读取 .cache 下的图片' });
    }
    try {
      const st = await fsp.stat(abs);
      res.writeHead(200, {
        'Content-Type': abs.toLowerCase().endsWith('.png') ? 'image/png' : 'image/jpeg',
        'Content-Length': st.size,
        'Cache-Control': 'no-store',
      });
      fs.createReadStream(abs).pipe(res);
    } catch {
      return json(res, 404, { ok: false, error: '图片不存在' });
    }
    return undefined;
  }

  return json(res, 404, { ok: false, error: 'unknown api: ' + p });
}

const STATIC_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
};

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://' + HOST + ':' + PORT);
  try {
    if (url.pathname.startsWith('/api/')) {
      await handleApi(req, res, url);
      return;
    }
    const rel = url.pathname === '/' ? 'index.html' : url.pathname.replace(/^\/+/, '');
    const abs = path.resolve(PUBLIC_DIR, rel);
    if (!abs.startsWith(PUBLIC_DIR + path.sep)) {
      res.writeHead(403).end('forbidden');
      return;
    }
    const st = await fsp.stat(abs);
    res.writeHead(200, {
      'Content-Type': STATIC_TYPES[path.extname(abs).toLowerCase()] || 'application/octet-stream',
      'Content-Length': st.size,
      'Cache-Control': 'no-store',
    });
    fs.createReadStream(abs).pipe(res);
  } catch (e) {
    if (url.pathname.startsWith('/api/')) {
      json(res, 500, { ok: false, error: String((e && e.message) || e) });
    } else {
      res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' }).end('not found');
    }
  }
});

server.listen(PORT, HOST, () => {
  console.log(`[console] listening http://${HOST}:${PORT}`);
  console.log(`[console] workspace ${ROOT}`);
  console.log(`[console] cache     ${CACHE}`);
  console.log(`[console] allowReal ${ALLOW_REAL ? 'yes' : 'no (dry-run only)'}`);
});

process.on('SIGINT', () => {
  console.log('[console] bye');
  server.close(() => process.exit(0));
});
