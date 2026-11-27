"use strict";
// Independent webpage collector. No registration fields, orders, solvers or account state.
const fs=require('node:fs'),path=require('node:path'),{parseArgs}=require('node:util');
const {observeCaptchaImages,pickCaptchaImage,sha256Hex}=require('./capture-lib.cjs');
const {refreshCaptcha}=require('./flow-mechanics.cjs');
const sleep=ms=>new Promise(resolve=>setTimeout(resolve,ms));

async function guard(page) {
  const pending=new Set();let failure='',blocked=0;
  const challenge=url=>/\/captcha(?:\?|$)/.test(url);
  await page.route('**/*',route=>{
    const req=route.request(),url=new URL(req.url());
    if(!['GET','HEAD','OPTIONS'].includes(req.method()) && /^\/account(?:\/|$)/.test(url.pathname)) {
      blocked++;failure='registration_request_blocked';return route.abort();
    }
    return route.continue();
  });
  page.on('request',req=>{if(challenge(req.url()))pending.add(req);});
  page.on('requestfinished',req=>pending.delete(req));
  page.on('requestfailed',req=>{if(pending.delete(req))failure='captcha_request_failed';});
  page.on('response',res=>{if(challenge(res.url()) && res.status()>=400)failure=`captcha_http_${res.status()}`;});
  return {pending:()=>pending.size>0,blocked:()=>blocked,
    check:()=>{if(failure)throw Error(failure);if(page.isClosed())throw Error('collector_page_closed');}};
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

async function collect(page,{out,count=5,intervalMs=5000,wait=sleep,network}) {
  if(!Number.isInteger(count)||count<1||count>500)throw Error('count_must_be_1_to_500');
  fs.mkdirSync(path.dirname(out),{recursive:true});
  fs.mkdirSync(out); // A fresh directory prevents accidental overwrite of an earlier dataset.
  const seen=new Set(),rows=[];
  try {
    for(let index=0;index<count;index++) {
      const {frame,image,size}=await imageOnPage(page,network);
      const source=await image.evaluate(img=>{
        if(/^data:image\/(png|jpeg);base64,/i.test(img.src))return img.src;
        const canvas=document.createElement('canvas');canvas.width=img.naturalWidth;canvas.height=img.naturalHeight;
        canvas.getContext('2d').drawImage(img,0,0);return canvas.toDataURL('image/png');
      });
      const match=/^data:image\/(png|jpeg);base64,([A-Za-z0-9+/=]+)$/.exec(source);
      if(!match)throw Error('captcha_image_format_invalid');
      const bytes=Buffer.from(match[2],'base64'),digest=sha256Hex(bytes);
      if(seen.has(digest))throw Error('captcha_image_repeated');
      const file=`${String(index+1).padStart(4,'0')}.${match[1]==='jpeg'?'jpg':'png'}`;
      fs.writeFileSync(path.join(out,file),bytes,{flag:'wx',mode:0o600});seen.add(digest);
      rows.push({file,sha256:digest,width:size[0],height:size[1]});
      console.log(JSON.stringify({at:'harvest',saved:rows.length}));
      if(rows.length<count) {
        await wait(Math.max(3000,intervalMs));network.check();
        const refreshed=await refreshCaptcha(frame,{check:network.check,pending:network.pending});
        if(!refreshed.changed)throw Error('captcha_refresh_unchanged');
      }
    }
    return {state:'harvested',saved:rows.length,blocked_registration_requests:network.blocked()};
  } finally {
    fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({labels_file:'labels.csv',samples:rows},null,2),{mode:0o600});
    fs.writeFileSync(path.join(out,'labels.csv'),'file,label,label_verified,sha256\n'+rows.map(r=>`${r.file},,false,${r.sha256}`).join('\n')+'\n',{mode:0o600});
  }
}

async function main(argv=process.argv.slice(2)) {
  const {values}=parseArgs({args:argv,options:{count:{type:'string',default:'5'},out:{type:'string'},headless:{type:'boolean',default:false}}});
  const count=Number(values.count);
  if(!Number.isInteger(count)||count<1||count>500)throw Error('count_must_be_1_to_500');
  const out=path.resolve(values.out||`.cache/captcha-harvest-${Date.now()}`);
  if(fs.existsSync(out))throw Error('output_directory_already_exists');
  const {chromium}=require(path.resolve(__dirname,'../../.tools/js/node_modules/playwright-core'));
  const browser=await chromium.launch({channel:'chrome',headless:values.headless});
  try {
    const context=await browser.newContext({locale:'zh-CN',viewport:{width:1280,height:900},serviceWorkers:'block'});
    const page=await context.newPage(),network=await guard(page);
    await page.goto('https://account.apple.com/account',{waitUntil:'domcontentloaded',timeout:60000});
    const result=await collect(page,{out,count,network});
    console.log(JSON.stringify({...result,out}));return result;
  } finally {await browser.close();}
}
if(require.main===module)main().catch(error=>{console.error(JSON.stringify({state:'failed',error:error.message}));process.exitCode=1;});
module.exports={main,collect,guard,imageOnPage};
