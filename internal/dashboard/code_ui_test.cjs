const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const elements=new Map();
class Element{
 constructor(){this.textContent='';this.value='';this.children=[];this.dataset={};this.style={};this.disabled=false;this.hidden=false;this.options=[];this.listeners={};const classes=new Set();this.classList={add:(v)=>classes.add(v),remove:v=>classes.delete(v),contains:v=>classes.has(v),toggle:(v,on)=>on?classes.add(v):classes.delete(v)};}
 addEventListener(name,fn){this.listeners[name]=fn;}
 querySelector(){return new Element();}querySelectorAll(){return [];}
 append(...v){this.children.push(...v);}prepend(...v){this.children.unshift(...v);}replaceChildren(...v){this.children=v;this.textContent=v.filter(x=>typeof x==='string').join('');}
 setAttribute(){}removeAttribute(){}remove(){}focus(){}reset(){}showModal(){}close(){}
}
const $=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
const base={selected:'fixture',account:'fixture@example.test',accounts:[],devices:[],pending:null,icloud_activated:false,control_ready:true,code_ready:true,cn_family_default:{enabled:false},worker_capability_state:'supported',status:{state:'refresh_failed',protected:false,running:true,push_connected:true,authenticated:false,session_verified:false,renewal_stage:'device_query',renewal_http_status:401,automatic_refresh_paused:true,updated_at:'2026-11-26T00:00:00Z'}};
let fixture=structuredClone(base);const calls=[];const storage=new Map();
const context=vm.createContext({Option:Element,console,Intl,Date,Math,URL,Map,Set,JSON,Number,String,Boolean,Array,Promise,structuredClone,requestAnimationFrame:()=>0,setTimeout:()=>0,clearTimeout:()=>{},setInterval:()=>0,clearInterval:()=>{},
 document:{body:new Element(),getElementById:$,querySelector:()=>({content:'fixture'}),querySelectorAll:()=>[],createElement:()=>new Element(),createTextNode:v=>v,addEventListener:()=>{}},
 location:{hash:'#overview'},window:{addEventListener:()=>{},setTimeout:()=>0},navigator:{clipboard:{writeText:async()=>{}}},localStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},
 fetch:async(url,options={})=>{if(url.startsWith('/api/view'))return {ok:true,json:async()=>fixture};const body=JSON.parse(options.body||'{}');calls.push(body.action);assert.equal(body.action,'code','unexpected external action');return {ok:true,json:async()=>({ok:true,code:'123456',hide_at:new Date(Date.now()+60000).toISOString(),message:'本机验证码已生成；Apple 是否接受尚未验证'})};}
});
(async()=>{
 vm.runInContext(fs.readFileSync('internal/dashboard/web/app.js','utf8'),context);
 await new Promise(setImmediate);context.fixture=fixture;vm.runInContext('render(fixture)',context);
 assert.equal($('codeButton').disabled,false,'device 401 disabled local code');
 assert.equal($('authState').textContent,'当前会话未验证');
 await vm.runInContext("action('code')",context);
 assert.deepEqual(calls,['code']);assert.equal($('copyButton').disabled,false);assert.equal($('codeDisplay').classList.contains('shown'),true);assert.equal($('authState').textContent,'当前会话未验证');
 assert.ok($('sessionDescription').textContent.includes('401'));
 for(const [name,patch] of [['unready',{code_ready:false}],['stopped',{status:{...base.status,running:false}}],['protected',{status:{...base.status,protected:true,state:'account_locked'}}]]){
  fixture={...structuredClone(base),...patch};context.fixture=fixture;vm.runInContext('clearCode();render(fixture)',context);assert.equal($('codeButton').disabled,true,name+' enabled code button');
 }
 console.log('PASS: 401 permits manual code; no login/SMS action; health stays unverified; unready/stopped/protected disabled.');
})().catch(err=>{console.error(err.stack);process.exitCode=1;});
