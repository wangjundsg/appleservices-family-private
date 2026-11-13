// Cancel one hero-sms activation and take the refund.
//
// docs/run-20261113.md §4.7: after `create-account` exited non-zero the number
// sat on STATUS_WAIT_CODE until a human ran `setStatus&status=8` by hand. The
// cancellation is free and idempotent, so a failed run should always ask for it.
//
// Usage:
//   node tools/capture/refund.cjs --hero-key <key> --activation <id>
//
// Output: one JSON object on stdout
//   {"ok":true,"response":"ACCESS_CANCEL","activation":"1114967085","elapsed_ms":310}
// Exit codes: 0 = the provider accepted the cancellation (cancelled now or
// already cancelled), 1 = anything else. hero-sms itself rejects a number whose
// SMS code was already requested; that rejection is recorded, not retried.
//
// The API key never reaches stdout, a log line or an error message: hero takes
// it as a query parameter and the URL is never printed.

"use strict";

const HERO_API = "https://hero-sms.com/stubs/handler_api.php";

// The only two answers that mean "the money is back".
const REFUND_ACCEPTED = new Set(["ACCESS_CANCEL", "ACCESS_CANCEL_ALREADY"]);

// refundAccepted classifies the provider's answer. Anything else (BAD_ACTION,
// BAD_KEY, NO_ACTIVATION, a refusal because the code was used, an HTML error
// page) is not a refund.
function refundAccepted(response) {
  return REFUND_ACCEPTED.has(String(response == null ? "" : response).trim());
}

function heroURL(heroKey, activation) {
  const query = new URLSearchParams({
    api_key: String(heroKey),
    action: "setStatus",
    status: "8",
    id: String(activation),
  });
  return `${HERO_API}?${query.toString()}`;
}

// refundActivation asks hero-sms to cancel the number. It returns the plain
// answer {ok, response}; a transport failure throws and the caller decides
// whether that matters (the create driver logs it and keeps its own exit code).
async function refundActivation({ heroKey, activation, fetchImpl = globalThis.fetch, timeoutMs = 20_000 } = {}) {
  if (!heroKey || !activation) throw new Error("refund needs --hero-key and --activation");
  const started = Date.now();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetchImpl(heroURL(heroKey, activation), { signal: controller.signal });
    const text = String(await response.text()).trim();
    return {
      ok: refundAccepted(text),
      response: text.slice(0, 120),
      activation: String(activation),
      elapsed_ms: Date.now() - started,
    };
  } finally {
    clearTimeout(timer);
  }
}

// ---- CLI ------------------------------------------------------------------

function parseArgs(argv) {
  const options = { heroKey: "", activation: "" };
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i];
    const value = () => {
      const next = argv[i + 1];
      if (next === undefined || String(next).startsWith("--")) throw new Error(`missing value for ${flag}`);
      i += 1;
      return next;
    };
    switch (flag) {
      case "--hero-key":
        options.heroKey = value();
        break;
      case "--activation":
        options.activation = value();
        break;
      case "-h":
      case "--help":
        options.help = true;
        break;
      default:
        throw new Error(`unknown argument ${flag}`);
    }
  }
  return options;
}

async function main(argv) {
  let options;
  try {
    options = parseArgs(argv);
  } catch (e) {
    console.log(JSON.stringify({ ok: false, error: String(e.message || e) }));
    return 1;
  }
  if (options.help) {
    console.log("usage: node tools/capture/refund.cjs --hero-key <key> --activation <id>");
    return 0;
  }
  if (!options.heroKey || !options.activation) {
    console.log(JSON.stringify({ ok: false, error: "both --hero-key and --activation are required" }));
    return 1;
  }
  try {
    const result = await refundActivation(options);
    console.log(JSON.stringify(result));
    return result.ok ? 0 : 1;
  } catch (e) {
    console.log(
      JSON.stringify({
        ok: false,
        activation: options.activation,
        error: String(e && e.message ? e.message : e).slice(0, 160),
      }),
    );
    return 1;
  }
}

if (require.main === module) {
  main(process.argv.slice(2)).then((code) => process.exit(code));
}

module.exports = {
  HERO_API,
  main,
  parseArgs,
  refundAccepted,
  refundActivation,
};
