"use strict";

const fs = require("node:fs");

function readInput(file) {
  if (!file) return {};
  const value = JSON.parse(fs.readFileSync(file, "utf8"));
  if (!value || Array.isArray(value) || typeof value !== "object") throw new Error("invalid private input file");
  return value;
}

function expiry(value, now = Date.now()) {
  if (!value) return Infinity;
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed) || parsed <= now || parsed > now + 30 * 60_000) {
    throw new Error("activation expiry must be in the next 30 minutes");
  }
  return parsed;
}

// An ambiguous account may still need this number for its confirmation login.
function refundable(state) {
  return state === "failed" || state === "usage";
}

async function retainInput({ deadline, poll, notify, now = Date.now }) {
  if (!Number.isFinite(deadline)) return null;
  notify();
  while (now() < deadline) {
    const value = await poll(Math.min(30_000, deadline - now()));
    if (value && (typeof value !== "object" || value.code)) return value;
  }
  return null;
}

module.exports = { readInput, expiry, refundable, retainInput };
