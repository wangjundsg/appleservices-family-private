'use strict';
// Read-only bridge. Input stays on stdin; only allowlisted summaries leave stdout.
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
function fail(code) { throw Object.assign(Error(code), {code}); }
function keyFromDPAPI(file) {
  try {
    const key = execFileSync('powershell.exe', ['-NoProfile','-NonInteractive','-Command',
      '$ErrorActionPreference="Stop"; Import-Module ($PSHOME + "/Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1"); $s=ConvertTo-SecureString ([Console]::In.ReadToEnd().Trim()); $p=[Runtime.InteropServices.Marshal]::SecureStringToBSTR($s); try { [Console]::Out.Write([Runtime.InteropServices.Marshal]::PtrToStringBSTR($p)) } finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($p) }'],
      {input:fs.readFileSync(file,'utf8'),encoding:'utf8',timeout:10000,windowsHide:true,stdio:['pipe','pipe','ignore']}).trim();
    if (!/^og_[a-zA-Z0-9]{20,128}$/.test(key)) fail('credential_unavailable');
    return key;
  } catch { fail('credential_unavailable'); }
}
async function checkOrder(input, key, fetchImpl) {
  if (!input.order) return {orderReady:false, orderStatus:'not_bound'};
  const response = await fetchImpl('https://otpgmail.net/v1/orders/'+encodeURIComponent(input.order.orderId), {
    headers:{Authorization:'Bearer '+key},redirect:'error',signal:AbortSignal.timeout(15000),
  });
  if (!response.ok) fail(response.status===401?'unauthorized':response.status===429?'rate_limited':'order_read_failed');
  const value = await response.json(), order=value?.data, expected=input.order;
  if (value.success!==true || order?.orderId!==expected.orderId || order.email?.toLowerCase()!==expected.email.toLowerCase() ||
      order.service!=='wx' || order.domain!=='gmail.com' || order.warehouse!==expected.warehouse ||
      !['waiting_code','queued','completed','cancelled'].includes(order.status)) fail('order_mismatch');
  return {orderReady:order.status==='waiting_code',orderStatus:order.status};
}
async function probe(input, {fetchImpl=fetch, loadKey=keyFromDPAPI, inspect}={}) {
  const key=loadKey(input.credentialFile);
  if (input.kind==='order') return checkOrder(input,key,fetchImpl);
  if (input.kind!=='mail') fail('check_invalid');
  inspect ||= require(path.join(input.repo,'tools/capture/otpgmail.cjs')).inspect;
  const summary=await inspect({credentialFile:input.credentialFile,keyLoader:()=>key,fetchImpl});
  return {...summary,...await checkOrder(input,key,fetchImpl)};
}
function runCLI() {
  (async()=> {
    try { process.stdout.write(JSON.stringify(await probe(JSON.parse(fs.readFileSync(0,'utf8'))))); }
    catch (error) { process.stdout.write(JSON.stringify({error:/^(otpgmail_[a-z_]+|credential_unavailable|unauthorized|rate_limited|order_read_failed|order_mismatch|check_invalid)$/.test(error.code||'')?error.code:'provider_request_failed'})); process.exitCode=1; }
  })();
}
if (require.main===module) runCLI();
module.exports={probe,checkOrder};
