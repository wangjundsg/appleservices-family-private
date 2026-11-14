// Verify the Forge-owned context before account creation; never patch its fingerprint.
"use strict";
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("../../.tools/js/node_modules/playwright-core");

(async () => {
  const input = JSON.parse(fs.readFileSync(process.env.APPLESERVICES_CREATE_INPUT, "utf8"));
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${input["cdp-port"]}`);
  const context = browser.contexts()[0];
  if (!context) throw Error("missing_context");
  const page = await context.newPage();
  try {
    await page.goto("http://ip-api.com/json/?fields=status,countryCode,query", { waitUntil: "domcontentloaded", timeout: 30000 });
    const exit = JSON.parse(await page.locator("body").innerText());
    const seen = await page.evaluate(() => ({ ua: navigator.userAgent, platform: navigator.platform,
      locale: navigator.language, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      offset: new Date().getTimezoneOffset() }));
    if (exit.status !== "success" || exit.countryCode !== input.phonecc || seen.platform !== "Win32" || !seen.ua.includes("Windows NT") ||
        seen.locale !== input["expected-locale"] || seen.timezone !== input["expected-timezone"]) {
      throw Error("geo_mismatch");
    }
    fs.writeFileSync(path.join(input.outdir, "profile.json"), JSON.stringify({ ...seen, egress_ip: exit.query }), { mode: 0o600 });
    console.log(JSON.stringify({ state: "success" }));
  } finally { await page.close(); }
  process.exit(0);
})().catch(() => { console.log(JSON.stringify({ state: "failed", reason: "browser_preflight_failed" })); process.exit(1); });
