// Pure logic behind the Apple Account creation driver.
//
// Nothing here touches the browser, the filesystem or the network, so every
// decision can be exercised offline with `node --test tools/capture/*.test.cjs`
// (see capture-lib.test.cjs). The four pieces below are the fixes the live run
// of 2026-11-13 asked for (docs/run-20261113.md §4.3, §4.4, §4.8):
//
//   splitCommand / captchaArgv   --captcha-cmd accepts "interpreter script args"
//   pickCaptchaImage             the captcha <img> is the one next to the input
//   CaptchaTracker               the same image twice in a row trips a breaker
//   sha256Hex                    the run log identifies a captcha by its hash
//
// The OCR runner lives here too so the driver only has to say "run this and
// tell me the text"; a solver that fails must not be able to break the loop.

"use strict";

const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");

// One solve may legitimately take a few seconds against an online provider; a
// solver that needs longer than this is stuck and the run has to move on.
const CAPTCHA_CMD_TIMEOUT_MS = 120_000;

// ---- --captcha-cmd parsing (run-20261113 §4.3) ----------------------------

const QUOTES = new Set(['"', "'"]);
const WHITESPACE = /\s/;

// escapeNext reports whether a backslash in front of ch is an escape. Windows
// paths are everywhere in this pipeline ("tools\captcha\solver.py"), so a
// backslash before an ordinary character stays literal; only a quote, another
// backslash or a separator is escaped. A shell that ate "tools\captcha" is
// exactly the class of bug this parser must not introduce.
function escapeNext(ch) {
  return ch === "\\" || QUOTES.has(ch) || WHITESPACE.test(ch);
}

// splitCommand splits one command line into argv without a shell and without a
// dependency. Two rules matter for the real commands this repository runs:
//
//   "python tools/captcha/solver.py --provider yes"  -> ["python", "tools/...", "--provider", "yes"]
//   'python "tools/with space.py" --skip-local'      -> ["python", "tools/with space.py", "--skip-local"]
//
// An unbalanced quote is an error rather than a half-parsed argv: a wrong argv
// is exactly the failure that burned 16 rounds on 2026-11-13.
function splitCommand(text) {
  const input = String(text == null ? "" : text);
  const argv = [];
  let token = "";
  let started = false;
  let quote = "";
  for (let i = 0; i < input.length; i += 1) {
    const ch = input[i];
    const next = i + 1 < input.length ? input[i + 1] : "";
    if (quote) {
      if (ch === quote) {
        quote = "";
        continue;
      }
      if (ch === "\\" && quote === '"' && escapeNext(next)) {
        token += next;
        i += 1;
        continue;
      }
      token += ch;
      continue;
    }
    if (QUOTES.has(ch)) {
      quote = ch;
      started = true;
      continue;
    }
    if (ch === "\\" && escapeNext(next)) {
      token += next;
      i += 1;
      started = true;
      continue;
    }
    if (WHITESPACE.test(ch)) {
      if (started) {
        argv.push(token);
        token = "";
        started = false;
      }
      continue;
    }
    token += ch;
    started = true;
  }
  if (quote) throw new Error(`unterminated ${quote} quote in command`);
  if (started) argv.push(token);
  return argv;
}

// captchaArgv normalises the two accepted --captcha-cmd shapes into one argv:
//
//   '["python","tools/captcha/solver.py","--provider","yes"]'   JSON array (explicit argv)
//   'python tools/captcha/solver.py --provider yes'             command line
//   'tools/captcha/solver.py'                                   script only (legacy)
//
// The legacy single-token spelling keeps its old meaning: it is a *script* that
// python has to run, which is the form that worked in run-20261113 §4.3. A JSON
// array is taken literally, so a standalone executable can be named there.
function captchaArgv(cmd) {
  if (Array.isArray(cmd)) return validateArgv(cmd, "--captcha-cmd array");
  const text = String(cmd == null ? "" : cmd).trim();
  if (!text) throw new Error("empty --captcha-cmd");
  if (text.startsWith("[")) {
    let parsed;
    try {
      parsed = JSON.parse(text);
    } catch (e) {
      throw new Error(`--captcha-cmd is not valid JSON: ${e && e.message ? e.message : e}`);
    }
    return validateArgv(parsed, "--captcha-cmd JSON");
  }
  const argv = validateArgv(splitCommand(text), "--captcha-cmd");
  // Bare script path: python is the interpreter, exactly as before this change.
  return argv.length === 1 ? ["python", argv[0]] : argv;
}

function validateArgv(argv, source) {
  if (!Array.isArray(argv) || argv.length === 0) {
    throw new Error(`${source} must be a non-empty array of strings`);
  }
  return argv.map((item) => {
    if (typeof item !== "string" || item === "") {
      throw new Error(`${source} entries must be non-empty strings`);
    }
    return item;
  });
}

