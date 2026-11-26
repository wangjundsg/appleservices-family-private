// The four flow mechanics the 2026-11-14 G5 run (vi-VN, `.cache/cm/G5b`)
// proved broken, as small injectable pieces:
//
//   A  fillField / selectField   a rejected captcha re-typed the whole form,
//                                because every retry ran a full fillForm()
//   B  findSubmit / clickSubmit  the "Continue" click could land on "Mã Mới"
//                                (send a new code) because it was matched by a
//                                loose selector, and a disabled button looked
//                                exactly like a wrong captcha afterwards
//   C  emailCodeWindow           the email step gave up the moment no code was
//                                in the inbox: no grace window, no resend, and a
//                                whole rented number went with it
//   D  closeRecord               the window was closed without a reason, so a
//                                reader of run.jsonl could not tell "Apple
//                                refused" from "the script died"
//
// Nothing here touches the browser, the filesystem or the network: the caller
// injects the locator, the mouse, the clock and the log sink. That is what lets
// `node --test tools/capture/flow-mechanics.test.cjs` exercise all four
// mechanisms against fake locators instead of a live form.

"use strict";

const clip = (value, max) => String(value == null ? "" : value).slice(0, max);

// Fold for text matching only: Apple writes the same control in every locale,
// and "Gửi mã mới" / "Mã Mới" have to be recognised without shipping a table of
// every diacritic spelling.
const fold = (text) =>
  String(text == null ? "" : text)
    .normalize("NFD")
    .replace(/[\u0300-\u036f]/g, "")
    .toLowerCase()
    .replace(/\s+/g, " ")
    .trim();

// ---- D: the close record ---------------------------------------------------

// The reasons the user asked for verbatim, plus the two terminal screens the
// list did not cover (a phone code that never arrived, a screen that could not
// be classified). Every exit of the driver ends in exactly one of these.
const CLOSE_REASONS = Object.freeze([
  "success",
  "captcha-exhausted",
  "email-code-timeout",
  "submit-failed",
  "hard-reject",
  "script-error",
  "sms-code-timeout",
  "unknown-screen",
]);

function normalizeCloseReason(reason) {
  const text = String(reason == null ? "" : reason).trim();
  return CLOSE_REASONS.includes(text) ? text : "script-error";
}

// closeRecord is the one line every exit path writes. A reason that is not in
// the vocabulary is not dropped silently: it degrades to script-error, which is
// a lie only about the category, never about the fact that the run ended.
function closeRecord(reason, extra = {}) {
  return { reason: normalizeCloseReason(reason), ...extra };
}

// closeReasonFor derives the reason from what the run already knows, for the
// paths that throw instead of naming their ending (a usage refusal, a field
// mismatch, a torn frame). Order matters: the most specific evidence wins.
function closeReasonFor({ state = "", evidence = "", message = "" } = {}) {
  if (state === "success") return "success";
  const text = fold(`${evidence} ${message}`);
  if (/email.{0,6}code/.test(text)) return "email-code-timeout";
  if (/hard[-_ ]?reject/.test(text)) return "hard-reject";
  if (/submit|click[-_ ]timeout/.test(text)) return "submit-failed";
  if (/captcha/.test(text)) return "captcha-exhausted";
  if (/(?:phone|sms).{0,8}code/.test(text)) return "sms-code-timeout";
  if (state === "unknown" || state === "likely_success") return "unknown-screen";
  return "script-error";
}

// ---- A: fill only what needs filling --------------------------------------

// fillField reads the box before typing into it. A rejected submission leaves
// the other fields alone, so a retry that re-types the name, the birthday and
// the password is both slow and an automation tell. `type` is the driver's own
// human-paced typer; `log` is the driver's note().
function fieldError(label) {
  const error = new Error("field_readback_failed");
  error.state = "failed";
  error.evidence = `field_mismatch:${/^[a-z_]+$/.test(label) ? label : "field"}`;
  return error;
}

async function readField(locator, label) {
  try { return await locator.inputValue({ timeout: 5000 }); }
  catch { throw fieldError(label); }
}

