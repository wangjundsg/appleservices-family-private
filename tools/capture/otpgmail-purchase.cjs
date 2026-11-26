"use strict";
// Explicit purchase entry point; the mail reader remains read-only.
const fs=require("node:fs"),path=require("node:path");
const provider=require("./otpgmail.cjs");
const fail=reason=>new Error(reason);
function atomic(file,value) {
  const temp=file+".tmp";
  fs.writeFileSync(temp,JSON.stringify(value),{mode:0o600});fs.renameSync(temp,file);
}
async function purchaseReplacement(intent,{fetchImpl=fetch,keyLoader=provider.loadKey,inspect=provider.inspect}={}) {
  const cfg=JSON.parse(fs.readFileSync(intent.source_file,"utf8").replace(/^\uFEFF/,""));
  if(cfg.provider!=="otpgmail" || cfg.email!==intent.previous_email || ![1,2,4].includes(cfg.warehouse) ||
    !/^[a-f0-9]{32}$/.test(intent.acquisition_id) || !Number.isSafeInteger(intent.max_price_vnd) || intent.max_price_vnd<=0 || intent.max_price_vnd>700)
    throw fail("replacement_email_policy_invalid");
  const offer=(await inspect({credentialFile:cfg.credentialFile,fetchImpl,keyLoader})).offers.find(o=>o.warehouse===cfg.warehouse);
  if(!offer || offer.stock<1 || offer.priceVnd>intent.max_price_vnd)throw fail("replacement_email_offer_unavailable");
  const response=await fetchImpl("https://otpgmail.net/v1/orders",{method:"POST",redirect:"error",signal:AbortSignal.timeout(20000),
    headers:{Authorization:"Bearer "+keyLoader(cfg.credentialFile),"Content-Type":"application/json","Idempotency-Key":intent.acquisition_id},
    body:JSON.stringify({service:"wx",domain:"gmail.com",warehouse:cfg.warehouse,quantity:1})});
  const receipt=await response.json();
  const order=Array.isArray(receipt?.data)&&receipt.data.length===1 ? receipt.data[0] : null;
  if(!response.ok || receipt.success!==true || !order || order.service!=="wx" || order.domain!=="gmail.com" ||
    order.warehouse!==cfg.warehouse || !/^[A-Za-z0-9_-]{1,128}$/.test(order.orderId||"") ||
    !/^[A-Za-z0-9._%+-]+@gmail\.com$/.test(order.email||"") || order.email.toLowerCase()===cfg.email.toLowerCase() ||
    !["waiting_code","queued"].includes(order.status) || !Number.isSafeInteger(order.price) || order.price<0 || order.price>intent.max_price_vnd)
    throw fail("replacement_email_purchase_unconfirmed");
  return {provider:"otpgmail",orderId:order.orderId,email:order.email,warehouse:order.warehouse,credentialFile:cfg.credentialFile};
}
async function main() {
  const input=path.resolve(process.argv[2]),output=path.resolve(process.argv[3]);
  const intent=JSON.parse(fs.readFileSync(input,"utf8"));
  // Crash recovery reuses the acquired order instead of making a second POST.
  if(fs.existsSync(output))return;
  const orderFile=path.join(path.dirname(output),"replacement-otpgmail-order.json");
  const order=fs.existsSync(orderFile)?JSON.parse(fs.readFileSync(orderFile,"utf8")):await purchaseReplacement(intent);
  atomic(orderFile,order);
  atomic(output,{email:order.email,order_file:orderFile,previous_email:intent.previous_email});
}
if(require.main===module)main().then(()=>console.log(JSON.stringify({state:"mailbox_ready"}))).catch(()=>{
  console.error(JSON.stringify({state:"needs_review",reason:"replacement_email_purchase_unconfirmed"}));process.exitCode=3;
});
module.exports={purchaseReplacement};
