// Unit tests for the four flow mechanics rebuilt after the 2026-11-14 G5 run.
//
// Run with:  node --test tools/capture/flow-mechanics.test.cjs
//
// Every case drives flow-mechanics.cjs through fake locators, a fake mouse and a
// fake clock, so the behaviour that broke on the live form (a re-typed form, a
// click that landed on "Mã Mới", an email step with no grace period, a close
// with no reason) is pinned without a browser. The last section asserts the
// driver actually routes through these helpers — a helper that exists and is not
// wired is how the defects came back the first time.

"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");

const mech = require("./flow-mechanics.cjs");

// ---- fakes ----------------------------------------------------------------

// fakeControl is a Playwright locator as far as these helpers are concerned:
// value read, clear, type, select, text, enabled, scroll, box, click.
function fakeControl({ value = "", text = "", type = "button", enabled = true, box = { x: 100, y: 200, width: 120, height: 40 } } = {}) {
  const state = { value, text, type, enabled, box, clicks: 0, scrolls: 0, fills: [], selected: null };
  const control = {
    state,
    async inputValue() {
      return state.value;
    },
    async fill(v) {
      state.fills.push(v);
      state.value = String(v);
    },
    async pressSequentially(ch) {
      state.value += ch;
    },
    async innerText() {
      return state.text;
    },
    async getAttribute(name) {
      return name === "type" ? state.type : null;
    },
    async isEnabled() {
      return state.enabled;
    },
    async scrollIntoViewIfNeeded() {
      state.scrolls += 1;
    },
    async boundingBox() {
      return state.box;
    },
    async selectOption(v) {
      state.selected = v;
      state.value = v;
    },
    async click() {
      state.clicks += 1;
      if (state.onClick) await state.onClick();
    },
  };
  return control;
}

function fakeFrame(controls) {
  return {
    locator(selector) {
      const list = String(selector).includes('[type="submit"]')
        ? controls.filter((c) => c.state.type === "submit")
        : controls;
      return { count: async () => list.length, nth: (i) => list[i] };
    },
  };
}

function fakeMouse() {
  return {
    moves: [],
    async move(x, y, options) {
      this.moves.push({ x, y, options });
    },
  };
}

// A clock that only moves when the code sleeps, so the 10s "wait for enabled"
// and the 90s/120s email windows are asserted in real units without waiting.
function fakeClock() {
  let t = 0;
  const sleeps = [];
  return {
    now: () => t,
    sleep: async (ms) => {
      sleeps.push(ms);
      t += ms;
    },
    sleeps,
  };
}

// ---- A: only the fields that need filling ---------------------------------

test("A: a field that already holds the value is not typed into again", async () => {
  const control = fakeControl({ value: "Lucy" });
  const events = [];
  let typed = 0;
  const out = await mech.fillField(control, "Lucy", "first_name", {
    type: async () => {
      typed += 1;
    },
    log: (kind, data) => events.push({ kind, ...data }),
  });
  assert.equal(out.kept, true);
  assert.equal(typed, 0, "a correct field was typed into");
  assert.equal(control.state.fills.length, 0, "a correct field was cleared");
  assert.equal(events.at(-1).at, "field-kept");
  assert.equal(events.at(-1).field, "first_name");
});

test("A: a field with a stale value is cleared before it is retyped", async () => {
  const control = fakeControl({ value: "james167@outlook.com" });
  const typed = [];
  const out = await mech.fillField(control, "james187@outlook.com", "apple_id", {
    type: async (locator, value) => {
      typed.push(value);
      locator.state.value = value;
    },
    log: () => {},
  });
  assert.equal(out.kept, false);
  assert.deepEqual(control.state.fills, [""], "the old value was appended to instead of cleared");
  assert.deepEqual(typed, ["james187@outlook.com"]);
  assert.equal(control.state.value, "james187@outlook.com");
});

test("A: a birthday dropdown that already shows the wanted month is left alone", async () => {
  const month = fakeControl({ value: "04", type: "select-one" });
  const day = fakeControl({ value: "01", type: "select-one" });
  const events = [];
  const choose = async (locator, value) => {
    locator.state.selected = value;
    locator.state.value = value;
  };
  const log = (kind, data) => events.push(data);
  await mech.selectField(month, "04", "birth_month", { choose, log });
  await mech.selectField(day, "16", "birth_day", { choose, log });
  assert.equal(month.state.selected, null, "the month dropdown was re-selected");
  assert.equal(day.state.selected, "16");
  assert.deepEqual(events.map((e) => [e.at, e.field]), [
    ["field-kept", "birth_month"],
  ]);
});

