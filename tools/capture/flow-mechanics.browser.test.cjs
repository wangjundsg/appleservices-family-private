"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const mech = require("./flow-mechanics.cjs");
const chrome = process.env.TEST_CHROME_EXE || "C:/Program Files/Google/Chrome/Application/chrome.exe";

for (const headless of [true, false]) test(`real Chrome long country list skips hidden options (headless=${headless})`, {skip: !fs.existsSync(chrome)}, async () => {
  const {chromium} = require(path.resolve(__dirname, "../../.tools/js/node_modules/playwright-core"));
  const browser = await chromium.launch({executablePath:chrome, headless});
  try {
    const page = await browser.newPage();
    await page.route("**/*", route => route.abort());
    await page.setContent(`<title>Local dropdown regression</title><input name="firstName" value="Fixed First"><input name="lastName" value="Fixed Last">
      <select style="margin:100px;width:450px">${Array.from({length:220}, (_,i)=>`<option value="${i}" ${i===218?'selected':''} ${i%7===0?'hidden':''}>Country ${i}</option>`).join("")}</select>`);
    const locator=page.locator("select");
    await mech.selectField(locator,"197","country",{choose:async(l,v)=>{
      await l.click();await page.waitForTimeout(300);
      await mech.selectByKeyboard(l,v,{stepMs:100,settleMs:300});
    }});
    assert.equal(await locator.inputValue(),"197");
    assert.equal(await page.locator('[name="firstName"]').inputValue(),"Fixed First");
    assert.equal(await page.locator('[name="lastName"]').inputValue(),"Fixed Last");
  } finally { await browser.close(); }
});

test("headed Chrome: long popup near viewport bottom starts from a known keyboard anchor", {skip:!fs.existsSync(chrome)}, async()=>{
  const {chromium}=require(path.resolve(__dirname,"../../.tools/js/node_modules/playwright-core"));
  const browser=await chromium.launch({executablePath:chrome,headless:false});
  try {
    const page=await browser.newPage({viewport:{width:1532,height:900}});
    await page.route("**/*",route=>route.abort());
    await page.setContent(`<title>Local country popup regression</title><input value="Unchanged name">
      <select style="display:block;margin:300px 400px 0;width:720px;height:84px;font-size:24px">${Array.from({length:245},(_,i)=>`<option value="${i}" ${i===241?'selected':''}>Country ${i}</option>`).join("")}</select>`);
    const locator=page.locator("select");
    await locator.click();await page.waitForTimeout(400);
    // Popup highlight can move while the committed DOM value is still 241.
    await locator.press("End");
    assert.equal(await locator.inputValue(),"241");
    await mech.selectByKeyboard(locator,"134",{stepMs:100,settleMs:300});
    await locator.press("Tab");
    assert.equal(await locator.inputValue(),"134");
    assert.equal(await page.locator("input").inputValue(),"Unchanged name");
  }finally{await browser.close();}
});

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

test("real Chrome: Chinese then English CAPTCHA refresh clears old answer and requires a changed image", {skip:!fs.existsSync(chrome)},async()=>{
  const {chromium}=require(path.resolve(__dirname,"../../.tools/js/node_modules/playwright-core"));
  const browser=await chromium.launch({executablePath:chrome,headless:true});
  try {
    const page=await browser.newPage();
    for(const label of ["新验证码","New Code"]) {
      await page.setContent(`<img id="challenge"><input name="captcha" value="OLD1"><button>${label}</button>
        <script>(()=>{let n=0;function next(){const c=document.createElement('canvas');c.width=126;c.height=56;const x=c.getContext('2d');x.fillStyle=++n%2?'red':'blue';x.fillRect(0,0,126,56);document.querySelector('img').src=c.toDataURL();}next();document.querySelector('button').onclick=next;})();</script>`);
      const before=await page.locator("img").getAttribute("src");
      await mech.refreshCaptcha(page);
      assert.equal(await page.locator('input[name="captcha"]').inputValue(),"");
      assert.notEqual(await page.locator("img").getAttribute("src"),before);
      await page.locator("button").evaluate(el=>el.onclick=()=>{});
      await assert.rejects(mech.refreshCaptcha(page,{timeout:200}),/Timeout/);
    }
  }finally{await browser.close();}
});

test("real Chrome: Chinese and English Continue ignores hidden submits and refresh buttons",{skip:!fs.existsSync(chrome)},async()=>{
  const {chromium}=require(path.resolve(__dirname,"../../.tools/js/node_modules/playwright-core"));
  const browser=await chromium.launch({executablePath:chrome,headless:true});
  try {
    const page=await browser.newPage();
    for(const [next,refresh] of [["继续","新验证码"],["Continue","New Code"]]) {
      await page.setContent(`<button type="submit" hidden>Continue</button>
        <button type="submit" onclick="window.refreshed=true">${refresh}</button>
        <button type="button" aria-label="${next}" onclick="window.continued=true"></button>
        <script>window.refreshed=false;window.continued=false;</script>`);
      const result=await mech.clickSubmit(page,{sleep:async()=>{}});
      assert.equal(result.clicked,true);
      assert.equal(await page.evaluate(()=>window.continued),true);
      assert.equal(await page.evaluate(()=>window.refreshed),false);
    }
  }finally{await browser.close();}
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
