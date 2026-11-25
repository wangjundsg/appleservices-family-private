'use strict';
const {test}=require('node:test');
const assert=require('node:assert/strict');
const {verifyMailbox,safeError}=require('./mailbox-verify.cjs');
test('mail-only permission is verified on the addressed mailbox, without profile requests',async()=>{
  let refreshed=0;
  const mail={token:async()=>{refreshed++;},json:async(url,init)=>{
    const parsed=new URL(url);
    assert.equal(decodeURIComponent(parsed.pathname),'/v1.0/users/fixture+tag@hotmail.com/messages');
    assert.equal(parsed.searchParams.get('$top'),'1');
    assert.equal(init.headers.Authorization,'Bearer fixture-token');
    return {value:[{id:'fixture-message',toRecipients:[{emailAddress:{address:'FIXTURE+TAG@hotmail.com'}}],body:{content:'private content 001234'}}]};
  }};
  const result=await verifyMailbox(mail,{access_token:'fixture-token'},'fixture+tag@hotmail.com');
  assert.equal(refreshed,1);assert.equal(result.addressedMailboxRead,true);assert.equal(result.bodyObserved,true);
  assert.equal(result.recipientMatched,true);assert.equal(result.newVerificationCodeTested,false);
  assert.equal(JSON.stringify(result).includes('001234'),false);
});
test('empty mailbox does not become proof of observed body or received code',async()=>{
  const result=await verifyMailbox({token:async()=>{},json:async()=>({value:[]})},{access_token:'fixture'},'fixture@hotmail.com');
  assert.equal(result.bodyObserved,false);assert.equal(result.recipientMatched,false);assert.equal(result.newVerificationCodeTested,false);
});
test('token rejection stops before mail and does not retry or widen scope',async()=>{
  let calls=0;const failure=Object.assign(Error('private-token'),{code:'token_invalid'});
  await assert.rejects(verifyMailbox({token:async()=>{throw failure;},json:async()=>{calls++;}},{},'fixture@hotmail.com'),error=>error===failure);
  assert.equal(calls,0);assert.equal(safeError(failure),'token_invalid');
  assert.equal(safeError(Error('private-token')),'mailbox_verification_failed');
});
test('mail rejection is preserved without refresh retries; malformed success is refused',async()=>{
  let calls=0;const mail={token:async()=>{calls++;},json:async()=>{throw Object.assign(Error(),{code:'permission_denied'});}};
  await assert.rejects(verifyMailbox(mail,{},'fixture@hotmail.com'),{code:'permission_denied'});assert.equal(calls,1);
  mail.json=async()=>({value:[{id:'fixture'}]});
  await assert.rejects(verifyMailbox(mail,{},'fixture@hotmail.com'),{code:'upstream_response_invalid'});
});
