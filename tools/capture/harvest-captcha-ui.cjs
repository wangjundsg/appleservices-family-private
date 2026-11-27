"use strict";
// Independent webpage collector. No registration fields, orders, solvers or account state.
const fs=require('node:fs'),path=require('node:path'),{parseArgs}=require('node:util');
const {observeCaptchaImages,pickCaptchaImage}=require('./capture-lib.cjs');
const {refreshCaptcha}=require('./flow-mechanics.cjs');
const {control,runWorkers}=require('./harvest-pool.cjs');
const sleep=ms=>new Promise(resolve=>setTimeout(resolve,ms));

async function guard(page,controller=control()) {
  const pending=new Set();let failure='',blocked=0;
  const challenge=url=>/\/captcha(?:\?|$)/.test(url);
  await page.route('**/*',async route=>{
    const req=route.request(),url=new URL(req.url());
    if(!['GET','HEAD','OPTIONS'].includes(req.method()) && /^\/account(?:\/|$)/.test(url.pathname)) {
      blocked++;failure='registration_request_blocked';controller.stop(Error(failure));return route.abort();
    }
    if(challenge(req.url())){try{await controller.turn();}catch{return route.abort();}}
    return route.continue();
  });
  page.on('request',req=>{if(challenge(req.url()))pending.add(req);});
  page.on('requestfinished',req=>pending.delete(req));
  page.on('requestfailed',req=>{if(pending.delete(req)){failure='captcha_request_failed';controller.stop(Error(failure));}});
  page.on('response',res=>{if(challenge(res.url()) && res.status()>=400){failure=`captcha_http_${res.status()}`;if(res.status()!==503)controller.stop(Error(failure));}});
  return {pending:()=>pending.size>0,blocked:()=>blocked,
    check:()=>{controller.check();if(failure)throw Error(failure);if(page.isClosed())throw Error('collector_page_closed');}};
}

async function imageOnPage(page,network,timeoutMs=45000) {
  const deadline=Date.now()+timeoutMs;
  while(Date.now()<deadline) {
    network.check();
    if(!network.pending())for(const frame of page.frames()) {
      const state=await observeCaptchaImages(frame).catch(()=>null);
      if(!state?.scopePresent)continue;
      if(state.unavailable)throw Error('captcha_service_unavailable');
      const picked=pickCaptchaImage(state);
      if(picked.ok)return {frame,image:frame.locator('img').nth(picked.index),size:picked.natural};
    }
    await sleep(200);
  }
  throw Error('captcha_image_timeout');
}

function pageWorker(page,network,start) {
  return {network,start,capture:async()=>{
      const {image,size}=await imageOnPage(page,network);
      const source=await image.evaluate(img=>{
        if(/^data:image\/(png|jpeg);base64,/i.test(img.src))return img.src;
        const canvas=document.createElement('canvas');canvas.width=img.naturalWidth;canvas.height=img.naturalHeight;
        canvas.getContext('2d').drawImage(img,0,0);return canvas.toDataURL('image/png');
      });
      const match=/^data:image\/(png|jpeg);base64,([A-Za-z0-9+/=]+)$/.exec(source);
      if(!match)throw Error('captcha_image_format_invalid');
      return {bytes:Buffer.from(match[2],'base64'),ext:match[1]==='jpeg'?'jpg':'png',width:size[0],height:size[1]};
    },refresh:async()=>{
        const {frame}=await imageOnPage(page,network);
        const refreshed=await refreshCaptcha(frame,{check:network.check,pending:network.pending});
        if(!refreshed.changed)throw Error('captcha_refresh_unchanged');
    }};
}
async function openPage(page,controller) {
  await controller.turn();
  const response=await page.goto('https://account.apple.com/account',{waitUntil:'domcontentloaded',timeout:60000});
  if(response?.status()===503)throw Error('page_http_503');
}
const recoverableWindowError=error=>['captcha_http_503','page_http_503','captcha_image_timeout'].includes(error?.message);
function browserWorker(browser,controller) {
  let context,page,network={check(){},blocked:()=>0},worker;
  const reset=async()=>{
    await context?.close().catch(()=>{});
    context=await browser.newContext({locale:'zh-CN',viewport:{width:1280,height:900},serviceWorkers:'block'});
    page=await context.newPage();network=await guard(page,controller);
    worker=pageWorker(page,network,()=>openPage(page,controller));
  };
  return {get network(){return network;},start:async()=>{await reset();await worker.start();},
    capture:()=>worker.capture(),refresh:()=>worker.refresh(),
    recover:async error=>{
      if(!recoverableWindowError(error))return false;
      while(recoverableWindowError(error)) {
        console.log(JSON.stringify({at:'worker-recycle',reason:error.message}));
        await reset();
        try {await worker.start();return true;} catch(next) {error=next;}
      }
      throw error;
    }};
}
async function collect(page,options) {
  return runWorkers([pageWorker(page,options.network)],options);
}

async function main(argv=process.argv.slice(2)) {
  const {values}=parseArgs({args:argv,options:{count:{type:'string',default:'5'},out:{type:'string'},windows:{type:'string',default:'3'},resume:{type:'boolean',default:false},headless:{type:'boolean',default:false}}});
  const count=Number(values.count);
  const windows=Number(values.windows);
  if(!Number.isInteger(count)||count<1||count>10000)throw Error('count_must_be_1_to_10000');
  if(!Number.isInteger(windows)||windows<1||windows>6)throw Error('windows_must_be_1_to_6');
  const out=path.resolve(values.out||`.cache/captcha-harvest-${Date.now()}`);
  if(fs.existsSync(out)&&!values.resume)throw Error('output_directory_already_exists');
  const {chromium}=require(path.resolve(__dirname,'../../.tools/js/node_modules/playwright-core'));
  const browser=await chromium.launch({channel:'chrome',headless:values.headless});
  try {
    const controller=control(),workers=[];
    for(let i=0;i<Math.min(windows,count);i++) {
      workers.push(browserWorker(browser,controller));
    }
    const result=await runWorkers(workers,{out,count,resume:values.resume,controller});
    console.log(JSON.stringify({...result,out}));return result;
  } finally {await browser.close();}
}
if(require.main===module)main().catch(error=>{console.error(JSON.stringify({state:'failed',error:error.message}));process.exitCode=1;});
module.exports={main,collect,guard,imageOnPage,pageWorker,browserWorker,openPage,recoverableWindowError};
