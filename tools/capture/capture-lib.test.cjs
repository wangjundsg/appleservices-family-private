// Tests for the pure logic behind the creation driver (capture-lib.cjs) plus the
// refund classifier (refund.cjs).
//
// Run with:  node --test tools/capture/*.test.cjs
//
// These cover the four fixes from docs/run-20261113.md that have to be provable
// without a browser, a proxy or an Apple page:
//
//   §4.3  --captcha-cmd takes an interpreter, a script and its arguments
//   §4.4  the captcha img is the one next to the input, and an unloaded one
//         stops the run instead of being replaced by a decorative picture
//   §4.8  the same capture repeated twice in a row trips the breaker
//   §4.7  only ACCESS_CANCEL / ACCESS_CANCEL_ALREADY count as a refund

"use strict";

const assert = require("node:assert/strict");
const { test } = require("node:test");

const lib = require("./capture-lib.cjs");
const refund = require("./refund.cjs");

// ---- §4.3: --captcha-cmd parsing -------------------------------------------

test("§4.3 splitCommand keeps a plain command line in argv order", () => {
  assert.deepEqual(lib.splitCommand("python tools/captcha/solver.py --provider yes"), [
    "python",
    "tools/captcha/solver.py",
    "--provider",
    "yes",
  ]);
});

test("§4.3 splitCommand respects paired quotes and collapses separators", () => {
  assert.deepEqual(lib.splitCommand('python "tools/with space.py" --skip-local'), [
    "python",
    "tools/with space.py",
    "--skip-local",
  ]);
  assert.deepEqual(lib.splitCommand("a   'b c'   d"), ["a", "b c", "d"]);
  assert.deepEqual(lib.splitCommand('  --flag="a b"  '), ["--flag=a b"]);
  // A Windows path keeps its separators: the second real run used exactly this
  // spelling and a parser that ate the backslashes would break it again.
  assert.deepEqual(lib.splitCommand("python tools\\captcha\\solver.py"), ["python", "tools\\captcha\\solver.py"]);
  // An escaped separator still joins one token.
  assert.deepEqual(lib.splitCommand("python my\\ solver.py"), ["python", "my solver.py"]);
});

test("§4.3 splitCommand rejects an unbalanced quote instead of guessing", () => {
  assert.throws(() => lib.splitCommand('python "tools/captcha/solver.py'), /unterminated/);
  assert.throws(() => lib.splitCommand("python 'solver.py"), /unterminated/);
});

test("§4.3 captchaArgv accepts the JSON array form", () => {
  assert.deepEqual(lib.captchaArgv('["python","tools/captcha/solver.py","--provider","yes"]'), [
    "python",
    "tools/captcha/solver.py",
    "--provider",
    "yes",
  ]);
  // Whitespace inside the JSON does not matter; it is parsed, not split.
  assert.deepEqual(lib.captchaArgv('[ "python", "tools/captcha/solver.py" ]'), ["python", "tools/captcha/solver.py"]);
});

test("§4.3 captchaArgv keeps the legacy single-script form working", () => {
  // Bare script path: python runs it, exactly as the second live run spelled it.
  assert.deepEqual(lib.captchaArgv("tools\\captcha\\solver.py"), ["python", "tools\\captcha\\solver.py"]);
  assert.deepEqual(lib.captchaArgv('"tools\\captcha\\solver.py"'), ["python", "tools\\captcha\\solver.py"]);
  // A JSON array is explicit argv: one entry stays one entry (a standalone exe).
  assert.deepEqual(lib.captchaArgv('["solver.exe"]'), ["solver.exe"]);
});

test("§4.3 captchaArgv rejects malformed arrays and empty input", () => {
  assert.throws(() => lib.captchaArgv(""), /empty/);
  assert.throws(() => lib.captchaArgv("   "), /empty/);
  assert.throws(() => lib.captchaArgv('["python",]'), /not valid JSON/);
  assert.throws(() => lib.captchaArgv("[]"), /non-empty/);
  assert.throws(() => lib.captchaArgv('["python", 7]'), /non-empty strings/);
  assert.throws(() => lib.captchaArgv('["python", ""]'), /non-empty strings/);
});

