"use strict";
const {test}=require("node:test"),assert=require("node:assert/strict");
const {EventEmitter}=require("node:events");
const fs=require("node:fs"),path=require("node:path");
const flow=require("./account-flow.cjs");
function driver() {
  const source=fs.readFileSync(path.join(__dirname,"create-account.cjs"),"utf8");
  const start=source.indexOf("  // Review A2: this listener"),end=source.indexOf("  // Screenshot helper.",start);
  const page=new EventEmitter();
  const check=new Function("page","flow","nativeMode","pendingCaptchaRequests","note",source.slice(start,end)+";return checkCreationFailure;")(page,flow,false,new Set(),()=>{});
  const request=(endpoint="/account/validate")=>{
    const req={url:()=>"https://fixture.invalid"+endpoint,method:()=>"POST"};page.emit("request",req);return req;
  };
  const respond=(req,status,body="{}")=>page.emit("response",{url:req.url,request:()=>req,status:()=>status,text:async()=>body});
  return {check,request,respond};
}
test("successful corrected validation replaces old failures in the actual driver",async()=>{
  const d=driver();
  for(const status of [400,400,200])d.respond(d.request(),status);
  await assert.doesNotReject(d.check());
});
test("an older response body resolving late cannot restore a superseded failure",async()=>{
  const d=driver();let finish;
  d.respond(d.request(),400,new Promise(resolve=>{finish=resolve;}));
  d.respond(d.request(),200);finish("{}");
  await assert.doesNotReject(d.check());
});
test("responses arriving out of request order use the newest validation request",async()=>{
  const d=driver(),older=d.request(),newer=d.request();
  d.respond(newer,200);d.respond(older,400);
  await assert.doesNotReject(d.check());
});
test("latest validation errors and final account rejection still stop the driver",async()=>{
  for(const final of [false,true]) {
    const d=driver();d.respond(d.request(),200);
    d.respond(d.request(final?"/account":"/account/validate"),400);
    if(final)d.respond(d.request(),200);
    await assert.rejects(d.check(),/creation_rejected/);
  }
});
test("HTTP 200 with a business error cannot clear a failure",async()=>{
  const d=driver();d.respond(d.request(),400);
  d.respond(d.request(),200,JSON.stringify({errorCode:"-34607001"}));
  await assert.rejects(d.check(),/creation_rejected/);
});
