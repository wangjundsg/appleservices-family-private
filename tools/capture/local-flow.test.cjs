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


test("unavailable mailbox messages are distinct from format, network, and code errors",()=>{
  for(const text of [
    "This email address is not available. Choose a different address.",
    "此电子邮件地址不可用。请选择其他地址。",
    "此电子邮件地址无法使用。请选择其他电子邮件地址。",
    "此電子郵件地址無法使用。請選擇其他地址。",
  ]) assert.equal(flow.emailUnavailable(text),true,text);
  for(const text of ["Enter a valid email address.","验证码不正确", "当前无法获取 Captcha。",
    "Your account cannot be created at this time.", "Email service is not available.", "Verify your email address"]) {
    assert.equal(flow.emailUnavailable(text),false,text);
  }
});

test("inline mailbox rejection replaces on the current form before CAPTCHA and waits for validation",async()=>{
  const fs=require("node:fs"),vm=require("node:vm");
  const source=fs.readFileSync(require.resolve("./create-account.cjs"),"utf8");
  const start=source.indexOf("  async function ensureEmailAvailable(fr) {");
  assert.notEqual(start,-1,"driver needs an inline mailbox validation gate");
  const helper=source.slice(start,source.indexOf("  const fillForm =",start));
  let value="old@example.test",text="This email address is not available. Choose a different address.",pending=true;
  const events=[];
  const fr={locator:selector=>selector==='body'?{innerText:async()=>text}:{
    isVisible:async()=>true,inputValue:async()=>value,press:async()=>{events.push("blur");pending=true;}
  }};
  const ctx=vm.createContext({email:value,nativeMode:true,leaseDeadline:Infinity,flow,
    pendingEmailValidations:{get size(){return pending?1:0;}},
    mech:{...mech,waitForScreen:opts=>mech.waitForScreen({...opts,sleep:async()=>{if(pending && value==="new@example.test")text="";pending=false;}})},
    sleep:async()=>{},note:()=>{},blurField:async field=>field.press("Tab"),
    replaceEmail:async opts=>{assert.equal(opts.onForm,true);events.push("replace");ctx.email="new@example.test";return true;},
    fillIfNeeded:async(_field,next)=>{events.push("fill");value=next;}
  });
  vm.runInContext(helper+"globalThis.check=ensureEmailAvailable",ctx);
  await ctx.check(fr);events.push("captcha");
  assert.deepEqual(events,["replace","fill","blur","captcha"]);
  assert.equal(value,"new@example.test");
  text="This email address is not available. Choose a different address.";
  ctx.replaceEmail=async()=>false;
  await assert.rejects(ctx.check(fr),/email_unavailable/);
  let purchases=0;
  ctx.replaceEmail=async()=>{purchases++;return true;};
  ctx.mech={...mech,waitForScreen:async opts=>opts.observe()};
  await assert.rejects(ctx.check(fr),/email_unavailable/);
  assert.equal(purchases,3,"three continuously rejected replacements must stop without a fourth purchase");
  text="Email service is not available.";
  ctx.replaceEmail=async()=>{throw Error("must not purchase for a generic error");};
  await ctx.check(fr);
});


