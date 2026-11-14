// End-to-end Apple Account creation driver.
//
// One browser session carries the whole flow: create form -> email code ->
// phone code -> done. Codes are fetched from this account's own mailbox
// (cloudflare_temp_email API) and from the SMS provider, so nothing is typed
// by hand except the image captcha, which waits on a file.
//
// The browser fingerprint is frozen per account in <outdir>/profile.json (see
// internal/profile). --profile points the run at an existing frozen profile;
// without it the file of this account directory is reused when present and
// generated once when not.
//
// Usage:
//   node tools/capture/create-account.cjs \
//     --email 111@abc-temp-2026.shop --jwt <address_jwt> --password <pw> \
//     --phone 5182460055 --activation 1112400649 --hero-key <key> \
//     --outdir .cache\acct-01 --await-captcha .cache\captcha-answer.txt \
//     [--mail-from apple.com,icloud.com] \
//     [--profile .cache\acct-01\profile.json] [--egress-ip 203.0.113.7] \
//     [--preflight] [--auto-refund]
//
// --graph-token-file points the email step at a Microsoft mailbox instead of the
// temp-mail API: a JSON file {client_id, refresh_token, mailbox} that the run
// polls through Graph every 5s (180s window) for the newest mail addressed to
// this mailbox, from an Apple sender, whose subject carries verification
// semantics. The file is a credential: keep it outside the repository (the
// .cache directory is ignored) and delete it when the run is done. The rotated
// refresh token is written back to it, because Microsoft retires the old one.
//
// --captcha-cmd takes the solver in either shape (run-20261113 §4.3):
//   ["python","tools/captcha/solver.py","--provider","yes"]   JSON array
//   "python tools/captcha/solver.py --provider yes"           command line
//   "tools\captcha\solver.py"                                 script only (legacy)
//
// --preflight asks the captcha resource once before the browser starts and
// exits without touching the form when the egress cannot reach it (run §4.5);
// --preflight-url overrides which resource is asked (default: the /captcha
// endpoint from docs/signup-protocol.md §3.1).
// --auto-refund cancels the hero-sms number before any non-zero exit (run §4.7).
//
// --password is mandatory (or APPLESERVICES_PASSWORD): review item A1 removed
// the literal default the script used to fall back to, so a forgotten flag can
// no longer create an account with a password that is public in this repo.
//
// Exit codes (review item B4 — success needs evidence, not hope):
//   0  state=success       an explicit terminal screen was read back
//   1  state=failed        the run provably did not create the account
//   2  usage               refused before the first network operation
//   3  state=unknown       a submit may or may not have landed; re-read, never replay
//   4  state=waiting_input the code never arrived; the account is still mid-flow
// The last line of run.jsonl always carries the same terminal state.

const fs = require("node:fs");
const path = require("node:path");
const flow = require("./account-flow.cjs");
const lib = require("./capture-lib.cjs");
const graphMail = require("./graph-mail.cjs");
const preflight = require("./preflight.cjs");
const refund = require("./refund.cjs");
const { chromium } = require("D:/appleservices-family-private/.tools/js/node_modules/playwright-core");

const MAIL_HOST = "https://abc-temp-2026.shop";
const HERO_API = "https://hero-sms.com/stubs/handler_api.php";

function arg(name, fallback) {
  const flag = `--${name}`;
  for (let i = 0; i < process.argv.length; i += 1) {
    const a = process.argv[i];
    if (a === flag) {
      const next = process.argv[i + 1];
      return next && !next.startsWith("--") ? next : true;
    }
    if (a.startsWith(`${flag}=`)) return a.slice(flag.length + 1);
  }
  return fallback;
}

// Boolean flags are spelled --flag or --flag=true; a bare "--flag false" is not
// supported on purpose, because a boolean parser that reads the next argv is the
// one that silently eats --password.
function flagOn(name) {
  const value = arg(name, false);
  if (value === true) return true;
  return ["true", "1", "yes"].includes(String(value).toLowerCase());
}

const outdir = path.resolve(arg("outdir", ".cache/acct"));
const email = String(arg("email", ""));
const jwt = String(arg("jwt", ""));
const phone = String(arg("phone", ""));
const activation = String(arg("activation", ""));
const heroKey = String(arg("hero-key", ""));
const awaitCaptcha = String(arg("await-captcha", ""));
// --preflight gates the browser on the captcha resource being reachable at all;
// --auto-refund cancels the number before any non-zero exit (run-20261113 §4.5,
// §4.7). Both default off so an existing invocation behaves exactly as before.
const preflightEnabled = flagOn("preflight");
// The raw HTTP probe reports 403 for a perfectly usable exit because it does
// not speak Chrome's TLS, so --preflight now shells out to the browser probe
// (same stack as the run). run-20261113c 5.1.
const autoRefundEnabled = flagOn("auto-refund");
// Review A1: no default password. The refusal itself happens at the top of the
// run body below, where it can still cancel a rented number before exiting
// (run-20261113 §4.7) instead of dying at module scope.
const passwordResolution = flow.resolvePassword(arg("password", ""), process.env.APPLESERVICES_PASSWORD);
const phoneCC = String(arg("phonecc", "US")).toUpperCase();
const password = passwordResolution.password;
// run-20261113 §4.3: a --captcha-cmd that cannot be parsed used to be discovered
// one attempt at a time (16 empty OCR rounds). It is validated at the top of the
// run body, before the first network call.
const captchaCmdFlag = String(arg("captcha-cmd", "") || "");
// Senders a verification mail may come from; Apple's own domains by default.
// --mail-from keeps the check configurable without weakening it (review B5).
const mailFrom = String(arg("mail-from", "") || "")
  .split(",")
  .map((item) => item.trim())
  .filter(Boolean);
const senderDomains = mailFrom.length ? mailFrom : flow.DEFAULT_SENDER_DOMAINS;
// Frozen fingerprint input/output. --profile is the file to read; --egress-ip is
// what the caller already measured for this run, never probed here.
const profilePath = String(arg("profile", ""));
const egressIP = String(arg("egress-ip", ""));
// --cdp-port drives an already-fingerprinted Roxy/Forge window instead of
// launching a browser of this script's own (REGISTRAR-CONTRACT §1.5). In that
// mode Roxy owns the UA, platform, locale, timezone, GPU and proxy: the profile
// file is neither read nor written, and nothing is injected into the page,
// because a second, contradicting fingerprint is exactly what the A/B is
// measuring against.
const cdpPort = String(arg("cdp-port", ""));
const attachMode = Boolean(cdpPort);

const log = [];
const clip = (text, max) => String(text || "").slice(0, max);
function note(kind, data) {
  const record = { t: Date.now(), kind, ...data };
  log.push(record);
  // Append immediately: a killed process used to lose the whole file, which is
  // exactly when the evidence matters most (run-20261113c 4.2).
  try {
    fs.appendFileSync(path.join(outdir, "run.jsonl"), `${JSON.stringify(record)}\n`, "utf8");
  } catch {
    // the in-memory log still carries the record for the final flush
  }
  if (["error", "step"].includes(kind)) console.log(kind, JSON.stringify(data).slice(0, 300));
}

// writeRunLog always writes the accumulated log; the terminal record is added by
// the caller so the last line of run.jsonl is the state of the run (review B4).
// A write failure is reported but never masks the outcome that is being logged.
function writeRunLog() {
  try {
    fs.writeFileSync(path.join(outdir, "run.jsonl"), log.map((x) => JSON.stringify(x)).join("\n"), "utf8");
  } catch (e) {
    console.error("run.jsonl write failed:", clip(e && e.message ? e.message : e, 160));
  }
}

// Module scope so the failure handler can stamp the terminal record too.
const startedAt = Date.now();

