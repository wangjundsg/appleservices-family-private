"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs"), path = require("node:path");
const mech = require("./flow-mechanics.cjs");
const chrome = process.env.TEST_CHROME_EXE || "C:/Program Files/Google/Chrome/Application/chrome.exe";

test("local Chrome loads a delayed widget and stops missing forms within one budget", {skip: !fs.existsSync(chrome)}, async () => {
  const {chromium} = require(path.resolve(__dirname, "../../.tools/js/node_modules/playwright-core"));
  const browser = await chromium.launch({executablePath: chrome, headless: true});
  try {
    const page = await browser.newPage();
    let withForm = true;
    await page.route("https://fixture.invalid/**", route => route.fulfill({contentType: "text/html",
      body: route.request().url().includes("/widget/account/")
        ? withForm ? '<script>setTimeout(()=>document.body.innerHTML=\'<input name="appleId">\',150)</script>' : '<body>Loading</body>'
        : '<body><script>setTimeout(()=>document.body.innerHTML=\'<iframe src="/widget/account/test"></iframe>\',150)</script></body>'}));
    const frame = await mech.loadSignupPage(page, {url: "https://fixture.invalid/account", timeoutMs: 10000});
    assert.equal(await frame.locator('input[name="appleId"]').isVisible(), true);
    withForm = false;
    const start = performance.now();
    await assert.rejects(mech.loadSignupPage(page, {url: "https://fixture.invalid/account", timeoutMs: 700}),
      {state: "failed", evidence: "page_load_timeout"});
    assert.ok(performance.now() - start < 2000, "missing form must not fall back to the default 30-second locator wait");
    await page.unrouteAll();
    await page.route("**/*", route => route.abort("connectionreset"));
    await assert.rejects(mech.loadSignupPage(page, {url: "https://fixture.invalid/account", timeoutMs: 3000}),
      {state: "failed", evidence: "page_load_failed"});
  } finally {await browser.close();}
});
