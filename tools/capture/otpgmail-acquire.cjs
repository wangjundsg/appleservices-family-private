"use strict";
// First mailbox acquisition; the pipeline holds the task lock for this process.
const fs = require("node:fs"), path = require("node:path"), {randomUUID} = require("node:crypto");
const provider = require("./otpgmail.cjs");
const fail = reason => Object.assign(new Error(reason), {reason});
const read = file => JSON.parse(fs.readFileSync(file, "utf8").replace(/^\uFEFF/, ""));
function atomic(file, value) {
  const temp = file + "." + randomUUID() + ".tmp";
  const fd = fs.openSync(temp, "wx", 0o600);
  try {fs.writeFileSync(fd, JSON.stringify(value)); fs.fsyncSync(fd);} finally {fs.closeSync(fd);}
  fs.renameSync(temp, file);
}

async function acquire(input, output, {fetchImpl=fetch, keyLoader=provider.loadKey, inspect=provider.inspect, now=Date.now}={}) {
  const policy = read(input).mail_purchase;
  if (!policy || !path.isAbsolute(policy.credentialFile || "") || ![1,2,4].includes(policy.warehouse) ||
      !Number.isSafeInteger(policy.max_price_vnd) || policy.max_price_vnd <= 0 || policy.max_price_vnd > 700) {
    throw fail("mailbox_purchase_policy_invalid");
  }
  const ledgerFile = path.join(path.dirname(output), "mail-purchase.json");
  let ledger = fs.existsSync(ledgerFile) ? read(ledgerFile) : null;
  if (ledger && (JSON.stringify(ledger.policy) !== JSON.stringify(policy) ||
      !/^[a-f0-9-]{36}$/.test(ledger.key || "") || !Number.isSafeInteger(ledger.startedAt))) {
    throw fail("mailbox_purchase_requires_readback");
  }
  if (fs.existsSync(output) && !ledger) throw fail("mailbox_purchase_requires_readback");
  const key = keyLoader(policy.credentialFile);
  if (!ledger || ledger.unissued) {
    const inventory = await inspect({credentialFile: policy.credentialFile, fetchImpl, keyLoader: () => key});
    const offer = inventory.offers.find(o => o.warehouse === policy.warehouse);
    if (!offer || offer.stock < 1) throw fail("mailbox_stock_unavailable");
    if (offer.priceVnd > policy.max_price_vnd) throw fail("mailbox_price_exceeded");
    if (inventory.balanceVnd < offer.priceVnd) throw fail("mailbox_balance_insufficient");
    ledger = ledger ? {...ledger, unissued:false} : {policy, key: randomUUID(), startedAt: now()};
    atomic(ledgerFile, ledger); // Durable before the only paid operation.
  }
  let order;
  if (ledger.orderId) {
    order = await provider.get("orders/" + ledger.orderId, key, {fetchImpl, timeoutMs: 20000});
  } else {
    // Provider keys expire after 24h. Leave a margin; never replay an expired key.
    if (now() < ledger.startedAt || now() - ledger.startedAt >= 23 * 3600000) {
      throw fail("mailbox_purchase_requires_readback");
    }
    const response = await fetchImpl("https://otpgmail.net/v1/orders", {
      method: "POST", redirect: "error", signal: AbortSignal.timeout(20000),
      headers: {Authorization: "Bearer " + key, "Content-Type": "application/json", "Idempotency-Key": ledger.key},
      body: JSON.stringify({service: "wx", domain: "gmail.com", warehouse: policy.warehouse, quantity: 1}),
    });
    const receipt = await response.json();
    if (!response.ok || receipt?.success !== true) {
      const code = receipt?.error?.code;
      if (["OUT_OF_STOCK", "NO_MAILS_AVAILABLE", "INSUFFICIENT_BALANCE"].includes(code)) {
        ledger.unissued = true;
        atomic(ledgerFile, ledger);
      }
      if (["OUT_OF_STOCK", "NO_MAILS_AVAILABLE"].includes(code)) throw fail("mailbox_stock_unavailable");
      if (code === "INSUFFICIENT_BALANCE") throw fail("mailbox_balance_insufficient");
      throw fail("mailbox_purchase_requires_readback");
    }
    order = Array.isArray(receipt.data) && receipt.data.length === 1 ? receipt.data[0] : null;
    if (/^[A-Za-z0-9_-]{1,128}$/.test(order?.orderId || "")) {
      ledger.orderId = order.orderId;
      atomic(ledgerFile, ledger); // Preserve the ID even if the rest needs review.
    }
  }
  if (!order || order.orderId !== ledger.orderId || order.service !== "wx" || order.domain !== "gmail.com" ||
      order.warehouse !== policy.warehouse || !/^[A-Za-z0-9._%+-]+@gmail\.com$/i.test(order.email || "") ||
      !Number.isSafeInteger(order.price) || order.price < 0 || order.price > policy.max_price_vnd ||
      !["waiting_code", "queued"].includes(order.status) || !Array.isArray(order.otp) || order.otp.length) {
    throw fail("mailbox_order_not_ready");
  }
  const result = {provider: "otpgmail", orderId: order.orderId, email: order.email.toLowerCase(),
    warehouse: order.warehouse, credentialFile: policy.credentialFile, priceVnd: order.price, status: order.status};
  if (fs.existsSync(output)) {
    const previous = read(output);
    if (previous.orderId !== result.orderId || previous.email !== result.email) throw fail("mailbox_purchase_requires_readback");
  }
  atomic(output, result);
  if (order.status === "queued") throw fail("mailbox_order_pending");
  return result;
}

if (require.main === module) {
  acquire(path.resolve(process.argv[2]), path.resolve(process.argv[3]))
    .then(() => console.log(JSON.stringify({state: "mailbox_ready"})))
    .catch(error => {
      const reason = /^mailbox_[a-z_]+$/.test(error.reason || "") ? error.reason : "mailbox_purchase_requires_readback";
      console.error(JSON.stringify({state: "needs_review", reason})); process.exitCode = 3;
    });
}
module.exports = {acquire};
