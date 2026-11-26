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

test("real Chrome: localized SMS menu selects resend and replacement, never Call Me", {skip: !fs.existsSync(chrome)}, async () => {
  const {chromium}=require(path.resolve(__dirname,"../../.tools/js/node_modules/playwright-core"));
  const browser=await chromium.launch({executablePath:chrome,headless:true});
  try {
    const page=await browser.newPage();
    await page.route("**/*",route=>route.abort());
    for(const [help,resend,replace] of [
      ["Didn't get a code?","Resend Text","Use a Different Number"],
      ["没有收到验证码？","重新发送短信","使用其他号码"],
      ["没有收到验证码？","重新发送短信","使用其他电话号码"],
      ["Não recebeu um código?","Reenviar SMS","Usar outro número"]]) {
      await page.setContent(`<button onclick="document.querySelector('#menu').hidden=false">${help}</button>
        <div id="menu" hidden><button onclick="window.sent++;this.parentNode.hidden=true">${resend}</button>
        <button onclick="window.calls++">Call Me</button>
        <button onclick="document.querySelector('#number').hidden=false;this.parentNode.hidden=true">${replace}</button></div>
        <form id="number" hidden><select name="phoneCode"><option value="US">+1</option><option value="PT">+351 Portugal</option></select>
        <input name="phoneNumber"></form><script>window.sent=0;window.calls=0;</script>`);
      const click=locator=>locator.click();
      await mech.phoneMenuAction(page,mech.PHONE_RESEND_LABELS,click);
      assert.equal(await page.evaluate(()=>window.sent),1);
      await mech.phoneMenuAction(page,mech.PHONE_REPLACE_LABELS,click);
      const phone=page.locator('[name=phoneNumber]');
      await phone.waitFor({state:'visible'});
      await mech.fillField(phone,'912345678','phone_number',{type:(l,v)=>l.pressSequentially(v)});
      await mech.selectField(page.locator('[name=phoneCode]'),'PT','dial_code',
        {choose:async(l,v)=>{await l.click();await mech.selectByKeyboard(l,v);}});
      assert.equal(await phone.inputValue(),'912345678');
      assert.equal(await page.locator('[name=phoneCode]').inputValue(),'PT');
      assert.equal(await page.evaluate(()=>window.calls),0);
    }
  } finally {await browser.close();}
});