// runCaptchaCommand runs one solve and never throws: the caller gets
// {ok, text, ms} and decides what "no answer" means. A non-zero exit, a
// timeout and an unparseable command all come back as ok=false, so a broken
// solver cannot abort a run that still has attempts left.
function runCaptchaCommand(cmd, pngPath, options = {}) {
  const started = Date.now();
  const done = (result) => ({ ...result, ms: Date.now() - started });
  let argv;
  try {
    argv = captchaArgv(cmd);
  } catch (e) {
    return done({ ok: false, text: "", error: String(e && e.message ? e.message : e) });
  }
  const image = String(pngPath == null ? "" : pngPath);
  if (!image) return done({ ok: false, text: "", error: "missing captcha image path" });

  const [file, ...prefix] = argv;
  const timeout = Number.isFinite(options.timeoutMs) && options.timeoutMs > 0
    ? options.timeoutMs
    : CAPTCHA_CMD_TIMEOUT_MS;
  const result = spawnSync(file, [...prefix, image], {
    encoding: "utf8",
    timeout,
    windowsHide: true,
    maxBuffer: 1 << 20,
  });
  const text = String(result.stdout || "").trim();
  if (result.error) {
    return done({ ok: false, text, error: String(result.error.message || result.error) });
  }
  if (result.status !== 0) {
    return done({ ok: false, text, error: `exit ${result.status}` });
  }
  return done({ ok: true, text });
}

// ---- captcha image selection (run-20261113 §4.4) --------------------------

// The size window the old heuristic used: a real 126x56 captcha is small, and
// so is the illustration that was mistaken for one (378x169 is not, but the
// page's rendering size was), which is why size alone is only a fallback now.
const CAPTCHA_MAX_WIDTH = 200;
const CAPTCHA_MAX_HEIGHT = 120;

function naturalSize(entry) {
  const natural = Array.isArray(entry && entry.natural) ? entry.natural : [];
  const width = Number(natural[0]) || 0;
  const height = Number(natural[1]) || 0;
  return { width, height };
}

function loaded(entry) {
  const { width, height } = naturalSize(entry);
  return width > 0 && height > 0;
}

// pickCaptchaImage decides which <img> is the captcha.
//
// The DOM side (create-account.cjs) hands in two lists of {index, natural:[w,h]}
// entries in document order:
//
//   adjacent  imgs that sit in the same container as input[name="captcha"] and
//             before it in DOM order -- the captcha image, when the page has one
//   images    every <img> on the page, used only for the size fallback
//
// A captcha image that did not load (naturalWidth/naturalHeight 0, what a 503
// on the image resource produces) is *not* a reason to pick something else next
// to it: the run reports "captcha image not loaded" and stops, because OCR on
// the wrong picture only buys 16 rejected answers (run-20261113 §4.4).
function pickCaptchaImage({ adjacent = [], images = [] } = {}) {
  const near = Array.isArray(adjacent) ? adjacent : [];
  const all = Array.isArray(images) ? images : [];

  if (near.length) {
    const hit = near.find(loaded);
    if (!hit) return { ok: false, src_kind: "adjacent", reason: "captcha image not loaded" };
    const { width, height } = naturalSize(hit);
    return { ok: true, index: Number(hit.index), natural: [width, height], src_kind: "adjacent" };
  }

  const hit = all.find((entry) => {
    const { width, height } = naturalSize(entry);
    return loaded(entry) && width <= CAPTCHA_MAX_WIDTH && height <= CAPTCHA_MAX_HEIGHT;
  });
  if (!hit) return { ok: false, src_kind: "size", reason: "captcha image not loaded" };
  const { width, height } = naturalSize(hit);
  return { ok: true, index: Number(hit.index), natural: [width, height], src_kind: "size" };
}

// ---- same-image circuit breaker (run-20261113 §4.8) -----------------------

// CaptchaTracker notices that the page keeps handing back the same picture.
// `duplicate` counts consecutive repeats; reaching `stuckAfterDuplicates`
// (default 2, i.e. the third identical capture) means the captcha resource is
// not refreshing at all and the run must stop instead of paying for another
// solve of the same bytes.
class CaptchaTracker {
  constructor({ stuckAfterDuplicates = 2 } = {}) {
    this.stuckAfterDuplicates = Math.max(1, Number(stuckAfterDuplicates) || 2);
    this.lastHash = "";
    this.duplicates = 0;
    this.seen = 0;
  }

  observe(hash) {
    const digest = String(hash == null ? "" : hash);
    this.seen += 1;
    if (!digest) {
      this.lastHash = "";
      this.duplicates = 0;
      return { duplicate: false, duplicates: 0, stuck: false };
    }
    const duplicate = digest === this.lastHash;
    this.duplicates = duplicate ? this.duplicates + 1 : 0;
    this.lastHash = digest;
    return { duplicate, duplicates: this.duplicates, stuck: this.duplicates >= this.stuckAfterDuplicates };
  }
}

// ---- run-log helpers ------------------------------------------------------

// sha256Hex identifies a captcha capture without keeping the pixels: the log
// records a prefix, which is enough to prove "same image again" and to compare
// two runs of the same page.
function sha256Hex(data) {
  return crypto.createHash("sha256").update(data).digest("hex");
}

module.exports = {
  CAPTCHA_CMD_TIMEOUT_MS,
  CAPTCHA_MAX_HEIGHT,
  CAPTCHA_MAX_WIDTH,
  CaptchaTracker,
  captchaArgv,
  pickCaptchaImage,
  runCaptchaCommand,
  sha256Hex,
  splitCommand,
};
