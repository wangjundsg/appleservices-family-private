"use strict";
const {test} = require("node:test"), assert = require("node:assert/strict");
const fs = require("node:fs"), os = require("node:os"), path = require("node:path");
const {acquire} = require("./otpgmail-acquire.cjs");
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "mail-acquire-"));
  t.after(() => {assert.equal(path.dirname(dir), os.tmpdir()); fs.rmSync(dir, {recursive:true, force:true});});
  const input = path.join(dir, "input.json"), output = path.join(dir, "otpgmail-order.json");
  const policy = {credentialFile:path.join(dir, "key.dpapi"), warehouse:2, max_price_vnd:700};
  fs.writeFileSync(input, JSON.stringify({mail_purchase:policy}));
  const order = {orderId:"fixture-order", email:"Fixture@gmail.com", service:"wx", domain:"gmail.com", warehouse:2,
    price:700, status:"waiting_code", otp:[]};
  const calls = [];
  const deps = {keyLoader:()=>"fixture-secret", now:()=>100000,
    inspect:async()=>({balanceVnd:1000, offers:[{warehouse:2, priceVnd:700, stock:1}]}),
    fetchImpl:async(url, init)=>{calls.push({url, init});return {ok:true,json:async()=>({success:true,data:init.method==="POST"?[order]:order})};}};
  return {dir,input,output,policy,order,calls,deps,run:()=>acquire(input,output,deps)};
}
test("first purchase saves intent before POST and resumes with GET only", async t=>{
  const f=fixture(t), fetch=f.deps.fetchImpl;
  f.deps.fetchImpl=async(url,init)=>{
    assert.equal(JSON.parse(fs.readFileSync(path.join(f.dir,"mail-purchase.json"))).key,init.headers["Idempotency-Key"]);
    return fetch(url,init);
  };
  const order=await f.run();
  assert.equal(order.email,"fixture@gmail.com");
  assert.deepEqual(JSON.parse(f.calls[0].init.body),{service:"wx",domain:"gmail.com",warehouse:2,quantity:1});
  f.deps.fetchImpl=fetch;
  f.deps.inspect=()=>{throw Error("must not check inventory for an owned order");};
  await f.run();
  assert.deepEqual(f.calls.map(c=>c.init.method),["POST","GET"]);
  assert.ok(f.calls[1].url.endsWith("/orders/fixture-order"));
  assert.ok(!fs.readFileSync(f.output,"utf8").includes("fixture-secret"));
});
test("lost response reuses the identical key and body, then persists the recovered order",async t=>{
  const f=fixture(t), fetch=f.deps.fetchImpl;
  let original;
  f.deps.fetchImpl=async(_url,init)=>{original=init;throw Error("lost response");};
  await assert.rejects(f.run());
  f.deps.inspect=()=>{throw Error("recovery is independent of current inventory");};
  f.deps.fetchImpl=fetch;
  await f.run();
  assert.equal(f.calls[0].init.headers["Idempotency-Key"],original.headers["Idempotency-Key"]);
  assert.equal(f.calls[0].init.body,original.body);
});
test("unknown purchase beyond key lifetime never sends a second POST",async t=>{
  const f=fixture(t);
  f.deps.fetchImpl=async()=>{throw Error("lost");};
  await assert.rejects(f.run());
  f.deps.now=()=>100000+23*3600000;
  f.deps.fetchImpl=()=>{throw Error("must not request");};
  await assert.rejects(f.run(),/mailbox_purchase_requires_readback/);
});
test("queued mailbox keeps its order and only queries on resume",async t=>{
  const f=fixture(t);f.order.status="queued";
  await assert.rejects(f.run(),/mailbox_order_pending/);
  assert.ok(fs.existsSync(f.output));
  f.order.status="waiting_code";await f.run();
  assert.deepEqual(f.calls.map(c=>c.init.method),["POST","GET"]);
});
test("stock, price and balance gates never spend",async t=>{
  const f=fixture(t);
  for(const [stock,price,balance,reason] of [[0,700,1000,"stock_unavailable"],[1,701,1000,"price_exceeded"],[1,700,699,"balance_insufficient"]]) {
    f.deps.inspect=async()=>({balanceVnd:balance,offers:[{warehouse:2,priceVnd:price,stock}]});
    await assert.rejects(f.run(),new RegExp(reason));
    assert.equal(f.calls.length,0);
    assert.ok(!fs.existsSync(path.join(f.dir,"mail-purchase.json")));
  }
});
test("overpriced or unusable receipts retain the original order ID without buying again",async t=>{
  const f=fixture(t);f.order.price=701;
  await assert.rejects(f.run(),/mailbox_order_not_ready/);
  assert.equal(JSON.parse(fs.readFileSync(path.join(f.dir,"mail-purchase.json"))).orderId,f.order.orderId);
  await assert.rejects(f.run(),/mailbox_order_not_ready/);
  assert.deepEqual(f.calls.map(c=>c.init.method),["POST","GET"]);
  f.order.price=700;f.order.status="cancelled";
  await assert.rejects(f.run(),/mailbox_order_not_ready/);
});
test("edited purchase policy cannot turn an uncertain purchase into a new one",async t=>{
  const f=fixture(t);f.deps.fetchImpl=async()=>{throw Error("lost");};
  await assert.rejects(f.run());
  fs.writeFileSync(f.input,JSON.stringify({mail_purchase:{...f.policy,warehouse:1}}));
  await assert.rejects(f.run(),/mailbox_purchase_requires_readback/);
});
test("a confirmed unissued order checks price again before retrying after stock returns",async t=>{
  const f=fixture(t);let posts=0;
  f.deps.fetchImpl=async()=>{posts++;return {ok:false,json:async()=>({success:false,error:{code:"OUT_OF_STOCK"}})};};
  await assert.rejects(f.run(),/mailbox_stock_unavailable/);
  f.deps.inspect=async()=>({balanceVnd:1000,offers:[{warehouse:2,priceVnd:701,stock:1}]});
  await assert.rejects(f.run(),/mailbox_price_exceeded/);
  assert.equal(posts,1);
});
