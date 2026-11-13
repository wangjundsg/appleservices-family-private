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
  const exit = source.indexOf("process.exit(exitCode)");
  assert.ok(final > 0, "the terminal state is not logged");
  assert.ok(exit > final, "the terminal state is not logged before the exit");
  // The exit code is derived from the classified state, never a literal.
  assert.match(source, /const exitCode = flow\.EXIT_CODES\[finalState\]/);
  // A refund happens before the terminal record, so run.jsonl still ends on the
  // state of the run (review B4 + run-20261113 §4.7).
  const refund = source.indexOf("await autoRefund(finalState)");
  assert.ok(refund > 0 && refund < final, "the refund is not requested before the terminal record");
  assert.match(source, /flow\.classifyScreen\(/, "screens are not classified");
  // Success is only reachable from the classified terminal screen.
  assert.match(source, /if \(verdict === "success"\)/);
  assert.match(source, /finalState = "waiting_input"/);
  // The failure handler names its outcome too, including a pre-run refusal.
  assert.match(source, /e && e\.state === "usage" \? "usage" : "failed"/);
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

// ---- run-20261113 §4.3: --captcha-cmd is parsed, not stuffed into argv[2] --

test("§4.3: the captcha command is parsed and run through the shared runner", () => {
  const source = read("create-account.cjs");
  assert.match(source, /lib\.runCaptchaCommand\(/, "the solver is not run through capture-lib");
  // The old call handed the whole command string to python as one filename.
  assert.equal(/execFileSync\(\s*"python"/.test(source), false, "the old one-argv execFileSync is still there");
  assert.equal(/execFileSync/.test(source), false, "the driver still spawns the solver itself");
});

// ---- run-20261113 §4.4: the captcha image is the one next to the input ----

test("§4.4: the captcha image is chosen by DOM affiliation, not by size", () => {
  const source = read("create-account.cjs");
  assert.match(source, /lib\.pickCaptchaImage\(/, "the shared picker is not used");
  // The old heuristic picked the first img in a size window with no DOM check.
  assert.equal(/naturalWidth\s*<=\s*200/.test(source), false, "the size-only heuristic is still in the driver");
  assert.match(source, /input\[name="captcha"\]/, "the picker is not anchored on the captcha input");
  assert.match(source, /src_kind/, "the capture method is not recorded");
  // A captcha that did not load stops the run before any OCR call.
  const unavailable = source.indexOf('at: "captcha-unavailable"');
  const ocr = source.indexOf("lib.runCaptchaCommand(");
  assert.ok(unavailable > 0 && unavailable < ocr, "an unloaded captcha does not stop the run before OCR");
});

// ---- run-20261113 §4.8: the same capture twice trips a breaker ------------

test("§4.8: repeated captures are hashed and the breaker fires before OCR", () => {
  const source = read("create-account.cjs");
  assert.match(source, /new lib\.CaptchaTracker\(/, "no breaker is armed");
  assert.match(source, /lib\.sha256Hex\(/, "the capture is not hashed");
  const stuck = source.indexOf('at: "captcha_image_stuck"');
  const ocr = source.indexOf("lib.runCaptchaCommand(");
  assert.ok(stuck > 0, "the breaker never reports captcha_image_stuck");
  assert.ok(stuck < ocr, "the breaker fires after the solver was already paid");
  assert.match(source, /duplicates:/, "the duplicate count is not logged");
});

// ---- run-20261113 §4.5 / §4.7: preflight and refund are wired, not just built --

test("§4.5: --preflight runs before the browser exists", () => {
  const source = read("create-account.cjs");
  const gate = source.indexOf('at: "preflight"');
  const launch = source.indexOf("chromium.launch(");
  assert.ok(gate > 0, "the preflight gate is not wired");
  assert.ok(launch > gate, "the preflight runs after the browser was launched");
  assert.match(source, /preflight\.probe\(/, "the probe is not the shared helper");
});

test("§4.7: --auto-refund cancels the number on every non-zero exit", () => {
  const source = read("create-account.cjs");
  assert.equal(source.split("await autoRefund(").length - 1, 2, "a non-zero exit path skips the refund");
  assert.match(source, /refund\.refundActivation\(/, "the refund does not go through the shared helper");
  assert.match(source, /note\("refund"/, "the refund result is not recorded in run.jsonl");
  // A usage refusal is thrown through the same handler, so it refunds too.
  const usage = source.indexOf('err.state = "usage"');
  const launch = source.indexOf("chromium.launch(");
  assert.ok(usage > 0 && usage < launch, "the usage refusal does not run through the failure handler");
  assert.match(source, /process\.exit\(flow\.EXIT_CODES\[state\]\)/, "the failure handler exits on its state");
});
