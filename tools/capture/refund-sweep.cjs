// Collect refunds that hero refused because the activation was younger than
// its two-minute floor. create-account parks those in refunds-pending.json
// (one per run directory) instead of blocking the run.
//
// Usage:
//   node tools/capture/refund-sweep.cjs --hero-key <key> --dir .cache [--dry-run]
//
// Every *.json file found under --dir that is named refunds-pending.json is
// scanned. Entries whose not_before has passed are retried once; the ones that
// still refuse stay in the file with their reason, so nothing is silently lost.

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const refund = require("./refund.cjs");

const argv = process.argv.slice(2);
function arg(name, fallback = "") {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}
const flagOn = (name) => argv.includes(`--${name}`);

const heroKey = arg("hero-key") || process.env.HERO_KEY || "";
const root = path.resolve(arg("dir", ".cache"));
const dryRun = flagOn("dry-run");

function findQueues(dir, out = []) {
  let entries = [];
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return out;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) findQueues(full, out);
    else if (entry.name === "refunds-pending.json") out.push(full);
  }
  return out;
}

(async () => {
  if (!heroKey) {
    console.error(JSON.stringify({ ok: false, error: "missing --hero-key or HERO_KEY" }));
    process.exit(1);
  }
  const queues = findQueues(root);
  const report = { swept: 0, refunded: 0, still_waiting: 0, failed: 0, files: queues.length };

  for (const queuePath of queues) {
    let queue;
    try {
      queue = JSON.parse(fs.readFileSync(queuePath, "utf8"));
    } catch {
      continue;
    }
    const keep = [];
    for (const item of queue) {
      const ready = Date.parse(item.not_before || 0) <= Date.now();
      if (!ready) {
        keep.push(item);
        report.still_waiting += 1;
        continue;
      }
      if (dryRun) {
        keep.push(item);
        continue;
      }
      report.swept += 1;
      let result;
      try {
        result = await refund.refundActivation({ heroKey, activation: item.activation });
      } catch (error) {
        result = { ok: false, response: "", error: String(error && error.message) };
      }
      if (result.ok) {
        report.refunded += 1;
      } else {
        report.failed += 1;
        keep.push({ ...item, not_before: new Date(Date.now() + 300000).toISOString(), last: result.response || "" });
      }
    }
    if (!dryRun) fs.writeFileSync(queuePath, JSON.stringify(keep, null, 2), "utf8");
  }

  console.log(JSON.stringify(report));
  process.exit(report.failed > 0 ? 1 : 0);
})();
