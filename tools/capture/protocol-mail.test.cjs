'use strict';
const {test}=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),os=require('node:os'),path=require('node:path');
const {receive}=require('./protocol-mail.cjs');
test('task-bound code stays in a private result and exact scope reaches the shared reader',async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'protocol-mail-'));
 try{
  const request={challenge_id:'a'.repeat(32),output:path.join(dir,'result.json'),reader:'outlook',file:'private-config',mailbox:'fixture@hotmail.com',since:Date.now()-5000,excluded:['seen']};
  let options;
  const reply=await receive(request,async input=>{options=input;return {code:'001234',messageId:'new',created_at:new Date().toISOString()};});
  assert.deepEqual(reply,{found:true});assert.equal(options.recipient,request.mailbox);assert.equal(options.source,'registration');assert.equal(options.platform,'apple');assert.equal(options.since,request.since);assert.deepEqual(options.excluded,['seen']);
  const saved=JSON.parse(fs.readFileSync(request.output));assert.equal(saved.code,'001234');assert.equal(saved.challenge_id,request.challenge_id);
  await assert.rejects(receive(request,async()=>({code:'999999',messageId:'other',created_at:new Date().toISOString()})),{code:'EEXIST'});
  assert.equal(JSON.parse(fs.readFileSync(request.output)).code,'001234');
 }finally{fs.rmSync(dir,{recursive:true,force:true});}
});
test('no code, old mail, consumed mail and malformed code never produce a result',async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'protocol-mail-'));
 try{
  const request={challenge_id:'b'.repeat(32),output:path.join(dir,'result.json'),mailbox:'fixture@hotmail.com',since:Date.now()-5000,excluded:['seen']};
  assert.deepEqual(await receive(request,async()=>null),{found:false});
  for(const hit of [{code:'001234',messageId:'old',created_at:new Date(request.since-1).toISOString()},
    {code:'001234',messageId:'seen',created_at:new Date().toISOString()},
    {code:1234,messageId:'new',created_at:new Date().toISOString()}])await assert.rejects(receive(request,async()=>hit),/protocol_mail_response_invalid/);
  assert.equal(fs.existsSync(request.output),false);
 }finally{fs.rmSync(dir,{recursive:true,force:true});}
});
