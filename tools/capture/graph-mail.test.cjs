// Tests for the Graph mail reader (tools/capture/graph-mail.cjs).
//
// Run with:  node --test tools/capture/graph-mail.test.cjs
//
// The reader exists so the create driver can take the Apple verification code
// out of a Microsoft mailbox without a human. Two things are worth testing
// without a live credential: the three-way match (this mailbox, Apple sender,
// verification subject, newest wins) and the promise that the code never leaves
// the function except as a return value the caller logs around.

"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { test } = require("node:test");
const graph = require("./graph-mail.cjs");

const MAILBOX = "amber@hotmail.com";

function tokenFile(extra = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "graph-mail-"));
  const file = path.join(dir, "graph.json");
  fs.writeFileSync(
    file,
    JSON.stringify({
      client_id: "00000000-0000-0000-0000-000000000000",
      refresh_token: "RT-1",
      mailbox: MAILBOX,
      ...extra,
    }),
    "utf8",
  );
  return file;
}

function graphMail({ id = "m1", from = "no_reply@email.apple.com", subject = "Your Apple Account verification code", received = "2026-11-13T01:00:00Z", html = "<div><b>123456</b></div>", to = [MAILBOX] } = {}) {
  return {
    id,
    subject,
    from: { emailAddress: { address: from } },
    toRecipients: to.map((a) => ({ emailAddress: { address: a } })),
    ccRecipients: [],
    receivedDateTime: received,
    bodyPreview: "",
    body: { contentType: "html", content: html },
  };
}

// A fetch stub that answers /token and $select=.../me/messages from a queue.
function stubFetch({ token = { status: 200, body: { access_token: "AT", refresh_token: "RT-2", expires_in: 3600 } }, pages = [] } = {}) {
  const calls = [];
  const impl = async (url, init = {}) => {
    calls.push({ url: String(url), body: String(init.body || "") });
    if (String(url).includes("/oauth2/v2.0/token")) {
      return {
        ok: token.status >= 200 && token.status < 300,
        status: token.status,
        text: async () => JSON.stringify(token.body),
      };
    }
    const page = pages.length > 1 ? pages.shift() : pages[0] || [];
    return { ok: true, status: 200, text: async () => JSON.stringify({ value: page }) };
  };
  impl.calls = calls;
  return impl;
}

test("readTokenFile refuses a file without the three driving keys", () => {
  const file = tokenFile();
  assert.equal(graph.readTokenFile(file).mailbox, MAILBOX);
  assert.throws(() => graph.readTokenFile(tokenFile({ mailbox: "" })), /missing mailbox/);
  assert.throws(() => graph.readTokenFile(tokenFile({ refresh_token: "" })), /missing refresh_token/);
  assert.throws(() => graph.readTokenFile(path.join(os.tmpdir(), "nope-graph.json")), /unreadable/);
});

test("readTokenFile defaults to the .default scope Graph actually honours", () => {
  assert.equal(graph.readTokenFile(tokenFile()).scope, "https://graph.microsoft.com/.default");
});

test("exchangeToken persists the rotated refresh token so the next run still works", async () => {
  const file = tokenFile();
  const cfg = graph.readTokenFile(file);
  const res = await graph.exchangeToken(cfg, stubFetch());
  assert.equal(res.ok, true);
  assert.equal(res.access_token, "AT");
  assert.equal(JSON.parse(fs.readFileSync(file, "utf8")).refresh_token, "RT-2");
});

test("exchangeToken falls back to Mail.ReadWrite when .default is refused", async () => {
  const file = tokenFile();
  const cfg = graph.readTokenFile(file);
  const answers = [
    { status: 400, body: { error: "invalid_grant", error_description: "AADSTS70000" } },
    { status: 200, body: { access_token: "AT2", refresh_token: "RT-3" } },
  ];
  const impl = async (url, init = {}) => {
    const next = answers.shift();
    return { ok: next.status === 200, status: next.status, text: async () => JSON.stringify(next.body) };
  };
  const res = await graph.exchangeToken(cfg, impl);
  assert.equal(res.ok, true);
  assert.equal(res.access_token, "AT2");
  assert.equal(res.scope, "https://graph.microsoft.com/Mail.ReadWrite");
});

