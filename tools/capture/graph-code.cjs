// Print the verification code that just arrived in a Graph-readable mailbox.
//
// graph-mail.cjs refuses to put the code on disk or in its own receipt, which
// is right for a log but useless for automation: this wrapper polls the same
// code path and prints only {ok, code} on stdout. Nothing is written.
//
// Usage: node tools/capture/graph-code.cjs --file <graph.json> --mailbox <address> --since <ISO> [--wait ms]

"use strict";

const { pollCode, readTokenFile } = require("./graph-mail.cjs");

function arg(name, fallback = "") {
  const argv = process.argv.slice(2);
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}

(async () => {
  const file = arg("file");
  if (!file) {
    console.log(JSON.stringify({ ok: false, error: "--file is required" }));
    process.exit(2);
  }
  let cfg;
  try {
    cfg = readTokenFile(file);
  } catch (error) {
    console.log(JSON.stringify({ ok: false, error: String(error.message || error) }));
    process.exit(1);
  }
  const mailbox = arg("mailbox").trim().toLowerCase();
  const since = Date.parse(arg("since"));
  const timeoutMs = Number(arg("wait", "150000"));
  if (!mailbox || mailbox !== cfg.mailbox || !Number.isFinite(since) || !Number.isFinite(timeoutMs) || timeoutMs <= 0 || timeoutMs > 600000) {
    console.log(JSON.stringify({ ok: false, error: "mailbox binding, since and wait are required and must be valid" }));
    process.exit(2);
  }
  const hit = await pollCode({ file: cfg.file, mailbox, since, timeoutMs, clockSkewMs: 0 });
  console.log(JSON.stringify({ ok: Boolean(hit.ok), code: hit.code || "", error: hit.error || "" }));
  process.exit(hit.ok ? 0 : 1);
})().catch((error) => {
  console.log(JSON.stringify({ ok: false, error: String(error && error.message ? error.message : error) }));
  process.exit(1);
});
