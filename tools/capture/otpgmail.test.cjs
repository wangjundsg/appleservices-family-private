'use strict';
const {test}=require('node:test'), assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),os=require('node:os');
const {inspect,pickCode,pollCode}=require('./otpgmail.cjs');
const {readCode}=require('./mail-reader.cjs');
const mailbox='fixture@gmail.com', at=Math.floor(Date.now()/1000)-2;
const cfg={provider:'otpgmail',orderId:'fixture-order',email:mailbox,warehouse:2,credentialFile:'unused'};
const order={orderId:cfg.orderId,service:'wx',domain:'gmail.com',email:mailbox,warehouse:2,status:'completed',otp:[{code:'001234',receivedAt:at}]};
const scope={mailbox,since:(at-2)*1000,until:Date.now(),excluded:[]};
function fixture(t) {
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'otpgmail-test-'));
  t.after(()=>fs.rmSync(dir,{recursive:true,force:true}));
  const file=path.join(dir,'order.json');fs.writeFileSync(file,JSON.stringify(cfg));return file;
}
test('read-only check uses Bearer headers, fixed host and only returns whitelisted data',async()=>{
  const routes=[];
  const result=await inspect({credentialFile:'unused',keyLoader:()=> 'fixture-secret',fetchImpl:async(url,init)=>{
    routes.push(url);assert.equal(init.method,'GET');assert.equal(init.redirect,'error');
    assert.equal(init.headers.Authorization,'Bearer fixture-secret');assert.ok(!url.includes('fixture-secret'));
    return {ok:true,json:async()=>({success:true,data:url.endsWith('/balance')?{balance:52560,private:'hidden'}:
      [{code:'wx',name:'Apple',offers:[{warehouse:2,domain:'gmail.com',price:700,stock:12,private:'hidden'}]}]})};
  }});
  assert.equal(routes.length,2);assert.deepEqual(result,{balanceVnd:52560,service:'wx',domain:'gmail.com',offers:[{warehouse:2,priceVnd:700,stock:12}]});
});
test('task binding, time window, leading zeros and duplicate prevention',()=>{
  const hit=pickCode(order,cfg,scope);assert.equal(hit.code,'001234');assert.ok(!hit.messageId.includes('001234'));
  assert.equal(pickCode(order,cfg,{...scope,excluded:[hit.messageId]}),null);
  assert.equal(pickCode(order,cfg,{...scope,since:at*1000}),null);
  assert.equal(pickCode(order,cfg,{...scope,until:(at-1)*1000}),null);
  for(const change of [{orderId:'other'},{service:'fb'},{email:'other@gmail.com'},{warehouse:1},{domain:'icloud.com'},{status:'unknown'}])
    assert.throws(()=>pickCode({...order,...change},cfg,scope),/otpgmail_order_mismatch/);
  assert.throws(()=>pickCode({...order,status:'cancelled'},cfg,scope),/otpgmail_order_cancelled/);
});
test('newest invalid or ambiguous mail cannot fall back to an old code',()=>{
  assert.throws(()=>pickCode({...order,otp:[...order.otp,{code:'welcome',receivedAt:at+1}]},cfg,scope),/code_unrecognized/);
  assert.throws(()=>pickCode({...order,otp:[...order.otp,{code:'009999',receivedAt:at}]},cfg,scope),/code_ambiguous/);
  assert.throws(()=>pickCode({...order,otp:[{code:'001234',receivedAt:'invalid'}]},cfg,scope),/otpgmail_response_invalid/);
});
test('poll reads only the bound order and never buys, resends, cancels or extends',async t=>{
  const file=fixture(t),calls=[];
  const options={...scope,file,recipient:mailbox,source:'registration',platform:'apple',timeoutMs:1000,keyLoader:()=> 'fixture',fetchImpl:async(url,init)=>{
    calls.push(url);assert.equal(init.method,'GET');return {ok:true,json:async()=>({success:true,data:order})};
  }};
  assert.equal((await pollCode(options)).code,'001234');
  assert.deepEqual(calls,['https://otpgmail.net/v1/orders/fixture-order']);
  await assert.rejects(pollCode({...options,source:'forwarded'}),/otpgmail_scope_invalid/);
  await assert.rejects(pollCode({...options,mailbox:'other@gmail.com',recipient:'other@gmail.com'}),/otpgmail_config_invalid/);
  assert.equal(await readCode({...options,reader:'otpgmail',timeoutMs:0}),null);
  assert.equal(calls.length,1);
});
test('authentication and HTTP errors remain terminal and do not expose upstream secrets',async t=>{
  const base={...scope,file:fixture(t),recipient:mailbox,source:'registration',platform:'apple',timeoutMs:1000,keyLoader:()=> 'fixture-secret'};
  for(const [status,expected] of [[401,'otpgmail_unauthorized'],[429,'otpgmail_rate_limited'],[500,'otpgmail_read_failed']]) {
    await assert.rejects(pollCode({...base,fetchImpl:async()=>({ok:false,status,json:async()=>({error:{message:'fixture-secret'}})})}),
      error=>error.message===expected&&!String(error).includes('fixture-secret'));
  }
});

