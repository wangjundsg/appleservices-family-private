"use strict";
const {test}=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),os=require('node:os');
const {runWorkers,control}=require('./harvest-pool.cjs');
const network={check(){},blocked:()=>0};
test('parallel capture enforces one quota, deduplicates and resumes without overwriting labels',async()=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'harvest-pool-'));const out=path.join(root,'dataset');
  try {
    const make=id=>{let n=0;return {network,refresh:async()=>{},capture:async()=>({bytes:Buffer.from(n++===0?'same':`${id}-${n}`),ext:'png',width:160,height:70})};};
    assert.equal((await runWorkers([make(1),make(2),make(3)],{out,count:7,wait:async()=>{}})).saved,7);
    const labels=fs.readFileSync(path.join(out,'labels.csv'),'utf8')+'human-note';fs.writeFileSync(path.join(out,'labels.csv'),labels);
    const result=await runWorkers([make(4),make(5)],{out,count:9,resume:true,wait:async()=>{}});
    assert.equal(result.saved,9);assert.equal(fs.readFileSync(path.join(out,'labels.csv'),'utf8'),labels);
    const rows=JSON.parse(fs.readFileSync(path.join(out,'manifest.json'))).samples;
    assert.equal(new Set(rows.map(row=>row.sha256)).size,9);
  } finally {fs.rmSync(root,{recursive:true,force:true});}
});
test('one service failure stops every worker and persists the shared cooldown',async()=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'harvest-stop-')),out=path.join(root,'data');let writes=0;
  try {
    const failed={network,refresh:async()=>{},capture:async()=>{throw Error('captcha_http_429');}};
    const delayed={network,refresh:async()=>{},capture:async()=>{await new Promise(r=>setTimeout(r,10));writes++;return {bytes:Buffer.from('late'),ext:'png'};}};
    await assert.rejects(runWorkers([failed,delayed],{out,count:5}),/429/);
    assert.equal(JSON.parse(fs.readFileSync(path.join(out,'manifest.json'))).samples.length,0);
    await assert.rejects(runWorkers([delayed],{out,count:5,resume:true}),/cooldown/);
    assert.equal(writes,1);
  } finally {fs.rmSync(root,{recursive:true,force:true});}
});
test('global request slots are paced and pending slots stop after a rejection',async()=>{
  let now=0;const times=[];
  const c=control({now:()=>now,wait:async ms=>{now+=ms;},gapMs:1000});
  await Promise.all([1,2,3].map(async()=>{await c.turn();times.push(now);}));
  assert.deepEqual(times,[0,1000,2000]);
  c.stop(Error('limited'));await assert.rejects(c.turn(),/limited/);
});
test('a recoverable worker failure replaces that worker and keeps its quota',async()=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'harvest-recover-')),out=path.join(root,'data');let captures=0,restarts=0;
  try {
    const worker={network,refresh:async()=>{},capture:async()=>{if(captures++===0)throw Error('captcha_http_503');return {bytes:Buffer.from(`ok-${captures}`),ext:'png',width:160,height:70};},
      recover:async error=>{assert.match(error.message,/503/);restarts++;return true;}};
    const result=await runWorkers([worker],{out,count:2,wait:async()=>{}});
    assert.equal(result.saved,2);assert.equal(restarts,1);assert.equal(captures,3);
  } finally {fs.rmSync(root,{recursive:true,force:true});}
});