test("§4.3 runCaptchaCommand passes the image path last and reports the text", () => {
  const script = "process.stdout.write('abcde')";
  const result = lib.runCaptchaCommand([process.execPath, "-e", script], "C:\\tmp\\captcha-1.png", {
    timeoutMs: 20000,
  });
  assert.equal(result.ok, true);
  assert.equal(result.text, "abcde");
  assert.ok(result.ms >= 0);
});

test("§4.3 runCaptchaCommand turns a non-zero exit into ok=false, not an exception", () => {
  const result = lib.runCaptchaCommand([process.execPath, "-e", "process.exit(3)"], "captcha.png");
  assert.equal(result.ok, false);
  assert.equal(result.text, "");
  assert.match(result.error, /exit 3/);
});

test("§4.3 runCaptchaCommand treats a timeout as ok=false", () => {
  const result = lib.runCaptchaCommand(
    [process.execPath, "-e", "setTimeout(function () {}, 5000)"],
    "captcha.png",
    { timeoutMs: 150 },
  );
  assert.equal(result.ok, false);
  assert.match(String(result.error).toLowerCase(), /timed? ?out|etimedout/);
});

test("§4.3 runCaptchaCommand never throws on a bad command string", () => {
  const result = lib.runCaptchaCommand('python "unterminated', "captcha.png");
  assert.equal(result.ok, false);
  assert.match(result.error, /unterminated/);
});

// ---- §4.4: captcha image selection -----------------------------------------

// The page from the live run: a 378x169 privacy illustration plus a captcha
// <img> that never loaded because /captcha answered 503.
function runPage({ captchaLoaded = false } = {}) {
  return {
    images: [
      { index: 0, natural: [378, 169] }, // "two people shaking hands"
      { index: 1, natural: captchaLoaded ? [126, 56] : [0, 0] },
    ],
    adjacent: [{ index: 1, natural: captchaLoaded ? [126, 56] : [0, 0] }],
  };
}

test("§4.4 the img next to input[name=captcha] wins over a bigger neighbour", () => {
  const pick = lib.pickCaptchaImage(runPage({ captchaLoaded: true }));
  assert.deepEqual(pick, { ok: true, index: 1, natural: [126, 56], src_kind: "adjacent" });
});

test("§4.4 an unloaded captcha stops the run instead of selecting another image", () => {
  const pick = lib.pickCaptchaImage(runPage());
  assert.equal(pick.ok, false);
  assert.equal(pick.src_kind, "adjacent");
  assert.equal(pick.reason, "captcha image not loaded");
});

test("§4.4 the size heuristic is only a fallback when no adjacent img exists", () => {
  const pick = lib.pickCaptchaImage({
    adjacent: [],
    images: [
      { index: 0, natural: [378, 169] }, // too big for the captcha window
      { index: 2, natural: [126, 56] },
    ],
  });
  assert.deepEqual(pick, { ok: true, index: 2, natural: [126, 56], src_kind: "size" });
});

test("§4.4 the fallback still refuses a page with no loaded image in the window", () => {
  const pick = lib.pickCaptchaImage({
    adjacent: [],
    images: [
      { index: 0, natural: [0, 0] },
      { index: 1, natural: [640, 480] },
    ],
  });
  assert.equal(pick.ok, false);
  assert.equal(pick.src_kind, "size");
  assert.equal(pick.reason, "captcha image not loaded");
});

test("§4.4 an empty observation is reported, never treated as index 0", () => {
  const pick = lib.pickCaptchaImage({});
  assert.equal(pick.ok, false);
  assert.equal(pick.reason, "captcha image not loaded");
});

// ---- §4.8: same-image circuit breaker --------------------------------------

test("§4.8 a repeated capture counts once, the second repeat trips the breaker", () => {
  const tracker = new lib.CaptchaTracker();
  assert.deepEqual(tracker.observe("a"), { duplicate: false, duplicates: 0, stuck: false });
  assert.deepEqual(tracker.observe("a"), { duplicate: true, duplicates: 1, stuck: false });
  assert.deepEqual(tracker.observe("a"), { duplicate: true, duplicates: 2, stuck: true });
});

