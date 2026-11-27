// export-captcha-dataset.cjs — collect the image challenge samples already on disk
// into one folder with a label file, so they can be used for model work.
//
// What it pairs:
//   captcha-attempt-N.png  <->  captcha-attempt-N.txt   (JSON: {attempt, answer, error, at})
//   captcha-answer.txt     <->  the newest captcha*.png in the same folder
//
// Usage:
//   node tools/capture/export-captcha-dataset.cjs [--root .cache] [--out .cache/captcha-dataset]
//
// Output:
//   <out>/images/<sha12>.png
//   <out>/labels.csv     columns: file,label,verdict,attempt,source_dir
//
// "verdict" is accepted / rejected / unknown, inferred from the saved page text:
// a rejection keeps the challenge on screen, an acceptance moves past it.

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

const ROOT = path.resolve(arg("root", ".cache"));
const OUT = path.resolve(arg("out", ".cache/captcha-dataset"));

function walk(dir, acc = []) {
  let entries = [];
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return acc;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, acc);
    else acc.push(full);
  }
  return acc;
}

function sha12(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex").slice(0, 12);
}

// A rejection leaves the challenge (and its error text) on the page; an
// acceptance is followed by a screen that no longer mentions the characters.
const REJECT_HINT = /characters in the image|type the characters|captcha|verify/i;

function verdictFrom(errorText) {
  if (!errorText) return "unknown";
  return REJECT_HINT.test(errorText) ? "rejected" : "accepted";
}

function labelOf(raw) {
  try {
    const data = JSON.parse(raw);
    return { answer: String(data.answer || "").trim(), error: String(data.error || ""), attempt: data.attempt || "" };
  } catch {
    const answer = String(raw).trim().split(/\s+/)[0] || "";
    return { answer, error: "", attempt: "" };
  }
}

function main() {
  const files = walk(ROOT);
  const images = new Set(files.filter((f) => /\.(png|jpg|jpeg)$/i.test(f) && /captcha/i.test(path.basename(f))));
  const rows = [];
  const seen = new Set();

  const add = (image, answer, verdict, attempt, dir) => {
    const label = String(answer || "").replace(/[^0-9A-Za-z]/g, "");
    if (label.length < 3 || label.length > 8) return;
    if (!fs.existsSync(image)) return;
    const digest = sha12(image);
    if (seen.has(digest)) return;
    seen.add(digest);
    fs.mkdirSync(path.join(OUT, "images"), { recursive: true });
    fs.copyFileSync(image, path.join(OUT, "images", `${digest}.png`));
    rows.push({ file: `images/${digest}.png`, label, verdict, attempt, source_dir: path.dirname(image) });
  };

  // 1) attempt files: image and answer share a basename
  for (const file of files) {
    const base = path.basename(file);
    if (!/^captcha-attempt-\d+\.txt$/i.test(base)) continue;
    const image = path.join(path.dirname(file), base.replace(/\.txt$/i, ".png"));
    const { answer, error, attempt } = labelOf(fs.readFileSync(file, "utf8"));
    add(image, answer, verdictFrom(error), attempt, path.dirname(file));
  }

  // 2) a bare captcha-answer.txt pairs with the newest challenge image beside it
  for (const file of files) {
    if (path.basename(file) !== "captcha-answer.txt") continue;
    const dir = path.dirname(file);
    const local = [...images].filter((i) => path.dirname(i) === dir && !/captcha-attempt-/i.test(i));
    if (!local.length) continue;
    local.sort((a, b) => fs.statSync(b).mtimeMs - fs.statSync(a).mtimeMs);
    const { answer } = labelOf(fs.readFileSync(file, "utf8"));
    add(local[0], answer, "unknown", "", dir);
  }

  if (!rows.length) {
    console.log("no labelled samples found");
    return 1;
  }

  const header = "file,label,verdict,attempt,source_dir\n";
  const body = rows
    .map((r) => [r.file, r.label, r.verdict, r.attempt, `"${r.source_dir}"`].join(","))
    .join("\n");
  fs.writeFileSync(path.join(OUT, "labels.csv"), header + body + "\n", "utf8");

  const accepted = rows.filter((r) => r.verdict === "accepted").length;
  const rejected = rows.filter((r) => r.verdict === "rejected").length;
  console.log(`samples: ${rows.length}  accepted: ${accepted}  rejected: ${rejected}  unknown: ${rows.length - accepted - rejected}`);
  console.log(`images on disk: ${images.size}`);
  console.log(`out: ${OUT}`);
  return 0;
}

process.exit(main());
