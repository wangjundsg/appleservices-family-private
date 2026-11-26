'use strict';
// Read-only provider adapter. Orders are task-owned; polling never buys or resends.
// Private order file: {provider:'otpgmail', orderId, email, warehouse, credentialFile}.
// credentialFile points to a current-Windows-user DPAPI file, never a plaintext key.
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
const {createHash} = require('node:crypto');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const fail = code => Object.assign(Error(code), {code});
function readConfig(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8').replace(/^\uFEFF/, '')); }
  catch { throw fail('otpgmail_config_invalid'); }
}
function loadKey(file) {
  if (typeof file !== 'string' || !path.isAbsolute(file)) throw fail('otpgmail_config_invalid');
  try {
    const encrypted = fs.readFileSync(file, 'utf8').trim();
    const key = execFileSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command',
      // PS7 parents can leave PSModulePath pointing at incompatible modules for Windows PowerShell.
      '$ErrorActionPreference="Stop"; Import-Module ($PSHOME + "/Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1"); $s = ConvertTo-SecureString ([Console]::In.ReadToEnd().Trim()); $p = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($s); try { [Console]::Out.Write([Runtime.InteropServices.Marshal]::PtrToStringBSTR($p)) } finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($p) }'],
    {input:encrypted, encoding:'utf8', windowsHide:true, timeout:10000, stdio:['pipe','pipe','ignore']}).trim();
    if (!/^og_[a-zA-Z0-9]{20,128}$/.test(key)) throw Error();
    return key;
  } catch { throw fail('otpgmail_credential_unavailable'); }
}
async function get(route, key, {fetchImpl=fetch, timeoutMs=15000}={}) {
  if (!/^(balance|services|orders\/[a-zA-Z0-9_-]{1,128})$/.test(route)) throw fail('otpgmail_route_invalid');
  let response;
  try {
    response = await fetchImpl('https://otpgmail.net/v1/'+route, {
      method:'GET', redirect:'error', signal:AbortSignal.timeout(Math.max(1, Math.ceil(timeoutMs))),
      headers:{Authorization:'Bearer '+key, Accept:'application/json'},
    });
  } catch { throw fail('otpgmail_request_failed'); }
  let value;
  try { value = await response.json(); } catch { throw fail('otpgmail_response_invalid'); }
  if (!response.ok || value?.success !== true) {
    const code = response.status === 401 ? 'otpgmail_unauthorized' : response.status === 429
      ? 'otpgmail_rate_limited' : 'otpgmail_read_failed';
    throw fail(code); // Upstream bodies may contain secrets or message contents.
  }
  return value.data;
}
async function inspect({credentialFile, fetchImpl=fetch, keyLoader=loadKey}) {
  const key = keyLoader(credentialFile);
  const balance = await get('balance', key, {fetchImpl});
  const services = await get('services', key, {fetchImpl});
  const apple = Array.isArray(services) && services.find(s=>s.code==='wx' && /apple/i.test(s.name));
  if (!Number.isSafeInteger(balance?.balance) || balance.balance < 0 || !Array.isArray(apple?.offers)) throw fail('otpgmail_response_invalid');
  const offers = apple.offers.filter(o=>o.domain==='gmail.com').map(o=>{
    if (![1,2,4].includes(o.warehouse) || !Number.isSafeInteger(o.price) || o.price < 0 ||
        !Number.isSafeInteger(o.stock) || o.stock < 0) throw fail('otpgmail_response_invalid');
    return {warehouse:o.warehouse, priceVnd:o.price, stock:o.stock};
  });
  return {balanceVnd:balance.balance, service:'wx', domain:'gmail.com', offers};
}
function pickCode(order, cfg, {mailbox, since, until, excluded=[]}) {
  if (order?.orderId !== cfg.orderId || order.service !== 'wx' || order.domain !== 'gmail.com' ||
      order.email?.toLowerCase() !== mailbox || order.warehouse !== cfg.warehouse || !Array.isArray(order.otp) ||
      !['waiting_code','completed','cancelled','queued'].includes(order.status)) throw fail('otpgmail_order_mismatch');
  if (order.status === 'cancelled') throw fail('otpgmail_order_cancelled');
  if (order.otp.some(x=>!Number.isSafeInteger(x?.receivedAt) || x.receivedAt<=0 || typeof x.code!=='string')) throw fail('otpgmail_response_invalid');
  const messages = order.otp.map(x=>({code:x.code, at:x.receivedAt*1000,
    messageId:'otpgmail:'+createHash('sha256').update(JSON.stringify([order.orderId,x.receivedAt,x.code])).digest('hex')}))
    .filter(x=>x.at>since && x.at<=until).sort((a,b)=>b.at-a.at);
  if (!messages.length) return null;
  const newest=messages[0];
  if (messages.some(x=>x.at===newest.at && x.code!==newest.code)) throw fail('code_ambiguous');
  if (!/^\d{6}$/.test(newest.code)) throw fail('code_unrecognized');
  if (excluded.includes(newest.messageId)) return null;
  return {code:newest.code, messageId:newest.messageId, created_at:new Date(newest.at).toISOString()};
}
async function pollCode({file, mailbox, recipient, source, platform, since, until, excluded=[], timeoutMs,
  fetchImpl=fetch, keyLoader=loadKey}) {
  if (source!=='registration' || platform!=='apple' || recipient!==mailbox) throw fail('otpgmail_scope_invalid');
  const cfg=readConfig(file);
  if (cfg?.provider!=='otpgmail' || !/^[a-zA-Z0-9_-]{1,128}$/.test(cfg.orderId||'') ||
      typeof cfg.email!=='string' || cfg.email.toLowerCase()!==mailbox || !mailbox.endsWith('@gmail.com') ||
      ![1,2,4].includes(cfg.warehouse)) throw fail('otpgmail_config_invalid');
  if (!timeoutMs) return null;
  const deadline=Date.now()+timeoutMs, key=keyLoader(cfg.credentialFile);
  while (Date.now()<deadline) {
    const order=await get('orders/'+cfg.orderId,key,{fetchImpl,timeoutMs:Math.min(15000,deadline-Date.now())});
    const hit=pickCode(order,cfg,{mailbox,since,until:until??Date.now(),excluded});
    if (hit) return hit;
    const remaining=deadline-Date.now();
    if (remaining<=0) break;
    await sleep(Math.min(4000,remaining));
  }
  return null;
}
if (require.main===module) {
  (async()=>{
    try {
      if (process.argv[2]!=='check') throw fail('otpgmail_usage_check_only');
      const credentialFile=process.argv[3] || path.resolve(__dirname,'../../.cache/otpgmail/api-key.dpapi');
      console.log(JSON.stringify(await inspect({credentialFile})));
    } catch (error) {
      console.log(JSON.stringify({error:/^(otpgmail_[a-z_]+)$/.test(error.code||'')?error.code:'otpgmail_check_failed'}));
      process.exitCode=1;
    }
  })();
}
module.exports={inspect,pollCode,pickCode,loadKey};
