'use strict';
// Read-only reconciliation: never construct Outlook Store or copy credentials.
const fs=require('node:fs'), path=require('node:path');
const {parseArgs}=require('node:util');
const normalize=value=>String(value||'').trim().toLowerCase();
function read(file, fallback) {
  try { return JSON.parse(fs.readFileSync(file,'utf8').replace(/^\uFEFF/,'')); }
  catch(error) { if(error.code==='ENOENT')return fallback; throw Error('inventory_file_invalid'); }
}
function rows(value) {
  if (!value || typeof value !== 'object') throw Error('inventory_shape_invalid');
  return Array.isArray(value)?value:Object.values(value);
}
function reconcile({legacy=[],workbench={},usage={},reservations=[],protectionClaims=[]}) {
  const items=new Map();
  function entry(address) {
    address=normalize(address);
    if(!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(address))throw Error('inventory_address_invalid');
    if(!items.has(address))items.set(address,{address,records:[],registrationEvidence:[]});
    return items.get(address);
  }
  for(const [origin,list] of [['apple',rows(legacy)],['outlook',rows(workbench)]]) {
    for(const row of list) {
      const item=entry(row.address||row.email||row.mailbox);
      item.records.push({origin,source:String(row.source||''),reader:String(row.reader||''),status:String(row.status||''),
        ...(origin==='apple'?{credentialsSaved:Boolean(row.client_id&&row.refresh_token)}:{})});
    }
  }
  if(!usage || Array.isArray(usage) || typeof usage!=='object')throw Error('inventory_usage_invalid');
  for(const address of Object.keys(usage))entry(address).registrationEvidence.push({kind:'legacy_use',state:'unreconciled'});
  for(const row of reservations)entry(row.email).registrationEvidence.push({kind:'panel_request',state:row.state||'unknown'});
  const dependencies=rows(protectionClaims).filter(row=>row.state!=='released'||row.binding_uncertain===true).map(row=>({
    address:normalize(row.address),target:normalize(row.target_account),state:String(row.state||'unknown'),bindingUncertain:row.binding_uncertain===true,
  }));
  const result=[...items.values()].map(item=>({...item,
    sourceConflict:new Set(item.records.map(r=>r.source).filter(Boolean)).size>1,
    readOwner:item.records.some(r=>r.origin==='outlook')?'8802':'legacy',
    registrationBlocked:item.registrationEvidence.length>0,
    authorization:'not_checked',
  })).sort((a,b)=>a.address.localeCompare(b.address));
  return {counts:{unique:result.length,overlap:result.filter(r=>new Set(r.records.map(x=>x.origin)).size>1).length,
    legacyCredentialsSaved:result.filter(r=>r.records.some(x=>x.credentialsSaved)).length,
    registrationBlocked:result.filter(r=>r.registrationBlocked).length,sourceConflicts:result.filter(r=>r.sourceConflict).length},
    items:result,protectionDependencies:dependencies};
}
function inventory({repoRoot=path.resolve(__dirname,'../..'),outlookRoot='D:/outlook-workbench',backupUsage}={}) {
  const workbench=path.join(outlookRoot,'.cache/workbench');
  const panel=path.join(repoRoot,'.cache/panel');
  const reservations=[];
  if(fs.existsSync(panel))for(const dir of fs.readdirSync(panel,{withFileTypes:true})) {
    if(!dir.isDirectory()||!/^reg-/.test(dir.name))continue;
    const request=read(path.join(panel,dir.name,'request.json'),null);
    if(request)reservations.push({email:request.email,state:read(path.join(panel,dir.name,'result.json'),{}).status||'unknown'});
  }
  const backup=backupUsage||path.join(workbench,'backup_email_usage.json');
  return {...reconcile({legacy:read(path.join(repoRoot,'.cache/mailboxes.json'),[]),
    workbench:read(path.join(workbench,'mailboxes.json'),{}),usage:read(path.join(repoRoot,'.cache/mailbox-usage.json'),{}),
    reservations,protectionClaims:read(backup,[])}),coverage:{panelPresent:fs.existsSync(panel),backupUsagePresent:fs.existsSync(backup),
      accountRegistryIncluded:false,liveAuthorizationChecked:false}};
}
if(require.main===module) {
  try {
    const {values}=parseArgs({options:{'repo-root':{type:'string'},'outlook-root':{type:'string'},'backup-usage':{type:'string'}}});
    console.log(JSON.stringify(inventory({repoRoot:values['repo-root'],outlookRoot:values['outlook-root'],backupUsage:values['backup-usage']}),null,2));
  } catch { console.error('mailbox_inventory_failed');process.exitCode=1; }
}
module.exports={reconcile,inventory};
