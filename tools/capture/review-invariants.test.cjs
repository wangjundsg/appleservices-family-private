// Source-level invariants for the account-creation drivers.
//
// Run with:  node --test tools/capture/review-invariants.test.cjs
//
// account-flow.test.cjs proves the decisions are right; these assertions prove
// the *scripts* actually route through them. Without this, a later refactor can
// reintroduce a hard-coded password or an unconditional force-click while every
// unit test stays green. Each case names the review item it protects.

"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");

const dir = __dirname;
const read = (name) => fs.readFileSync(path.join(dir, name), "utf8");

// ---- A1: no repository-visible default password ---------------------------

test("A1: no capture script ships a default password", () => {
  const scripts = fs
    .readdirSync(dir)
    .filter((name) => name.endsWith(".cjs") && !name.endsWith(".test.cjs"));
  for (const name of scripts) {
    const source = read(name);
    assert.equal(
      /arg\(\s*"password"\s*,\s*"[^"]+"/.test(source),
      false,
      `${name} still passes a default value for --password`,
    );
  }
});

test("A1: both form-filling scripts resolve the password and refuse without it", () => {
  for (const name of ["create-account.cjs", "capture.cjs"]) {
    const source = read(name);
    assert.match(source, /flow\.resolvePassword\(/, `${name} does not resolve --password through the shared helper`);
    assert.match(source, /flow\.missingPasswordReport\(/, `${name} has no structured refusal`);
    // The refusal has to happen before the browser exists.
    const refusal = source.indexOf("flow.missingPasswordReport(");
    const launch = source.indexOf("chromium.launch(");
    assert.ok(refusal > 0 && launch > refusal, `${name} launches the browser before checking the password`);
  }
});

// ---- A2: redaction is wired into the scripts, not just into the helpers ---

test("A2: verification codes never reach the run log", () => {
  const source = read("create-account.cjs");
  assert.equal(/note\("step",\s*\{\s*at:\s*"(?:email|sms)-code"/.test(source), false, "a code value is still logged");
  assert.match(source, /flow\.codeLogEntry\(/, "the code log entry does not go through the redacting helper");
});

test("A2: the code-filled screenshot masks the authentication inputs first", () => {
  const source = read("create-account.cjs");
  assert.match(source, /code-filled`,\s*\{\s*maskAuth:\s*true\s*\}/, "the code-filled screenshot is not masked");
  assert.match(source, /addStyleTag\(\{\s*content:\s*AUTH_MASK_CSS\s*\}\)/, "no mask style is injected into the widget frame");
});

test("A2: response bodies are summarised, not copied", () => {
  const source = read("create-account.cjs");
  assert.match(source, /flow\.summarizeHTTP\(/, "the response listener does not use the whitelist");
  assert.equal(/note\("http",\s*\{\s*url:\s*u,\s*status:\s*res\.status\(\),\s*body\s*\}/.test(source), false, "raw bodies are still logged");
});

test("A2: gsa-login prints the length of the SMS code, never the code", () => {
  const source = read("gsa-login.cjs");
  assert.equal(/sms code \$\{code\}/.test(source), false, "the SMS code is still printed");
  assert.match(source, /sms code received \(len=\$\{code\.length\}\)/);
});

// ---- A3: force-click only behind the confirmation gate --------------------

test("A3: the force-click sits behind the timeout readback", () => {
  const source = read("create-account.cjs");
  const gate = source.indexOf("flow.clickTimeoutOutcome(");
  const forced = source.indexOf("click({ force: true })");
  assert.ok(gate > 0, "clickContinue does not classify the timeout");
  assert.ok(forced > gate, "the force-click is not guarded by the timeout classification");
  assert.equal(source.split("click({ force: true })").length - 1, 1, "more than one unconditional force-click");
  // The readback helper must inspect the widget, otherwise "same screen" is a guess.
  assert.match(source, /async function readPhase\(page\)/);
});

// ---- B4: the terminal state is explicit and gates the exit code -----------

test("B4: the run exits on its state, and the state is the last log record", () => {
  const source = read("create-account.cjs");
  const final = source.indexOf('note("final", { state: finalState');
  const exit = source.indexOf("process.exit(flow.EXIT_CODES[finalState]");
  assert.ok(final > 0 && exit > final, "the terminal state is not logged before the exit");
  assert.match(source, /flow\.classifyScreen\(/, "screens are not classified");
  // Success is only reachable from the classified terminal screen.
  assert.match(source, /if \(verdict === "success"\)/);
  assert.match(source, /finalState = "waiting_input"/);
  // The failure handler names its outcome too.
  assert.match(source, /const state = e && e\.state === "unknown" \? "unknown" : "failed"/);
});

// ---- B5: the mailbox check is the shared three-way match ------------------

test("B5: the mailbox poll goes through the three-way match", () => {
  const source = read("create-account.cjs");
  assert.match(source, /flow\.pickEmailCode\(body\.results/, "the mail picker is not used");
  assert.match(source, /address:\s*email/, "the picker is not bound to this run's mailbox");
  assert.match(source, /senderDomains/, "the picker does not check the sender");
  // The old "first <b>code</b> in the first ten mails" shortcut is gone.
  assert.equal(/for \(const mail of body\.results/.test(source), false, "the old unordered loop is still there");
});
