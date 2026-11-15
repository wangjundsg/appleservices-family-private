// Harvest the create widget's telemetry (jslog / v2/jslogs) from a real Chrome.
//
// The protocol client cannot invent these batches: a fabricated event that
// disagrees with the action timeline is a stronger bot signal than silence.
// What this script does is capture *real* envelopes from one page load, so the
// replay layer has genuine structure, real event names and real field names.
//
// Only page loads happen here: no form fill, no submit, no account is touched.
//
// Usage:
//   node tools/capture/jslog-harvest.cjs [--seconds 40] [--country USA]
//        [--out .cache/jslog/capture.json] [--headful]
//
// Output: a JSON file (mode 0600) with {capturedAt, urls, envelopes[]} plus a
// one-line summary on stdout. Bodies are kept verbatim; nothing is anonymised,
// so the file must stay under .cache/ and never be committed.

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

const TELEMETRY_PATHS = ["/jslog", "/v2/jslogs"];

(async () => {
  const seconds = Number(arg("seconds", "40"));
  const country = arg("country", "USA");
  const out = arg("out", path.join(".cache", "jslog", "capture.json"));
  const locale = arg("locale", "en-US");
  const timezoneId = arg("tz", "America/New_York");

  const browser = await chromium.launch({
    channel: "chrome",
    headless: !flag("headful"),
    args: ["--disable-blink-features=AutomationControlled", `--lang=${locale}`],
  });
  const context = await browser.newContext({ locale, timezoneId, viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();

  const envelopes = [];
  const urls = new Set();

  page.on("request", (request) => {
    const url = new URL(request.url());
    if (!TELEMETRY_PATHS.some((prefix) => url.pathname === prefix || url.pathname.endsWith(prefix))) {
      return;
    }
    const headers = request.headers();
    urls.add(url.origin + url.pathname);
    envelopes.push({
      at: Date.now(),
      method: request.method(),
      path: url.pathname,
      query: url.search || "",
      contentType: headers["content-type"] || "",
      headers: {
        accept: headers.accept || "",
        "accept-language": headers["accept-language"] || "",
        "x-apple-i-request-context": headers["x-apple-i-request-context"] || "",
      },
      body: request.postData() || "",
    });
  });

  const target = `https://appleid.apple.com/account?countryCode=${encodeURIComponent(country)}`;
  let navigation = "ok";
  try {
    await page.goto(target, { waitUntil: "domcontentloaded", timeout: 60_000 });
  } catch (error) {
    navigation = String((error && error.message) || error);
  }

  // The widget flushes in batches; give it time and a couple of scrolls so the
  // capture sees more than the first flush.
  const deadline = Date.now() + seconds * 1000;
  let scrolls = 0;
  while (Date.now() < deadline) {
    await page.waitForTimeout(2000);
    if (scrolls < 3) {
      await page.mouse.wheel(0, 400).catch(() => {});
      scrolls += 1;
    }
  }

  await context.close();
  await browser.close();

  const withBody = envelopes.filter((item) => item.body);
  const payload = {
    schemaVersion: 1,
    capturedAt: new Date().toISOString(),
    target,
    navigation,
    locale,
    timezone: timezoneId,
    urls: Array.from(urls),
    envelopes,
  };
  const target_path = path.resolve(out);
  fs.mkdirSync(path.dirname(target_path), { recursive: true });
  fs.writeFileSync(target_path, JSON.stringify(payload, null, 2), { mode: 0o600 });

  console.log(
    JSON.stringify({
      ok: withBody.length > 0,
      envelopes: envelopes.length,
      withBody: withBody.length,
      bytes: withBody.reduce((sum, item) => sum + item.body.length, 0),
      urls: Array.from(urls),
      navigation,
      out,
    }),
  );
  process.exit(withBody.length > 0 ? 0 : 1);
})().catch((error) => {
  console.log(JSON.stringify({ ok: false, error: String((error && error.message) || error) }));
  process.exit(1);
});
