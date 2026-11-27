"use strict";
const {test}=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),os=require('node:os'),path=require('node:path'),{spawnSync}=require('node:child_process');
const {browserWorker,browserLaunchOptions,collect,guard,openPage,recoverableWindowError}=require('./harvest-captcha-ui.cjs');
const {chromium}=require(path.resolve(__dirname,'../../.tools/js/node_modules/playwright-core'));

test('collector can bind one browser to an explicit credential-free proxy',()=>{
  assert.deepEqual(browserLaunchOptions(true),{channel:'chrome',headless:true});
  assert.deepEqual(browserLaunchOptions(true,'socks5://127.0.0.1:17907'),{channel:'chrome',headless:true,proxy:{server:'socks5://127.0.0.1:17907'}});
  assert.throws(()=>browserLaunchOptions(true,'ftp://127.0.0.1:21'),/proxy_url_invalid/);
  assert.throws(()=>browserLaunchOptions(true,'socks5://user:pass@127.0.0.1:17907'),/proxy_url_invalid/);
});

test('webpage collection saves five source images, refreshes four times and never fills or submits',async()=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'harvest-test-')),browser=await chromium.launch({channel:'chrome',headless:true});
  try {
    const page=await browser.newPage(),network=await guard(page),waits=[];
    await page.setContent('<form onsubmit="window.submissions++;return false"><input name="appleId"><div class="captcha"><div class="captcha-container"><img></div><input name="captcha"><button type="button" onclick="next()">新验证码</button></div><button>继续</button></form><script>window.submissions=0;window.round=0;function next(){let c=document.createElement("canvas");c.width=126;c.height=56;let x=c.getContext("2d");x.fillStyle="white";x.fillRect(0,0,126,56);x.fillStyle="black";x.fillText("Ab3D"+(++window.round),10,30);document.querySelector("img").src=c.toDataURL();}next();</script>');
    const out=path.join(root,'images'),result=await collect(page,{out,count:5,network,wait:async ms=>waits.push(ms)});
    assert.equal(result.state,'harvested');assert.equal(result.saved,5);assert.equal(result.blocked_registration_requests,0);
    assert.equal(await page.evaluate(()=>window.round),5);assert.equal(await page.evaluate(()=>window.submissions),0);
    assert.equal(await page.locator('[name=appleId]').inputValue(),'');assert.equal(await page.locator('[name=captcha]').inputValue(),'');
    assert.deepEqual(waits,[5000,5000,5000,5000]);
    const samples=JSON.parse(fs.readFileSync(path.join(out,'manifest.json'))).samples;
    assert.equal(new Set(samples.map(s=>s.sha256)).size,5);assert.ok(samples.every(s=>s.width===126&&s.height===56&&!s.label_verified));
    await assert.rejects(collect(page,{out,count:5,network}),/EEXIST/);
    await page.evaluate(()=>{fetch('https://fixture.invalid/account',{method:'POST'}).catch(()=>{});});
    await assert.rejects(page.waitForResponse('https://fixture.invalid/account',{timeout:200}));
    assert.equal(network.blocked(),1);assert.throws(network.check,/registration_request_blocked/);
  } finally {await browser.close();fs.rmSync(root,{recursive:true,force:true});}
});

test('legacy harvesting flag stops the creation driver before any browser or order activity',()=>{
  for(const args of [['--harvest-captcha','5'],['--harvest-captcha=5']]) {
    const result=spawnSync(process.execPath,[path.join(__dirname,'create-account.cjs'),'--native-browser',...args],{encoding:'utf8'});
    assert.equal(result.status,2);assert.match(result.stderr,/harvest-captcha-ui/);assert.equal(result.stdout,'');
  }
});

test('captcha 503 is local to one window so it can be replaced',async()=>{
  const handlers={},stopped=[];
  const page={route:async()=>{},on:(event,handler)=>{handlers[event]=handler;},isClosed:()=>false};
  const controller={turn:async()=>{},check(){},stop:error=>stopped.push(error)};
  const network=await guard(page,controller);
  handlers.response({url:()=> 'https://appleid.apple.com/captcha',status:()=>503});
  assert.throws(network.check,/captcha_http_503/);assert.deepEqual(stopped,[]);
  assert.equal(recoverableWindowError(Error('captcha_http_503')),true);
  assert.equal(recoverableWindowError(Error('page_http_503')),true);
  assert.equal(recoverableWindowError(Error('captcha_image_timeout')),true);
  assert.equal(recoverableWindowError(Error('registration_request_blocked')),false);
});

test('an account page 503 is detected immediately instead of waiting for an image',async()=>{
  let turns=0;
  await assert.rejects(openPage({goto:async()=>({status:()=>503})},{turn:async()=>{turns++;}}),/page_http_503/);
  assert.equal(turns,1);
});

test('a window keeps reopening while replacement pages still return 503',async()=>{
  let contexts=0;
  const browser={newContext:async()=>{const id=++contexts;return {close:async()=>{},newPage:async()=>({route:async()=>{},on:()=>{},isClosed:()=>false,
    goto:async()=>({status:()=>id<3?503:200})})};}};
  const worker=browserWorker(browser,{turn:async()=>{},check(){},stop(){}});
  await assert.rejects(worker.start(),/page_http_503/);
  assert.equal(await worker.recover(Error('page_http_503')),true);assert.equal(contexts,3);
});
