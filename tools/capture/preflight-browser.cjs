// Browser-based egress preflight: the only honest way to ask "would the real
// creation run get through on this exit?" is to use the same stack the run
// uses. A raw HTTP probe sends a non-browser TLS fingerprint and gets judged
// differently (403 instead of the browser's 503), so its verdict is useless.
//
// Usage:
//   node tools/capture/preflight-browser.cjs [--proxy http://host:port]
//        [--proxy-user U] [--proxy-pass P] [--timeout 45]
//        [--url https://account.apple.com/account]
//
// Output: one JSON line. Exit 0 when the captcha image actually loaded,
// 2 when it did not, 1 on a launch/navigation error.

const path = require("node:path");

const argv = process.argv.slice(2);
function arg(name, fallback = "") {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}

const target = arg("url", "https://account.apple.com/account");
const proxy = arg("proxy", "");
const proxyUser = arg("proxy-user", "");
const proxyPass = arg("proxy-pass", "");
const timeout = Number(arg("timeout", "45")) * 1000;

function loadPlaywright() {
  const candidates = [
    path.resolve(__dirname, "..", "..", ".tools", "js", "node_modules", "playwright-core"),
    "playwright-core",
    "playwright",
  ];
  for (const candidate of candidates) {
    try {
      return require(candidate);
    } catch {
      // try the next location
    }
  }
  throw new Error("playwright not found");
}

(async () => {
  const started = Date.now();
  const { chromium } = loadPlaywright();
  const launch = { channel: "chrome", headless: true };
  if (proxy) {
    launch.proxy = { server: proxy };
    if (proxyUser) {
      launch.proxy.username = proxyUser;
      launch.proxy.password = proxyPass;
    }
  }

  let browser;
  try {
    browser = await chromium.launch(launch);
  } catch (error) {
    console.log(JSON.stringify({ ok: false, stage: "launch", error: String(error).slice(0, 200) }));
    process.exit(1);
  }

  const page = await browser.newPage();
  let navError = "";
  try {
    await page.goto(target, { waitUntil: "domcontentloaded", timeout });
  } catch (error) {
    navError = String(error).slice(0, 200);
  }

  // The creation form lives in an AuthKit iframe; give it a moment to mount.
  let frame = null;
  for (let i = 0; i < 20 && !frame; i += 1) {
    frame = page.frames().find((f) => f.url().includes("/widget/account/"));
    if (!frame) await page.waitForTimeout(1000);
  }

  // The captcha only exists on the form step; the landing page just shows a
  // sign-in box. Click through to "create" so the probe measures the real gate.
  if (frame && (process.argv.includes("--click-create") || process.env.PREFLIGHT_CLICK === "1")) {
    const create = frame.getByText(/create (your )?apple account|create account|don.t have an? apple account/i).first();
    await create.click({ timeout: 15000 }).catch(() => {});
    for (let i = 0; i < 20; i += 1) {
      const has = await frame.locator('input[name="captcha"]').count().catch(() => 0);
      if (has) break;
      await page.waitForTimeout(1000);
    }
  }

  let captcha = { found: false };
  if (frame) {
    captcha = await frame
      .evaluate(() => {
        const inputs = [...document.querySelectorAll('input[name="captcha"]')];
        const input = inputs[0];
        const imgs = [...document.querySelectorAll("img")];
        const scored = imgs.map((img) => {
          const box = img.getBoundingClientRect();
          const near = input ? Math.abs(box.top - input.getBoundingClientRect().top) : 9999;
          return { w: img.naturalWidth, h: img.naturalHeight, near, alt: img.alt || "" };
        });
        const loaded = scored.filter((s) => s.w > 0 && s.w <= 200 && s.h <= 120);
        return {
          found: loaded.length > 0,
          candidates: scored.slice(0, 6),
          hasInput: Boolean(input),
        };
      })
      .catch(() => ({ found: false, error: "evaluate failed" }));
  }

  const result = {
    ok: Boolean(captcha.found),
    stage: captcha.found ? "captcha-loaded" : frame ? "captcha-missing" : "no-widget-frame",
    url: target,
    via: proxy ? "proxy" : "direct",
    frame: Boolean(frame),
    has_input: Boolean(captcha.hasInput),
    candidates: captcha.candidates || [],
    nav_error: navError,
    elapsed_ms: Date.now() - started,
  };
  console.log(JSON.stringify(result));
  await browser.close().catch(() => {});
  process.exit(result.ok ? 0 : 2);
})();