test("§4.8 a different image resets the counter", () => {
  const tracker = new lib.CaptchaTracker();
  tracker.observe("a");
  tracker.observe("a");
  assert.deepEqual(tracker.observe("b"), { duplicate: false, duplicates: 0, stuck: false });
  assert.deepEqual(tracker.observe("b"), { duplicate: true, duplicates: 1, stuck: false });
});

test("§4.8 the live run would have stopped on the third identical capture", () => {
  const tracker = new lib.CaptchaTracker();
  // 16 byte-identical 378x169 captures, sha256 c0184710… in the real run.
  const stuckAt = [];
  for (let attempt = 1; attempt <= 16; attempt += 1) {
    const gate = tracker.observe("c0184710d3d21fb3");
    if (gate.stuck) stuckAt.push(attempt);
  }
  assert.deepEqual(stuckAt, [3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]);
  assert.equal(tracker.seen, 16);
});

test("§4.8 an unreadable capture never counts as a duplicate", () => {
  const tracker = new lib.CaptchaTracker();
  assert.deepEqual(tracker.observe(""), { duplicate: false, duplicates: 0, stuck: false });
  assert.deepEqual(tracker.observe(""), { duplicate: false, duplicates: 0, stuck: false });
});

test("§4.8 the threshold is configurable and cannot be set to zero", () => {
  const once = new lib.CaptchaTracker({ stuckAfterDuplicates: 1 });
  once.observe("a");
  assert.equal(once.observe("a").stuck, true);
  const guarded = new lib.CaptchaTracker({ stuckAfterDuplicates: 0 });
  guarded.observe("a");
  guarded.observe("a");
  assert.equal(guarded.observe("a").stuck, true);
});

test("§4.8 sha256Hex identifies the capture without keeping the pixels", () => {
  assert.equal(lib.sha256Hex("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  assert.equal(lib.sha256Hex(Buffer.from("abc")), lib.sha256Hex("abc"));
});

// ---- §4.7: refund classification -------------------------------------------

test("§4.7 only ACCESS_CANCEL and ACCESS_CANCEL_ALREADY count as a refund", () => {
  assert.equal(refund.refundAccepted("ACCESS_CANCEL"), true);
  assert.equal(refund.refundAccepted("ACCESS_CANCEL_ALREADY"), true);
  assert.equal(refund.refundAccepted("  ACCESS_CANCEL\n"), true);
  assert.equal(refund.refundAccepted("STATUS_WAIT_CODE"), false);
  assert.equal(refund.refundAccepted("BAD_ACTION"), false);
  assert.equal(refund.refundAccepted("ACCESS_CANCELED"), false);
  assert.equal(refund.refundAccepted(""), false);
  assert.equal(refund.refundAccepted(null), false);
});

test("§4.7 refundActivation asks hero to cancel and never echoes the key", async () => {
  const seen = [];
  const fakeFetch = async (url) => {
    seen.push(String(url));
    return { text: async () => "ACCESS_CANCEL" };
  };
  const result = await refund.refundActivation({
    heroKey: "hero-key-value",
    activation: "1114967085",
    fetchImpl: fakeFetch,
  });
  assert.equal(result.ok, true);
  assert.equal(result.response, "ACCESS_CANCEL");
  assert.equal(seen.length, 1);
  assert.match(seen[0], /action=setStatus/);
  assert.match(seen[0], /status=8/);
  assert.match(seen[0], /id=1114967085/);
  assert.equal(JSON.stringify({ ...result }).includes("hero-key-value"), false);
});

test("§4.7 a hero refusal is reported, not retried", async () => {
  const result = await refund.refundActivation({
    heroKey: "k",
    activation: "1",
    fetchImpl: async () => ({ text: async () => "BAD_ACTION" }),
  });
  assert.equal(result.ok, false);
  assert.equal(result.response, "BAD_ACTION");
});

test("§4.7 refundActivation refuses to run without hero arguments", async () => {
  await assert.rejects(() => refund.refundActivation({ heroKey: "", activation: "1" }), /hero-key/);
  await assert.rejects(() => refund.refundActivation({ heroKey: "k", activation: "" }), /hero-key/);
});