async function fillField(locator, value, label, { type, log = () => {}, normalize = String } = {}) {
  const want = String(value == null ? "" : value);
  let have = await readField(locator, label);
  if (normalize(have) === normalize(want)) {
    log("step", { at: "field-kept", field: label });
    return { kept: true, typed: false };
  }
  if (typeof type !== "function") throw fieldError(label);
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      if (have) {
        await locator.press("ControlOrMeta+A", { timeout: 5000 });
        await locator.press("Backspace", { timeout: 5000 });
        if (await readField(locator, label)) throw fieldError(label);
      }
      await type(locator, want, label);
      // Blur triggers the form's own validation before the readback.
      await locator.press("Tab", { timeout: 5000 });
    } catch { throw fieldError(label); }
    have = await readField(locator, label);
    if (normalize(have) === normalize(want)) {
      log("step", { at: "field-verified", field: label, corrected: attempt === 1 });
      return { kept: false, typed: true, corrected: attempt === 1 };
    }
  }
  throw fieldError(label);
}

// selectField is the same read-first rule for the three birthday dropdowns.
async function selectField(locator, value, label, { choose, log = () => {} } = {}) {
  const want = String(value == null ? "" : value);
  const have = await readField(locator, label);
  if (have === want) {
    log("step", { at: "field-kept", field: label });
    return { kept: true, selected: false };
  }
  if (typeof choose !== "function") throw fieldError(label);
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      await choose(locator, want, label);
      await locator.press("Tab", { timeout: 5000 });
    } catch { throw fieldError(label); }
    if (await readField(locator, label) === want) {
      log("step", { at: "field-verified", field: label, corrected: attempt === 1 });
      return { kept: false, selected: true, corrected: attempt === 1 };
    }
  }
  throw fieldError(label);
}

// Use the native select's keyboard behavior; no DOM value assignment.
async function selectByKeyboard(locator, value) {
  const index = await locator.evaluate((el, wanted) => Array.from(el.options)
    .filter(option => !option.disabled && !option.parentElement.disabled)
    .findIndex(option => option.value === wanted), String(value));
  if (index < 0) throw new Error("select_option_unavailable");
  await locator.press("Home", { timeout: 5000 });
  for (let i = 0; i < index; i++) await locator.press("ArrowDown", { timeout: 5000 });
  await locator.press("Enter", { timeout: 5000 });
}

// ---- B: the submit button -------------------------------------------------

// The labels Apple uses for "commit this step". "Tiếp tục" is the vi-VN copy
// and "下一步" the zh copy; the fold turns the first into "tiep tuc".
const SUBMIT_LABELS = Object.freeze(["continue", "继续", "繼續", "tiếp tục", "下一步"]);

// Controls that must never be mistaken for the submit button. "Mã Mới" /
// "Send a new code" / "换一张" all mean "throw the captcha away and draw
// another", which is the worst possible thing to press after typing a code.
const NOT_SUBMIT = /换一张|換一張|new code|ma moi|refresh|another code/i;

const isNotSubmit = (text) => NOT_SUBMIT.test(fold(text)) || NOT_SUBMIT.test(String(text == null ? "" : text));

const textOf = async (locator) =>
  String((await locator.innerText({ timeout: 2000 }).catch(() => "")) || "")
    .replace(/\s+/g, " ")
    .trim();

// findSubmit resolves the button to press, in the order the user asked for:
// a real `button[type="submit"]` first, then the label match. Coordinates are
// never used to decide what to press.
async function findSubmit(frame, { labels = SUBMIT_LABELS, log = () => {} } = {}) {
  const excluded = [];

  const typed = frame.locator('button[type="submit"]');
  const typedCount = await typed.count().catch(() => 0);
  for (let i = 0; i < typedCount; i += 1) {
    const candidate = typed.nth(i);
    const text = await textOf(candidate);
    if (isNotSubmit(text)) {
      excluded.push(text);
      continue;
    }
    return { ok: true, via: "type=submit", locator: candidate, text, excluded };
  }

  const all = frame.locator("button");
  const count = Math.max(await all.count().catch(() => 0), typedCount);
  const wanted = labels.map((label) => fold(label));
  for (let i = 0; i < count; i += 1) {
    const candidate = all.nth(i);
    const text = await textOf(candidate);
    if (!text || isNotSubmit(text)) continue;
    const hay = fold(text);
    if (!wanted.some((label) => hay.includes(label))) continue;
    return { ok: true, via: "label", locator: candidate, text, excluded };
  }
  return { ok: false, reason: "submit-not-found", excluded };
}

