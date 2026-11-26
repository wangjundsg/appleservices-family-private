"use strict";
const fail=reason=>Object.assign(new Error(reason),{state:"unknown",closeReason:"captcha-exhausted",evidence:reason});

// One answer belongs to one image. Three consecutive refreshes is a hard cap.
async function readChallenge({capture,recognize,fallback,manual,refresh,clear,progress=async()=>false,log=()=>{},available=()=>true,maxRounds=4}) {
  let previous="";
  const rounds=fallback ? Math.max(1,Math.min(4,maxRounds)) : 1;
  for(let round=0;round<rounds;round++) {
    let image;
    if(round>0) {
      if(!available())throw fail("captcha_attempt_deadline");
      if(await progress())return {advanced:true};
      await clear();await refresh();
      log({at:"captcha-refreshed",round:round+1,refreshes:1,refresh_limit:3});
    }
    for(let refreshes=round>0?1:0;refreshes<=3;refreshes++) {
    if(!available())throw fail("captcha_attempt_deadline");
    if(await progress()) return {advanced:true};
    await clear();
    let result;
    image=null;
    try { image=await capture(); }
    catch(error) { if(!error.captchaUnavailable)throw error; }
    if(image) {
      if(previous && previous===image.digest)throw fail("captcha_image_stuck");
      previous=image.digest;
      log({at:"captcha-ready",round:round+1,refreshes,refresh_limit:3});
      if(recognize) {
        const answer=await recognize(image);
        if(await progress())return {advanced:true};
        if(/^[A-Za-z0-9]{4,6}$/.test(answer))return {answer};
      } else {
        result=await manual(image);
        if(result?.advanced)return {advanced:true};
        if(/^[A-Za-z0-9]{3,6}$/.test(result?.answer||""))return result;
        if(!result?.refresh)throw fail("captcha_answer_timeout");
      }
    }
    if(refreshes===3)break;
    if(await progress())return {advanced:true};
    await clear();
    await refresh();
    log({at:"captcha-refreshed",round:round+1,refreshes:refreshes+1,refresh_limit:3});
    }
    if(fallback && image && available()) {
      log({at:"captcha-remote-fallback",round:round+1});
      const answer=await fallback(image);
      if(await progress())return {advanced:true};
      if(/^[A-Za-z0-9]{4,6}$/.test(answer))return {answer};
    }
  }
  throw fail(fallback ? "captcha_attempt_limit" : "captcha_refresh_limit");
}
module.exports={readChallenge};
