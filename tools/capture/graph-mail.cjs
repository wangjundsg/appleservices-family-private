// Graph mail reader: turn an Outlook/Microsoft-account refresh token into the
// verification code Apple just mailed to that mailbox.
//
// Why this exists: the create driver used to poll one specific temp-mail API
// (cloudflare_temp_email) for its code, so a mailbox that speaks IMAP/Graph was
// unusable without a human copying digits into a file. A refresh token for the
// Microsoft consumer endpoint is enough to read the inbox through Graph, which
// makes the run unattended again.
//
// Two facts about the consumer endpoint that cost a round of debugging:
//   * a token minted with `https://graph.microsoft.com/Mail.Read` is refused for
//     a refresh_token grant ("AADSTS70000 ... scopes requested are unauthorized");
//     the token has to be re-requested with `https://graph.microsoft.com/.default`,
//     which comes back as an opaque MSA token that Graph accepts.
//   * asking for no scope at all returns a token for Outlook REST
//     (outlook.office.com), which Graph rejects with "The token could not be read".
//
// Security: the code itself is never returned to a log. Callers get it in memory
// and are expected to log only the message id and the elapsed time, the same
// contract account-flow.codeLogEntry implements for the temp-mail path. A rotated
// refresh token is written back to the token file (mode 0600) because Microsoft
// invalidates the old one.
//
// Usage (CLI, for verification only):
//   node tools/capture/graph-mail.cjs --file .cache/apple-run/graph.json --list
//   node tools/capture/graph-mail.cjs --file .cache/apple-run/graph.json --poll --since <iso>

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const flow = require("./account-flow.cjs");

const TOKEN_URL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token";
const GRAPH = "https://graph.microsoft.com/v1.0";
// Default scope that actually yields a Graph-usable token for an MSA.
const DEFAULT_SCOPE = "https://graph.microsoft.com/.default";
const SELECT = "id,subject,from,toRecipients,ccRecipients,receivedDateTime,bodyPreview,body";
const POLL_INTERVAL_MS = 5000;
const POLL_TIMEOUT_MS = 180000;

// readTokenFile reads {client_id, refresh_token, mailbox} and refuses a file that
// cannot drive the flow. The complaint names the missing key and never echoes a
// value, so it is safe to print into a run log.
function readTokenFile(file) {
  const resolved = path.resolve(String(file || ""));
  let raw;
  try {
    raw = fs.readFileSync(resolved, "utf8");
  } catch (e) {
    throw new Error(`graph token file unreadable: ${path.basename(resolved)}`);
  }
  let cfg;
  try {
    cfg = JSON.parse(raw);
  } catch {
    throw new Error("graph token file is not JSON");
  }
  const missing = ["client_id", "refresh_token", "mailbox"].filter((k) => !String(cfg[k] || "").trim());
  if (missing.length) throw new Error(`graph token file is missing ${missing.join(", ")}`);
  return {
    file: resolved,
    client_id: String(cfg.client_id).trim(),
    refresh_token: String(cfg.refresh_token).trim(),
    mailbox: String(cfg.mailbox).trim().toLowerCase(),
    scope: String(cfg.scope || DEFAULT_SCOPE).trim() || DEFAULT_SCOPE,
  };
}

// persistRotation keeps the file usable after the first exchange: Microsoft
// returns a new refresh token and retires the old one, so losing it means the
// next run has to be handed a fresh credential by a human.
function persistRotation(cfg, nextToken) {
  if (!nextToken || nextToken === cfg.refresh_token) return false;
  const body = JSON.parse(fs.readFileSync(cfg.file, "utf8"));
  body.refresh_token = nextToken;
  body.client_id = cfg.client_id;
  body.mailbox = cfg.mailbox;
  if (!body.scope) body.scope = cfg.scope;
  const tmp = `${cfg.file}.tmp`;
  fs.writeFileSync(tmp, `${JSON.stringify(body, null, 2)}\n`, { encoding: "utf8", mode: 0o600 });
  fs.renameSync(tmp, cfg.file);
  cfg.refresh_token = nextToken;
  return true;
}

