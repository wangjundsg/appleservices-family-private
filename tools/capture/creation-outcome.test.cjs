"use strict";
const {test} = require("node:test"), assert = require("node:assert/strict");
const fs = require("node:fs");
const flow = require("./account-flow.cjs"), mech = require("./flow-mechanics.cjs");

test("driver records submission uncertainty, skips load diagnostics and closes its browser", async () => {
  const source = fs.readFileSync(require.resolve("./create-account.cjs"), "utf8");
  const handler = source.slice(source.lastIndexOf("})().catch(async (e) => {") + "})().catch(".length, source.lastIndexOf("});") + 1);
  for (const [submitted, error, expectedState, expectedCode, screenshots] of [
    [false, {message: "page_load_failed", state: "failed", evidence: "page_load_timeout"}, "failed", 1, 0],
    [false, {message: "script failure"}, "failed", 1, 1],
    [true, {message: "script failure", state: "failed"}, "unknown", 3, 1],
    [true, {message: "page_transition_timeout", state: "unknown"}, "unknown", 3, 1],
    [true, {message: "Apple refused", state: "failed", evidence: "hard-reject:1"}, "failed", 1, 1],
  ]) {
    const events = [], refunds = [];
    let output, exitCode, shots = 0, closed = 0;
    const deps = {flow, mech, submissionMayHaveLanded: submitted, phoneSubmitted: false, startedAt: Date.now(),
      clip: (text, max) => String(text).slice(0, max), note: (kind, event) => events.push({kind, ...event}),
      failShot: async () => {shots++;}, autoRefund: async state => refunds.push(state),
      logClose: () => {}, writeRunLog: () => {}, closeOwnedBrowser: async () => {closed++;},
      console: {error: text => {output = JSON.parse(text);}}, process: {exit: code => {exitCode = code;}}};
    const catchError = new Function(...Object.keys(deps), "return " + handler)(...Object.values(deps));
    await catchError(error);
    assert.equal(output.state, expectedState);
    assert.equal(output.submitted, submitted);
    assert.equal(events.find(event => event.kind === "final").submitted, submitted);
    assert.equal(exitCode, expectedCode);
    assert.equal(shots, screenshots);
    assert.equal(closed, 1);
    assert.deepEqual(refunds, [expectedState]);
  }
});
