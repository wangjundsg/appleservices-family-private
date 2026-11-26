const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

const source = fs.readFileSync('internal/dashboard/web/app.js', 'utf8');
const start = source.indexOf("$('loginForm').addEventListener('submit'");
const end = source.indexOf("$('smsForm').addEventListener", start);
assert.ok(start >= 0 && end > start);

(async () => {
  for (const ok of [false, true]) {
    let submit, closed = 0, cleared = 0;
    const calls = [], stored = new Map(), notices = [];
    const elements = {
      loginForm: {addEventListener: (_, handler) => {submit = handler;}},
      appleID: {value: 'owner@example.test'}, password: {value: 'private-password', focus() {}},
      twofactor: {value: 'sms'}, loginDialog: {close: () => closed++},
    };
    const context = vm.createContext({
      $: id => elements[id], busy: false, loginMode: 'add', loginAccount: '', selectedAccount: 'previous',
      api: async (path) => {calls.push(path); return {ok, account_id: 'new-account', message: 'fixture result'};},
      localStorage: {setItem: (key, value) => stored.set(key, value)},
      clearCode: () => cleared++, notice: (message, error) => notices.push({message, error}), poll: async () => {},
    });
    vm.runInContext(source.slice(start, end), context);
    await submit({preventDefault() {}});
    assert.equal(context.selectedAccount, 'new-account');
    assert.equal(stored.get('selected-account'), 'new-account');
    assert.ok(closed > 0, 'saved account left in add dialog');
    assert.equal(cleared, 1, 'previous account code remained visible');
    assert.equal(elements.password.value, '');
    assert.equal(context.busy, false);
    assert.deepEqual(calls, ['accounts/add'], 'unexpected login or SMS retry');
    assert.equal(Boolean(notices[0].error), !ok);
  }
  console.log('PASS: saved account selected on success/failure; password cleared; no automatic login/SMS retry.');
})().catch(error => { console.error(error); process.exitCode = 1; });
