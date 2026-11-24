(function (root) {
  'use strict';
  async function runCNFamily({role, call, onStage = () => {}, wait = ms => new Promise(resolve => setTimeout(resolve, ms))}) {
    if (!['organizer', 'member'].includes(role)) throw Error('family_role_mismatch');
    async function step(name) {
      onStage(name);
      const result = await call(name);
      if (!result?.ok) throw Error(result?.message || result?.reason || result?.stage || 'family_readback_required');
      return result;
    }
    async function waitFor(key) {
      for (let i = 0; i < 45; i++) {
        const state = await step('observe');
        if (state[key]) return;
        if (state.verification_required) throw Error('请在当前账号面板完成验证码，然后继续流程');
        await wait(2000);
      }
      throw Error('后台尚未就绪，请核对账号登录状态后继续');
    }
    const initial = await step('observe');
    if (!initial.activated && !initial.activation_ready) {
      await step('connect');
      await waitFor('activation_ready');
    }
    const activated = await step('activate');
    if (!activated.activation?.completed) throw Error('请先授权当前账号的 iCloud 条款与激活');
    await step('connect');
    await waitFor('online');
    if (role === 'organizer') {
      const plan = await step('subscription-plan');
      if (!plan.existing) {
        if (plan.purchase_action !== 'storage-skill-purchase-cn') throw Error('subscription_offer_unsupported');
        return {ok: true, completed: false, stage: 'payment-required'};
      }
      await step('subscription-verify');
      await step('subscription-cancel');
      await step('family-create');
      await step('family-share');
    } else {
      await step('family-join');
      await step('family-join-storage');
    }
    await step('family-verify');
    return {ok: true, completed: true, stage: 'completed'};
  }
  if (typeof module !== 'undefined' && module.exports) module.exports = {runCNFamily};
  else root.runCNFamily = runCNFamily;
})(typeof globalThis !== 'undefined' ? globalThis : this);
