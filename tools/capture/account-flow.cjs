// Pure decision helpers shared by the Apple Account creation scripts.
//
// Nothing in this file touches the browser, the filesystem or the network: it
// takes what the run already observed and returns a verdict. That is deliberate
// — the two things the code review insisted on (never invent a success, never
// put a credential into a log) are the two things that are hardest to eyeball
// inside a 600-line Playwright script, so they live here where they can be
// tested with `node --test tools/capture/account-flow.test.cjs`.
//
// Review items implemented here:
//   A1  no hard-coded default password (resolvePassword / missingPasswordReport)
//   A2  only a whitelist of fields reaches run.jsonl (summarizeHTTP / codeLogEntry)
//   A3  a timed-out click is classified instead of blindly repeated (clickTimeoutOutcome)
//   B4  every screen maps to one state; only explicit evidence is success (classifyScreen)
//   B5  three-way mail match: newest by created_at, addressed to this mailbox,
//       from a trusted sender, subject with verification semantics (pickEmailCode)

"use strict";

// Exit codes of the driver scripts. 0 is reserved for "the account demonstrably
// exists"; everything else tells the caller whether retrying is pointless.
const EXIT_CODES = Object.freeze({
  success: 0, // explicit terminal evidence only (review B4)
  failed: 1, // something provably did not happen; safe to retry from scratch
  usage: 2, // the run never started (missing/!valid arguments)
  unknown: 3, // a submission may or may not have happened: re-read, never replay
  waiting_input: 4, // code never arrived; the account is still mid-flow
  likely_success: 5, // the page navigated away after the SMS code; confirm by signing in
});

const PASSWORD_ENV = "APPLESERVICES_PASSWORD";

// resolvePassword implements review A1. The driver used to fall back to a
// literal password baked into the source, so any run that forgot --password
// silently created an account whose password the whole repository knows. There
// is no default any more: --password first, APPLESERVICES_PASSWORD second (so
// the value does not have to sit in the process list), otherwise the caller
// gets an empty result and has to stop before the first network operation.
function resolvePassword(argvPassword, envPassword) {
  const fromArg = typeof argvPassword === "string" ? argvPassword : "";
  if (fromArg.trim()) return { password: fromArg, source: "--password" };
  const fromEnv = typeof envPassword === "string" ? envPassword : "";
  if (fromEnv.trim()) return { password: fromEnv, source: PASSWORD_ENV };
  return { password: "", source: "" };
}

// The refusal is structured and value-free: a caller can branch on it and an
// operator can read it, and neither of them ever sees a password.
function missingPasswordReport(script, reason) {
  return {
    error: "missing_required_argument",
    argument: "--password",
    alternative: PASSWORD_ENV,
    script,
    reason,
  };
}

// ---- mail (review B5) ------------------------------------------------------

// Only mails that actually carry a code are candidates. Apple's sign-in mails
// say "verification code"/"security code"; the Chinese copy says 验证码.
const VERIFICATION_SUBJECT = /verif|security code|confirmation code|sign[- ]?in code|two[- ]?factor|one[- ]?time|验证码|驗證碼/i;

// Apple code mail senders. A temp-mail inbox receives whatever the internet
// throws at it, so the sender is part of the match, not decoration.
const DEFAULT_SENDER_DOMAINS = Object.freeze(["apple.com", "icloud.com"]);

// parseMailTime normalises the two shapes the mailbox API uses
// ("2026-11-13T01:00:00Z" and the space-separated "2026-11-13 01:00:00", which
// is UTC). Anything unparseable returns NaN and is dropped by the caller: a
// mail with a broken clock must not be promoted to "newest".
function parseMailTime(value) {
  const text = typeof value === "string" ? value.trim() : "";
  if (!text) return NaN;
  let candidate = text;
  if (!/[zZ]$|[+-]\d{2}:?\d{2}$/.test(candidate)) {
    candidate = `${candidate.includes(" ") ? candidate.replace(" ", "T") : candidate}Z`;
  }
  const parsed = Date.parse(candidate);
  return Number.isFinite(parsed) ? parsed : NaN;
}