// ---- B: the submit button -------------------------------------------------

test("B: type=submit wins, and the press is scrolled, hovered and settled", async () => {
  const submit = fakeControl({ text: "Continue", type: "submit" });
  const other = fakeControl({ text: "Go back", type: "button" });
  const clock = fakeClock();
  const mouse = fakeMouse();
  const events = [];
  const out = await mech.clickSubmit(fakeFrame([other, submit]), {
    mouse,
    sleep: clock.sleep,
    now: clock.now,
    rand: () => 0,
    log: (kind, data) => events.push({ kind, ...data }),
    tag: "test",
  });
  assert.equal(out.clicked, true);
  assert.equal(out.via, "type=submit");
  assert.equal(submit.state.clicks, 1, "the submit button was not pressed exactly once");
  assert.equal(other.state.clicks, 0, "a non-submit control was pressed");
  assert.equal(submit.state.scrolls, 1, "the button was not scrolled into view");
  assert.equal(mouse.moves.length, 1, "the pointer was not moved to the button");
  assert.deepEqual({ x: mouse.moves[0].x, y: mouse.moves[0].y }, { x: 160, y: 220 }, "the hover missed the centre");
  assert.ok(clock.sleeps.includes(300), "the pointer did not settle before the press");
  assert.equal(events.at(-1).at, "submit-clicked");
});

test("B: a type=submit control whose text is the refresh button is skipped", async () => {
  // The vi-VN form carries "Mã Mới" (send a new code) and "Tiếp tục" (continue).
  // Pressing the first one after typing a code throws the code away.
  const refresh = fakeControl({ text: "Mã Mới", type: "submit" });
  const submit = fakeControl({ text: "Tiếp tục", type: "submit" });
  const clock = fakeClock();
  const out = await mech.clickSubmit(fakeFrame([refresh, submit]), {
    mouse: fakeMouse(),
    sleep: clock.sleep,
    now: clock.now,
    rand: () => 0,
    log: () => {},
  });
  assert.equal(out.clicked, true);
  assert.equal(refresh.state.clicks, 0, "the refresh control was pressed");
  assert.equal(submit.state.clicks, 1);
  assert.equal(mech.isNotSubmit("换一张"), true);
  assert.equal(mech.isNotSubmit("Send a new code"), true);
  assert.equal(mech.isNotSubmit("Continue"), false);
});

test("B: a button that never becomes enabled is recorded, not re-clicked", async () => {
  const disabled = fakeControl({ text: "Continue", type: "submit", enabled: false });
  const clock = fakeClock();
  const events = [];
  const out = await mech.clickSubmit(fakeFrame([disabled]), {
    mouse: fakeMouse(),
    sleep: clock.sleep,
    now: clock.now,
    rand: () => 0,
    log: (kind, data) => events.push({ kind, ...data }),
  });
  assert.equal(out.clicked, false);
  assert.equal(out.reason, "submit-disabled");
  assert.equal(disabled.state.clicks, 0, "a disabled button was clicked anyway");
  const record = events.find((e) => e.at === "submit-disabled");
  assert.ok(record, "no submit-disabled record");
  assert.equal(record.kind, "error");
  assert.equal(record.waited_ms, 10000, "the enabled wait was not the full 10s");
});

test("B: no submit control at all is recorded as submit-not-found", async () => {
  const clock = fakeClock();
  const events = [];
  const out = await mech.clickSubmit(fakeFrame([fakeControl({ text: "Go back" })]), {
    mouse: fakeMouse(),
    sleep: clock.sleep,
    now: clock.now,
    log: (kind, data) => events.push({ kind, ...data }),
    tag: "step-1",
  });
  assert.equal(out.clicked, false);
  assert.equal(out.reason, "submit-not-found");
  assert.deepEqual(events.map((e) => e.at), ["submit-not-found"]);
  assert.equal(events[0].tag, "step-1");
});

