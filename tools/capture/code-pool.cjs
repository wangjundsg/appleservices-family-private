'use strict';
// Apple consumes the Outlook service contract, never its mailbox credentials.
const fs = require('node:fs');
const notBefore = new Map();
const sleep = ms => new Promise(resolve => setTimeout(resolve,ms));
function config(file) {
  let value;
  try { value = JSON.parse(fs.readFileSync(file, 'utf8').replace(/^\uFEFF/, '')); }
  catch { throw Error('code_pool_config_unavailable'); }
  let url;
  try { url = new URL(value.baseUrl); } catch { throw Error('code_pool_config_invalid'); }
  if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || url.username || url.password || !value.apiToken) throw Error('code_pool_config_invalid');
  return value;
}
function validateHit(value, {mailbox, since, until, excluded}) {
  const received = Date.parse(value.received_at);
  const sender = String(value.from || '').toLowerCase().split('@').pop();
  const apple = ['apple.com','icloud.com'].some(domain => sender === domain || sender.endsWith('.'+domain));
  if (value.ok !== true || !/^\d{6}$/.test(value.code) || typeof value.code !== 'string' ||
      value.mailbox !== mailbox.toLowerCase() || !apple || typeof value.message_id !== 'string' || !value.message_id ||
      excluded.includes(value.message_id) || !Number.isFinite(received) || received <= since || received > until) throw Error('code_pool_response_invalid');
  return {code:value.code, messageId:value.message_id, created_at:value.received_at};
}
async function pollCode({file, mailbox, since, timeoutMs, excluded=[], fetchImpl=fetch}) {
  const cfg = config(file), deadline = Date.now()+timeoutMs;
  while (Date.now() < deadline) {
    const cooling=(notBefore.get(file)||0)-Date.now();
    if(cooling>0) { await sleep(Math.min(cooling,Math.max(0,deadline-Date.now()))); continue; }
    const until = Date.now(), remaining = deadline-Date.now();
    let response;
    try {
      response = await fetchImpl(new URL('/api/v1/code',cfg.baseUrl), {
        method:'POST', redirect:'error', signal:AbortSignal.timeout(Math.max(1,Math.min(30000,remaining))),
        headers:{'Content-Type':'application/json',Authorization:'Bearer '+cfg.apiToken},
        body:JSON.stringify({mailbox,since:new Date(since).toISOString(),until:new Date(until).toISOString(),wait_seconds:0,exclude_message_ids:excluded}),
      });
    } catch { notBefore.set(file,Date.now()+4000); continue; }
    let value; try { value = await response.json(); } catch { throw Error('code_pool_response_invalid'); }
    if (response.ok && value.ok) return validateHit(value,{mailbox,since,until,excluded});
    const temporary = (response.ok && value.error === 'no_code_yet') || (value.retryable === true && [408,409,429,502,503].includes(response.status));
    if (!temporary) throw Error('code_pool_read_rejected');
    const retryAfter=Number(value.retry_after_seconds || 0);
    if (!Number.isFinite(retryAfter) || retryAfter<0) throw Error('code_pool_response_invalid');
    const delay = Math.max(4000, retryAfter*1000);
    notBefore.set(file,Date.now()+delay);
  }
  return null;
}
module.exports={config,validateHit,pollCode};