// clickSubmit presses the resolved button once and never retries it. A missing
// or permanently disabled button is a named record, not another loop: the old
// code looped the whole form four times against a button that was already
// telling us the submission would not be accepted.
async function clickSubmit(
  frame,
  { mouse = null, log = () => {}, tag = "", labels = SUBMIT_LABELS, now = Date.now, sleep, rand = Math.random, waitEnabledMs = 10000 } = {},
) {
  const found = await findSubmit(frame, { labels, log });
  if (!found.ok) {
    log("error", { at: "submit-not-found", tag, excluded: (found.excluded || []).slice(0, 3) });
    return { clicked: false, reason: "submit-not-found" };
  }
  const button = found.locator;
  let enabled = await button.isEnabled({ timeout: 2000 }).catch(() => true);
  let waited = 0;
  const deadline = now() + waitEnabledMs;
  while (!enabled && now() < deadline) {
    await sleep(500);
    waited += 500;
    enabled = await button.isEnabled({ timeout: 2000 }).catch(() => true);
  }
  if (!enabled) {
    log("error", { at: "submit-disabled", tag, text: clip(found.text, 60), waited_ms: waited, via: found.via });
    return { clicked: false, reason: "submit-disabled", locator: button, text: found.text };
  }

  await button.scrollIntoViewIfNeeded({ timeout: 5000 }).catch(() => {});
  const box = await button.boundingBox().catch(() => null);
  if (box && mouse && typeof mouse.move === "function") {
    await mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 14 }).catch(() => {});
  }
  // Hover, settle, then press: a pointer that lands and clicks in the same tick
  // is as synthetic as a filled value.
  await sleep(300 + Math.floor(rand() * 400));

  try {
    await button.click({ timeout: 15000 });
  } catch (e) {
    log("step", {
      at: "submit-click-timeout",
      tag,
      via: found.via,
      text: clip(found.text, 60),
      error: clip(e && e.message ? e.message : e, 160),
    });
    return { clicked: false, reason: "click-timeout", locator: button, text: found.text, error: clip(e && e.message ? e.message : e, 160) };
  }
  log("step", { at: "submit-clicked", tag, via: found.via, text: clip(found.text, 60) });
  return { clicked: true, via: found.via, locator: button, text: found.text };
}

// ---- C: the email code grace window ---------------------------------------

// The copy of the "send it again" control, in the locales this pipeline meets.
const RESEND_LABELS = Object.freeze([
  "resend",
  "send a new code",
  "send new code",
  "resend code",
  "重新发送",
  "重新傳送",
  "gửi lại",
  "gửi mã mới",
  "envoyer un nouveau code",
  "erneut senden",
]);

// Phone challenge controls are rendered as a menu.  Keep the visible-copy
// fallbacks localized; the caller still scopes them to the active widget.
const PHONE_HELP_LABELS = Object.freeze([
  "didn't get a code", "didn’t get a code", "没有收到验证码", "未收到验证码",
  "não recebeu um código", "não recebeu o código", "nao recebeu um codigo",
]);
const PHONE_RESEND_LABELS = Object.freeze([
  "resend text", "resend code", "重新发送短信", "重新傳送簡訊", "重新傳送短信",
  "reenviar sms", "reenviar mensagem", "reenviar código", "reenviar codigo",
]);
const PHONE_REPLACE_LABELS = Object.freeze([
  "use a different number", "使用其他号码", "使用其他电话号码", "使用其他電話號碼", "使用其他電話",
  "usar um número diferente", "usar outro número", "usar um numero diferente", "usar outro numero",
]);

// channelCode flattens the three pickers (string from the SMS API, object from
// Graph, null from a timeout) into "the code or nothing".
function channelCode(picked) {
  if (!picked) return "";
  if (typeof picked === "string") return picked;
  return String(picked.code || "");
}

// Keep both delivery windows, but never request another verification message.
// Legacy callers may still supply `resend`; it is deliberately ignored.
async function emailCodeWindow({ waitCode, now = Date.now, deadline = Infinity, firstMs = 300000, secondMs = 300000 } = {}) {
  const started = now();
  // Individual readers accept at most five minutes; retain two bounded windows.
  const wait = async ms => {
    const remaining = Math.min(ms, 300000, deadline - now());
    return remaining > 0 ? waitCode(remaining) : null;
  };
  let picked = await wait(firstMs);
  const resent = { asked: false, ok: false, how: "disabled" };
  if (!channelCode(picked)) {
    picked = await wait(secondMs);
  }
  const waitedMs = now() - started;
  const code = channelCode(picked);
  return {
    picked: code ? picked : null,
    code,
    resent,
    waitedMs,
    timedOut: !code,
    rescanned: Boolean(resent && resent.ok),
  };
}

