"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const {spawnSync} = require("node:child_process");
const {createHash} = require("node:crypto");
const entry = path.join(__dirname, "create-account.cjs");

test("actual CLI reports loaded source hashes before reading private inputs", () => {
  const result = spawnSync(process.execPath, [entry, "--version"], {encoding: "utf8",
    env: {...process.env, APPLESERVICES_CREATE_INPUT: "nonexistent-private-input"}});
  assert.equal(result.status, 0, result.stderr);
  const version = JSON.parse(result.stdout);
  assert.equal(version.entry, entry);
  assert.equal(version.automaticResend.email, false);
  assert.equal(version.automaticResend.phone.maxPerNumber, 1);
  for (const [file, hash] of Object.entries(version.files)) {
    assert.equal(hash, createHash("sha256").update(fs.readFileSync(path.join(__dirname, file))).digest("hex"));
  }
});

test("the executable refuses unsafe require instead of starting registration", () => {
  const result = spawnSync(process.execPath, ["-e", `require(${JSON.stringify(entry)})`], {encoding: "utf8"});
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /create_account_is_cli_use_flow_mechanics/);
});

test("real startup receipt identifies the same executor without network or credentials", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "creation-entry-check-"));
  try {
    const result = spawnSync(process.execPath, [entry, "--outdir", dir], {encoding: "utf8", timeout: 10000,
      env: {...process.env, APPLESERVICES_CREATE_INPUT: "", APPLESERVICES_PASSWORD: ""}});
    assert.equal(result.status, 2, result.stderr);
    const events = fs.readFileSync(path.join(dir, "run.jsonl"), "utf8").trim().split(/\r?\n/).map(JSON.parse);
    assert.equal(events[0].kind, "executor-version");
    assert.equal(events[0].entry, entry);
    assert.equal(events[0].files["create-account.cjs"], createHash("sha256").update(fs.readFileSync(entry)).digest("hex"));
    assert.equal(events.at(-1).state, "usage");
  } finally {
    assert.equal(path.dirname(path.resolve(dir)), path.resolve(os.tmpdir()));
    assert.ok(path.basename(dir).startsWith("creation-entry-check-"));
    fs.rmSync(dir, {recursive: true, force: true});
  }
});
