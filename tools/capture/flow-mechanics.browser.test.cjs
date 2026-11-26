"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const mech = require("./flow-mechanics.cjs");
const chrome = process.env.TEST_CHROME_EXE || "C:/Program Files/Google/Chrome/Application/chrome.exe";

test("real Chrome: readback, one repair, native selects and zero resend", {skip: !fs.existsSync(chrome)}, async () => {
  const {chromium} = require(path.resolve(__dirname, "../../.tools/js/node_modules/playwright-core"));
  const browser = await chromium.launch({executablePath: chrome, headless: true});
  try {
    const page = await browser.newPage();
    page.setDefaultTimeout(3000);
    await page.route("**/*", route => route.abort());
    await page.setContent(`<input id="recover" onblur="if (++this.blurs === 1) this.value='truncated'">
      <input id="broken" onblur="this.value='truncated'">
      <input id="phone" onblur="this.value=this.value.replace(/(\\d{3})(\\d{3})(\\d{3})/,'$1 $2 $3')">
      <select id="country"><option disabled value="">Choose</option><option value="US">USA</option>
      <optgroup disabled label="unavailable"><option value="FR">France</option></optgroup>
      <option value="PT">Portugal</option></select>
      <button id="resend" onclick="window.resends++">Send new code</button>
      <script>document.querySelector('#recover').blurs=0;window.resends=0;</script>`);
    const type = (locator, value) => locator.pressSequentially(value);
    const recovered = await mech.fillField(page.locator("#recover"), "correct", "first_name", {type});
    assert.equal(recovered.corrected, true);
    assert.equal(await page.locator("#recover").inputValue(), "correct");
    await assert.rejects(mech.fillField(page.locator("#broken"), "correct", "last_name", {type}), /field_readback_failed/);
    await mech.fillField(page.locator("#phone"), "967365000", "phone_number", {type, normalize: v => v.replace(/\D/g, "")});
    const choose = async (locator, value) => { await locator.click(); await mech.selectByKeyboard(locator, value); };
    await mech.selectField(page.locator("#country"), "PT", "country", {choose});
    assert.equal(await page.locator("#country").inputValue(), "PT");
    await mech.selectField(page.locator("#country"), "US", "country", {choose});
    assert.equal(await page.locator("#country").inputValue(), "US");
    await assert.rejects(mech.selectField(page.locator("#country"), "FR", "country", {choose}), /field_readback_failed/);
    const result = await mech.emailCodeWindow({waitCode: async () => null, resend: () => page.locator("#resend").click()});
    assert.equal(result.timedOut, true);
    assert.equal(await page.evaluate(() => window.resends), 0);
  } finally { await browser.close(); }
});