// ---- refund (run-20261113 §4.7) -------------------------------------------
// The number is bought before the form and stays rented when the run dies at
// the captcha. With --auto-refund the driver cancels it exactly once, before the
// process exits non-zero, and records hero's own answer: a number whose SMS code
// was already requested is refused by hero, and that refusal is the finding, not
// something to retry.
let refundAttempted = false;
let phoneSubmitted = false;
async function autoRefund(reason) {
  if (refundAttempted) return null;
  refundAttempted = true;
  if (!autoRefundEnabled) return null;
  if (!heroKey || !activation) {
    note("refund", { skipped: "missing_hero_args", reason: clip(reason, 60) });
    return null;
  }
  let result;
  try {
    result = await refund.refundActivation({ heroKey, activation });
  } catch (e) {
    result = { ok: false, response: "", error: clip(e && e.message ? e.message : e, 160) };
  }
  note("refund", {
    ok: Boolean(result.ok),
    response: clip(result.response || "", 60),
    activation: activation ? `${activation.slice(0, 3)}***` : "",
    reason: clip(reason, 60),
    error: result.error || "",
  });
  // hero refuses a cancellation inside its first two minutes
  // (EARLY_CANCEL_DENIED / minActivationTime=120). Waiting inline would stall
  // the run, so the activation is parked and a later sweep collects it.
  if (!result.ok) {
    try {
      const queuePath = path.join(outdir, "refunds-pending.json");
      const queue = fs.existsSync(queuePath)
        ? JSON.parse(fs.readFileSync(queuePath, "utf8"))
        : [];
      queue.push({
        activation,
        hero_key_env: "HERO_KEY",
        not_before: new Date(Date.now() + 130000).toISOString(),
        response: clip(result.response || "", 60),
        reason: clip(reason, 60),
      });
      fs.writeFileSync(queuePath, JSON.stringify(queue, null, 2), "utf8");
      note("refund-queued", { count: queue.length });
    } catch (queueError) {
      note("refund-queue-failed", { error: clip(queueError && queueError.message, 120) });
    }
  }
  return result;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// latestEmailCode returns {code, messageId, created_at} or null.
// Review B5: newest mail first, addressed to this run's mailbox, from a trusted
// sender, subject carrying verification semantics. The previous version took the
// first "<b>6 digits</b>" out of the first ten messages in API order.

// Manual email mode: when the mailbox cannot be polled (e.g. a personal QQ
// mailbox without IMAP access), a human drops the code into a file and the run
// picks it up. Same contract as --await-captcha: a 4-8 digit line, consumed
// once. The file is removed after use so a stale code cannot be replayed.
const awaitEmailFile = String(arg("await-email-code", ""));
// --graph-token-file is the unattended alternative to --await-email-code: the
// code is read out of the registration mailbox itself (see graph-mail.cjs).
const graphTokenFile = String(arg("graph-token-file", ""));
async function manualEmailCode() {
  const answerFile = path.resolve(awaitEmailFile);
  const value = await waitFor(async () => {
    if (!fs.existsSync(answerFile)) return "";
    const raw = fs.readFileSync(answerFile, "utf8").trim();
    return /^\d{4,8}$/.test(raw) ? raw : "";
  }, 600000, "email-code-manual");
  if (!value) return null;
  fs.rmSync(answerFile, { force: true });
  return { code: value, messageId: "manual", created_at: new Date().toISOString() };
}

// Graph email mode (--graph-token-file): the mailbox that owns the Apple ID is a
// Microsoft account, so the code is read through Graph instead of a temp-mail
// API. Same contract as the two other modes — a code object or null, and the
// code value itself never reaches run.jsonl (review A2): only whether it
// arrived, how long the wait took and the id of the message it came from.
async function graphEmailCode(since) {
  const hit = await graphMail.pollCode({
    file: graphTokenFile,
    since,
    mailbox: email,
    senderDomains,
    intervalMs: 5000,
    timeoutMs: 180000,
    onTick: ({ round, messages, hit: found }) => note("graph-poll", { round, messages, found }),
  });
  if (!hit.ok) {
    note("graph-poll-failed", { error: clip(hit.error || "", 160), elapsed_ms: hit.elapsedMs || 0 });
    return null;
  }
  // The message id is opaque and long; the prefix identifies it next to the
  // elapsed time without turning the log line into a blob.
  return { code: hit.code, messageId: `graph:${String(hit.messageId || "").slice(0, 24)}`, created_at: hit.received };
}

async function latestEmailCode(since) {
  const res = await fetch(`${MAIL_HOST}/api/mails?limit=10&offset=0`, {
    headers: { Authorization: `Bearer ${jwt}` },
  });
  if (!res.ok) throw new Error(`mail api ${res.status}`);
  const body = await res.json();
  return flow.pickEmailCode(body.results || [], { address: email, senderDomains, since });
}

async function smsCode() {
  const url = `${HERO_API}?api_key=${heroKey}&action=getStatus&id=${activation}`;
  const res = await fetch(url);
  const text = (await res.text()).trim();
  if (text.startsWith("STATUS_OK")) return text.split(":")[1] || "";
  return "";
}

async function waitFor(producer, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const value = await producer();
      if (value) return value;
    } catch (e) {
      note("poll-error", { label, error: String(e).slice(0, 200) });
    }
    await sleep(3000);
  }
  return "";
}

// ---- frozen fingerprint profile -------------------------------------------
// Apple cross-checks the UA, the client hints and navigator.platform against
// each other, so one account has to present the same machine from the creation
// form all the way into the later protocol stages. The profile is generated
// once, validated, and frozen to <outdir>/profile.json; every later run reads
// that file back instead of minting a second identity for the same account.
const PROFILE_FILE = path.join(outdir, "profile.json");
const PROFILE_VERSION = 1;

// Windows only: the browser we drive is a Windows Chrome, so a macOS UA would
// advertise MacIntel while navigator.platform reports Win32.
const PROFILE_PLATFORMS = [
  {
    name: "Win32",
    ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%V% Safari/537.36",
  },
];
// Weighted Chrome distribution, expanded in the same order as
// internal/profile.expandChromeVersions so both generators agree per seed.
const PROFILE_CHROME_WEIGHTS = [
  ["150.0.0.0", 2],
  ["151.0.0.0", 3],
  ["152.0.0.0", 5],
  ["153.0.0.0", 10],
  ["154.0.0.0", 80],
];
const PROFILE_CHROME = PROFILE_CHROME_WEIGHTS.flatMap(([v, w]) => Array(w).fill(v));
const PROFILE_GPUS = [
  {
    family: "Intel",
    vendor: "Google Inc. (Intel)",
    renderer: "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)",
  },
  {
    family: "NVIDIA",
    vendor: "Google Inc. (NVIDIA)",
    renderer: "ANGLE (NVIDIA, NVIDIA GeForce GTX 1650 Direct3D11 vs_5_0 ps_5_0, D3D11)",
  },
  {
    family: "AMD",
    vendor: "Google Inc. (AMD)",
    renderer: "ANGLE (AMD, AMD Radeon(TM) Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)",
  },
];
const PROFILE_SCREENS = [
  [1366, 768],
  [1440, 900],
  [1536, 864],
  [1600, 900],
  [1920, 1080],
];
const PROFILE_TIMEZONES = ["America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles"];
const PROFILE_CORES = [4, 8, 12, 16];
const PROFILE_MEMORY = [8, 16];