test("codeFromText prefers the bold run and ignores a year or a long number", () => {
  assert.equal(graph.codeFromText("<b>123456</b>"), "123456");
  assert.equal(graph.codeFromText("<p>Your code is 987654</p>"), "987654");
  assert.equal(graph.codeFromText("<p>Copyright 2026 Apple Inc.</p>"), "");
  assert.equal(graph.codeFromText("<p>Call 09510293847 for help</p>"), "");
});

test("pickCode matches mailbox, sender and subject, newest first", () => {
  const messages = [
    { id: "old", subject: "Your Apple Account verification code", from: "no_reply@email.apple.com", to: [MAILBOX], receivedDateTime: "2026-11-13T01:00:00Z", body: "<b>111111</b>", bodyPreview: "" },
    { id: "new", subject: "Your Apple Account verification code", from: "no_reply@email.apple.com", to: [MAILBOX], receivedDateTime: "2026-11-13T01:02:00Z", body: "<b>222222</b>", bodyPreview: "" },
    { id: "other-inbox", subject: "Your Apple Account verification code", from: "no_reply@email.apple.com", to: ["someone@example.com"], receivedDateTime: "2026-11-13T01:03:00Z", body: "<b>333333</b>", bodyPreview: "" },
    { id: "other-sender", subject: "Your Apple Account verification code", from: "noreply@github.com", to: [MAILBOX], receivedDateTime: "2026-11-13T01:04:00Z", body: "<b>444444</b>", bodyPreview: "" },
    { id: "no-subject-semantics", subject: "Welcome to Apple", from: "no_reply@email.apple.com", to: [MAILBOX], receivedDateTime: "2026-11-13T01:05:00Z", body: "<b>555555</b>", bodyPreview: "" },
  ];
  const hit = graph.pickCode(messages, { mailbox: MAILBOX });
  assert.ok(hit, "a matching mail must be found");
  assert.equal(hit.messageId, "new");
  assert.equal(hit.code, "222222");
});

test("pickCode ignores a mail older than the run start", () => {
  const stale = {
    id: "stale",
    subject: "Your Apple Account verification code",
    from: "no_reply@email.apple.com",
    to: [MAILBOX],
    receivedDateTime: "2026-11-13T00:10:00Z",
    body: "<b>123456</b>",
    bodyPreview: "",
  };
  const hit = graph.pickCode([stale], { mailbox: MAILBOX, since: Date.parse("2026-11-13T01:00:00Z") });
  assert.equal(hit, null);
});

test("pollCode returns the code and only the message id crosses the boundary", async () => {
  const file = tokenFile();
  const pages = [[], [graphMail()]];
  const fetchImpl = stubFetch({ pages: [...pages] });
  const ticks = [];
  const hit = await graph.pollCode({
    file,
    since: Date.parse("2026-11-13T00:59:00Z"),
    intervalMs: 5,
    timeoutMs: 500,
    fetchImpl,
    onTick: (t) => ticks.push(t),
  });
  assert.equal(hit.ok, true);
  assert.equal(hit.code, "123456");
  assert.equal(hit.messageId, "m1");
  assert.ok(ticks.length >= 2, "the poll reports each round it waits through");
});

test("pollCode reports a refusal instead of throwing", async () => {
  const file = tokenFile();
  const fetchImpl = stubFetch({ token: { status: 400, body: { error: "invalid_grant", error_description: "AADSTS70000" } } });
  const hit = await graph.pollCode({ file, intervalMs: 5, timeoutMs: 50, fetchImpl });
  assert.equal(hit.ok, false);
  assert.match(hit.error, /token: invalid_grant/);
});

test("pollCode gives up with a reason that names the inbox size", async () => {
  const file = tokenFile();
  const fetchImpl = stubFetch({ pages: [[graphMail({ from: "noreply@github.com", subject: "launch code" })]] });
  const hit = await graph.pollCode({ file, intervalMs: 5, timeoutMs: 60, fetchImpl });
  assert.equal(hit.ok, false);
  assert.match(hit.error, /no matching mail/);
});
