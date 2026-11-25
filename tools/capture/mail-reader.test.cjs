'use strict';
const test=require('node:test'), assert=require('node:assert/strict');
const fs=require('node:fs'), os=require('node:os'), path=require('node:path');
const {readCode}=require('./mail-reader.cjs');
const mailbox='owner@hotmail.com', recipient='alias@icloud.com';
function fixture(t, data) {
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'mail-reader-'));
  t.after(()=>fs.rmSync(dir,{recursive:true,force:true}));
  const file=path.join(dir,'private.json'); fs.writeFileSync(file,JSON.stringify(data)); return file;
}
test('registration passes task scope to 8802 and keeps fixed refusal details', async t=>{
  const file=fixture(t,{baseUrl:'http://127.0.0.1:8802',apiToken:'fixture'});
  const since=Date.now()-5000, until=Date.now();
  const options={reader:'outlook',file,mailbox,recipient:mailbox,source:'registration',since,until,excluded:['seen'],timeoutMs:1000};
  const result=await readCode({...options,fetchImpl:async(url,init)=>{
    const body=JSON.parse(init.body);
    assert.equal(body.recipient,mailbox); assert.equal(body.source,'registration'); assert.equal(body.platform,'apple');
    assert.equal(Date.parse(body.until),until); assert.deepEqual(body.exclude_message_ids,['seen']);
    return {ok:true,json:async()=>({ok:true,code:'002026',mailbox,message_id:'new',received_at:new Date(since+1000).toISOString(),from:'appleid@id.apple.com'})};
  }});
  assert.equal(result.code,'002026');
  await assert.rejects(readCode({...options,fetchImpl:async()=>({ok:false,status:409,json:async()=>({error:'authorization_required'})})}),
    error=>error.code==='authorization_required' && error.status===409);
});
test('forwarded Graph mail matches alias, sender, window and ID without accepting inbox-addressed mail', async t=>{
  const file=fixture(t,{client_id:'fixture',refresh_token:'fixture',mailbox});
  const since=Date.now()-5000, until=Date.now();
  const message=(id,to,from='code@openai.com')=>({id,subject:'Your verification code',from:{emailAddress:{address:from}},
    toRecipients:[{emailAddress:{address:to}}],receivedDateTime:new Date(since+1000).toISOString(),body:{contentType:'text',content:'002026'}});
  const result=await readCode({reader:'graph',file,mailbox,recipient,source:'forwarded',platform:'openai',since,until,excluded:['seen'],timeoutMs:1000,
    fetchImpl:async url=>({ok:true,status:200,text:async()=>JSON.stringify(String(url).includes('/token')?{access_token:'fixture'}:
      {value:[message('wrong-inbox',mailbox),message('seen',recipient),message('wrong-platform',recipient,'code@apple.com'),message('match',recipient)]})})});
  assert.equal(result.code,'002026'); assert.equal(result.messageId,'match');
});
test('unsupported contracts and missing task constraints fail before network access', async()=>{
  const base={reader:'outlook',mailbox,recipient:mailbox,source:'registration',since:Date.now()-1000,
    fetchImpl:async()=>assert.fail('must not call network')};
  for(const change of [{source:'forwarded',recipient},{platform:'openai'}, {since:undefined}, {until:0},
    {excluded:['']},{recipient:'bad'},{source:'unknown'},{reader:'imap'},{timeoutMs:-1},{recipient}]) {
    await assert.rejects(readCode({...base,...change}));
  }
});
