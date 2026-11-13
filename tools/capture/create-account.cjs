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
//     [--profile .cache\acct-01\profile.json] [--egress-ip 203.0.113.7]
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

const outdir = path.resolve(arg("outdir", ".cache/acct"));
const email = String(arg("email", ""));
const jwt = String(arg("jwt", ""));
const phone = String(arg("phone", ""));
const activation = String(arg("activation", ""));
const heroKey = String(arg("hero-key", ""));
const awaitCaptcha = String(arg("await-captcha", ""));
// Review A1: no default password, and the check runs here — before the account
// directory is created and long before the browser makes its first request.
const passwordResolution = flow.resolvePassword(arg("password", ""), process.env.APPLESERVICES_PASSWORD);
if (!passwordResolution.password) {
  console.error(
    JSON.stringify(flow.missingPasswordReport("create-account.cjs", "the Apple Account creation form requires a password")),
  );
  process.exit(flow.EXIT_CODES.usage);
}
const password = passwordResolution.password;
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
fs.mkdirSync(outdir, { recursive: true });

const log = [];
const clip = (text, max) => String(text || "").slice(0, max);
function note(kind, data) {
  log.push({ t: Date.now(), kind, ...data });
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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// latestEmailCode returns {code, messageId, created_at} or null.
// Review B5: newest mail first, addressed to this run's mailbox, from a trusted
// sender, subject carrying verification semantics. The previous version took the
// first "<b>6 digits</b>" out of the first ten messages in API order.
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

async function fillDigits(frame, code) {
  const boxes = frame.locator('input[type="tel"]');
  const count = await boxes.count();
  note("code-boxes", { count });
  if (count >= code.length) {
    for (let i = 0; i < code.length; i += 1) {
      await boxes.nth(i).fill(code[i]).catch(() => {});
    }
  } else {
    await boxes.first().fill(code).catch(() => {});
  }
  await sleep(800);
}

// The page carries several images (privacy icon, logos). The captcha is the
// small one, so pick by natural size instead of DOM order.
async function captchaImage(fr) {
  const idx = await fr
    .evaluate(() => {
      const imgs = [...document.querySelectorAll("img")];
      return imgs.findIndex(
        (x) => x.naturalWidth > 0 && x.naturalWidth <= 200 && x.naturalHeight <= 120,
      );
    })
    .catch(() => -1);
  return fr.locator("img").nth(idx >= 0 ? idx : 0);
}
// Apple can show the image captcha on any step, including the phone step
// ("Security Code Error"). Solve it the same way as on the first form.
async function solveCaptcha(page, fr, tag) {
  const answerFile = path.resolve(arg("await-captcha", ".cache/captcha-answer.txt"));
  fs.rmSync(answerFile, { force: true });
  await captchaImage(fr)
    .screenshot({ path: path.join(outdir, `${tag}.png`), scale: "css" })
    .catch(() => {});
  const answer = await waitFor(async () => {
    if (!fs.existsSync(answerFile)) return "";
    const v = fs.readFileSync(answerFile, "utf8").trim();
    return v.length >= 4 ? v : "";
  }, 180000, "captcha");
  if (!answer) return "";
  await fr.fill('input[name="captcha"]', answer);
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
    note("step", { at: "click-timeout", tag, error: clip(e && e.message ? e.message : e, 160) });
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
  // Resolve the fingerprint before a browser exists: a profile another stage
  // already froze is validated here, so a stale or edited file fails fast
  // instead of driving one account under two different identities.
  const frozen = loadProfile();
  const profile = frozen.profile;
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

  const launchOpts = {
    channel: "chrome",
    headless: true,
    args: ["--disable-blink-features=AutomationControlled"],
  };
  const proxy = arg("proxy", "");
  if (proxy) {
    launchOpts.proxy = { server: String(proxy) };
    if (arg("proxy-user", "")) {
      launchOpts.proxy.username = String(arg("proxy-user", ""));
      launchOpts.proxy.password = String(arg("proxy-pass", ""));
    }
  }
  const browser = await chromium.launch(launchOpts);

  // What actually ran goes next to what was intended. The engine label is
  // rebuilt from the UA that goes out (never hand-typed), and the exit address
  // is whatever the caller already measured with --egress-ip; this script does
  // not probe the network itself. Other fields are written back untouched.
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

  const context = await browser.newContext({
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
  const page = await context.newPage();
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

  const fillForm = async (fr) => {
    await fr.fill('input[name="firstName"]', arg("first", "James"));
    await fr.fill('input[name="lastName"]', arg("last", "Carter"));
    await fr.selectOption('select[name="countrySelect"]', "USA");
    const selects = fr.locator("select");
    await selects.nth(1).selectOption("07");
    await selects.nth(2).selectOption("18");
    await selects.nth(3).selectOption("1992");
    await fr.fill('input[name="appleId"]', email);
    await fr.fill('input[name="password"]', password);
    await fr.fill('input[name="confirmPassword"]', password);
    await fr.selectOption('select[name="phoneCode"]', "US");
    await fr.fill('input[name="phoneNumber"]', phone);
  };

  // A wrong captcha does not burn anything: Apple just refreshes the image and
  // stays on the form. Loop until the form is behind us.
  const answerFile = path.resolve(awaitCaptcha || ".cache/captcha-answer.txt");
  let submitted = false;
  for (let attempt = 1; attempt <= 16 && !submitted; attempt += 1) {
    const fr = await widgetFrame(page);
    if (!fr) break;
    await fillForm(fr);
    fs.rmSync(answerFile, { force: true });

    // Render the 126x56 image at 3x before capturing it so the characters are
    // legible without external image tooling.
    const img = await captchaImage(fr);
    await img.evaluate((el) => {
      el.style.width = "378px";
      el.style.height = "168px";
      el.style.imageRendering = "pixelated";
    }).catch(() => {});
    await sleep(300);
    await img.screenshot({ path: path.join(outdir, `captcha-${attempt}.png`), scale: "css" }).catch(() => {});
    fs.rmSync(path.join(outdir, "captcha.png"), { force: true });
    fs.copyFileSync(path.join(outdir, `captcha-${attempt}.png`), path.join(outdir, "captcha.png"));
    note("step", { at: "captcha-ready", attempt });

    // Prefer the offline OCR when a command was supplied: it answers in under a
    // second, so a rejected captcha only costs one more round trip. Fall back
    // to a human/answer file when OCR returns nothing usable.
    let answer = "";
    const ocrCmd = arg("captcha-cmd", "");
    if (ocrCmd) {
      try {
        const { execFileSync } = require("node:child_process");
        answer = String(
          execFileSync("python", [String(ocrCmd), path.join(outdir, `captcha-${attempt}.png`)], {
            encoding: "utf8",
            timeout: 30000,
          }),
        ).trim();
      } catch (e) {
        note("ocr-error", { error: String(e).slice(0, 160) });
        answer = "";
      }
      if (answer && (answer.length < 4 || answer.length > 6)) { note("step", { at: "ocr-short", attempt, answer }); answer = ""; }
      note("step", { at: "ocr-answer", attempt, answer });
      if (!answer) {
        // OCR read nothing usable: refresh the image and try again instead of
        // blocking on a human. A refresh costs nothing and keeps the run
        // unattended.
        const fresh = fr.getByRole("button", { name: /new code/i }).first();
        if (await fresh.count().catch(() => 0)) await fresh.click().catch(() => {});
        await sleep(2500);
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
    note("step", { at: "captcha-answer", attempt, answer });

    await fr.fill('input[name="captcha"]', answer);
    await fr.locator('input[name="captcha"]').press("Tab").catch(() => {});
    await sleep(1200);
    await clickContinue(page, fr, `create-submit-${attempt}`);
    await sleep(9000);
    await shot(`01-submit-${attempt}`);
    const stillForm = await (await widgetFrame(page))
      ?.locator('input[name="captcha"]')
      .count()
      .catch(() => 0);
    submitted = !stillForm;
    if (!submitted) {
      // A rejected captcha leaves the same image on screen; ask for a new one
      // before spending another OCR pass on it.
      const fresh = fr.getByRole("button", { name: /new code/i }).first();
      if (await fresh.count().catch(() => 0)) await fresh.click().catch(() => {});
      await sleep(2500);
    }
    note("step", { at: "submit-result", attempt, stillForm: Boolean(stillForm) });
  }
  if (!submitted) throw new Error("captcha never accepted");
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
    const picked =
      verdict === "email_step"
        ? await waitFor(() => latestEmailCode(startedAt), 180000, "email-code")
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
    await fillDigits(current, code);
    // Review A2: mask the authentication inputs before this screenshot.
    await shot(`0${step + 1}-code-filled`, { maskAuth: true });
    await clickContinue(page, current, verdict);
    await sleep(9000);
    await shot(`0${step + 1}-after`, { maskAuth: true });
  }
  if (!finalEvidence) finalEvidence = "no-terminal-screen";

  // The terminal record goes last so a reader of run.jsonl sees the same state
  // this process exits with.
  note("final", { state: finalState, evidence: clip(finalEvidence, 160), elapsed_ms: Date.now() - startedAt });
  writeRunLog();
  // A close failure must not turn a recorded outcome into a second final record.
  await browser.close().catch(() => {});
  console.log(JSON.stringify({ state: finalState, evidence: finalEvidence, outdir }));
  process.exit(flow.EXIT_CODES[finalState] ?? flow.EXIT_CODES.failed);
})().catch(async (e) => {
  // Review B4: a thrown error is a named outcome too — `unknown` when a submit
  // may have landed, `failed` when it provably did not — and it is the last
  // record of run.jsonl either way.
  const state = e && e.state === "unknown" ? "unknown" : "failed";
  note("error", { error: clip(e && e.message ? e.message : e, 300) });
  note("final", { state, evidence: clip((e && e.evidence) || "", 160), elapsed_ms: Date.now() - startedAt });
  writeRunLog();
  console.error(JSON.stringify({ state, error: clip(e && e.message ? e.message : e, 200) }));
  process.exit(flow.EXIT_CODES[state]);
});
