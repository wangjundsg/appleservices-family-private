"use strict";
const {test}=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path');
const flow=require('./account-flow.cjs'),mechanics=require('./flow-mechanics.cjs');
const chrome=process.env.TEST_CHROME_EXE || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

test('actual native observer waits through transient frames, requests and CN/EN verification screens', {skip:!fs.existsSync(chrome)},async()=>{
  const {chromium}=require(path.resolve(__dirname,'../../.tools/js/node_modules/playwright-core'));
  const browser=await chromium.launch({executablePath:chrome,headless:true});
  try {
    const page=await browser.newPage();
    await page.route('https://fixture.invalid/**',route=>route.fulfill({body:'<body></body>',contentType:'text/html'}));
    await page.goto('https://fixture.invalid/widget/account/test');
    const source=fs.readFileSync(path.join(__dirname,'create-account.cjs'),'utf8');
    const start=source.indexOf('  async function observeNativeScreen()'),end=source.indexOf('  // Screenshot helper.',start);
    let clock=0,missing=false,advance=async()=>{};
    const posts=new Map(),captchas=new Set();
    const deps={widgetFrame:async()=>missing?null:page.mainFrame(),page,flow,email:'fixture@example.invalid',phone:'912345678',
      hardRefusal:async(_frame,text)=>({hit:text.includes('此时无法创建你的账户')}),
      nativePosts:posts,pendingCaptchaRequests:captchas,nativeCompletedPostVersion:1,leaseDeadline:45000,
      sleep:async ms=>{clock+=ms;await advance();},
      mech:{waitForScreen:opts=>mechanics.waitForScreen({...opts,now:()=>clock})}};
    const driver=new Function(...Object.keys(deps),source.slice(start,end)+';return {observeNativeScreen,waitNativeScreen,waitNativeTransition};')(...Object.values(deps));
    const codePage=heading=>`<h1>${heading}</h1>${'<input type="tel">'.repeat(6)}`;
    for(const [email,phone] of [['验证你的电子邮件地址','验证你的手机号码'],['Verify your email','Verify your phone number']]) {
      clock=0;await page.setContent('<input name="appleId">');
      const before=await driver.observeNativeScreen();posts.set('request',1);captchas.add('captcha');
      advance=async()=>{
        missing=clock<600;
        if(clock===600)await page.setContent(codePage(email));
        if(clock===1200)posts.clear();
        if(clock===1800)captchas.clear();
      };
      assert.equal((await driver.waitNativeTransition(before,0)).state,'email_step');
      assert.equal(clock,2000,'a pending response must block readiness, without a nine-second floor');
      const emailScreen=await driver.observeNativeScreen();clock=0;
      advance=async()=>{missing=clock<12000;if(clock===12000)await page.setContent(codePage(phone));};
      assert.equal((await driver.waitNativeTransition(emailScreen,1)).state,'phone_step');
      assert.equal(clock,12200,'a transition taking more than nine seconds must still complete');
    }
    clock=0;missing=false;advance=async()=>{};
    await page.setContent('<h1>验证你的电子邮件地址</h1>');
    await assert.rejects(driver.waitNativeScreen(),/page_transition_timeout/);
    assert.equal(clock,45000,'a heading without ready code controls must not start polling');
    clock=0;await page.setContent(codePage('Verify your email')+'<input aria-invalid="true">');
    const same=await driver.observeNativeScreen();
    assert.equal((await driver.waitNativeTransition(same,0)).state,'email_step');
    clock=0;await page.setContent('<h1>此时无法创建你的账户</h1>');
    assert.equal((await driver.waitNativeScreen()).refusal,true);
    clock=0;await page.setContent('<h1>你的 Apple 账户已创建</h1>');
    assert.equal((await driver.waitNativeScreen()).state,'success');
    clock=0;await page.goto('https://fixture.invalid/account/manage');missing=true;
    assert.equal((await driver.waitNativeScreen()).state,'success');
  } finally {await browser.close();}
});