// senderDomain accepts both "no_reply@email.apple.com" and the header form
// "Apple <no_reply@email.apple.com>".
function senderDomain(source) {
  const text = String(source || "").toLowerCase();
  const at = text.lastIndexOf("@");
  if (at < 0) return "";
  return text.slice(at + 1).replace(/[^a-z0-9.-]+.*$/, "").replace(/\.$/, "");
}

function senderMatches(source, allowed) {
  const domain = senderDomain(source);
  if (!domain) return false;
  const wanted = (Array.isArray(allowed) && allowed.length ? allowed : DEFAULT_SENDER_DOMAINS)
    .map((item) => String(item || "").toLowerCase().replace(/^\./, "").trim())
    .filter(Boolean);
  return wanted.some((item) => domain === item || domain.endsWith(`.${item}`));
}

function recipientOf(mail) {
  return String((mail && mail.address) || "").trim().toLowerCase();
}

// Subject lives on the API object when the mailbox exposes it; otherwise read
// it back from the raw MIME header so the check still holds.
function mailSubject(mail) {
  const direct = typeof mail?.subject === "string" ? mail.subject.trim() : "";
  if (direct) return direct;
  const raw = typeof mail?.raw === "string" ? mail.raw : "";
  const match = /^Subject:\s*(.+)$/im.exec(raw);
  return match ? match[1].trim() : "";
}

// pickEmailCode is the whole of review B5. The previous code walked the first
// ten messages in whatever order the API returned them and took the first
// "<b>123456</b>" it saw, so an old mail, a mail for a different address on the
// same inbox, or an unrelated 6-digit number could be typed into Apple's form.
// Now every candidate must clear four checks, the newest one wins, and only the
// message id (never the code) is what callers are expected to log.
function pickEmailCode(mails, options = {}) {
  const address = String(options.address || "").trim().toLowerCase();
  const since = Number.isFinite(options.since) ? options.since : 0;
  const allowed = options.senderDomains;
  const list = Array.isArray(mails) ? mails : [];
  const candidates = [];
  for (const mail of list) {
    if (!mail || typeof mail !== "object") continue;
    const createdAt = parseMailTime(mail.created_at);
    if (!Number.isFinite(createdAt)) continue;
    if (since && createdAt < since) continue;
    if (!address || recipientOf(mail) !== address) continue;
    if (!senderMatches(mail.source, allowed)) continue;
    const subject = mailSubject(mail);
    if (!VERIFICATION_SUBJECT.test(subject)) continue;
    const hit = /<b>(\d{6})<\/b>/.exec(String(mail.raw || ""));
    if (!hit) continue;
    candidates.push({ code: hit[1], messageId: String(mail.message_id || mail.id || ""), createdAt });
  }
  if (!candidates.length) return null;
  candidates.sort((a, b) => b.createdAt - a.createdAt);
  // A second mail invalidates the first code, so only the newest survives.
  const newest = candidates[0];
  return {
    code: newest.code,
    messageId: newest.messageId,
    created_at: new Date(newest.createdAt).toISOString(),
  };
}

// codeLogEntry is what run.jsonl is allowed to keep for a received code:
// whether one arrived, how long it took, and which message it came from.
// Review A2: the code itself never reaches the log.
function codeLogEntry({ received, elapsedMs = 0, messageId = "", kind = "" } = {}) {
  const entry = { code_received: Boolean(received), elapsed_ms: Number(elapsedMs) || 0 };
  if (kind) entry.kind = kind;
  if (messageId) entry.message_id = String(messageId);
  return entry;
}

// ---- HTTP logging (review A2) ---------------------------------------------

function errorCodeFromBody(body) {
  const text = typeof body === "string" ? body : "";
  if (!text.trim()) return "";
  let parsed = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    return "";
  }
  if (!parsed || typeof parsed !== "object") return "";
  const numbers = [];
  const push = (value) => {
    if (typeof value === "string" || typeof value === "number") {
      const text = String(value).trim();
      if (text) numbers.push(text);
    }
  };
  push(parsed.errorCode);
  push(parsed.serviceErrors && parsed.serviceErrors[0] && parsed.serviceErrors[0].code);
  push(parsed.code);
  return numbers[0] || "";
}