// exchangeToken trades the refresh token for an access token. The scope
// fallback exists because a mailbox can be consented for Graph but refuse the
// narrower Mail.Read scope; the .default scope is what the consumer endpoint
// actually honours.
async function exchangeToken(cfg, fetchImpl = globalThis.fetch) {
  const scopes = cfg.scope === DEFAULT_SCOPE ? [cfg.scope, "https://graph.microsoft.com/Mail.ReadWrite"] : [cfg.scope];
  let last = "";
  for (const scope of scopes) {
    const body = new URLSearchParams({
      grant_type: "refresh_token",
      client_id: cfg.client_id,
      refresh_token: cfg.refresh_token,
      scope,
    });
    const res = await fetchImpl(TOKEN_URL, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
    });
    const text = await res.text();
    let json = null;
    try {
      json = JSON.parse(text);
    } catch {
      /* non-JSON body */
    }
    if (res.ok && json && json.access_token) {
      persistRotation(cfg, json.refresh_token);
      return { ok: true, access_token: json.access_token, scope, expires_in: Number(json.expires_in) || 0 };
    }
    const code = (json && json.error) || `http_${res.status}`;
    const desc = String((json && json.error_description) || "").split("\n")[0].slice(0, 120);
    last = `${code} ${desc}`.trim();
  }
  return { ok: false, error: last || "token exchange refused" };
}

// listMessages returns the newest messages, normalised to the shape the mail
// picker understands. Recipient matching reads the RFC822 header Graph returns
// in `internetMessageHeaders` when asked, but toRecipients is enough here: Apple
// always mails the Apple ID address itself.
async function listMessages(cfg, token, { top = 25, fetchImpl = globalThis.fetch } = {}) {
  const url = `${GRAPH}/me/messages?$top=${top}&$select=${SELECT}&$orderby=receivedDateTime desc`;
  const res = await fetchImpl(url, { headers: { Authorization: `Bearer ${token}` } });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    /* non-JSON body */
  }
  if (!res.ok || !json) {
    const code = (json && json.error && (json.error.code || (json.error.innerError && json.error.innerError.code))) || "";
    const msg = String((json && json.error && json.error.message) || text).slice(0, 160);
    return { ok: false, status: res.status, error: `${code} ${msg}`.trim(), messages: [] };
  }
  const raw = Array.isArray(json.value) ? json.value : [];
  const messages = raw.map((m) => ({
    id: String(m.id || ""),
    subject: String(m.subject || ""),
    from: String((m.from && m.from.emailAddress && m.from.emailAddress.address) || ""),
    to: [
      ...(Array.isArray(m.toRecipients) ? m.toRecipients : []),
      ...(Array.isArray(m.ccRecipients) ? m.ccRecipients : []),
    ].map((r) => String((r && r.emailAddress && r.emailAddress.address) || "").toLowerCase()),
    receivedDateTime: String(m.receivedDateTime || ""),
    bodyPreview: String(m.bodyPreview || ""),
    body: String((m.body && m.body.contentType === "html" ? stripTags(m.body.content) : m.body && m.body.content) || ""),
  }));
  return { ok: true, status: res.status, messages };
}

function stripTags(html) {
  return String(html || "")
    .replace(/<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<script[\s\S]*?<\/script>/gi, " ")
    .replace(/<br\s*\/?>/gi, "\n")
    .replace(/<[^>]+>/g, " ")
    .replace(/&nbsp;/gi, " ")
    .replace(/\s+/g, " ");
}

