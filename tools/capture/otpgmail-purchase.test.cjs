"use strict";
const {test}=require("node:test"),assert=require("node:assert/strict"),fs=require("node:fs"),os=require("node:os"),path=require("node:path");
const {purchaseReplacement}=require("./otpgmail-purchase.cjs");
function fixture(t) {
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),"mail-replace-"));t.after(()=>fs.rmSync(dir,{recursive:true,force:true}));
  const source=path.join(dir,"order.json");
  fs.writeFileSync(source,JSON.stringify({provider:"otpgmail",email:"old@gmail.com",warehouse:2,credentialFile:"fixture-private"}));
  return {source_file:source,previous_email:"old@gmail.com",acquisition_id:"a".repeat(32),max_price_vnd:700};
}
test("one bounded mailbox purchase uses frozen warehouse and idempotency key",async t=>{
  const intent=fixture(t),calls=[];
  const order=await purchaseReplacement(intent,{keyLoader:()=>"test-key",inspect:async()=>({offers:[{warehouse:2,stock:1,priceVnd:700}]}),
    fetchImpl:async(url,init)=>{calls.push({url,init});return {ok:true,json:async()=>({success:true,data:[{
      service:"wx",domain:"gmail.com",warehouse:2,email:"new@gmail.com",orderId:"fixture-new",price:700,status:"waiting_code"}]})};}});
  assert.equal(calls.length,1);assert.equal(calls[0].init.headers["Idempotency-Key"],intent.acquisition_id);
  assert.deepEqual(JSON.parse(calls[0].init.body),{service:"wx",domain:"gmail.com",warehouse:2,quantity:1});
  assert.equal(order.email,"new@gmail.com");assert.equal(order.credentialFile,"fixture-private");
});
test("price increase and exhausted stock stop before spending",async t=>{
  const intent=fixture(t);
  for(const offer of [{warehouse:2,stock:1,priceVnd:701},{warehouse:2,stock:0,priceVnd:700}]) {
    await assert.rejects(purchaseReplacement(intent,{inspect:async()=>({offers:[offer]}),fetchImpl:()=>{throw Error("must not buy");}}),/offer_unavailable/);
  }
});
