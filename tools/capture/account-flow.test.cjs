// Tests for the pure decision helpers used by the account-creation drivers.
//
// Run with:  node --test tools/capture/account-flow.test.cjs
//
// These cover the review items that cannot be observed from the outside without
// a live account: A1 (no default password), A2 (nothing but a whitelist reaches
// run.jsonl), A3 (a timed-out click is not repeated blindly), B4 (an unclear
// ending is never success) and B5 (three-way mail match). B6 lives in
// icloud/hme_test.go, where the session file is under the test's control.

"use strict";

const assert = require("node:assert/strict");
const { test } = require("node:test");
const flow = require("./account-flow.cjs");

const MAILBOX = "111@abc-temp-2026.shop";

// A mail shaped like the ones the temp-mailbox API returns: `source` is the
// sender, `address` the recipient.
function codeMail(overrides = {}) {
  return {
    id: 1,
    message_id: "msg-1",
    source: "Apple <no_reply@email.apple.com>",
    address: MAILBOX,
    subject: "Your Apple Account verification code",
    created_at: "2026-11-13 01:00:00",
    raw: "<html><body><b>111111</b></body></html>",
    ...overrides,
  };
}

// ---- A1: no hard-coded default password -----------------------------------

test("A1: a missing password resolves to nothing, never to a built-in default", () => {
  assert.deepEqual(flow.resolvePassword("", ""), { password: "", source: "" });
  assert.deepEqual(flow.resolvePassword(undefined, undefined), { password: "", source: "" });
  assert.deepEqual(flow.resolvePassword(null, null), { password: "", source: "" });
  // `--password` without a value is parsed as the boolean true by both scripts.
  assert.deepEqual(flow.resolvePassword(true, ""), { password: "", source: "" });
  // Whitespace is not a password.
  assert.equal(flow.resolvePassword("   ", "").password, "");
});

test("A1: --password wins, APPLESERVICES_PASSWORD is the fallback", () => {
  assert.deepEqual(flow.resolvePassword("s3cret!", ""), { password: "s3cret!", source: "--password" });
  assert.deepEqual(flow.resolvePassword("", "from-env"), { password: "from-env", source: flow.PASSWORD_ENV });
  assert.equal(flow.resolvePassword("from-arg", "from-env").password, "from-arg");
});

test("A1: the refusal is structured and carries no value", () => {
  const report = flow.missingPasswordReport("create-account.cjs", "the form needs a password");
  assert.deepEqual(report, {
    error: "missing_required_argument",
    argument: "--password",
    alternative: flow.PASSWORD_ENV,
    script: "create-account.cjs",
    reason: "the form needs a password",
  });
  assert.equal(Object.prototype.hasOwnProperty.call(report, "password"), false);
});

// ---- A2: nothing but a whitelist reaches run.jsonl -------------------------

test("A2: a response is logged as status, path and error code only", () => {
  const summary = flow.summarizeHTTP({
    url: "https://account.apple.com/account/validate?widgetKey=WIDGET-SECRET&id=1",
    status: 409,
    body: JSON.stringify({
      appleId: MAILBOX,
      captcha: "ab12cd",
      serviceErrors: [{ code: "-21669", message: "Incorrect verification code" }],
    }),
  });
  assert.deepEqual(summary, { status: 409, path: "/account/validate", error_code: "-21669" });
  const rendered = JSON.stringify(summary);
  for (const leak of ["WIDGET-SECRET", "abc-temp-2026", "ab12cd", "Incorrect verification"]) {
    assert.equal(rendered.includes(leak), false, `log record leaked ${leak}`);
  }
});

test("A2: a body that is empty or not JSON still yields a whitelist-only record", () => {
  assert.deepEqual(flow.summarizeHTTP({ url: "https://account.apple.com/account/validate", status: 200 }), {
    status: 200,
    path: "/account/validate",
    error_code: "",
  });
  assert.equal(flow.summarizeHTTP({ url: "x", status: 500, body: "<html>oops</html>" }).error_code, "");
  assert.equal(flow.summarizeHTTP({}).status, 0);
  // An unparsable URL degrades to the raw path instead of throwing.
  assert.equal(flow.summarizeHTTP({ url: "not a url?x=1", status: 302 }).path.length > 0, true);
});

test("A2: a received code is logged as arrival plus timing, never as the code", () => {
  const entry = flow.codeLogEntry({ received: true, elapsedMs: 4210, messageId: "msg-7" });
  assert.deepEqual(entry, { code_received: true, elapsed_ms: 4210, message_id: "msg-7" });
  const missing = flow.codeLogEntry({ received: false, elapsedMs: 180000 });
  assert.deepEqual(missing, { code_received: false, elapsed_ms: 180000 });
  assert.equal(/\d{6}/.test(JSON.stringify(entry)), false);
});