// The phone flow has a different contract from email: wait five minutes,
// request exactly one new SMS, then wait five more minutes.  A second timeout
// hands control to the caller's replacement-number path; it never loops.
async function phoneCodeWindow({ waitCode, resend, now = Date.now, deadline = Infinity,
  firstMs = 300000, secondMs = 300000 } = {}) {
  const started = now();
  const wait = async ms => {
    const remaining = Math.min(ms, 300000, deadline - now());
    return remaining > 0 ? waitCode(remaining) : null;
  };
  let picked = await wait(firstMs);
  let resent = { asked: false, ok: false, how: "not-needed" };
  if (!channelCode(picked) && now() < deadline) {
    resent = await resend();
    if (resent && resent.ok) picked = await wait(secondMs);
  }
  const code = channelCode(picked);
  return {
    picked: code ? picked : null,
    code,
    resent,
    waitedMs: now() - started,
    timedOut: !code,
    replace: !code && Boolean(resent && resent.ok) && now() < deadline,
  };
}

function freshSMSCode(status, excluded = new Set()) {
  const match = /^STATUS_OK:(\d{6})$/.exec(String(status).trim());
  return match && !excluded.has(match[1]) ? match[1] : "";
}

async function onceAction(marker, action) {
  const fs = require("node:fs");
  fs.writeFileSync(marker, JSON.stringify({state: "intent", at: Date.now()}), {flag: "wx", mode: 0o600});
  await action();
  fs.writeFileSync(marker, JSON.stringify({state: "completed", at: Date.now()}), {mode: 0o600});
}

async function phoneMenuAction(frame, labels, click, sleep = ms => new Promise(resolve => setTimeout(resolve, ms))) {
  const help = await findAction(frame, PHONE_HELP_LABELS);
  if (!help.ok) throw new Error("phone_help_not_found");
  await click(help.locator);
  let action;
  for (let i = 0; i < 20; i++) {
    action = await findAction(frame, labels);
    if (action.ok) break;
    await sleep(100);
  }
  if (!action.ok) throw new Error("phone_action_not_found");
  await click(action.locator);
}

// findAction is the text-matched finder used for "send it again": a button or a
// link whose visible text is one of `labels`. It deliberately ignores the
// submit exclusions, because on the email step "Send a new code" is the action
// we want, not the one we fear.
async function findAction(frame, labels, { selector = "button, a, [role=button]" } = {}) {
  const list = frame.locator(selector);
  const count = await list.count().catch(() => 0);
  const wanted = labels.map((label) => fold(label));
  for (let i = 0; i < count; i += 1) {
    const candidate = list.nth(i);
    if (typeof candidate.isVisible === "function" && !await candidate.isVisible()) continue;
    if (typeof candidate.isEnabled === "function" && !await candidate.isEnabled()) continue;
    const text = await textOf(candidate);
    if (!text) continue;
    const hay = fold(text);
    if (!wanted.some((label) => hay.includes(label))) continue;
    return { ok: true, locator: candidate, text };
  }
  return { ok: false, reason: "action-not-found" };
}

// clickElement is the calm version of a click: scroll, hover the centre, settle,
// press. Used for the resend control so it does not carry the submit machinery
// (and can never be logged as a submit).
async function clickElement(locator, { mouse = null, sleep, rand = Math.random } = {}) {
  await locator.scrollIntoViewIfNeeded({ timeout: 5000 }).catch(() => {});
  const box = await locator.boundingBox().catch(() => null);
  if (box && mouse && typeof mouse.move === "function") {
    await mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 12 }).catch(() => {});
  }
  await sleep(300 + Math.floor(rand() * 400));
  await locator.click({ timeout: 15000 });
  return { clicked: true };
}

module.exports = {
  CLOSE_REASONS,
  NOT_SUBMIT,
  PHONE_HELP_LABELS,
  PHONE_REPLACE_LABELS,
  PHONE_RESEND_LABELS,
  RESEND_LABELS,
  SUBMIT_LABELS,
  channelCode,
  clickElement,
  clickSubmit,
  closeReasonFor,
  closeRecord,
  emailCodeWindow,
  fillField,
  findAction,
  findSubmit,
  fold,
  isNotSubmit,
  normalizeCloseReason,
  phoneCodeWindow,
  phoneMenuAction,
  freshSMSCode,
  onceAction,
  selectField,
  selectByKeyboard,
};