test("inline replacement reuses the mailbox handoff without Back, submission, or a new task",async()=>{
  const fs=require("node:fs"),vm=require("node:vm"),path=require("node:path");
  const source=fs.readFileSync(require.resolve("./create-account.cjs"),"utf8");
  const start=source.indexOf("  async function replaceEmail(");
  const helper=source.slice(start,source.indexOf("  async function waitForVerification",start));
  const outdir=path.resolve("fixture-existing-job"),order=path.join(outdir,"replacement-otpgmail-order.json");
  const events=[];
  const ctx=vm.createContext({nativeMode:true,emailReplacements:0,email:"old@gmail.com",otpGmailOrderFile:"old-order.json",
    usedEmailMessages:["old-message"],outdir,path,page:{},arg:()=>"handoff.json",
    widgetFrame:async()=>({locator:()=>({waitFor:async()=>{},count:async()=>1})}),
    mech:{findAction:async()=>{throw Error("must not go Back from editable form");}},
    note:(_kind,event)=>events.push(event.state),
    waitFor:async check=>check(),
    fs:{existsSync:()=>true,readFileSync:()=>JSON.stringify({previous_email:"old@gmail.com",email:"new@gmail.com",order_file:order})},
    submitCreateForm:async()=>{throw Error("must not recursively submit");},
  });
  vm.runInContext(helper+"globalThis.replace=replaceEmail",ctx);
  assert.equal(await ctx.replace({onForm:true}),true);
  assert.equal(ctx.email,"new@gmail.com");assert.equal(ctx.otpGmailOrderFile,order);
  assert.equal(ctx.usedEmailMessages.length,0);assert.equal(ctx.emailReplacements,1);
  assert.deepEqual(events,["replacement_email_required"]);
  for(let round=2;round<=3;round++) {
    const previous=ctx.email;
    const nextOrder=path.join(outdir,`replacement-email-${round}`,"replacement-otpgmail-order.json");
    let reads=0;
    ctx.fs.readFileSync=()=>JSON.stringify(++reads===1 ? {replacement:round-1} :
      {replacement:round,previous_email:previous,email:`round${round}@gmail.com`,order_file:nextOrder});
    ctx.waitFor=async check=>{assert.equal(await check(),"");return check();};
    assert.equal(await ctx.replace({onForm:true}),true);
    assert.equal(ctx.emailReplacements,round);
    assert.equal(ctx.otpGmailOrderFile,nextOrder);
  }
  assert.equal(await ctx.replace({onForm:true}),false,"stop after three replacements");
  const fill=source.slice(source.indexOf("  const fillForm ="),source.indexOf("  async function submitCreateForm"));
  assert.ok(fill.indexOf('await ensureEmailAvailable(fr)')>fill.indexOf('password, "password"'));
  assert.ok(fill.indexOf('await ensureEmailAvailable(fr)')<fill.indexOf('password, "confirm_password"'));
});


test("phone replacement consumes three distinct rounds and ignores stale handoffs",async()=>{
  const fs=require("node:fs"),vm=require("node:vm");
  const source=fs.readFileSync(require.resolve("./create-account.cjs"),"utf8");
  const helper=source.slice(source.indexOf("async function replacePhone(page)"),source.indexOf("async function waitFor(producer"));
  const events=[];
  const ctx=vm.createContext({phoneReplacements:0,nativeMode:true,activation:"1000",phone:"912345670",phoneCC:"PT",
    leaseDeadline:Date.now()+3600000,heroKey:"fixture",excludedSMS:new Set(),SMS_API_BASE:"fixture-api",
    arg:()=>"handoff.json",phoneAction:async()=>{},widgetFrame:async()=>({locator:()=>({waitFor:async()=>{}})}),
    mech:{PHONE_REPLACE_LABELS:[]},note:(_kind,e)=>{if(e.state)events.push(e.replacement);},
    captureSMSBaseline:async()=>{},selectIfNeeded:async()=>{},fillIfNeeded:async()=>{},
    oncePhoneAction:async(_key,fn)=>fn(),clickContinue:async()=>{},fs:{existsSync:()=>true},
  });
  vm.runInContext(helper+"globalThis.replace=replacePhone",ctx);
  for(let round=1;round<=3;round++) {
    const previous=ctx.activation;let reads=0;
    ctx.fs.readFileSync=()=>JSON.stringify(++reads===1?{replacement:0}:
      {replacement:round,previous_activation:previous,activation:String(1000+round),phone:String(912345670+round),phonecc:"PT",expires_at:new Date(Date.now()+3600000).toISOString()});
    ctx.waitFor=async check=>{if(round===1){reads=1;return check();}assert.equal(await check(),null);return check();};
    assert.equal(await ctx.replace({}),true);
    assert.equal(ctx.phoneReplacements,round);
  }
  assert.equal(await ctx.replace({}),false);
  assert.deepEqual(events,[1,2,3]);
});
