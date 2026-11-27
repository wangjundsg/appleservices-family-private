"use strict";
const {test}=require("node:test"),assert=require("node:assert/strict");
const fs=require("node:fs"),path=require("node:path");
const {readChallenge}=require("./captcha-session.cjs");
const source=fs.readFileSync(path.join(__dirname,"create-account.cjs"),"utf8");
function remote(result) {
  const start=source.indexOf("  const remoteCommand="),end=source.indexOf("  const result=await captchaSession.readChallenge",start);
  const calls=[];
  const lib={runCaptchaCommandAsync:async(cmd,png,options)=>{calls.push({cmd,png,...options});return result;}};
  const recognize=new Function("captchaRemoteOnly","captchaCmdFlag","captchaFallbackFlag","deadline","nativeRemoteCaptchas","lib","note",
    source.slice(start,end)+";return recognizeRemote;")(true,"remote-fixture","unused-local",Date.now()+240000,new Set(),lib,()=>{});
  return {recognize,calls};
}
test("native remote policy reads immediately, with no preliminary refresh or local model",async()=>{
  const reader=remote({ok:true,text:"aB3D",error:"",ms:1});let refreshes=0;
  const result=await readChallenge({capture:async()=>({png:"fixture.png",digest:"one"}),recognize:reader.recognize,
    clear:()=>{},refresh:async()=>{refreshes++;}});
  assert.equal(result.answer,"aB3D");assert.equal(refreshes,0);
  assert.equal(reader.calls.length,1);assert.equal(reader.calls[0].cmd,"remote-fixture");
  assert.equal(reader.calls[0].timeoutMs,75000);
  assert.match(source,/recognize: captchaRemoteOnly \? recognizeRemote/);
  assert.match(source,/fallback: captchaRemoteOnly \? null : recognizeRemote/);
});
test("remote transport failures stop without another refresh",async()=>{
  for(const error of ["http_403","network_error","missing_key","solver_timeout"]) {
    const reader=remote({ok:false,text:"",error,ms:1});let refreshes=0;
    await assert.rejects(readChallenge({capture:async()=>({png:"fixture.png",digest:"one"}),recognize:reader.recognize,
      clear:()=>{},refresh:async()=>{refreshes++;}}),/captcha_remote_unavailable/);
    assert.equal(refreshes,0);assert.equal(reader.calls.length,1);
  }
});
test("an unreadable remote answer has at most three refreshes and four reads",async()=>{
  const reader=remote({ok:false,text:"",error:"solver_failed",ms:1});let captures=0,refreshes=0;
  await assert.rejects(readChallenge({capture:async()=>({png:"fixture.png",digest:String(++captures)}),recognize:reader.recognize,
    clear:()=>{},refresh:async()=>{refreshes++;}}),/captcha_refresh_limit/);
  assert.equal(refreshes,3);assert.equal(reader.calls.length,4);
});
test("remote-only reads never pay again for a previously submitted image",async()=>{
  const reader=remote({ok:true,text:"aB3D",error:"",ms:1}),image={png:"fixture.png",digest:"same"};
  await reader.recognize(image);
  await assert.rejects(reader.recognize(image),/captcha_image_stuck/);
  assert.equal(reader.calls.length,1);
});
