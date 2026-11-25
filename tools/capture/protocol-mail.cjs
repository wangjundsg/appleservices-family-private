'use strict';
// Private subprocess bridge: OTPs go only to a task file, never stdout/stderr.
const fs=require('node:fs');
const {readCode}=require('./mail-reader.cjs');
const SAFE_ERRORS=new Set(['code_ambiguous','code_unrecognized','graph_read_failed','mail_deadline_exceeded',
  'token_invalid','authorization_required','permission_denied','unauthorized','resource_busy','upstream_rate_limited',
  'mailbox_unknown','mail_time_range_invalid','code_pool_config_unavailable','code_pool_config_invalid']);
async function receive(request,reader=readCode) {
  if (!request || !/^[a-f0-9]{32}$/.test(request.challenge_id) || typeof request.output!=='string') throw Error('protocol_mail_request_invalid');
  const hit=await reader({reader:request.reader,file:request.file,mailbox:request.mailbox,recipient:request.mailbox,
    source:'registration',platform:'apple',since:request.since,excluded:request.excluded,timeoutMs:8000});
  if (!hit) return {found:false};
  if (typeof hit.code!=='string'||!/^\d{6}$/.test(hit.code)||typeof hit.messageId!=='string'||!hit.messageId||
      !Number.isFinite(Date.parse(hit.created_at))||Date.parse(hit.created_at)<=request.since||
      Date.parse(hit.created_at)>Date.now()+60000||(request.excluded||[]).includes(hit.messageId)) throw Error('protocol_mail_response_invalid');
  fs.writeFileSync(request.output,JSON.stringify({challenge_id:request.challenge_id,code:hit.code,
    message_id:hit.messageId,received_at:hit.created_at}),{flag:'wx',mode:0o600});
  return {found:true};
}
if(require.main===module){
  (async()=>{
    try{
      const input=fs.readFileSync(0,'utf8');
      if(Buffer.byteLength(input)>65536)throw Error('protocol_mail_request_invalid');
      console.log(JSON.stringify(await receive(JSON.parse(input))));
    }catch(error){
      console.log(JSON.stringify({found:false,error:SAFE_ERRORS.has(error.code||error.message)?error.code||error.message:'protocol_mail_read_failed'}));
      process.exitCode=1;
    }
  })();
}
module.exports={receive};
