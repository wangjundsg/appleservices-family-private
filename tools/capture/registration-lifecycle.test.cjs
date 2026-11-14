"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { expiry, refundable, retainInput } = require("./registration-lifecycle.cjs");

test("only a proven failure or unused input may refund the activation", () => {
  for (const state of ["unknown", "waiting_input", "likely_success", "success", "unexpected"]) assert.equal(refundable(state), false);
  assert.equal(refundable("failed"), true);
  assert.equal(refundable("usage"), true);
});
test("late code continues the same pending operation without a resend", async () => {
  let clock = 0, notices = 0, calls = 0;
  const value = await retainInput({ deadline: 60_000, now: () => clock, notify: () => notices++, poll: async (window) => {
    calls++; clock += window; return calls === 2 ? { code: "123456" } : null;
  } });
  assert.equal(value.code, "123456");
  assert.equal(notices, 1);
  assert.equal(calls, 2);
});
test("waiting ends at the original lease expiry", async () => {
  let clock = 0;
  const windows = [];
  assert.equal(await retainInput({ deadline: 45_000, now: () => clock, notify: () => {}, poll: async n => {
    windows.push(n); clock += n; return null;
  } }), null);
  assert.deepEqual(windows, [30_000, 15_000]);
  assert.throws(() => expiry("bad"));
  assert.throws(() => expiry(new Date(0).toISOString(), 1));
});

test("driver sets the lease once and checks both country fields before a submit", () => {
  const source = fs.readFileSync(path.join(__dirname, "create-account.cjs"), "utf8");
  assert.equal(source.split('leaseDeadline = lifecycle.expiry(').length - 1, 1);
  assert.match(source, /dial !== phoneCC \|\| country !==/);
  assert.match(source, /phoneSubmitted = verdict === "phone_step"/);
  assert.match(source, /!lifecycle.refundable\(reason\)/);
});
