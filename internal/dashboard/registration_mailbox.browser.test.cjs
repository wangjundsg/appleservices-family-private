"use strict";
const {test}=require("node:test"),assert=require("node:assert/strict");
const fs=require("node:fs"),path=require("node:path");
const chrome="C:/Program Files/Google/Chrome/Application/chrome.exe";
test("panel starts without a pre-bought mailbox, saves policy, and resumes the same mailbox job",{skip:!fs.existsSync(chrome)},async()=>{
  const {chromium}=require(path.resolve(__dirname,"../../.tools/js/node_modules/playwright-core"));
  const browser=await chromium.launch({executablePath:chrome,headless:true});
  try {
    const page=await browser.newPage(),errors=[];page.on("pageerror",error=>errors.push(error.message));
    let settings={mailProvider:"otpgmail",smsProvider:"hero",credentialFile:"C:/fixture/key.dpapi",orderFile:"",mailWarehouse:2,mailMaxPriceVnd:700,
      phoneCountry:"PL",durationHours:0,proxyProfile:"current",proxyPort:10005,localBrowserProxy:"http://127.0.0.1:7897"};
    let job=null,starts=0,resumes=0;
    await page.route("**/*",async route=>{
      const request=route.request(),url=new URL(request.url());
      assert.equal(url.host,"fixture.invalid","tests must never contact providers");
      const name=url.pathname;
      if(name==="/")return route.fulfill({contentType:"text/html",body:fs.readFileSync(path.join(__dirname,"web/index.html"),"utf8")
        .replace(/<script src="\/(?:app|cn-family)\.js" defer><\/script>/g,"")});
      if(["/app.css","/register.css","/register.js"].includes(name))return route.fulfill({contentType:name.endsWith("js")?"application/javascript":"text/css",body:fs.readFileSync(path.join(__dirname,"web",name.slice(1)),"utf8")});
      let value;
      if(name==="/api/register/config")value={purchaseEnabled:true,budgetConfigured:true,mailboxConfigured:true,smsConfigured:true,resources:settings,familyOrganizers:[]};
      else if(name==="/api/register/resources") {
        if(request.method()==="POST")settings=request.postDataJSON().settings;
        value={settings,purchase:{maxPrice:"0.2",maxTotal:"0.4"},proxyProfiles:[{id:"current",name:"Fixture"}],purchaseEnabled:true,
          orderConfigured:false,orderMessage:"未绑定已有邮箱订单；任务内购买",smsCredentialLoaded:true,proxyConfigured:true};
      } else if(name==="/api/register" && request.method()==="POST") {
        starts++;assert.equal(request.postDataJSON().automatic,true);
        job={jobId:"reg-20261128-abcdefabcdefabcd",stage:"mailbox",state:"needs_review",browserMode:"local",automatic:true,active:false,startedAt:new Date().toISOString(),message:"邮箱订单仍在分配中"};
        value={jobId:job.jobId};
      } else if(name.endsWith("/continue-mailbox")) {
        resumes++;assert.equal(name,"/api/register/"+job.jobId+"/continue-mailbox");
        job={...job,active:true,state:"running"};value={ok:true};
      } else if(name.endsWith("/log"))value=[];
      else if(name==="/api/register")value={jobs:job?[job]:[]};
      else throw Error("unexpected fixture route "+name);
      await route.fulfill({contentType:"application/json",body:JSON.stringify(value)});
    });
    await page.goto("http://fixture.invalid/");
    await page.locator("#resourceWorkbench summary").click();
    await page.locator('#resourceForm [name=mailWarehouse]').selectOption("4");
    await page.locator('#resourceForm [name=mailMaxPriceVnd]').fill("600");
    await page.locator('#resourceForm button[type=submit]').click();
    await page.waitForFunction(()=>document.getElementById("resourceNotice").textContent.includes("已保存"));
    assert.equal(settings.mailWarehouse,4);assert.equal(settings.mailMaxPriceVnd,600);assert.equal(starts,0);
    await page.locator("#registerToggle").click();
    await page.locator('#registerForm [name=familyRole]').selectOption("none");
    await page.locator('#registerForm [name=mode]').selectOption("direct");
    await page.locator('#registerForm [name=acceptTerms]').check();
    assert.equal(await page.locator("#registerSubmit").isEnabled(),true);
    await page.locator("#registerSubmit").click();
    await page.locator("#registerContinueMailbox").waitFor({state:"visible"});
    assert.match(await page.locator("#registerState").innerText(),/购买邮箱/);
    await page.locator("#registerContinueMailbox").click();
    await page.locator("#registerContinueMailbox").waitFor({state:"hidden"});
    assert.equal(starts,1);assert.equal(resumes,1);assert.deepEqual(errors,[]);
  } finally {await browser.close();}
});