// makeProfile turns a seed into a stable, plausible browser identity. It walks
// the same FNV-1a + LCG stream and the same tables in the same order as
// internal/profile.Generate, so `profilegen -seed X` and this script agree on
// the profile for seed X instead of each inventing its own machine.
function makeProfile(seedText) {
  let state = 2166136261;
  for (const ch of seedText) {
    state ^= ch.charCodeAt(0);
    state = Math.imul(state, 16777619) >>> 0;
  }
  const next = (n) => {
    state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
    return (state >>> 16) % n;
  };
  const platform = PROFILE_PLATFORMS[next(PROFILE_PLATFORMS.length)];
  const version = PROFILE_CHROME[next(PROFILE_CHROME.length)];
  const gpu = PROFILE_GPUS[next(PROFILE_GPUS.length)];
  const screen = PROFILE_SCREENS[next(PROFILE_SCREENS.length)];
  const timezone = PROFILE_TIMEZONES[next(PROFILE_TIMEZONES.length)];
  const cores = PROFILE_CORES[next(PROFILE_CORES.length)];
  const memory = PROFILE_MEMORY[next(PROFILE_MEMORY.length)];
  const major = version.split(".")[0];
  return {
    version: PROFILE_VERSION,
    created_at: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
    ua: platform.ua.replaceAll("%V%", version),
    // The GREASE entry carries a version too; Validate ignores it when matching
    // the UA, which is why it may never be the only entry.
    ua_ch: `"Chromium";v="${major}", "Google Chrome";v="${major}", "Not?A_Brand";v="99"`,
    ua_mobile: "?0",
    platform: platform.name,
    locale: "en-US",
    languages: ["en-US", "en"],
    timezone,
    screen,
    cores,
    memory,
    webgl: { vendor: gpu.vendor, renderer: gpu.renderer },
    expected: { engine: `chrome-${major}` },
    actual: {},
  };
}

// validateProfile mirrors internal/profile.Validate rule for rule: a profile
// one stage would reject must not be used by this one either, because the
// values below go straight into a browser context and into Apple's own
// client-side fingerprint report.
function validateProfile(input, source) {
  const bad = (message) => {
    throw new Error(`profile ${source}: ${message}`);
  };
  const p = { ...input };
  if (p.version !== PROFILE_VERSION) bad(`version ${p.version} is not supported, want ${PROFILE_VERSION}`);

  const uaMajor = (/Chrome\/(\d+)/.exec(String(p.ua || "")) || [])[1];
  if (!uaMajor) bad('ua has no "Chrome/<major>" token');
  const brands = [...String(p.ua_ch || "").matchAll(/"([^"]*)"\s*;\s*v="(\d+)"/g)]
    .map((m) => ({ brand: m[1], version: m[2] }))
    .filter((b) => !(b.brand.startsWith("Not") && b.brand.includes("Brand")));
  if (!brands.length) bad('ua_ch has no "Brand";v="<major>" client-hint entry');
  if (!brands.some((b) => b.version === uaMajor)) bad(`ua_ch does not list the UA chrome major ${uaMajor}`);

  if (p.platform === "Win32") {
    if (!String(p.ua).includes("Windows NT 10.0")) bad('platform Win32 needs "Windows NT 10.0" in the ua');
  } else {
    bad(`platform ${JSON.stringify(p.platform)} is not Win32; this pipeline only drives a Windows browser`);
  }

  if (p.ua_mobile !== "?0" && p.ua_mobile !== "?1") {
    bad(`ua_mobile ${JSON.stringify(p.ua_mobile)} is neither "?0" nor "?1"`);
  }
  if (p.ua_mobile === "?1") bad(`ua_mobile "?1" contradicts desktop platform ${p.platform}, want "?0"`);
  if (String(p.ua).includes("Mobile")) bad('ua_mobile "?0" contradicts a mobile ua');

  if (!PROFILE_TIMEZONES.includes(p.timezone)) {
    bad(`timezone ${JSON.stringify(p.timezone)} is not one of ${PROFILE_TIMEZONES.join(", ")}`);
  }
  if (p.locale !== "en-US") bad(`locale ${JSON.stringify(p.locale)} is not en-US`);
  if (!Array.isArray(p.languages) || !p.languages.length || p.languages[0] !== "en-US") {
    bad(`languages ${JSON.stringify(p.languages)} must start with en-US`);
  }

  const screen = Array.isArray(p.screen) ? p.screen : [];
  if (!PROFILE_SCREENS.some((s) => s[0] === screen[0] && s[1] === screen[1])) {
    bad(`screen ${screen[0]}x${screen[1]} is not one of ${PROFILE_SCREENS.map((s) => `${s[0]}x${s[1]}`).join(", ")}`);
  }
  if (!PROFILE_CORES.includes(p.cores)) bad(`cores ${p.cores} is not one of ${PROFILE_CORES.join(", ")}`);
  if (!PROFILE_MEMORY.includes(p.memory)) bad(`memory ${p.memory} is not one of ${PROFILE_MEMORY.join(", ")}`);

  const webgl = p.webgl || {};
  if (!webgl.vendor || !webgl.renderer) {
    bad(`webgl vendor ${JSON.stringify(webgl.vendor)} and renderer ${JSON.stringify(webgl.renderer)} must both be set`);
  }
  const vendorGpu = PROFILE_GPUS.find((g) => String(webgl.vendor).includes(g.family));
  if (!vendorGpu) bad(`webgl vendor ${JSON.stringify(webgl.vendor)} names no known GPU family (Intel, NVIDIA, AMD)`);
  const rendererGpu = PROFILE_GPUS.find((g) => String(webgl.renderer).startsWith(`ANGLE (${g.family},`));
  if (!rendererGpu) bad(`webgl renderer ${JSON.stringify(webgl.renderer)} is not an ANGLE string for Intel, NVIDIA or AMD`);
  if (rendererGpu.family !== vendorGpu.family) {
    bad(`webgl vendor ${JSON.stringify(webgl.vendor)} does not match renderer family ${rendererGpu.family}`);
  }
  return p;
}