// codeFromText pulls Apple's 6-digit code out of the mail body. Apple puts it in
// a bold/emphasised element, so a bare "<b>123456</b>" is preferred; the caller
// gets six digits bounded by non-digits either way so a year or a phone number
// fragment cannot be mistaken for the code.
function codeFromText(text) {
  const body = String(text || "");
  const strong = /<b>\s*(\d{6})\s*<\/b>/i.exec(body);
  if (strong && !isYearish(strong[1])) return strong[1];
  const flat = stripTags(body);
  const loose = /(?<!\d)(\d{6})(?!\d)/.exec(flat);
  if (!loose) return "";
  const value = loose[1];
  if (isYearish(value)) return "";
  const around = flat.slice(Math.max(0, loose.index - 90), loose.index + 90).toLowerCase();
  if (/code|verification|verif|验证码|驗證碼|コード|kode/.test(around)) return value;
  return value;
}

function isYearish(digits) {
  const n = Number(digits);
  return n >= 1970 && n <= 2100;
}

// pickCode applies the same three-way match the temp-mail picker uses (this
// mailbox, Apple as sender, verification semantics in the subject) and then the
// newest-wins rule: a second mail invalidates the first code.
function pickCode(messages, { mailbox, since = 0, senderDomains = ["apple.com"] } = {}) {
  const address = String(mailbox || "").toLowerCase();
  const allowed = (Array.isArray(senderDomains) && senderDomains.length ? senderDomains : ["apple.com"]).map((d) =>
    String(d || "").toLowerCase().trim(),
  );
  let best = null;
  for (const m of Array.isArray(messages) ? messages : []) {
    const received = Date.parse(m.receivedDateTime);
    if (!Number.isFinite(received)) continue;
    if (since && received < since) continue;
    if (!address || !m.to.includes(address)) continue;
    const domain = (m.from.split("@")[1] || "").toLowerCase();
    const trusted = allowed.some((d) => domain === d || domain.endsWith(`.${d}`)) || /apple/i.test(domain);
    if (!trusted) continue;
    if (!flow.VERIFICATION_SUBJECT.test(m.subject) && !/验证码|驗證碼|verification code|security code/i.test(m.subject)) continue;
    const code = codeFromText(m.body) || codeFromText(m.bodyPreview);
    if (!code) continue;
    if (!best || received > best.received) best = { code, messageId: m.id, received, subject: m.subject, from: m.from };
  }
  if (!best) return null;
  return {
    code: best.code,
    messageId: best.messageId,
    received: new Date(best.received).toISOString(),
    subject: best.subject,
    from: best.from,
  };
}

