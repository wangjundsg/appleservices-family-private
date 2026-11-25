'use strict';
// Deliberate credential verification in a private, isolated store. No server or maintenance loop.
const fs = require('node:fs');
const path = require('node:path');
const {parseArgs} = require('node:util');
const ERRORS = new Set(['token_invalid','permission_denied','oauth_config_invalid','upstream_rate_limited',
  'upstream_unavailable','upstream_response_invalid','upstream_error','token_refresh_deferred',
  'writer_locked_stop_or_recover','mailbox_unknown','verification_store_required','verification_batch_invalid']);
const safeError = error => ERRORS.has(error?.code) ? error.code : 'mailbox_verification_failed';

async function verifyMailbox(mail, secret, email, signal) {
  await mail.token(secret, signal);
  // Mail.Read does not imply User.Read. Verify the requested mailbox through the mail API,
  // without requiring /me profile access or requesting extra consent.
  const endpoint = new URL('https://graph.microsoft.com/v1.0/users/' + encodeURIComponent(email) + '/messages');
  endpoint.searchParams.set('$top','1');
  endpoint.searchParams.set('$select','id,toRecipients,receivedDateTime,body');
  const data = await mail.json(endpoint.href, {headers:{Authorization:'Bearer '+secret.access_token}}, signal);
  if (!Array.isArray(data?.value) || data.value.some(m => !m || typeof m.id !== 'string' || !Array.isArray(m.toRecipients))) {
    throw Object.assign(Error(),{code:'upstream_response_invalid'});
  }
  return {addressedMailboxRead:true, bodyObserved:data.value.some(m=>typeof m.body?.content==='string'),
    recipientMatched:data.value.some(m=>m.toRecipients.some(r=>r?.emailAddress?.address?.toLowerCase()===email)),
    newVerificationCodeTested:false};
}

async function main() {
  let release;
  try {
    const {values} = parseArgs({options:{store:{type:'string'},'outlook-root':{type:'string'},mailbox:{type:'string'}}});
    if (!values.store) throw Object.assign(Error(),{code:'verification_store_required'});
    const root = path.resolve(values.store);
    const relative = path.relative(path.resolve(__dirname,'../../.cache'),root);
    if (!relative || relative.startsWith('..') || path.isAbsolute(relative) || !fs.existsSync(path.join(root,'vault.json'))) {
      throw Object.assign(Error(),{code:'verification_store_required'});
    }
    const consoleRoot = path.join(values['outlook-root'] || 'D:/outlook-workbench','tools/console');
    release = require(path.join(consoleRoot,'writer-lock.cjs')).acquire(root);
    const {Store,atomic} = require(path.join(consoleRoot,'store.cjs'));
    const {Mail} = require(path.join(consoleRoot,'mail.cjs'));
    const store = new Store(root), mail = new Mail(store,null);
    const selected = values.mailbox ? [values.mailbox.trim().toLowerCase()] : Object.keys(store.mailboxes);
    if (!selected.length || selected.length>5) throw Object.assign(Error(),{code:'verification_batch_invalid'});
    const results=[];
    for (const email of selected) {
      const secret=store.vault.mailboxes[email];
      if (!secret || store.mailboxes[email]?.reader!=='graph') throw Object.assign(Error(),{code:'mailbox_unknown'});
      try {
        const result=await store.exclusive('mail:'+email,()=>verifyMailbox(mail,secret,email,AbortSignal.timeout(45000)));
        results.push({ok:true,...result});
      } catch(error) { results.push({ok:false,error:safeError(error)}); }
      // No raw Microsoft responses, mail content, identifiers, addresses or tokens in the receipt.
      console.log(JSON.stringify({index:results.length,...results.at(-1)}));
      if (!results.at(-1).ok) break;
    }
    atomic(path.join(root,'read-verification.json'),{checkedAt:new Date().toISOString(),results});
    if (results.some(r=>!r.ok)) process.exitCode=1;
  } catch(error) { console.error(safeError(error));process.exitCode=1; }
  finally { release?.(); }
}
if(require.main===module) main();
module.exports={verifyMailbox,safeError};
