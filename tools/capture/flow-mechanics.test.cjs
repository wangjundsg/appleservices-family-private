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
  const state = { value, text, type, enabled, box, clicks: 0, scrolls: 0, fills: [], keys: [], selected: null };
  const control = {
    state,
    async inputValue() {
      return state.value;
    },
    async fill(v) {
      state.fills.push(v);
      state.value = String(v);
    },
    async press(key) {
      state.keys.push(key);
      if (key === "Backspace") state.value = "";
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
  assert.deepEqual(control.state.fills, [], "clearing should use keyboard input");
  assert.deepEqual(control.state.keys.slice(0, 2), ["ControlOrMeta+A", "Backspace"]);
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
    ["field-verified", "birth_day"],
  ]);
});

test("A: truncated input is read back and corrected exactly once", async () => {
  const control = fakeControl();
  let writes = 0;
  const result = await mech.fillField(control, "expected", "first_name", {
    type: async (loc, value) => { loc.state.value = ++writes === 1 ? "truncated" : value; },
  });
  assert.equal(writes, 2);
  assert.equal(result.corrected, true);
  assert.equal(control.state.value, "expected");
});

test("A: persistent mismatch stops after two writes and never logs field contents", async () => {
  const control = fakeControl();
  let writes = 0;
  const logs = [];
  await assert.rejects(mech.fillField(control, "private-fixture", "password", {
    type: async loc => { writes++; loc.state.value = "wrong-fixture"; },
    log: (...event) => logs.push(event),
  }), error => error.message === "field_readback_failed" && error.evidence === "field_mismatch:password");
  assert.equal(writes, 2);
  assert.doesNotMatch(JSON.stringify(logs), /private-fixture|wrong-fixture/);
});

test("A: formatted phone readback matches digits without a second write", async () => {
  const control = fakeControl();
  let writes = 0;
  await mech.fillField(control, "967365000", "phone_number", {
    normalize: value => value.replace(/\D/g, ""),
    type: async loc => { writes++; loc.state.value = "967 365 000"; },
  });
  assert.equal(writes, 1);
});

test("A: unreadable initial field stops before typing", async () => {
  let writes = 0;
  await assert.rejects(mech.fillField({ inputValue: async () => { throw Error("secret fixture"); } }, "wanted", "email", {
    type: async () => { writes++; },
  }), /field_readback_failed/);
  assert.equal(writes, 0);
});