// pollCode is the create driver's entry point: refresh the token once, then read
// the inbox until a matching code appears or the window closes. Every failure is
// reported as {ok:false, error} instead of thrown, so the driver keeps its own
// exit-code contract.
async function pollCode({
  file,
  since = 0,
  mailbox = "",
  senderDomains,
  intervalMs = POLL_INTERVAL_MS,
  timeoutMs = POLL_TIMEOUT_MS,
  fetchImpl = globalThis.fetch,
  onTick = () => {},
} = {}) {
  const started = Date.now();
  let cfg;
  try {
    cfg = readTokenFile(file);
  } catch (e) {
    return { ok: false, error: String(e && e.message ? e.message : e), elapsedMs: 0 };
  }
  const target = String(mailbox || cfg.mailbox).toLowerCase();
  const tok = await exchangeToken(cfg, fetchImpl);
  if (!tok.ok) return { ok: false, error: `token: ${tok.error}`, elapsedMs: Date.now() - started };

  let rounds = 0;
  let lastError = "";
  let seen = 0;
  // `since` is the moment the form was submitted; a mail that arrives a second
  // earlier is still this run's code, so the window is nudged back.
  const floor = since ? since - 90_000 : 0;
  while (Date.now() - started < timeoutMs) {
    rounds += 1;
    const list = await listMessages(cfg, tok.access_token, { fetchImpl });
    if (!list.ok) {
      lastError = list.error || `http ${list.status}`;
      // An expired token mid-poll is worth one silent refresh; anything else is
      // retried on the next tick.
      if (/InvalidAuthenticationToken|token could not be read|401/i.test(lastError)) {
        const again = await exchangeToken(cfg, fetchImpl);
        if (again.ok) tok.access_token = again.access_token;
      }
    } else {
      seen = list.messages.length;
      const hit = pickCode(list.messages, { mailbox: target, since: floor, senderDomains });
      onTick({ round: rounds, messages: seen, hit: Boolean(hit) });
      if (hit) {
        return {
          ok: true,
          code: hit.code,
          messageId: hit.messageId,
          received: hit.received,
          rounds,
          elapsedMs: Date.now() - started,
        };
      }
      lastError = "";
    }
    if (Date.now() - started + intervalMs >= timeoutMs) break;
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  return {
    ok: false,
    error: lastError || `no matching mail in ${rounds} poll(s) over ${Math.round((Date.now() - started) / 1000)}s (inbox ${seen})`,
    rounds,
    elapsedMs: Date.now() - started,
  };
}

// ---- CLI ------------------------------------------------------------------

function parseArgs(argv) {
  const out = { file: "", list: false, poll: false, since: "" };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === "--list") out.list = true;
    else if (a === "--poll") out.poll = true;
    else if (a === "--file") out.file = argv[++i] || "";
    else if (a === "--since") out.since = argv[++i] || "";
    else if (a === "--help" || a === "-h") out.help = true;
    else throw new Error(`unknown argument ${a}`);
  }
  return out;
}

async function main(argv) {
  let options;
  try {
    options = parseArgs(argv);
  } catch (e) {
    console.log(JSON.stringify({ ok: false, error: String(e.message || e) }));
    return 1;
  }
  if (options.help) {
    console.log("usage: node tools/capture/graph-mail.cjs --file <graph.json> [--list|--poll] [--since ISO]");
    return 0;
  }
  if (!options.file) {
    console.log(JSON.stringify({ ok: false, error: "--file is required" }));
    return 1;
  }
  let cfg;
  try {
    cfg = readTokenFile(options.file);
  } catch (e) {
    console.log(JSON.stringify({ ok: false, error: String(e.message || e) }));
    return 1;
  }
  if (options.poll) {
    const hit = await pollCode({ file: cfg.file, since: options.since ? Date.parse(options.since) : 0 });
    // Only the message id and the timing: the code is not a loggable value.
    console.log(
      JSON.stringify({
        ok: hit.ok,
        mailbox: cfg.mailbox,
        message_id: hit.messageId || "",
        rounds: hit.rounds || 0,
        elapsed_ms: hit.elapsedMs || 0,
        error: hit.error || "",
      }),
    );
    return hit.ok ? 0 : 1;
  }
  const tok = await exchangeToken(cfg);
  if (!tok.ok) {
    console.log(JSON.stringify({ ok: false, error: `token: ${tok.error}` }));
    return 1;
  }
  const list = await listMessages(cfg, tok.access_token);
  if (!list.ok) {
    console.log(JSON.stringify({ ok: false, error: list.error }));
    return 1;
  }
  console.log(
    JSON.stringify(
      {
        ok: true,
        mailbox: cfg.mailbox,
        scope: tok.scope,
        messages: list.messages.map((m) => ({
          received: m.receivedDateTime,
          from: m.from,
          subject: m.subject.slice(0, 90),
          to_self: m.to.includes(cfg.mailbox),
        })),
      },
      null,
      2,
    ),
  );
  return 0;
}

if (require.main === module) {
  main(process.argv.slice(2)).then((code) => process.exit(code));
}

module.exports = {
  DEFAULT_SCOPE,
  TOKEN_URL,
  codeFromText,
  exchangeToken,
  listMessages,
  parseArgs,
  persistRotation,
  pickCode,
  pollCode,
  readTokenFile,
  stripTags,
};
