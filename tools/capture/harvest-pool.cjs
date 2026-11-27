"use strict";
const fs=require('node:fs'),path=require('node:path');
const {sha256Hex}=require('./capture-lib.cjs');
const sleep=ms=>new Promise(resolve=>setTimeout(resolve,ms));
function atomic(file,value){const tmp=file+'.tmp';fs.writeFileSync(tmp,value,{mode:0o600});fs.renameSync(tmp,file);}
function control({gapMs=1000,wait=sleep,now=Date.now}={}) {
  let failure=null,last=-Infinity,tail=Promise.resolve();
  const check=()=>{if(failure)throw failure;};
  return {check,stop:error=>{failure ||= error;},turn:()=>{
    const turn=tail.then(async()=>{check();await wait(Math.max(0,last+gapMs-now()));check();last=now();});
    tail=turn.catch(()=>{});return turn;
  }};
}
function openStore(out,{resume=false,count}) {
  if(!Number.isInteger(count)||count<1||count>10000)throw Error('count_must_be_1_to_10000');
  if(fs.existsSync(out)&&!resume)throw Error('EEXIST: use --resume for this dataset');
  fs.mkdirSync(out,{recursive:true});
  const lock=path.join(out,'.harvest.lock');
  if(fs.existsSync(lock)) {
    const pid=Number(fs.readFileSync(lock,'utf8'));
    if(!Number.isInteger(pid)||pid<=0)throw Error('dataset_locked');
    try {process.kill(pid,0);throw Error('dataset_locked');}
    catch(error){if(error.code!=='ESRCH')throw error;}
    fs.unlinkSync(lock);
  }
  fs.writeFileSync(lock,String(process.pid),{flag:'wx'});
  try {
    const file=path.join(out,'manifest.json');
    const prior=fs.existsSync(file)?JSON.parse(fs.readFileSync(file,'utf8')):{samples:[]};
    if(prior.pause_until && Date.now()<prior.pause_until)throw Error('collection_cooldown_active');
    const rows=prior.samples,seen=new Set();
    if(!Array.isArray(rows))throw Error('manifest_invalid');
    for(const row of rows) {
      if(path.basename(row.file)!==row.file||!row.file.match(/\.(png|jpg)$/))throw Error('manifest_path_invalid');
      if(sha256Hex(fs.readFileSync(path.join(out,row.file)))!==row.sha256||seen.has(row.sha256))throw Error('manifest_hash_invalid');
      seen.add(row.sha256);
    }
    let inflight=0;
    const save=(extra={})=>atomic(file,JSON.stringify({labels_file:'labels.csv',samples:rows,...extra},null,2));
    save();
    return {rows,claim:()=>{if(rows.length+inflight>=count)return false;inflight++;return true;},release:()=>inflight--,
      add:sample=>{
        const hash=sha256Hex(sample.bytes);if(seen.has(hash))return false;
        const name=`${hash}.${sample.ext}`;
        const target=path.join(out,name);
        if(fs.existsSync(target)){if(sha256Hex(fs.readFileSync(target))!==hash)throw Error('image_hash_invalid');}
        else fs.writeFileSync(target,sample.bytes,{flag:'wx',mode:0o600});
        rows.push({file:name,sha256:hash,width:sample.width,height:sample.height});seen.add(hash);save();return true;
      },finish:error=>{
        save(error?{state:'paused',reason:error.message,pause_until:Date.now()+60000}:{state:'harvested'});
        const csv='file,label,label_verified,sha256\n'+rows.map(r=>`${r.file},,false,${r.sha256}`).join('\n')+'\n';
        atomic(path.join(out,'labels-template.csv'),csv);
        if(!fs.existsSync(path.join(out,'labels.csv')))atomic(path.join(out,'labels.csv'),csv);
      },close:()=>fs.unlinkSync(lock)};
  } catch(error){fs.unlinkSync(lock);throw error;}
}
async function runWorkers(workers,{out,count=5,resume=false,intervalMs=5000,wait=sleep,controller=control()}) {
  const store=openStore(out,{resume,count});let failure;
  try {
    const results=await Promise.allSettled(workers.map(async worker=>{
      let started=false,refresh=false,duplicates=0;
      try {
        while(store.claim()) {
          try {
            controller.check();worker.network.check();
            if(!started){await worker.start?.();started=true;}
            if(refresh){await wait(Math.max(3000,intervalMs));controller.check();worker.network.check();await worker.refresh();}
            const sample=await worker.capture();controller.check();
            if(store.add(sample)){duplicates=0;console.log(JSON.stringify({at:'harvest',saved:store.rows.length,target:count}));}
            else if(++duplicates>=3)throw Error('captcha_image_repeated');
            refresh=true;
          } catch(error) {
            if(await worker.recover?.(error)){started=true;refresh=false;duplicates=0;continue;}
            controller.stop(error);throw error;
          } finally {store.release();}
        }
      } catch(error){throw error;}
    }));
    failure=results.find(r=>r.status==='rejected')?.reason;
    if(failure)throw failure;
    return {state:'harvested',saved:store.rows.length,blocked_registration_requests:workers.reduce((n,w)=>n+w.network.blocked(),0)};
  } finally {try{store.finish(failure);}finally{store.close();}}
}
module.exports={control,openStore,runWorkers};