test("B: a click that times out is reported once and never retried", async () => {
  const submit = fakeControl({ text: "Continue", type: "submit" });
  submit.state.onClick = async () => {
    throw new Error("Timeout 15000ms exceeded.");
  };
  const clock = fakeClock();
  const out = await mech.clickSubmit(fakeFrame([submit]), {
    mouse: fakeMouse(),
    sleep: clock.sleep,
    now: clock.now,
    rand: () => 0,
    log: () => {},
  });
  assert.equal(out.clicked, false);
  assert.equal(out.reason, "click-timeout");
  assert.equal(submit.state.clicks, 1, "the same button was pressed twice");
  assert.ok(out.locator, "the locator is not handed back for the readback decision");
});

// ---- C: the email code grace window ---------------------------------------

test("C: 90s, one resend, 120s more — and the code that lands in the second window is used", async () => {
  const clock = fakeClock();
  const windows = [];
  const events = [];
  let resends = 0;
  const out = await mech.emailCodeWindow({
    waitCode: async (ms) => {
      windows.push(ms);
      clock.sleep(ms);
      return windows.length === 1 ? "" : { code: "654321", messageId: "graph:abc" };
    },
    resend: async () => {
      resends += 1;
      return { asked: true, ok: true, how: "clicked" };
    },
    now: clock.now,
    log: (kind, data) => events.push({ kind, ...data }),
  });
  assert.deepEqual(windows, [90000, 120000], "the grace windows are not 90s then 120s");
  assert.equal(resends, 1, "the page was asked to resend more than once");
  assert.equal(out.timedOut, false);
  assert.equal(out.code, "654321");
  assert.equal(out.rescanned, true);
  assert.equal(out.waitedMs, 210000);
  assert.equal(events.filter((e) => e.at === "email-resend").length, 1);
});

test("C: a code that never arrives still costs the full 210s and reports it", async () => {
  const clock = fakeClock();
  const out = await mech.emailCodeWindow({
    waitCode: async (ms) => {
      clock.sleep(ms);
      return "";
    },
    resend: async () => ({ asked: false, ok: false, how: "not-found" }),
    now: clock.now,
    log: () => {},
  });
  assert.equal(out.timedOut, true);
  assert.equal(out.code, "");
  assert.equal(out.waitedMs, 210000);
  assert.equal(out.rescanned, false);
  assert.equal(out.picked, null);
});

test("C: a resend that throws does not end the run", async () => {
  const clock = fakeClock();
  const events = [];
  const out = await mech.emailCodeWindow({
    waitCode: async (ms) => {
      clock.sleep(ms);
      return "";
    },
    resend: async () => {
      throw new Error("frame was detached");
    },
    now: clock.now,
    log: (kind, data) => events.push(data),
  });
  assert.equal(out.timedOut, true);
  assert.equal(out.resent.how, "error");
  assert.match(out.resent.error, /detached/);
  const record = events.find((e) => e.at === "email-resend");
  assert.equal(record.asked, true);
  assert.equal(record.ok, false);
});

// ---- D: every exit names a reason ----------------------------------------

test("D: the six requested reasons plus the two terminal screens are the vocabulary", () => {
  for (const reason of ["success", "captcha-exhausted", "email-code-timeout", "submit-failed", "hard-reject", "script-error"]) {
    assert.ok(mech.CLOSE_REASONS.includes(reason), `${reason} is missing from the close vocabulary`);
    assert.equal(mech.normalizeCloseReason(reason), reason);
  }
  assert.deepEqual(
    ["sms-code-timeout", "unknown-screen"].filter((reason) => !mech.CLOSE_REASONS.includes(reason)),
    [],
  );
  assert.equal(mech.normalizeCloseReason("weird"), "script-error", "an unknown reason is not degraded");
  assert.equal(mech.closeRecord("nonsense").reason, "script-error");
  const record = mech.closeRecord("email-code-timeout", { waited: "210s", rescanned: "yes" });
  assert.deepEqual(record, { reason: "email-code-timeout", waited: "210s", rescanned: "yes" });
});