test("A: a dropdown reset is corrected once; a persistent reset fails", async () => {
  const control = fakeControl({ value: "01" });
  let writes = 0;
  const result = await mech.selectField(control, "07", "birth_month", {
    choose: async loc => { loc.state.value = ++writes === 1 ? "01" : "07"; },
  });
  assert.equal(result.corrected, true);
  assert.equal(writes, 2);
  await assert.rejects(mech.selectField(control, "09", "birth_month", {
    choose: async loc => { loc.state.value = "01"; },
  }), /field_readback_failed/);
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

test("C: both wait windows accept a late code without invoking a supplied resend callback", async () => {
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
  assert.deepEqual(windows, [300000, 300000], "each reader call must stay within its five-minute limit");
  assert.equal(resends, 0, "automatic resend must never be called");
  assert.equal(out.timedOut, false);
  assert.equal(out.code, "654321");
  assert.equal(out.rescanned, false);
  assert.equal(out.waitedMs, 600000);
  assert.equal(events.filter((e) => e.at === "email-resend").length, 0);
});

test("C: a code that never arrives waits ten minutes and reports it", async () => {
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
  assert.equal(out.waitedMs, 600000);
  assert.equal(out.rescanned, false);
  assert.equal(out.picked, null);
});

test("C: the lease deadline shortens the wait and an expired lease performs no read", async () => {
  const clock = fakeClock(), windows = [];
  const waitCode = async ms => { windows.push(ms); await clock.sleep(ms); return null; };
  const result = await mech.emailCodeWindow({waitCode, now: clock.now, deadline: 420000});
  assert.deepEqual(windows, [300000, 120000]);
  assert.equal(result.waitedMs, 420000);
  await mech.emailCodeWindow({waitCode, now: clock.now, deadline: 420000});
  assert.equal(windows.length, 2);
});

test("C: a code arriving after seven minutes is accepted without resending", async () => {
  const clock = fakeClock();
  let reads = 0;
  const result = await mech.emailCodeWindow({now: clock.now, waitCode: async ms => {
    await clock.sleep(++reads === 1 ? ms : 120000);
    return reads === 2 ? {code:"001234",messageId:"late-fixture"} : null;
  }, resend: () => { throw Error("must not resend"); }});
  assert.equal(result.waitedMs, 420000);
  assert.equal(result.code, "001234");
  assert.equal(result.resent.asked, false);
});

test("C: no-code timeout never invokes a throwing resend callback", async () => {
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
  assert.equal(out.resent.how, "disabled");
  assert.equal(out.resent.asked, false);
  assert.equal(events.some(e => e.at === "email-resend"), false);
});

// ---- E: one phone resend, then one replacement request -------------------

test("E: phone codes get two five-minute windows with exactly one resend", async () => {
  const clock = fakeClock();
  const windows = [];
  let resends = 0;
  const result = await mech.phoneCodeWindow({
    now: clock.now,
    waitCode: async ms => {
      windows.push(ms);
      await clock.sleep(ms);
      return windows.length === 2 ? { code: "654321", messageId: "sms:fixture" } : null;
    },
    resend: async () => {
      resends += 1;
      return { asked: true, ok: true, how: "resend-text" };
    },
  });
  assert.deepEqual(windows, [300000, 300000]);
  assert.equal(resends, 1);
  assert.equal(result.code, "654321");
  assert.equal(result.timedOut, false);
  assert.equal(result.resent.ok, true);
  assert.equal(result.waitedMs, 600000);
});

test("E: a second phone timeout requests replacement without a second resend", async () => {
  const clock = fakeClock();
  let resends = 0;
  const result = await mech.phoneCodeWindow({
    now: clock.now,
    waitCode: async ms => { await clock.sleep(ms); return null; },
    resend: async () => { resends += 1; return { asked: true, ok: true, how: "resend-text" }; },
  });
  assert.equal(resends, 1);
  assert.equal(result.code, "");
  assert.equal(result.timedOut, true);
  assert.equal(result.replace, true);
  assert.equal(result.waitedMs, 600000);
});

// ---- D: every exit names a reason ----------------------------------------

test("E: lease expiry never resends or starts replacement", async () => {
  const clock = fakeClock();
  const result = await mech.phoneCodeWindow({now: clock.now, deadline: clock.now()+10000,
    waitCode: async ms => {await clock.sleep(ms); return null;},
    resend: async () => {throw Error("must not resend after expiry");}});
  assert.equal(result.replace, false);
  assert.equal(result.resent.asked, false);
});

test("E: a first-window code avoids all resends; unknown resend result stops", async () => {
  let reads=0;
  const first=await mech.phoneCodeWindow({waitCode: async()=>"123456", resend:async()=>{throw Error("unexpected");}});
  assert.equal(first.code,"123456");
  const unknown=await mech.phoneCodeWindow({waitCode:async()=>{reads++;return null;},resend:async()=>({ok:false})});
  assert.equal(reads,1);
  assert.equal(unknown.replace,false);
});

test("E: stale, cancelled, retry and malformed SMS never become new codes", () => {
  const used=new Set(["123456"]);
  for(const status of ["STATUS_OK:123456","STATUS_WAIT_RETRY:654321","STATUS_CANCEL","STATUS_OK:123","STATUS_OK:123456:extra"])
    assert.equal(mech.freshSMSCode(status,used),"");
  assert.equal(mech.freshSMSCode("STATUS_OK:654321",used),"654321");
});

test("E: uncertain clicks and completed clicks both forbid replay after restart", async () => {
  const fs=require('node:fs'),os=require('node:os'),path=require('node:path');
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'sms-once-test-'));
  try {
    for(const fails of [true,false]) {
      const marker=path.join(dir,String(fails)+'.json');
      let calls=0;
      const click=async()=>{calls++;if(fails)throw Error('response lost');};
      if(fails)await assert.rejects(mech.onceAction(marker,click),/response lost/);
      else await mech.onceAction(marker,click);
      await assert.rejects(mech.onceAction(marker,click),/EEXIST/);
      assert.equal(calls,1);
      fs.unlinkSync(marker);
    }
  } finally {fs.rmdirSync(dir);}
});

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

const source = fs.readFileSync(path.join(__dirname, "create-account.cjs"), "utf8").replace(/\r\n/g, "\n");

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

test("wiring C: the email step waits without wiring any resend action", () => {
  assert.match(source, /mech\.emailCodeWindow\(/, "the email step has no grace window");
  assert.doesNotMatch(source, /resendEmailCode|mech\.RESEND_LABELS/, "an automatic resend action remains wired");
  // The pickers take the window they are given instead of owning a fixed one.
  assert.match(source, /graphEmailCode\(emailRequestedAt, windowMs\)/);
  const sentAt=source.indexOf('emailRequestedAt = Date.now();',source.indexOf('submissionMayHaveLanded = true;'));
  const submit=source.indexOf('await clickContinue(page, fr, `create-submit-');
  assert.ok(sentAt>0 && sentAt<submit,'email query starts after the send and can miss a fast OTP');
  assert.match(source,/poolEmailCode\(windowMs\)/);
  assert.match(source, /manualEmailCode\(windowMs\)/);
});

test("wiring E: the phone step uses the bounded resend and replacement handoff", () => {
  assert.match(source, /mech\.phoneCodeWindow\(/);
  assert.match(source, /replacement-number-file/);
  assert.match(source, /replacement_number_required/);
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