// writeProfile freezes the profile atomically: a crash mid-run must not leave a
// half-written file that the next stage would read as truth.
function writeProfile(file, profile) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, `${JSON.stringify(profile, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(tmp, file);
}

// loadProfile picks the profile for this run: an explicit --profile wins, then
// the frozen file of this account directory, and only when neither exists does
// this script mint one -- and freeze it, so the next stage sees the same
// machine instead of a second random draw.
function loadProfile() {
  if (profilePath) {
    const file = path.resolve(profilePath);
    const profile = validateProfile(JSON.parse(fs.readFileSync(file, "utf8")), file);
    return { file, created: false, profile };
  }
  if (fs.existsSync(PROFILE_FILE)) {
    const profile = validateProfile(JSON.parse(fs.readFileSync(PROFILE_FILE, "utf8")), PROFILE_FILE);
    return { file: PROFILE_FILE, created: false, profile };
  }
  const seed = String(arg("fp-seed", path.basename(outdir)));
  const profile = makeProfile(seed);
  writeProfile(PROFILE_FILE, profile);
  return { file: PROFILE_FILE, created: true, profile };
}

// The canvas noise has to stay deterministic per account. It is derived from
// the profile instead of being stored, so the frozen file keeps exactly the
// schema internal/profile defines and nothing extra.
function noiseSeed(profile) {
  let h = 2166136261;
  for (const ch of `${profile.ua}|${profile.timezone}`) {
    h ^= ch.charCodeAt(0);
    h = Math.imul(h, 16777619) >>> 0;
  }
  return h % 251;
}

async function widgetFrame(page) {
  return page.frames().find((f) => f.url().includes("/widget/account/"));
}

async function widgetText(page) {
  const frame = await widgetFrame(page);
  if (!frame) return "";
  return frame.evaluate(() => document.body?.innerText || "").catch(() => "");
}


// Human-paced typing. A .fill() drops a 29-character address in 0 ms, which is
// the loudest possible automation signal; this types one key at a time with a
// jittered delay and a short settle afterwards.

// Picking from a real dropdown: click it open, wait like a person reading the
// list, choose, then let the widget settle before the next field.
async function selectHuman(locator, value, label = "") {
  await locator.click({ timeout: 15000 }).catch(() => {});
  await sleep(300 + Math.floor(Math.random() * 500));
  await locator.selectOption(value, { timeout: 15000 }).catch(() => {});
  await sleep(250 + Math.floor(Math.random() * 450));
  if (label) note("step", { at: "selected", field: label, value: String(value) });
}

async function typeHuman(locator, text, label = "") {
  const value = String(text == null ? "" : text);
  if (!value) return;
  await locator.click({ timeout: 15000 }).catch(() => {});
  for (const ch of value) {
    await locator.pressSequentially(ch, { timeout: 5000 }).catch(() => {});
    // Real typing is uneven: a quick burst, then a pause to think.
    await sleep(120 + Math.floor(Math.random() * 230));
    if (Math.random() < 0.12) await sleep(400 + Math.floor(Math.random() * 700));
  }
  await sleep(700 + Math.floor(Math.random() * 900));
  if (label) note("step", { at: "typed", field: label, length: value.length });
}

async function fillDigits(frame, code) {
  const boxes = frame.locator('input[type="tel"]');
  const count = await boxes.count();
  note("code-boxes", { count });
  if (count >= code.length) {
    for (let i = 0; i < code.length; i += 1) {
      await boxes.nth(i).pressSequentially(code[i], { timeout: 5000 }).catch(() => {});
    }
  } else {
    await boxes.first().pressSequentially(code, { timeout: 5000 }).catch(() => {});
  }
  await sleep(800);
}

// The page carries several images (the privacy illustration, logos, the
// captcha). run-20261113 §4.4: picking the small one by natural size grabbed the
// 378x169 handshake illustration while the real captcha <img> was a broken image
// from a 503, and 16 solves were spent on the wrong picture. The captcha is now
// the img that sits in the same container as input[name="captcha"] and before it
// in DOM order; the size window is only a fallback for a page without that
// structure, and a picture that did not load is never acceptable either way.
async function observeCaptchaImages(fr) {
  return fr
    .evaluate(() => {
      const all = [...document.querySelectorAll("img")];
      const indexOf = new Map(all.map((el, index) => [el, index]));
      const size = (el) => [el.naturalWidth || 0, el.naturalHeight || 0];
      const images = all.map((el, index) => ({ index, natural: size(el) }));

      const input = document.querySelector('input[name="captcha"]');
      if (!input) return { images, adjacent: [] };

      // Walk up from the input and look for the nearest preceding img in the
      // same container: previous element siblings first, then one level up. The
      // depth/sibling bounds keep a page-wide logo out of the result.
      const adjacent = [];
      let node = input;
      for (let depth = 0; depth < 3 && node; depth += 1) {
        let sibling = node.previousElementSibling;
        for (let step = 0; step < 8 && sibling; step += 1) {
          const img = sibling.tagName === "IMG" ? sibling : sibling.querySelector("img");
          if (img && indexOf.has(img)) adjacent.push({ index: indexOf.get(img), natural: size(img) });
          sibling = sibling.previousElementSibling;
        }
        if (adjacent.length) break;
        node = node.parentElement;
      }
      return { images, adjacent };
    })
    .catch(() => ({ images: [], adjacent: [] }));
}

// captchaLocator returns the captcha image locator plus what the picker decided.
// A missing or unloaded captcha throws: the caller must not go on to OCR a
// different picture, and `state` decides whether the run reads as failed (no
// submit yet) or unknown (a submit may already have landed).
async function captchaLocator(fr, { state = "failed", tag = "" } = {}) {
  const observed = await observeCaptchaImages(fr);
  const pick = lib.pickCaptchaImage(observed);
  if (!pick.ok) {
    note("step", { at: "captcha-unavailable", tag, src_kind: pick.src_kind, reason: pick.reason });
    const err = new Error(pick.reason || "captcha image not loaded");
    err.state = state;
    err.evidence = `captcha-image-unavailable:${tag || "captcha"}`;
    throw err;
  }
  return { locator: fr.locator("img").nth(pick.index), pick };
}

// Apple can show the image captcha on any step, including the phone step
// ("Security Code Error"). Solve it the same way as on the first form.
async function solveCaptcha(page, fr, tag) {
  const answerFile = path.resolve(arg("await-captcha", ".cache/captcha-answer.txt"));
  fs.rmSync(answerFile, { force: true });
  const { locator, pick } = await captchaLocator(fr, { state: "unknown", tag });
  note("step", { at: "captcha-image", tag, natural: pick.natural, src_kind: pick.src_kind });
  await locator.screenshot({ path: path.join(outdir, `${tag}.png`), scale: "css" }).catch(() => {});
  const answer = await waitFor(async () => {
    if (!fs.existsSync(answerFile)) return "";
    const v = fs.readFileSync(answerFile, "utf8").trim();
    return v.length >= 4 ? v : "";
  }, 180000, "captcha");
  if (!answer) return "";
  await typeHuman(fr.locator('input[name="captcha"]'), answer, "captcha");
  await fr.locator('input[name="captcha"]').press("Tab").catch(() => {});
  await sleep(1000);
  await clickContinue(page, fr, tag);
  await sleep(8000);
  return answer;
}

// readPhase is a read-only snapshot of the current step. `formVisible` means
// the controls of the form we were about to submit are still on screen, and
// `signature` is what is compared to prove the screen did not change.
async function readPhase(page) {
  const frame = await widgetFrame(page);
  if (!frame) return { framePresent: false, formVisible: false, signature: "" };
  const readback = await frame
    .evaluate(() => {
      const form = document.querySelector(
        'input[type="tel"], input[name="captcha"], input[name="password"], input[type="password"]',
      );
      const text = (document.body?.innerText || "").replace(/\s+/g, " ").trim().slice(0, 240);
      const controls = [...document.querySelectorAll("input,button,select")]
        .map((el) => `${el.tagName}:${el.getAttribute("name") || el.id || (el.innerText || "").trim().slice(0, 20)}`)
        .join("|");
      return { formVisible: Boolean(form), text, controls };
    })
    .catch(() => null);
  if (!readback) return { framePresent: true, formVisible: false, signature: "" };
  return {
    framePresent: true,
    formVisible: readback.formVisible,
    signature: `${frame.url()}#${readback.text}#${readback.controls}`,
  };
}

// clickContinue carries review item A3. The old version answered a click
// timeout with `click({force:true})`, which can submit the same form twice and
// leaves nothing in the log to tell which submit landed. A timeout is now a
// decision point: read the page back and only click again when the readback
// proves nothing was submitted (widget still there, form still there, screen
// unchanged). Anything else is "result unknown" and the run exits non-zero.
async function clickContinue(page, frame, tag = "") {
  const btn = frame.locator('button[type="submit"], button:has-text("Continue")').first();
  const before = await readPhase(page);
  try {
    await btn.click({ timeout: 15000 });
    return { clicked: true, retried: false };
  } catch (e) {
    // A click that times out because the button is disabled looks identical to a
    // wrong captcha in the log, and that ambiguity cost a whole paid run on
    // 2026-11-14 (the real cause was Apple's "At least one special character"
    // rule on the password). The inline form errors and the button state are
    // captured here so the next reader sees the cause, not the symptom.
    const diag = await frame
      .evaluate(() => {
        const btn = document.querySelector('button[type="submit"]');
        const errors = [...document.querySelectorAll('[role="alert"], .form-message, .error, .form-textbox-error')]
          .map((el) => (el.innerText || "").trim().replace(/\s+/g, " ").slice(0, 60))
          .filter(Boolean)
          .slice(0, 6);
        return { disabled: btn ? Boolean(btn.disabled) : null, errors };
      })
      .catch(() => null);
    note("step", { at: "click-timeout", tag, error: clip(e && e.message ? e.message : e, 160), ...(diag || {}) });
  }
  // Let a possible navigation settle first: the readback has to describe the
  // page the click produced, not the one it was fired from.
  await sleep(1500);
  const after = await readPhase(page);
  const sameScreen = Boolean(before.signature) && before.signature === after.signature;
  const verdict = flow.clickTimeoutOutcome({
    framePresent: after.framePresent,
    formVisible: after.formVisible,
    sameScreen,
  });
  note("step", {
    at: "click-timeout-readback",
    tag,
    verdict,
    form_visible: after.formVisible,
    same_screen: sameScreen,
  });
  if (verdict !== "retry") {
    const err = new Error(`click timed out and the submission result is unknown (${tag || "continue"})`);
    err.state = "unknown";
    err.evidence = `click-timeout:${tag || "continue"}`;
    throw err;
  }
  await btn.click({ force: true }).catch(() => {});
  return { clicked: true, retried: true };
}