// ---- A3: a timed-out click is a decision point -----------------------------

test("A3: the click is repeated only when the readback proves nothing was submitted", () => {
  assert.equal(flow.clickTimeoutOutcome({ framePresent: true, formVisible: true, sameScreen: true }), "retry");
});

test("A3: every unconfirmed state is reported as unknown, never retried", () => {
  // The widget disappeared: the click may have submitted and navigated.
  assert.equal(flow.clickTimeoutOutcome({ framePresent: false, formVisible: true, sameScreen: true }), "unknown");
  // The form is gone but the widget is still there: a next step may be loading.
  assert.equal(flow.clickTimeoutOutcome({ framePresent: true, formVisible: false, sameScreen: true }), "unknown");
  // The screen changed: something happened, and clicking again could submit twice.
  assert.equal(flow.clickTimeoutOutcome({ framePresent: true, formVisible: true, sameScreen: false }), "unknown");
  // A readback that failed outright cannot prove anything.
  assert.equal(flow.clickTimeoutOutcome({}), "unknown");
  assert.equal(flow.clickTimeoutOutcome({ framePresent: true }), "unknown");
  assert.equal(flow.clickTimeoutOutcome({ formVisible: true }), "unknown");
  // Truthiness is not proof: only an explicit true allows another click.
  assert.equal(flow.clickTimeoutOutcome({ framePresent: "true", formVisible: 1, sameScreen: "yes" }), "unknown");
});

// ---- B4: success needs evidence -------------------------------------------

test("B4: the verification screens map to their own states", () => {
  assert.equal(
    flow.classifyScreen({ text: "We sent a verification code to 111@abc-temp-2026.shop" }),
    "email_step",
  );
  assert.equal(flow.classifyScreen({ text: "Enter the verification code sent to your email address" }), "email_step");
  assert.equal(flow.classifyScreen({ text: "We will send a text message with a code to your phone" }), "phone_step");
});

test("B4: an explicit terminal page is success", () => {
  assert.equal(flow.classifyScreen({ text: "", url: "https://account.apple.com/account/manage" }), "success");
  assert.equal(flow.classifyScreen({ text: "", url: "https://account.apple.com/account/home?x=1" }), "success");
  assert.equal(flow.classifyScreen({ text: "Welcome to Apple. You're all set." }), "success");
  assert.equal(flow.classifyScreen({ text: "Your Apple Account is ready" }), "success");
});

test("B4: a code prompt is never success even when the page also says welcome", () => {
  const text = "Welcome to Apple. Enter the verification code sent to your email address.";
  assert.equal(flow.classifyScreen({ text, url: "https://account.apple.com/account/manage" }), "email_step");
});

test("B4: an unclear ending is unknown, and unknown is a non-zero exit", () => {
  const unrecognised = [
    "",
    "Something went wrong. Try again later.",
    "This account cannot be created at this time.",
    "Apple Account Terms and Conditions",
    "Verification code" /* no verb, no recipient: not enough to act on */,
  ];
  for (const text of unrecognised) {
    assert.equal(flow.classifyScreen({ text }), "unknown", `classified ${JSON.stringify(text)} as not-unknown`);
  }
  // The sign-in landing page is not an account-management page either.
  assert.equal(flow.classifyScreen({ text: "Sign in to your Apple Account", url: "https://account.apple.com/account" }), "unknown");
  // Every state the driver can emit is in the documented set.
  assert.deepEqual([...flow.SCREEN_STATES], ["email_step", "phone_step", "success", "unknown"]);
  assert.equal(flow.EXIT_CODES.success, 0);
  for (const state of ["failed", "unknown", "waiting_input"]) {
    assert.notEqual(flow.EXIT_CODES[state], 0, `${state} must not exit 0`);
  }
});

// ---- B5: three-way mail match ---------------------------------------------

test("B5: the newest matching mail wins whatever order the API returns", () => {
  const mails = [
    codeMail({ id: 1, message_id: "old", created_at: "2026-11-13 01:00:00", raw: "<b>111111</b>" }),
    codeMail({ id: 2, message_id: "newest", created_at: "2026-11-13T01:05:00Z", raw: "<b>222222</b>" }),
    codeMail({ id: 3, message_id: "middle", created_at: "2026-11-13 01:02:00", raw: "<b>333333</b>" }),
  ];
  const picked = flow.pickEmailCode([...mails].reverse(), { address: MAILBOX });
  assert.equal(picked.code, "222222");
  assert.equal(picked.messageId, "newest");
  assert.equal(picked.created_at, "2026-11-13T01:05:00.000Z");
});