test("D: the reason is derived from the evidence the run already holds", () => {
  assert.equal(mech.closeReasonFor({ state: "success" }), "success");
  assert.equal(mech.closeReasonFor({ state: "waiting_input", evidence: "email_step-code-timeout" }), "email-code-timeout");
  assert.equal(mech.closeReasonFor({ state: "failed", evidence: "hard-reject:2" }), "hard-reject");
  assert.equal(mech.closeReasonFor({ state: "unknown", evidence: "submit-disabled:create-submit-1" }), "submit-failed");
  assert.equal(mech.closeReasonFor({ state: "unknown", evidence: "captcha_image_stuck" }), "captcha-exhausted");
  assert.equal(mech.closeReasonFor({ state: "waiting_input", evidence: "phone_step-code-timeout" }), "sms-code-timeout");
  assert.equal(mech.closeReasonFor({ state: "unknown", evidence: "unrecognized-screen:2" }), "unknown-screen");
  assert.equal(mech.closeReasonFor({ state: "failed", evidence: "field_mismatch:apple_id" }), "script-error");
});

// ---- wiring: the driver really calls all four ----------------------------

const source = fs.readFileSync(path.join(__dirname, "create-account.cjs"), "utf8");

test("wiring A: every main-form field goes through fillIfNeeded / selectIfNeeded", () => {
  for (const name of ["firstName", "lastName", "appleId", "password", "confirmPassword", "phoneNumber"]) {
    assert.match(
      source,
      new RegExp(`fillIfNeeded\\(fr\\.locator\\('input\\[name="${name}"\\]'\\)`),
      `${name} is not filled through the read-first helper`,
    );
  }
  for (const testid of ["select-month", "select-day", "select-year"]) {
    assert.match(
      source,
      new RegExp(`selectIfNeeded\\(fr\\.locator\\('select\\[data-testid="${testid}"\\]'\\)`),
      `${testid} is not selected through the read-first helper`,
    );
  }
  assert.equal(/await retype\(/.test(source), false, "the unconditional re-type helper is back");
});

test("wiring B: the submit is resolved by the shared finder, not by a loose selector", () => {
  assert.match(source, /mech\.clickSubmit\(/, "clickContinue does not use the shared submit finder");
  assert.equal(
    /button\[type="submit"\], button:has-text\("Continue"\)/.test(source),
    false,
    "the loose selector that could land on the refresh button is back",
  );
  // The force-click stays behind the readback decision (review A3).
  const gate = source.indexOf("flow.clickTimeoutOutcome(");
  const forced = source.indexOf("click({ force: true })");
  assert.ok(gate > 0 && forced > gate, "the force-click is no longer behind the readback");
});

test("wiring C: the email step runs the grace window and presses one resend", () => {
  assert.match(source, /mech\.emailCodeWindow\(/, "the email step has no grace window");
  assert.match(source, /resendEmailCode\(page, current\)/, "the resend is not wired into the window");
  assert.match(source, /mech\.findAction\(frame, mech\.RESEND_LABELS\)/, "the resend is not found by text");
  // The pickers take the window they are given instead of owning a fixed one.
  assert.match(source, /graphEmailCode\(stepStartedAt, windowMs\)/);
  assert.match(source, /manualEmailCode\(windowMs\)/);
});

test("wiring D: both exits write the close record before the terminal record", () => {
  assert.match(source, /if \(closeLogged\) return;/, "a run can write more than one close record");
  const successClose = source.indexOf("logClose(closeReason");
  const successFinal = source.indexOf('note("final", { state: finalState');
  assert.ok(successClose > 0, "the success/waiting exit writes no close reason");
  assert.ok(successClose < successFinal, "the close record is written after the terminal record");
  const errorClose = source.indexOf("logClose(\n    (e && e.closeReason)");
  const errorFinal = source.indexOf('note("final", { state, evidence');
  assert.ok(errorClose > 0, "the thrown exit writes no close reason");
  assert.ok(errorClose < errorFinal, "the close record is written after the terminal record (thrown exit)");
  // A named outcome uses its own category; everything else is derived.
  assert.match(source, /err\.closeReason = "hard-reject"/);
  assert.match(source, /err\.closeReason = "submit-failed"/);
  assert.match(source, /err\.closeReason = "captcha-exhausted"/);
  assert.match(source, /mech\.closeRecord\(reason, extra\)/, "the close record is not built by the shared helper");
});
