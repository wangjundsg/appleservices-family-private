#!/usr/bin/env node
// 取验证码（只读）：复用 tools/capture/graph-mail.cjs 的导出函数，不改它一行。
//
// 与 graph-mail.cjs --list 的区别：--list 只列 subject，验证码在正文里；
// 这里用它的 exchangeToken / listMessages / pickCode / codeFromText 把码抠出来。
//
// 用法：
//   node tools/console/mail-code.cjs --file .cache/console/tokens/mb1.json [--top 5]
//        [--senders apple.com,icloud.com,microsoft.com] [--any]
//
// 输出：一行 JSON（stdout）。token / password 一律不打印。

'use strict';

const mail = require('../capture/graph-mail.cjs');

function arg(name, fallback = '') {
  const argv = process.argv.slice(2);
  const i = argv.indexOf('--' + name);
  if (i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--')) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith('--' + name + '='));
  return eq ? eq.slice(name.length + 3) : fallback;
}
const hasFlag = (name) => process.argv.slice(2).includes('--' + name);

const DEFAULT_SENDERS = ['apple.com', 'icloud.com', 'microsoft.com', 'accountprotection.microsoft.com', 'microsoftonline.com'];

(async function main() {
  const file = arg('file');
  if (!file) {
    console.log(JSON.stringify({ ok: false, error: '--file is required' }));
    process.exit(1);
  }

  let cfg;
  try {
    cfg = mail.readTokenFile(file);
  } catch (e) {
    console.log(JSON.stringify({ ok: false, error: String(e.message || e) }));
    process.exit(1);
  }

  const tok = await mail.exchangeToken(cfg);
  if (!tok.ok) {
    console.log(JSON.stringify({ ok: false, mailbox: cfg.mailbox, error: 'token: ' + tok.error }));
    process.exit(1);
  }

  const top = Math.max(1, Math.min(25, Number(arg('top', '8')) || 8));
  const list = await mail.listMessages(cfg, tok.access_token, { top });
  if (!list.ok) {
    console.log(JSON.stringify({ ok: false, mailbox: cfg.mailbox, error: list.error }));
    process.exit(1);
  }

  const senders = arg('senders')
    ? arg('senders').split(',').map((s) => s.trim()).filter(Boolean)
    : DEFAULT_SENDERS;

  let hit = mail.pickCode(list.messages, { mailbox: cfg.mailbox, senderDomains: senders });
  if (!hit && hasFlag('any')) {
    // 兜底：任意发件人，只要正文里有 6 位码就取最新那封
    const sorted = list.messages.slice().sort((a, b) => Date.parse(b.receivedDateTime) - Date.parse(a.receivedDateTime));
    for (const m of sorted) {
      const code = mail.codeFromText(m.body) || mail.codeFromText(m.bodyPreview);
      if (code) {
        hit = { code, messageId: m.id, received: m.receivedDateTime, subject: m.subject, from: m.from };
        break;
      }
    }
  }

  const messages = list.messages.map((m) => ({
    received: m.receivedDateTime,
    from: m.from,
    subject: m.subject,
    code: mail.codeFromText(m.body) || mail.codeFromText(m.bodyPreview) || '',
  }));

  console.log(
    JSON.stringify({
      ok: true,
      mailbox: cfg.mailbox,
      scope: tok.scope,
      count: messages.length,
      code: hit ? hit.code : '',
      hit: hit ? { subject: hit.subject, from: hit.from, received: hit.received } : null,
      messages,
    }),
  );
})().catch((e) => {
  console.log(JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
  process.exit(1);
});
