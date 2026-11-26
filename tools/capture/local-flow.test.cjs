"use strict";
const {test}=require("node:test");
const assert=require("node:assert/strict");
const mech=require("./flow-mechanics.cjs");
const flow=require("./account-flow.cjs");

for(const channel of ["email", "phone"]) {
  test(`${channel}: local policy waits three minutes, resends once, then requests replacement`,async()=>{
    let now=0,resends=0;const windows=[];
    const out=await mech[`${channel}CodeWindow`]({resendEnabled:true,firstMs:180000,secondMs:180000,now:()=>now,
      waitCode:async ms=>{windows.push(ms);now+=ms;return null;},
      resend:async()=>{resends++;return {asked:true,ok:true};}});
    assert.deepEqual(windows,[180000,180000]);assert.equal(resends,1);
    assert.equal(out.replace,true);assert.equal(out.waitedMs,360000);
  });
}
test("Chinese and English delivery headings beat a quoted email address on phone screens",()=>{
  for(const text of ["验证电话号码 请输入短信验证码 owner@example.test", "Verify your phone number Enter the code for owner@example.test"]) {
    assert.equal(flow.classifyScreen({text,email:"owner@example.test"}),"phone_step");
  }
  for(const text of ["验证电子邮件地址 输入验证码", "驗證電子郵件地址 輸入驗證碼", "Verify your email address Enter code"]) {
    assert.equal(flow.classifyScreen({text}),"email_step");
  }
});
test("paced dropdown anchors at the nearer end and settles before leaving",async()=>{
  const keys=[],waits=[];
  const locator={evaluate:async()=>({index:10,current:8,total:12}),press:async key=>keys.push(key)};
  await mech.selectByKeyboard(locator,"11",{sleep:async ms=>waits.push(ms),stepMs:100,settleMs:300});
  assert.deepEqual(keys,["End","ArrowUp","Enter"]);
  assert.deepEqual(waits,[100,100,300]);
});
test("a user who advanced the page is never followed by a resend or replacement",async()=>{
  const result=await mech.emailCodeWindow({resendEnabled:true,firstMs:180000,secondMs:180000,
    waitCode:async()=>({advanced:true}),resend:async()=>{throw Error("must not resend");}});
  assert.equal(result.advanced,true);assert.equal(result.timedOut,false);assert.equal(result.replace,false);
});
test("manual CAPTCHA progression requires actual next-step inputs, never a transient missing image",()=>{
  assert.equal(flow.captchaAdvanced({state:"email_step",creationFormPresent:true,codeInputs:6}),false);
  assert.equal(flow.captchaAdvanced({state:"email_step",captchaPresent:true,codeInputs:6}),false);
  assert.equal(flow.captchaAdvanced({state:"email_step",codeInputs:0}),false);
  assert.equal(flow.captchaAdvanced({state:"unknown",codeInputs:6}),false);
  assert.equal(flow.captchaAdvanced({state:"email_step",codeInputs:6}),true);
  assert.equal(flow.captchaAdvanced({state:"phone_step",codeInputs:6}),true);
});
