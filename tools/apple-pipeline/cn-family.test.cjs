const test = require('node:test');
const assert = require('node:assert/strict');
const {runCNFamily} = require('../../internal/dashboard/web/cn-family.js');

function fixture(overrides = {}) {
  const calls = [];
  return {calls, call: async step => {
    calls.push(step);
    if (overrides[step]) return overrides[step];
    if (step === 'observe') return {ok:true, activated:true, online:true};
    if (step === 'activate') return {ok:true, activation:{completed:true}};
    if (step === 'subscription-plan') return {ok:true, existing:true};
    return {ok:true};
  }};
}
test('CN mother waits for attended payment without buying or sharing', async () => {
  const f = fixture({'subscription-plan':{ok:true,purchase_action:'storage-skill-purchase-cn'}});
  assert.equal((await runCNFamily({role:'organizer',call:f.call})).stage,'payment-required');
  assert.deepEqual(f.calls.slice(4),['subscription-plan']);
});
test('paid mother cancels renewal before creating and sharing', async () => {
  const f = fixture();
  assert.equal((await runCNFamily({role:'organizer',call:f.call})).completed,true);
  assert.deepEqual(f.calls.slice(4),['subscription-plan','subscription-verify','subscription-cancel','family-create','family-share','family-verify']);
});
test('member only activates and joins; no commerce calls', async () => {
  const f = fixture();
  assert.equal((await runCNFamily({role:'member',call:f.call})).completed,true);
  assert.deepEqual(f.calls.slice(4),['family-join','family-join-storage','family-verify']);
});
test('unknown cancellation stops without retry or family write', async () => {
  const f = fixture({'subscription-cancel':{ok:false,reason:'subscription_renewal_unconfirmed'}});
  await assert.rejects(runCNFamily({role:'organizer',call:f.call}),/subscription_renewal_unconfirmed/);
  assert.equal(f.calls.filter(x=>x==='subscription-cancel').length,1);
  assert.ok(!f.calls.includes('family-create'));
});
test('verification wait never loops password logins or sends SMS', async () => {
  let connects = 0;
  await assert.rejects(runCNFamily({role:'member',wait:async()=>{},call:async step=> {
    if(step==='connect') {connects++;return {ok:true};}
    return {ok:true,verification_required:connects>0};
  }}),/验证码/);
  assert.equal(connects,1);
});