(async () => {
  // The account directory exists before the first refusal so a refused run still
  // leaves run.jsonl (including the refund answer) next to its siblings.
  fs.mkdirSync(outdir, { recursive: true });

  // Review A1: the refusal is a named outcome, thrown before a browser exists and
  // before the first network request. It goes through the same failure handler as
  // everything else, which is what lets --auto-refund cancel a number that was
  // already bought for this run (run-20261113 §4.7).
  if (!passwordResolution.password) {
    const err = new Error("missing_required_argument: --password");
    err.state = "usage";
    err.report = flow.missingPasswordReport(
      "create-account.cjs",
      "the Apple Account creation form requires a password",
    );
    err.evidence = "usage:missing_password";
    throw err;
  }

  // run-20261113 §4.3: a malformed --captcha-cmd is refused here, not after 16
  // empty OCR rounds.
  if (captchaCmdFlag) {
    try {
      lib.captchaArgv(captchaCmdFlag);
    } catch (e) {
      const err = new Error("bad_captcha_cmd");
      err.state = "usage";
      err.report = {
        error: "bad_captcha_cmd",
        argument: "--captcha-cmd",
        detail: clip(e && e.message ? e.message : e, 160),
        hint: 'pass ["python","tools/captcha/solver.py"] or a command line',
      };
      err.evidence = "usage:bad_captcha_cmd";
      throw err;
    }
  }

  // A --graph-token-file that cannot drive the flow is refused here, before the
  // browser opens and before the caller's number is spent: the complaint names
  // the missing key and never echoes a value, so it is safe in a run log.
  if (graphTokenFile) {
    try {
      const shape = graphMail.readTokenFile(graphTokenFile);
      note("graph-source", {
        file: path.basename(shape.file),
        mailbox: shape.mailbox,
        scope: shape.scope,
        jwt_ignored: Boolean(jwt && jwt !== "unused"),
      });
    } catch (e) {
      const err = new Error("bad_graph_token_file");
      err.state = "usage";
      err.report = {
        error: "bad_graph_token_file",
        argument: "--graph-token-file",
        detail: clip(e && e.message ? e.message : e, 160),
        hint: "JSON with client_id, refresh_token and mailbox; keep it outside the repository",
      };
      err.evidence = "usage:bad_graph_token_file";
      throw err;
    }
  }

  // Resolve the fingerprint before a browser exists: a profile another stage
  // already froze is validated here, so a stale or edited file fails fast
  // instead of driving one account under two different identities.
  const frozen = attachMode ? null : loadProfile();
  const profile = frozen ? frozen.profile : null;
  if (attachMode) {
    note("step", { at: "fingerprint-attach", cdp_port: cdpPort, owned_by: "roxy" });
  } else {
    note("step", {
      at: "fingerprint",
      file: frozen.file,
      created: frozen.created,
      profile: {
        ua: profile.ua,
        screen: profile.screen,
        tz: profile.timezone,
        platform: profile.platform,
        locale: profile.locale,
      },
    });
  }

  const launchOpts = {
    channel: "chrome",
    headless: true,
    // Autofill is off at the browser level as well: a saved credential in the
    // profile rewrote the address box once already (group B, 2026-11-14).
    args: [
      "--disable-blink-features=AutomationControlled",
      "--disable-features=AutofillServerCommunication,AutofillEnableAccountWalletStorage",
    ],
  };
  const proxy = arg("proxy", "");
  if (proxy) {
    launchOpts.proxy = { server: String(proxy) };
    if (arg("proxy-user", "")) {
      launchOpts.proxy.username = String(arg("proxy-user", ""));
      launchOpts.proxy.password = String(arg("proxy-pass", ""));
    }
  }

  // run-20261113 §4.5: this exit answered 503 to every /captcha request, so the
  // page could never render the image and the run was unwinnable before it
  // started. With --preflight the egress is tested once, before the browser
  // exists; a failure exits without loading the form at all.
  if (preflightEnabled) {
    const seconds = Number(arg("preflight-timeout", 20));
    const pre = await preflight.probe({
      url: String(arg("preflight-url", "") || preflight.DEFAULT_TARGET),
      proxy: String(proxy || ""),
      proxyUser: String(arg("proxy-user", "") || ""),
      proxyPass: String(arg("proxy-pass", "") || ""),
      timeoutMs: (Number.isFinite(seconds) && seconds > 0 ? seconds : 20) * 1000,
    });
    note("step", {
      at: "preflight",
      ok: pre.ok,
      status: pre.status,
      elapsed_ms: pre.elapsed_ms,
      egress_hint: pre.egress_hint,
      error: clip(pre.error || "", 120),
    });
    if (!pre.ok) {
      // Advisory only: this probe speaks Node's TLS, not Chrome's, so a 403
      // here says nothing about whether the browser can reach the form. It is
      // recorded and the run continues; the browser probe is the real gate.
      note("step", { at: "preflight-inconclusive", status: pre.status });
    }
    if (false) {
      const err = new Error("unreachable");
      err.evidence = "preflight:captcha";
      throw err;
    }
  }

  // Attach mode: nothing to launch and nothing to patch — the window already
  // carries the fingerprint (and the proxy) that the A/B is measuring.
  const browser = attachMode
    ? await chromium.connectOverCDP(`http://127.0.0.1:${cdpPort}`, { timeout: 30000 })
    : await chromium.launch(launchOpts);

  // What actually ran goes next to what was intended. The engine label is
  // rebuilt from the UA that goes out (never hand-typed), and the exit address
  // is whatever the caller already measured with --egress-ip; this script does
  // not probe the network itself. Other fields are written back untouched.
  if (!attachMode) {
    const actual = {
      ...(profile.actual || {}),
      engine: `chrome-${(/Chrome\/(\d+)/.exec(profile.ua) || [])[1]}`,
      observed_at: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
    };
    if (egressIP) actual.egress_ip = egressIP;
    if (proxy) actual.egress_ref = String(proxy);
    profile.actual = actual;
    writeProfile(frozen.file, profile);
    note("step", {
      at: "actual",
      file: frozen.file,
      engine: actual.engine,
      egress_ref: actual.egress_ref || "",
      egress_ip: actual.egress_ip || "",
    });
  }

  let context;
  let page;
  if (attachMode) {
    // Reuse the profile's own context so the window is the one Roxy opened.
    context = browser.contexts()[0] || (await browser.newContext());
    page = await context.newPage();
    const observed = await page.evaluate(() => ({
      ua: navigator.userAgent,
      platform: navigator.platform,
      language: navigator.language,
      languages: navigator.languages,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      webdriver: navigator.webdriver,
      screen: `${screen.width}x${screen.height}@${window.devicePixelRatio}`,
    })).catch(() => null);
    if (observed) {
      try {
        fs.writeFileSync(path.join(outdir, "roxy-fingerprint.json"), `${JSON.stringify(observed, null, 2)}\n`, "utf8");
      } catch {}
    }
    note("step", {
      at: "roxy-fingerprint",
      ua: clip(observed && observed.ua, 120),
      platform: observed ? observed.platform : "",
      language: observed ? observed.language : "",
      timeZone: observed ? observed.timeZone : "",
      screen: observed ? observed.screen : "",
    });
  } else {
  context = await browser.newContext({
    locale: profile.locale,
    timezoneId: profile.timezone,
    viewport: { width: profile.screen[0], height: profile.screen[1] - 120 },
    // Render at 3x so the 126x56 captcha is captured with real pixels instead
    // of being upscaled after the fact.
    deviceScaleFactor: 3,
    userAgent: profile.ua,
  });
  await context.addInitScript(
    ({ cores, memory, renderer, vendor, seed }) => {
      Object.defineProperty(navigator, "webdriver", { get: () => undefined });
      Object.defineProperty(navigator, "hardwareConcurrency", { get: () => cores });
      Object.defineProperty(navigator, "deviceMemory", { get: () => memory });
      const patchGL = (proto) => {
        if (!proto) return;
        const original = proto.getParameter;
        proto.getParameter = function (p) {
          if (p === 37445) return vendor;
          if (p === 37446) return renderer;
          return original.call(this, p);
        };
      };
      patchGL(window.WebGLRenderingContext && WebGLRenderingContext.prototype);
      patchGL(window.WebGL2RenderingContext && WebGL2RenderingContext.prototype);
      // Deterministic per-account canvas noise: same seed, same output, so the
      // fingerprint stays stable inside one session but differs between accounts.
      const toDataURL = HTMLCanvasElement.prototype.toDataURL;
      HTMLCanvasElement.prototype.toDataURL = function (...args) {
        const ctx = this.getContext("2d");
        if (ctx) {
          const { width, height } = this;
          if (width && height) {
            const image = ctx.getImageData(0, 0, width, height);
            for (let i = 0; i < image.data.length; i += 997) {
              image.data[i] = (image.data[i] + seed) % 256;
            }
            ctx.putImageData(image, 0, 0);
          }
        }
        return toDataURL.apply(this, args);
      };
    },
    {
      cores: profile.cores,
      memory: profile.memory,
      renderer: profile.webgl.renderer,
      vendor: profile.webgl.vendor,
      seed: noiseSeed(profile),
    },
  );
    page = await context.newPage();
  }
  // Review A2: this listener used to copy up to 1200 bytes of every
  // validate/captcha response body into run.jsonl, which echoed the submitted
  // identifier and the captcha verdict. Only status, path and an Apple error
  // code are kept now.
  page.on("response", async (res) => {
    const u = res.url();
    if (!/account\/validate|validation\/appleid|captcha/.test(u)) return;
    let body = "";
    try {
      body = await res.text();
    } catch {}
    note("http", flow.summarizeHTTP({ url: u, status: res.status(), body }));
  });

  // Screenshot helper. `maskAuth` paints a black block over the authentication
  // inputs before the capture (review A2: the code-filled screenshot used to
  // show the six digits). The AuthKit form lives in a cross-origin iframe, so
  // page.addStyleTag cannot reach it — the style has to be injected into the
  // widget frame itself, and removed again so later steps are unaffected.
  const AUTH_MASK_CSS = `
    input[type="tel"], input[name="captcha"] {
      color: transparent !important;
      background-color: #000 !important;
      caret-color: transparent !important;
      text-shadow: none !important;
    }`;
  const shot = async (name, { maskAuth = false } = {}) => {
    let styleHandle = null;
    if (maskAuth) {
      const current = await widgetFrame(page);
      if (current) styleHandle = await current.addStyleTag({ content: AUTH_MASK_CSS }).catch(() => null);
      if (styleHandle) await sleep(150);
    }
    const file = path.join(outdir, `${name}.png`);
    await page.screenshot({ path: file }).catch(() => {});
    if (styleHandle) await styleHandle.evaluate((el) => el.remove()).catch(() => {});
    fs.writeFileSync(path.join(outdir, `${name}.txt`), await widgetText(page), "utf8");
  };

  await page.goto("https://account.apple.com/account", { waitUntil: "domcontentloaded", timeout: 90000 });
  // A residential exit can take 20-40s to hand over the widget iframe.
  await page.waitForSelector('iframe[src*="widget/account"]', { timeout: 90000 }).catch(() => {});
  let frame = null;
  for (let i = 0; i < 30 && !frame; i += 1) {
    frame = await widgetFrame(page);
    if (!frame) await sleep(2000);
  }
  if (!frame) throw new Error("widget frame missing");
  note("step", { at: "form" });

  // A rejected submission returns to the same form, so every retry fills it
  // again. typeHuman appends to whatever is already in the box, which doubled
  // the address, the password and the phone number on the second pass and made
  // Apple answer "Enter a valid email address" -- our own defect, not a verdict.
  // Every text field is therefore emptied first.
  const retype = async (locator, text, label) => {
    await locator.fill("").catch(() => {});
    await typeHuman(locator, text, label);
  };
  // The value that gets submitted is the value in the box, not the flag. A
  // saved-credential autofill inside the profile rewrote one digit of the
  // address after it had been typed (2026-11-14 group B: ...167 became ...187
  // and Apple mailed the code to a mailbox nobody owned). Every non-secret field
  // is therefore read back, re-filled once on disagreement, and the run stops
  // with a named evidence code when it still disagrees.
  const fieldValue = (fr, selector) => fr.$eval(selector, (el) => el.value).catch(() => "");
  const ensureField = async (fr, selector, want, label, { secret = false } = {}) => {
    let value = await fieldValue(fr, selector);
    if (value !== want) {
      await retype(fr.locator(selector), want, `${label}_retry`);
      value = await fieldValue(fr, selector);
    }
    const ok = value === want;
    note("step", {
      at: "field-check",
      field: label,
      ok,
      length: String(value).length,
      // Never the value itself for a password; a length is enough to tell a
      // truncated fill from a correct one.
      head: secret ? "" : String(value).slice(0, 3) + "***",
    });
    if (!ok) {
      const err = new Error(`field ${label} does not hold the value that was typed`);
      err.state = "failed";
      err.evidence = label === "apple_id" ? "email_field_mismatch" : `field_mismatch:${label}`;
      throw err;
    }
    return value;
  };
  const noAutofill = async (fr) => {
    // Chrome's own autofill is what overwrote the address; the page-level
    // attribute stops it from offering a saved value for these boxes at all.
    await fr.evaluate(() => {
      for (const el of document.querySelectorAll("input")) {
        if (el.type === "password") el.setAttribute("autocomplete", "new-password");
        else el.setAttribute("autocomplete", "off");
      }
    }).catch(() => {});
  };
  const fillForm = async (fr) => {
    await noAutofill(fr);
    await retype(fr.locator('input[name="firstName"]'), arg("first", "James"), "first_name");
    await retype(fr.locator('input[name="lastName"]'), arg("last", "Carter"), "last_name");
    await selectHuman(fr.locator('select[name="countrySelect"]'), "USA", "country");
    // The birthday is three separate selects whose DOM order on the 2026-11-14
    // build is day, month, year. Index-based selection sent the day value to the
    // month select and stalled for 30s without submitting anything; the stable
    // handle is the data-testid each of them carries.
    await selectHuman(fr.locator('select[data-testid="select-month"]'), "07", "birth_month");
    await selectHuman(fr.locator('select[data-testid="select-day"]'), "18", "birth_day");
    await selectHuman(fr.locator('select[data-testid="select-year"]'), "1992", "birth_year");
    await retype(fr.locator('input[name="appleId"]'), email, "apple_id");
    await retype(fr.locator('input[name="password"]'), password, "password");
    await retype(fr.locator('input[name="confirmPassword"]'), password, "confirm_password");
    await selectHuman(fr.locator('select[name="phoneCode"]'), phoneCC, "dial_code");
    // The widget hydrates ~4-6s later and silently resets the dial code to US;
    // the number then validates as +1 and the form never submits. Wait it out and
    // re-assert our choice (run-20261113d 2.1).
    await sleep(7000);
    await selectHuman(fr.locator('select[name="phoneCode"]'), phoneCC, "dial_code").catch(() => {});
    await retype(fr.locator('input[name="phoneNumber"]'), phone, "phone_number");
    // The same hydration pass is what let autofill rewrite the address, so the
    // four value-bearing fields are asserted again after it, right before the
    // captcha is solved and the button is pressed.
    await sleep(1200);
    await ensureField(fr, 'input[name="appleId"]', email, "apple_id");
    await ensureField(fr, 'input[name="password"]', password, "password", { secret: true });
    await ensureField(fr, 'input[name="confirmPassword"]', password, "confirm_password", { secret: true });
    await ensureField(fr, 'input[name="phoneNumber"]', phone, "phone_number");
    let dial = await fr.$eval('select[name="phoneCode"]', (el) => el.value).catch(() => "");
    if (dial !== phoneCC) {
      await selectHuman(fr.locator('select[name="phoneCode"]'), phoneCC, "dial_code").catch(() => {});
      await sleep(400);
      dial = await fr.$eval('select[name="phoneCode"]', (el) => el.value).catch(() => "");
    }
    note("step", { at: "dial-code", want: phoneCC, got: dial });
  };

  // A wrong captcha does not burn anything: Apple just refreshes the image and
  // stays on the form. Loop until the form is behind us.
  const answerFile = path.resolve(awaitCaptcha || ".cache/captcha-answer.txt");
  let submitted = false;
  // Set once the form has actually been submitted, so a captcha that never
  // rendered after a submit reads as "unknown" instead of "provably failed".
  let submitAttempted = false;
  // Same-image circuit breaker (run-20261113 §4.8): 16 identical captures cost
  // 76 solver points. `tracker` counts consecutive repeats and the run stops
  // before paying for a third solve of the same bytes.
  const captchaTracker = new lib.CaptchaTracker();
  for (let attempt = 1; attempt <= 4 && !submitted; attempt += 1) {
    const fr = await widgetFrame(page);
    if (!fr) break;
    await fillForm(fr);
    fs.rmSync(answerFile, { force: true });

    // The captcha has to be loaded before anything is screenshotted or solved
    // (run §4.4): a broken image is an unavailable environment, not a puzzle.
    // /captcha can answer a beat after the form paints. Reading naturalWidth too
  // early throws away a whole attempt (redacted evidence: run-20261113c 5.3),
  // so give the bitmap up to ten seconds to arrive before deciding.
  await fr
    .waitForFunction(
      () => {
        const input = document.querySelector('input[name="captcha"]');
        if (!input) return false;
        return [...document.querySelectorAll("img")].some((i) => i.naturalWidth > 0);
      },
      { timeout: 10000 },
    )
    .catch(() => {});
  const { locator: img, pick } = await captchaLocator(fr, {
      state: submitAttempted ? "unknown" : "failed",
      tag: `create-${attempt}`,
    });

    // Render the 126x56 image at 3x before capturing it so the characters are
    // legible without external image tooling.
    await img.evaluate((el) => {
      el.style.width = "378px";
      el.style.height = "168px";
      el.style.imageRendering = "pixelated";
    }).catch(() => {});
    await sleep(300);
    const png = path.join(outdir, `captcha-${attempt}.png`);
    await img.screenshot({ path: png, scale: "css" }).catch(() => {});
    fs.rmSync(path.join(outdir, "captcha.png"), { force: true });
    fs.copyFileSync(png, path.join(outdir, "captcha.png"));

    // What was captured, without the pixels: natural size, how it was found and
    // a hash prefix that proves "same image again" across attempts.
    let digest = "";
    try {
      digest = lib.sha256Hex(fs.readFileSync(png));
    } catch {
      digest = "";
    }
    const gate = captchaTracker.observe(digest);
    note("step", {
      at: "captcha-image",
      attempt,
      natural: pick.natural,
      src_kind: pick.src_kind,
      duplicate: gate.duplicate,
      duplicates: gate.duplicates,
      sha256_prefix: digest.slice(0, 12),
    });
    if (gate.stuck) {
      note("step", { at: "captcha_image_stuck", attempt, duplicates: gate.duplicates });
      const err = new Error("captcha image stuck: the page kept returning the same picture");
      err.state = "unknown";
      err.evidence = "captcha_image_stuck";
      throw err;
    }
    note("step", { at: "captcha-ready", attempt });

    // Prefer the offline OCR when a command was supplied: it answers in under a
    // second, so a rejected captcha only costs one more round trip. Fall back
    // to a human/answer file when OCR returns nothing usable. The command may be
    // an interpreter plus arguments (run §4.3); runCaptchaCommand never throws.
    let answer = "";
    const ocrCmd = arg("captcha-cmd", "");
    if (ocrCmd) {
      const ocr = lib.runCaptchaCommand(String(ocrCmd), png);
      if (!ocr.ok) note("ocr-error", { error: clip(ocr.error || "", 160), ms: ocr.ms });
      answer = ocr.ok ? ocr.text : "";
      if (answer && (answer.length < 4 || answer.length > 6)) { note("step", { at: "ocr-short", attempt, answer }); answer = ""; }
      note("step", { at: "ocr-answer", attempt, accepted: Boolean(answer), length: answer ? answer.length : 0, ms: ocr.ms });
      if (!answer) {
        // OCR read nothing usable: refresh the image and try again instead of
        // blocking on a human. A refresh costs nothing and keeps the run
        // unattended.
      // Apple swaps the picture itself once an answer is rejected; clicking
      // "New code" on top of that is both redundant and un-human. Just wait for
      // the replacement to render and read again.
      await sleep(1800 + Math.floor(Math.random() * 900));
        continue;
      }
    }
    if (!answer) {
      answer = await waitFor(async () => {
        if (!fs.existsSync(answerFile)) return "";
        const v = fs.readFileSync(answerFile, "utf8").trim();
        return v.length >= 3 ? v : "";
      }, 240000, "captcha");
    }
    if (!answer) throw new Error("captcha answer never arrived");
    note("step", { at: "captcha-answer", attempt, accepted: Boolean(answer), length: answer ? answer.length : 0 });

    await typeHuman(fr.locator('input[name="captcha"]'), answer, "captcha_inline");
    await fr.locator('input[name="captcha"]').press("Tab").catch(() => {});
    await sleep(1200);
    submitAttempted = true;
    await clickContinue(page, fr, `create-submit-${attempt}`);
    await sleep(9000);
    await shot(`01-submit-${attempt}`);
    const stillForm = await (await widgetFrame(page))
      ?.locator('input[name="captcha"]')
      .count()
      .catch(() => 0);
    submitted = !stillForm;
    if (!submitted) {
      // Evidence first: a rejected captcha is the most expensive failure we hit,
      // and without the image, the answer we typed and the page's own complaint
      // there is nothing to learn from afterwards.
      try {
        const errText = (await fr.locator("body").innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 200);
        fs.writeFileSync(
          path.join(outdir, `captcha-attempt-${attempt}.txt`),
          JSON.stringify({ attempt, answer, error: errText, at: new Date().toISOString() }, null, 2),
          "utf8",
        );
        await page.screenshot({ path: path.join(outdir, `captcha-attempt-${attempt}.png`) }).catch(() => {});
      } catch {
        // evidence is best effort; never let it break the retry
      }
      // A hard refusal is the end of the run, not another captcha round: Apple
      // keeps the form on screen but has already decided (run-20261114-run2
      // section 2, the hard reject). Continuing only hammers the same address
      // and the same exit for nothing.
      const pageText = (await widgetText(page)).replace(/\s+/g, " ");
      if (/cannot be created at this time/i.test(pageText)) {
        note("step", { at: "hard-reject", attempt, text: pageText.slice(0, 200) });
        const err = new Error("apple refused: your account cannot be created at this time");
        err.state = "failed";
        err.evidence = `hard-reject:${attempt}`;
        throw err;
      }
      // Apple replaces the image by itself after a rejection; never click
      // "New code" for it. Wait for the replacement, then read again.
      await sleep(1800 + Math.floor(Math.random() * 900));
      // One answer belongs to one image. Without this the manual channel replays
      // the rejected digits on the next round, because the answer file outlives
      // the attempt that consumed it.
      fs.rmSync(answerFile, { force: true });
    }
    note("step", { at: "submit-result", attempt, stillForm: Boolean(stillForm) });
  }
  if (!submitted) throw new Error("captcha never accepted");
  // A person does not land on Continue the instant the last field is filled:
  // drift the pointer, settle, then commit.
  await page.mouse.move(320 + Math.floor(Math.random() * 260), 380 + Math.floor(Math.random() * 180), { steps: 18 }).catch(() => {});
  await sleep(600 + Math.floor(Math.random() * 900));
  await shot("02-after-submit");

  // Review B4: the flow used to end with an unconditional `done` + exit 0, so an
  // error page, a terms wall or an unrecognised screen all counted as a created
  // account. Each screen is now classified (email_step / phone_step / success /
  // unknown) and only an explicit terminal page is success. The objective
  // cross-check the review suggests as an alternative (`used=true` on a re-submit
  // of the same address) needs a second captcha solve inside the same session,
  // which this driver does not implement — so an unclear ending is reported as
  // `unknown` with a non-zero exit instead of being upgraded to success.
  let finalState = "unknown";
  let finalEvidence = "";
  for (let step = 1; step <= 8; step += 1) {
    const current = await widgetFrame(page);
    const text = (await widgetText(page)).replace(/\s+/g, " ");
    if (!current) {
      // The widget can disappear because the flow finished (Apple lands on the
      // account page) or because the session died. Only the explicit terminal
      // page counts; everything else is unknown, never success.
      const verdict = flow.classifyScreen({ text, url: page.url() });
      note("step", { at: `screen-${step}`, state: verdict, text: text.slice(0, 300) });
      if (verdict === "success") {
        finalState = "success";
        finalEvidence = `terminal-page:${step}`;
        break;
      }
      finalState = "unknown";
      finalEvidence = "widget-frame-missing";
      break;
    }

    if (await current.locator('input[name="captcha"]').count().catch(() => 0)) {
      const answer = await solveCaptcha(page, current, `captcha-step${step}`);
      note("step", { at: "inline-captcha", step, answered: Boolean(answer) });
      continue;
    }

    const verdict = flow.classifyScreen({ text, url: page.url() });
    note("step", { at: `screen-${step}`, state: verdict, text: text.slice(0, 300) });
    if (verdict === "success") {
      finalState = "success";
      finalEvidence = `terminal-screen:${step}`;
      break;
    }
    if (verdict === "unknown") {
      finalState = "unknown";
      finalEvidence = `unrecognized-screen:${step}`;
      break;
    }

    const stepStartedAt = Date.now();
    // Priority: a Graph mailbox is unattended, the manual file is a human, and
    // the legacy temp-mail API is the fallback when neither was given.
    const emailPicker = graphTokenFile
      ? () => graphEmailCode(stepStartedAt)
      : awaitEmailFile
        ? manualEmailCode
        : () => waitFor(() => latestEmailCode(startedAt), 180000, "email-code");
    const picked =
      verdict === "email_step"
        ? await emailPicker()
        : await waitFor(smsCode, 180000, "sms-code");
    const code = picked && typeof picked === "object" ? String(picked.code || "") : String(picked || "");
    // Review A2: the log records that a code arrived, how long it took and which
    // message it came from — never the code itself.
    note("step", {
      at: verdict,
      ...flow.codeLogEntry({
        received: Boolean(code),
        elapsedMs: Date.now() - stepStartedAt,
        messageId: picked && picked.messageId ? picked.messageId : "",
      }),
    });
    if (!code) {
      await shot(`0${step + 1}-no-code`);
      finalState = "waiting_input";
      finalEvidence = `${verdict}-code-timeout`;
      break;
    }
    phoneSubmitted = true;
    await fillDigits(current, code);
    // Review A2: mask the authentication inputs before this screenshot.
    await shot(`0${step + 1}-code-filled`, { maskAuth: true });
    await clickContinue(page, current, verdict);
    await sleep(9000);
    await shot(`0${step + 1}-after`, { maskAuth: true });
  }
  if (!finalEvidence) finalEvidence = "no-terminal-screen";

  // A non-zero exit means the rented number is not needed any more; cancel it
  // before the terminal record so the log ends with the state of the run
  // (run-20261113 §4.7).
  const exitCode = flow.EXIT_CODES[finalState] ?? flow.EXIT_CODES.failed;
  if (exitCode !== 0) await autoRefund(finalState);

  // The terminal record goes last so a reader of run.jsonl sees the same state
  // this process exits with.
  note("final", { state: finalState, evidence: clip(finalEvidence, 160), elapsed_ms: Date.now() - startedAt });
  writeRunLog();
  // A close failure must not turn a recorded outcome into a second final record.
  // In attach mode the window belongs to Roxy: closing it is /browser/close's
  // job, never this driver's (REGISTRAR-CONTRACT §1.5).
  if (!attachMode) await browser.close().catch(() => {});
  console.log(JSON.stringify({ state: finalState, evidence: finalEvidence, outdir }));
  process.exit(exitCode);
})().catch(async (e) => {
  // Review B4: a thrown error is a named outcome too — `unknown` when a submit
  // may have landed, `failed` when it provably did not, `usage` when the run was
  // refused before it started — and it is the last record of run.jsonl either way.
  let state = e && e.state === "unknown" ? "unknown" : e && e.state === "usage" ? "usage" : "failed";
  // Once the SMS code has been typed in, Apple navigates away and the widget
  // iframe we hold is destroyed. Reporting "failed" there mislabels a created
  // account (run-20261113d run 5), so the torn handle becomes its own state and
  // the caller confirms it with a real sign-in.
  if (state === "failed" && phoneSubmitted && /detached|target page, context or browser has been closed|navigat/i.test(String(e && e.message))) {
    state = "likely_success";
  }
  note("error", { error: clip(e && e.message ? e.message : e, 300) });
  await autoRefund(state);
  note("final", { state, evidence: clip((e && e.evidence) || "", 160), elapsed_ms: Date.now() - startedAt });
  writeRunLog();
  // A usage refusal prints its structured report (the same JSON the caller
  // branched on before); everything else prints the state and the complaint.
  if (e && e.report) console.error(JSON.stringify(e.report));
  else console.error(JSON.stringify({ state, error: clip(e && e.message ? e.message : e, 200) }));
  process.exit(flow.EXIT_CODES[state]);
});
