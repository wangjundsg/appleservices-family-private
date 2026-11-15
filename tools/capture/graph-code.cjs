// Print the verification code that just arrived in a Graph-readable mailbox.
//
// graph-mail.cjs refuses to put the code on disk or in its own receipt, which
// is right for a log but useless for automation: this wrapper polls the same
// code path and prints only {ok, code} on stdout. Nothing is written.
//
// Usage: node tools/capture/graph-code.cjs --file <graph.json> [--since ISO] [--wait ms]

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
  const since = arg("since") ? Date.parse(arg("since")) : 0;
  const hit = await pollCode({ file: cfg.file, since });
  console.log(JSON.stringify({ ok: Boolean(hit.ok), code: hit.code || "", error: hit.error || "" }));
  process.exit(hit.ok ? 0 : 1);
})().catch((error) => {
  console.log(JSON.stringify({ ok: false, error: String(error && error.message ? error.message : error) }));
  process.exit(1);
});