// summarizeHTTP replaces the old listener that copied 1200 bytes of every
// validate/captcha response into run.jsonl. Those bodies echo the submitted
// identifier and the captcha verdict, so only three fields survive: the status,
// the path (never the query string, which carries widget keys) and an error
// code when Apple sent one.
function summarizeHTTP({ url = "", status = 0, body = "" } = {}) {
  let path = "";
  try {
    path = new URL(String(url)).pathname;
  } catch {
    path = String(url || "").split("?")[0];
  }
  return { status: Number(status) || 0, path, error_code: errorCodeFromBody(body) };
}

// ---- screen classification (review B4) ------------------------------------

// Terminal states of the creation flow. The old loop ended with an
// unconditional `done` + exit 0, so a terms wall, an error page or an expired
// session all looked like a created account. Success now needs an explicit
// terminal page: the account-management URL, or copy that only a finished flow
// shows. Anything else is `unknown`, never success.
const SUCCESS_URL = /\/account\/(?:manage|home)(?:[/?#]|$)/i;
const SUCCESS_TEXT = /you'?re all set|your apple account is ready|account (?:has been )?(?:created|created successfully)|welcome to apple|account created successfully|你的 Apple 账户已(?:创建|就绪)|账户已创建/i;
// Which delivery channel the current screen is waiting on. The channel, not the
// word "code", decides the step: both screens talk about a code, and the phone
// step often quotes the account's email address as well.
const EMAIL_DELIVERY = /(?:sent|delivered|emailed)[^.]{0,40}(?:your email|your inbox|[\w.+-]+@[\w-]+)|verification email|verify your email|check your (?:email|inbox)|email address/i;
const PHONE_DELIVERY = /text message|\bsms\b|your phone number|mobile number|sent to:? \+?\d|\+\d{1,3}[\s.-]?\(?\d{3}\)?[\s.-]?\d{3}[\s.-]?\d{4}|手机号|短信/i;

const SCREEN_STATES = Object.freeze(["email_step", "phone_step", "success", "unknown"]);

// classifyScreen maps one screen readback to exactly one of the four states the
// review asks for. A screen that still asks for a code is never success even if
// it also says "welcome", because that is exactly the page a stale run reads
// right before typing an old code.
function classifyScreen({ text = "", url = "" } = {}) {
  const body = String(text || "").replace(/\s+/g, " ").trim();
  const asksEmail = EMAIL_DELIVERY.test(body);
  const asksPhone = PHONE_DELIVERY.test(body);
  const askingForCode = asksEmail || asksPhone;
  if (!askingForCode && SUCCESS_URL.test(String(url || ""))) return "success";
  if (!askingForCode && SUCCESS_TEXT.test(body)) return "success";
  if (asksEmail) return "email_step";
  if (asksPhone) return "phone_step";
  return "unknown";
}

// ---- click timeout (review A3) --------------------------------------------

// clickTimeoutOutcome decides what may happen after a click timed out.
// The old code answered that question with `click({force:true})`, which can
// submit a form twice and leaves no way to tell afterwards which submit landed.
// Retrying is only allowed when the readback proves nothing was submitted: the
// widget is still there, the form is still on screen and the screen is
// byte-for-byte the same as before the click. Everything else is "unknown",
// which the driver turns into a non-zero exit with no second click.
function clickTimeoutOutcome({ framePresent, formVisible, sameScreen } = {}) {
  if (framePresent !== true) return "unknown";
  if (formVisible !== true) return "unknown";
  if (sameScreen !== true) return "unknown";
  return "retry";
}

module.exports = {
  DEFAULT_SENDER_DOMAINS,
  EXIT_CODES,
  PASSWORD_ENV,
  SCREEN_STATES,
  classifyScreen,
  clickTimeoutOutcome,
  codeLogEntry,
  errorCodeFromBody,
  mailSubject,
  missingPasswordReport,
  parseMailTime,
  pickEmailCode,
  recipientOf,
  resolvePassword,
  senderDomain,
  senderMatches,
  summarizeHTTP,
};
