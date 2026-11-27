"use strict";
const fs=require('node:fs'),path=require('node:path'),{createHash,randomUUID}=require('node:crypto');

// Optional training evidence must never interrupt the browser. Candidates still
// require confirmed labels; matching source bytes only proves image pairing.
function record(outdir,image,answer) {
  try {
    if(!image || !/^[A-Za-z0-9]{4,6}$/.test(answer))return false;
    const bytes=fs.readFileSync(image.png),digest=createHash('sha256').update(bytes).digest('hex');
    if(digest!==image.digest)return false;
    const base=path.join(outdir,`captcha-sample-${randomUUID()}`);
    fs.writeFileSync(base+'.png',bytes,{mode:0o600});
    fs.writeFileSync(base+'.json',JSON.stringify({answer,image_sha256:digest,pairing:'source_before_submit',label_verified:false}),{mode:0o600});
    return true;
  } catch {return false;}
}
module.exports={record};
