"use strict";
const {test}=require("node:test");
const assert=require("node:assert/strict");
const {readChallenge}=require("./captcha-session.cjs");
test("unreadable OCR refreshes at most three times, never submits an empty answer",async()=>{
  let captures=0,refreshes=0,clears=0;
  await assert.rejects(readChallenge({capture:async()=>({digest:String(++captures)}),
    recognize:async()=>"",refresh:async()=>{refreshes++;},clear:()=>{clears++;}}),/captcha_refresh_limit/);
  assert.equal(captures,4);assert.equal(refreshes,3);assert.equal(clears,7);
});
test("same image after refresh stops before another OCR call",async()=>{
  let reads=0;
  await assert.rejects(readChallenge({capture:async()=>({digest:"same"}),
    recognize:async()=>{reads++;return "";},refresh:async()=>{},clear:()=>{}}),/captcha_image_stuck/);
  assert.equal(reads,1);
});
test("manual page progression wins over a stale answer",async()=>{
  const result=await readChallenge({progress:async()=>true,capture:async()=>{throw Error("should not capture");},clear:()=>{}});
  assert.deepEqual(result,{advanced:true});
});
test("manual progression during OCR discards the now-stale OCR result",async()=>{
  let advanced=false;
  const result=await readChallenge({progress:async()=>advanced,capture:async()=>({digest:"one"}),clear:()=>{},
    recognize:async()=>{advanced=true;return "Ab3D";}});
  assert.deepEqual(result,{advanced:true});
});
test("refresh request is never interpreted as CAPTCHA text",async()=>{
  let count=0,refreshes=0;
  const result=await readChallenge({capture:async()=>({digest:String(++count)}),
    manual:async()=>count===1?{refresh:true}:{answer:"Ab3D"},refresh:async()=>{refreshes++;},clear:()=>{}});
  assert.equal(result.answer,"Ab3D");assert.equal(refreshes,1);
});
test("a damaged image is retried, unrelated failures stop immediately",async()=>{
  let captures=0;
  const result=await readChallenge({capture:async()=>{if(!captures++)throw Object.assign(Error("not loaded"),{captchaUnavailable:true});return {digest:"ok"};},
    recognize:async()=>"Ab3D",refresh:async()=>{},clear:()=>{}});
  assert.equal(result.answer,"Ab3D");
  await assert.rejects(readChallenge({capture:async()=>{throw Error("browser closed");},clear:()=>{}}),/browser closed/);
});
test("remote fallback runs only after three local refreshes; failure starts another bounded round",async()=>{
  let captures=0,refreshes=0,remotes=0;const order=[];
  const result=await readChallenge({capture:async()=>({digest:String(++captures)}),clear:()=>{},
    recognize:async()=>{order.push("local");return "";},refresh:async()=>{refreshes++;order.push("refresh");},
    fallback:async()=>{remotes++;order.push("remote");return remotes===2?"aB3D":"";}});
  assert.equal(result.answer,"aB3D");assert.equal(refreshes,6);assert.equal(remotes,2);assert.equal(captures,7);
  assert.deepEqual(order.slice(0,8),["local","refresh","local","refresh","local","refresh","local","remote"]);
});
test("remote failure is bounded and never requests human input",async()=>{
  let captures=0,remotes=0;
  await assert.rejects(readChallenge({capture:async()=>({digest:String(++captures)}),clear:()=>{},recognize:async()=>"",refresh:async()=>{},
    fallback:async()=>{remotes++;return "";},manual:()=>{throw Error("must not wait for a person");},maxRounds:2}),/captcha_attempt_limit/);
  assert.equal(remotes,2);
});

test("unchanged refreshes reach remote fallback after three attempts without repeating local OCR",async()=>{
  let refreshes=0,locals=0,remotes=0;
  const result=await readChallenge({capture:async()=>({digest:"same"}),clear:()=>{},
    recognize:async()=>{locals++;return "";},refresh:async()=>{refreshes++;return {changed:false};},
    fallback:async()=>{remotes++;return "Ab3D";}});
  assert.equal(result.answer,"Ab3D");assert.equal(refreshes,3);assert.equal(locals,1);assert.equal(remotes,1);
});

test("a failed fallback never pays again for the same stuck image",async()=>{
  let refreshes=0,remotes=0;
  await assert.rejects(readChallenge({capture:async()=>({digest:"same"}),clear:()=>{},recognize:async()=>"",
    refresh:async()=>{refreshes++;return {changed:false};},fallback:async()=>{remotes++;return "";}}),/captcha_image_stuck/);
  assert.equal(refreshes,6);assert.equal(remotes,1);
});

test("remote image budget is shared across challenge invocations",async()=>{
  const remoteDigests=new Set(["already-paid"]);let remotes=0;
  await assert.rejects(readChallenge({remoteDigests,capture:async()=>({digest:"already-paid"}),clear:()=>{},
    recognize:async()=>"",refresh:async()=>({changed:false}),fallback:async()=>{remotes++;return "Ab3D";}}),/captcha_image_stuck/);
  assert.equal(remotes,0);
});