test("B5: a code addressed to another mailbox is ignored", () => {
  const mails = [codeMail({ address: "999@other-temp.shop", created_at: "2026-11-13T01:09:00Z", raw: "<b>999999</b>" })];
  assert.equal(flow.pickEmailCode(mails, { address: MAILBOX }), null);
  // Case and surrounding whitespace do not decide the match.
  const same = codeMail({ address: " 111@ABC-Temp-2026.shop ", raw: "<b>454545</b>" });
  assert.equal(flow.pickEmailCode([same], { address: MAILBOX }).code, "454545");
});

test("B5: an untrusted sender is ignored", () => {
  assert.equal(flow.pickEmailCode([codeMail({ source: "no_reply@apple.evil.test" })], { address: MAILBOX }), null);
  assert.equal(flow.pickEmailCode([codeMail({ source: "x@evil.test" })], { address: MAILBOX }), null);
  assert.equal(flow.pickEmailCode([codeMail({ source: "" })], { address: MAILBOX }), null);
  // A lookalike that merely ends with the brand is not an Apple host.
  assert.equal(flow.pickEmailCode([codeMail({ source: "x@apples.com" })], { address: MAILBOX }), null);
  // The real ones, with and without a display name, both pass.
  assert.equal(flow.pickEmailCode([codeMail({ source: "no_reply@id.apple.com" })], { address: MAILBOX }).code, "111111");
  assert.equal(flow.pickEmailCode([codeMail({ source: "Apple <no_reply@email.apple.com>" })], { address: MAILBOX }).code, "111111");
});

test("B5: --mail-from replaces the trusted sender list", () => {
  const mail = codeMail({ source: "noreply@abc-temp-2026.shop", raw: "<b>515151</b>" });
  assert.equal(flow.pickEmailCode([mail], { address: MAILBOX }), null);
  assert.equal(flow.pickEmailCode([mail], { address: MAILBOX, senderDomains: ["abc-temp-2026.shop"] }).code, "515151");
});

test("B5: a mail without verification semantics in its subject is ignored", () => {
  assert.equal(flow.pickEmailCode([codeMail({ subject: "Your receipt from Apple" })], { address: MAILBOX }), null);
  assert.equal(flow.pickEmailCode([codeMail({ subject: "" })], { address: MAILBOX }), null);
  // The subject can also come from the raw MIME header.
  const fromRaw = codeMail({ subject: "", raw: "Subject: Your Apple verification code\r\n\r\n<b>424242</b>" });
  assert.equal(flow.pickEmailCode([fromRaw], { address: MAILBOX }).code, "424242");
});

test("B5: a mail whose created_at cannot be parsed is skipped, not promoted", () => {
  const mails = [
    codeMail({ message_id: "broken", created_at: "not-a-time", raw: "<b>666666</b>" }),
    codeMail({ message_id: "good", created_at: "2026-11-13 01:00:00", raw: "<b>121212</b>" }),
  ];
  const picked = flow.pickEmailCode(mails, { address: MAILBOX });
  assert.equal(picked.code, "121212");
  assert.equal(picked.messageId, "good");
  assert.equal(Number.isNaN(flow.parseMailTime("yesterday")), true);
  assert.equal(Number.isNaN(flow.parseMailTime("")), true);
  assert.equal(flow.parseMailTime("2026-11-13 01:00:00"), Date.UTC(2026, 10, 13, 1, 0, 0));
  assert.equal(flow.parseMailTime("2026-11-13T01:00:00Z"), Date.UTC(2026, 10, 13, 1, 0, 0));
});

test("B5: mails older than the run start are ignored", () => {
  const since = Date.parse("2026-11-13T01:04:00Z");
  assert.equal(flow.pickEmailCode([codeMail({ created_at: "2026-11-13 01:00:00" })], { address: MAILBOX, since }), null);
  assert.equal(
    flow.pickEmailCode([codeMail({ created_at: "2026-11-13 01:06:00", raw: "<b>787878</b>" })], { address: MAILBOX, since }).code,
    "787878",
  );
});

test("B5: an empty or malformed mailbox answer is not a code", () => {
  assert.equal(flow.pickEmailCode([], { address: MAILBOX }), null);
  assert.equal(flow.pickEmailCode(null, { address: MAILBOX }), null);
  assert.equal(flow.pickEmailCode([null, "x", 42], { address: MAILBOX }), null);
  // No mailbox known means no match, not "first code wins".
  assert.equal(flow.pickEmailCode([codeMail()], { address: "" }), null);
});
