"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const mech = require("./flow-mechanics.cjs");

function fixture({navigation = 60000, iframe = 20000, frameAt = 85000, form = 2000} = {}) {
  let clock = 0;
  const budgets = [];
  const wait = async (duration, options) => {
    budgets.push(options.timeout);
    clock += Math.min(duration, options.timeout);
    if (duration >= options.timeout) throw Object.assign(new Error("timeout"), {name: "TimeoutError"});
  };
  const frame = {url: () => "https://fixture.invalid/widget/account/test",
    locator: () => ({waitFor: options => wait(form, options)})};
  const page = {goto: (_url, options) => wait(navigation, options),
    waitForSelector: (_selector, options) => wait(iframe, options),
    frames: () => clock >= frameAt ? [frame] : []};
  return {page, frame, budgets, now: () => clock, sleep: async ms => {clock += ms;}};
}

test("initial navigation, iframe, frame and form share one 90-second deadline", async () => {
  const f = fixture();
  assert.equal(await mech.loadSignupPage(f.page, {url: "fixture", now: f.now, sleep: f.sleep}), f.frame);
  assert.deepEqual(f.budgets, [90000, 30000, 5000]);
  assert.equal(f.now(), 87000);
});

for (const [stage, options] of Object.entries({
  navigation: {navigation: 120000}, iframe: {iframe: 40000},
  frame: {frameAt: Infinity}, form: {form: 10000},
})) {
  test(`${stage} exhaustion is page load failure at the shared deadline`, async () => {
    const f = fixture(options);
    await assert.rejects(mech.loadSignupPage(f.page, {url: "fixture", now: f.now, sleep: f.sleep}),
      {message: "page_load_failed", state: "failed", evidence: "page_load_timeout", closeReason: "page-load-failed"});
    assert.equal(f.now(), 90000);
  });
}

test("network failure stops immediately and is not an Apple refusal", async () => {
  const f = fixture();
  f.page.goto = async () => {throw Error("net::ERR_CONNECTION_RESET");};
  await assert.rejects(mech.loadSignupPage(f.page, {url: "fixture", now: f.now, sleep: f.sleep}),
    {message: "page_load_failed", state: "failed", evidence: "page_load_failed"});
  assert.equal(f.now(), 0);
});

test("an exhausted budget never becomes Playwright's unlimited timeout zero", async () => {
  const f = fixture();
  await assert.rejects(mech.loadSignupPage(f.page, {url: "fixture", timeoutMs: 0, now: f.now}), /page_load_failed/);
  assert.deepEqual(f.budgets, []);
});
