// Harvest the create widget's fraud-detection material from a real Chrome.
//
// The pure-HTTP registrar sends X-Apple-I-FD-Client-Info itself; the value is a
// blob the widget builds from browser signals (`fdcBrowserData`). A stale blob
// (or none at all) is a risk signal, so this script opens the real create page
// once, lets the widget do its work, and prints the header value it actually
// sent plus the hidden field the form would submit.
//
// Usage:
//   node tools/capture/fd-harvest.cjs [--locale en-US] [--tz America/New_York]
//                                     [--out .cache/fd.json] [--headful]
//
// Output (stdout JSON): {fdClientInfo, fdcBrowserData, userAgent, locale, timezone}
// Nothing is submitted: the page is loaded and closed.

"use strict";

const fs = require("node:fs");
const path = require("node:path");

const MODULES = path.resolve(__dirname, "..", "..", ".tools", "js", "node_modules");
const { chromium } = require(path.join(MODULES, "playwright-core"));

function arg(name, fallback = "") {
  const argv = process.argv.slice(2);
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}

const flag = (name) => process.argv.includes(`--${name}`);

const CREATE_URL = "https://appleid.apple.com/account?countryCode=USA";

(async () => {
  const locale = arg("locale", "en-US");
  const timezoneId = arg("tz", "America/New_York");
  const out = arg("out", path.join(".cache", "fd.json"));

  const browser = await chromium.launch({
    channel: "chrome",
    headless: !flag("headful"),
    args: ["--disable-blink-features=AutomationControlled", `--lang=${locale}`],
  });
  const context = await browser.newContext({ locale, timezoneId, viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();

  let fdClientInfo = "";
  let seenRequests = 0;
  page.on("request", (request) => {
    const headers = request.headers();
    const value = headers["x-apple-i-fd-client-info"];
    if (value && !fdClientInfo) {
      fdClientInfo = value;
      seenRequests += 1;
    }
  });

  let fdcBrowserData = "";
  try {
    await page.goto(CREATE_URL, { waitUntil: "domcontentloaded", timeout: 60_000 });
    // The widget lives in an iframe and needs a moment to boot and stamp the
    // hidden field; the fd header also shows up on the first widget call.
    const deadline = Date.now() + 45_000;
    while (Date.now() < deadline && !(fdClientInfo && fdcBrowserData)) {
      await page.waitForTimeout(1000);
      if (!fdcBrowserData) {
        for (const frame of page.frames()) {
          try {
            const value = await frame.evaluate(() => {
              const el = document.getElementById("fdcBrowserData");
              return el ? String(el.getAttribute("value") || el.value || "") : "";
            });
            if (value) {
              fdcBrowserData = value;
              break;
            }
          } catch {
            // cross-origin or detached frame: keep looking
          }
        }
      }
    }
  } finally {
    await context.close();
    await browser.close();
  }

  const payload = {
    ok: Boolean(fdClientInfo || fdcBrowserData),
    fdClientInfo,
    fdcBrowserData,
    requests: seenRequests,
    locale,
    timezone: timezoneId,
    capturedAt: new Date().toISOString(),
  };
  if (fdClientInfo) {
    fs.mkdirSync(path.dirname(path.resolve(out)), { recursive: true });
    fs.writeFileSync(path.resolve(out), JSON.stringify(payload, null, 2), { mode: 0o600 });
    payload.out = out;
  }
  console.log(JSON.stringify(payload));
  process.exit(payload.ok ? 0 : 1);
})().catch((error) => {
  console.log(JSON.stringify({ ok: false, error: String(error && error.message ? error.message : error) }));
  process.exit(1);
});
