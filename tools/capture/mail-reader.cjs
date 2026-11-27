'use strict';
// One task-scoped entry point. Credential files stay with their existing owner.
const graph = require('./graph-mail.cjs');
const pool = require('./code-pool.cjs');
const PLATFORMS = Object.freeze({apple:['apple.com','icloud.com'],
  microsoft:['microsoft.com','accountprotection.microsoft.com'], openai:['openai.com']});
function address(value) {
  if (typeof value !== 'string' || value.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value.trim())) throw Error('mail_recipient_invalid');
  return value.trim().toLowerCase();
}
async function readCode({reader, file, mailbox, recipient, source, platform='apple', since,
  until, excluded=[], timeoutMs=30000, fetchImpl=fetch, onTick=()=>{}}) {
  mailbox = address(mailbox); recipient = address(recipient);
  if (!['registration','forwarded'].includes(source) || !Object.hasOwn(PLATFORMS, platform)) throw Error('mail_source_invalid');
  if (source === 'registration' && recipient !== mailbox) throw Error('mail_recipient_mismatch');
  if (!Number.isFinite(since) || since <= 0 || since > Date.now() || Date.now()-since > 86400000 ||
      (until !== undefined && (!Number.isFinite(until) || until < since || until > Date.now()+60000 || until-since > 86400000))) throw Error('mail_time_range_invalid');
  if (!Array.isArray(excluded) || excluded.length > 100 || excluded.some(id=>typeof id !== 'string' || !id || id.length > 2048)) throw Error('mail_excluded_invalid');
  if (!Number.isFinite(timeoutMs) || timeoutMs < 0 || timeoutMs > 300000) throw Error('mail_timeout_invalid');
  const options = {file,mailbox,recipient,source,platform,since,until,excluded,timeoutMs,fetchImpl};
  if (reader === 'otpgmail') return require('./otpgmail.cjs').pollCode({...options,onTick});
  if (reader === 'outlook') {
    // The current 8802 contract uses a global platform and cannot prove alias delivery.
    if (source !== 'registration' || platform !== 'apple') throw Error('code_pool_contract_unsupported');
    return pool.pollCode(options);
  }
  if (reader !== 'graph') throw Error('mail_reader_unsupported');
  if (!timeoutMs) return null;
  const signal = AbortSignal.timeout(Math.max(1, Math.ceil(timeoutMs)));
  const boundedFetch = (url, init={}) => fetchImpl(url, {...init,redirect:'error',signal});
  let hit;
  try {
    hit = await graph.pollCode({...options, fetchImpl:boundedFetch, senderDomains:PLATFORMS[platform], clockSkewMs:0, onTick});
  } catch { throw Error(signal.aborted ? 'mail_deadline_exceeded' : 'graph_read_failed'); }
  if (hit.ok) return {code:hit.code, messageId:hit.messageId, created_at:hit.received};
  if (hit.error?.startsWith('no matching mail')) return null;
  // Never expose OAuth/Graph response text through an orchestration error.
  const code = ['code_ambiguous','code_unrecognized','graph_mailbox_mismatch'].includes(hit.error) ? hit.error : 'graph_read_failed';
  throw Object.assign(Error(code), {code});
}
module.exports = {readCode};
