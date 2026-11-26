'use strict';
const {test}=require('node:test');
const assert=require('node:assert/strict');
const {probe}=require('./resource_probe.cjs');
const order={provider:'otpgmail',orderId:'fixture',email:'new@gmail.com',warehouse:2};
test('mail inspection exposes only adapter summary and bound order state',async()=>{
  const calls=[];
  const result=await probe({kind:'mail',credentialFile:'fixture',order},{
    loadKey:()=> 'private-key',
    inspect:async()=>({balanceVnd:100,offers:[{warehouse:2,priceVnd:70,stock:1}]}),
    fetchImpl:async(url,options)=>{calls.push([url,options]);return {ok:true,json:async()=>({success:true,data:{...order,service:'wx',domain:'gmail.com',status:'waiting_code',otp:[{code:'123456'}],password:'private-marker'}})};},
  });
  assert.equal(result.orderReady,true);assert.equal(calls.length,1);
  assert.equal(calls[0][1].redirect,'error');assert.equal(calls[0][1].method,undefined);
  assert.ok(!JSON.stringify(result).includes('123456'));assert.ok(!JSON.stringify(result).includes('private'));
});
test('bound order mismatch and cancelled/completed orders cannot authorize new use',async()=>{
  for(const status of ['cancelled','completed','queued']) {
    const result=await probe({kind:'order',order},{loadKey:()=> 'fixture',fetchImpl:async()=>({ok:true,json:async()=>({success:true,data:{...order,service:'wx',domain:'gmail.com',status}})})});
    assert.equal(result.orderReady,false);
  }
  await assert.rejects(probe({kind:'order',order},{loadKey:()=> 'fixture',fetchImpl:async()=>({ok:true,json:async()=>({success:true,data:{...order,email:'other@gmail.com',service:'wx',domain:'gmail.com',status:'waiting_code'}})})}),/order_mismatch/);
});
test('authorization failure is fixed error without raw provider body or retry',async()=>{
  let calls=0;
  await assert.rejects(probe({kind:'order',order},{loadKey:()=> 'fixture',fetchImpl:async()=>{calls++;return {ok:false,status:401,json:async()=>{throw Error('private-marker')}};}}),/unauthorized/);
  assert.equal(calls,1);
});
