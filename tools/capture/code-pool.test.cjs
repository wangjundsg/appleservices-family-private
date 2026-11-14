'use strict';
const test=require('node:test'), assert=require('node:assert/strict');
const fs=require('node:fs'), os=require('node:os'), path=require('node:path');
const {validateHit,pollCode}=require('./code-pool.cjs');
const since=Date.now()-10000, until=Date.now();
const hit={ok:true,code:'002026',mailbox:'owner@hotmail.com',message_id:'test-message',received_at:new Date(since+1000).toISOString(),from:'appleid@id.apple.com'};
const context={mailbox:hit.mailbox,since,until,excluded:[]};
test('preserves leading zero, checks sender, recipient, time and message dedup',()=>{
  assert.equal(validateHit(hit,context).code,'002026');
  for(const change of [{code:2026},{from:'x@evilapple.com'},{mailbox:'other@hotmail.com'},{received_at:new Date(since).toISOString()},{received_at:new Date(until+1000).toISOString()}]) {
    assert.throws(()=>validateHit({...hit,...change},context),/code_pool_response_invalid/);
  }
  assert.throws(()=>validateHit(hit,{...context,excluded:['test-message']}));
});
test('uses only the read-only service contract and never follows redirects',async t=>{
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'apple-code-test-'));
  t.after(()=>fs.rmSync(dir,{recursive:true,force:true}));
  const file=path.join(dir,'client.json');fs.writeFileSync(file,JSON.stringify({baseUrl:'http://127.0.0.1:8802',apiToken:'fixture-only'}));
  const result=await pollCode({file,mailbox:hit.mailbox,since,timeoutMs:1000,excluded:['old'],fetchImpl:async(url,options)=>{
    assert.equal(url.pathname,'/api/v1/code');assert.equal(options.redirect,'error');
    const body=JSON.parse(options.body);
    assert.equal(Date.parse(body.since),since);assert.deepEqual(body.exclude_message_ids,['old']);assert.equal(body.wait_seconds,0);
    return {ok:true,json:async()=>hit};
  }});
  assert.equal(result.code,'002026');
  await assert.rejects(pollCode({file,mailbox:hit.mailbox,since,timeoutMs:1000,fetchImpl:async()=>({ok:false,status:422,json:async()=>({error:'code_ambiguous',retryable:false})})}),/code_pool_read_rejected/);
  let calls=0;
  const limited=async()=>{calls++;return {ok:false,status:429,json:async()=>({error:'upstream_rate_limited',retryable:true,retry_after_seconds:1})};};
  assert.equal(await pollCode({file,mailbox:hit.mailbox,since,timeoutMs:20,fetchImpl:limited}),null);
  assert.equal(await pollCode({file,mailbox:hit.mailbox,since,timeoutMs:20,fetchImpl:limited}),null);
  assert.equal(calls,1,'a new waiting window bypassed Retry-After');
  fs.writeFileSync(file,JSON.stringify({baseUrl:'https://example.com',apiToken:'secret'}));
  await assert.rejects(pollCode({file,mailbox:hit.mailbox,since,timeoutMs:1000,fetchImpl:async()=>assert.fail('unexpected network')}),/code_pool_config_invalid/);
});