test('a transient order request failure retries within the original polling window',async t=>{
  let clock=Date.now(),calls=0;const ticks=[],waits=[];
  const result=await pollCode({...scope,file:fixture(t),recipient:mailbox,source:'registration',platform:'apple',timeoutMs:10000,keyLoader:()=> 'fixture',
    now:()=>clock,sleepImpl:async ms=>{waits.push(ms);clock+=ms;},onTick:event=>ticks.push(event),
    fetchImpl:async()=>{if(++calls===1)throw Error('private-response');return {ok:true,json:async()=>({success:true,data:order})};}});
  assert.equal(result.code,'001234');assert.equal(calls,2);assert.deepEqual(waits,[4000]);
  assert.equal(ticks[0].error,'otpgmail_request_failed');
  assert.equal(JSON.stringify(ticks).includes('private-response'),false);
});

test('an interrupted response body retries but malformed order data still fails',async t=>{
  let clock=Date.now(),calls=0;
  const options={...scope,file:fixture(t),recipient:mailbox,source:'registration',platform:'apple',timeoutMs:10000,keyLoader:()=> 'fixture',
    now:()=>clock,sleepImpl:async ms=>{clock+=ms;}};
  const result=await pollCode({...options,fetchImpl:async()=>({ok:true,json:async()=>{
    if(++calls===1)throw Object.assign(Error('private-response'),{name:'AbortError'});
    return {success:true,data:order};}})});
  assert.equal(result.code,'001234');assert.equal(calls,2);
  await assert.rejects(pollCode({...options,fetchImpl:async()=>({ok:true,json:async()=>({success:true,data:null})})}),/otpgmail_order_mismatch/);
  await assert.rejects(pollCode({...options,fetchImpl:async()=>({ok:true,json:async()=>{throw SyntaxError('private-response');}})}),/otpgmail_response_invalid/);
});

test('ten-second polling request expiry retains both three-minute email windows',async t=>{
  const {emailCodeWindow}=require('./flow-mechanics.cjs');
  let clock=Date.now(),requests=0;const started=clock,resends=[];
  const options={...scope,file:fixture(t),recipient:mailbox,source:'registration',platform:'apple',keyLoader:()=> 'fixture',
    now:()=>clock,sleepImpl:async ms=>{clock+=ms;},fetchImpl:async()=>{requests++;clock+=10000;throw Error('request timed out');}};
  const result=await emailCodeWindow({now:()=>clock,firstMs:180000,secondMs:180000,resendEnabled:true,
    resend:async()=>{resends.push(clock-started);return {asked:true,ok:true};},
    waitCode:async ms=>{const end=clock+ms;while(clock<end)await pollCode({...options,timeoutMs:Math.min(10000,end-clock)});return null;}});
  assert.deepEqual(resends,[180000]);assert.equal(clock-started,360000);assert.equal(requests,36);
  assert.equal(result.replace,true);
});
test('Windows DPAPI survives an inherited incompatible PowerShell module path',{skip:process.platform!=='win32'},async t=>{
  const file=fixture(t),credentialFile=path.join(path.dirname(file),'fixture.dpapi');
  const key='og_'+'fixture'.repeat(6);
  const encrypted=require('node:child_process').execFileSync('powershell.exe',['-NoProfile','-NonInteractive','-Command',
    'Import-Module ($PSHOME + "/Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1") -ErrorAction Stop; ConvertTo-SecureString -String ([Console]::In.ReadToEnd()) -AsPlainText -Force | ConvertFrom-SecureString'],
    {input:key,encoding:'utf8',windowsHide:true,stdio:['pipe','pipe','ignore']});
  fs.writeFileSync(credentialFile,encrypted);
  const previous=process.env.PSModulePath;process.env.PSModulePath=path.dirname(file);
  try {
    const result=await inspect({credentialFile,fetchImpl:async(url,init)=>{
      assert.equal(init.headers.Authorization,'Bearer '+key);
      return {ok:true,json:async()=>({success:true,data:url.endsWith('/balance')?{balance:0}:[{code:'wx',name:'Apple',offers:[]}]})};
    }});
    assert.equal(result.balanceVnd,0);
  } finally { if(previous===undefined)delete process.env.PSModulePath;else process.env.PSModulePath=previous; }
});
