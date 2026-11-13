// Drive `bin/login.exe` non-interactively: it requests an SMS, we poll the SMS
// provider for the new code, and hand it back on stdin. Produces a real GSA
// session (state/accounts/<dir>/session.json) for the freshly created account.
//
// Usage:
//   node tools/capture/gsa-login.cjs --appleid a@b.c --password p --dir state/accounts/x \
//     --hero-key K --activation 1112400649 [--proxy http://...] [--previous 810914]

const { spawn } = require("node:child_process");
const path = require("node:path");

function arg(name, fallback) {
  const flag = `--${name}`;
  for (let i = 0; i < process.argv.length; i += 1) {
    const a = process.argv[i];
    if (a === flag) {
      const next = process.argv[i + 1];
      return next && !next.startsWith("--") ? next : true;
    }
    if (a.startsWith(`${flag}=`)) return a.slice(flag.length + 1);
  }
  return fallback;
}

const appleid = String(arg("appleid", ""));
const password = String(arg("password", ""));
const dir = String(arg("dir", "state/accounts/us-01"));
const heroKey = String(arg("hero-key", ""));
const activation = String(arg("activation", ""));
const previous = String(arg("previous", ""));
const proxy = String(arg("proxy", ""));
const egressIP = String(arg("egress-ip", ""));

// Account-level profile: read the frozen fingerprint and record what was
// actually used. GSA's own User-Agent comes from the anisette identity, so it
// is NOT overridden here; only the stages that speak browser HTTP (web login,
// keepalive) consume APPLESERVICES_UA/LOCALE.
function readProfile(dir) {
  const file = path.join(dir, "profile.json");
  try {
    return JSON.parse(require("node:fs").readFileSync(file, "utf8"));
  } catch {
    return null;
  }
}

function observeProfile(dir, actual) {
  const file = path.join(dir, "profile.json");
  let doc;
  try {
    doc = JSON.parse(require("node:fs").readFileSync(file, "utf8"));
  } catch {
    return;
  }
  const prev = doc.actual || {};
  doc.actual = {
    ...prev,
    ...Object.fromEntries(Object.entries(actual).filter(([, v]) => v !== "" && v != null)),
    observed_at: new Date().toISOString(),
  };
  require("node:fs").writeFileSync(file, JSON.stringify(doc, null, 2), "utf8");
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function codeOnce() {
  const res = await fetch(
    `https://hero-sms.com/stubs/handler_api.php?api_key=${heroKey}&action=getStatus&id=${activation}`,
  );
  const text = (await res.text()).trim();
  if (!text.startsWith("STATUS_OK")) return "";
  const code = text.split(":")[1] || "";
  return code && code !== previous ? code : "";
}

(async () => {
  const profile = readProfile(path.resolve(dir));
const env = { ...process.env, APPLESERVICES_PASSWORD: password };
if (profile) {
  env.APPLESERVICES_PROFILE_DIR = path.resolve(dir);
  env.APPLESERVICES_UA = profile.ua || "";
  env.APPLESERVICES_LOCALE = profile.locale || "";
  env.APPLESERVICES_TIMEZONE = profile.timezone || "";
}
  if (proxy) {
    env.HTTPS_PROXY = proxy;
    env.HTTP_PROXY = proxy;
  }
  const child = spawn(path.resolve("bin/login.exe"), ["-appleid", appleid, "-dir", dir, "-twofactor", "sms", ...(arg("anisette", "") ? ["-anisette", String(arg("anisette", ""))] : [])], {
    env,
    stdio: ["pipe", "pipe", "pipe"],
  });

  let buffer = "";
  let codeSent = false;
  const consume = async (chunk, isErr) => {
    const text = chunk.toString();
    buffer += text;
    process.stdout.write(text);
    if (codeSent) return;
    if (!/six-digit code|verification code requested/i.test(text)) return;
    codeSent = true;
    const deadline = Date.now() + 240000;
    while (Date.now() < deadline) {
      const code = await codeOnce().catch(() => "");
      if (code) {
        // Review A2: the code used to be printed verbatim, so it landed in every
        // terminal log and wrapper transcript of a run. The length is enough to
        // tell "SMS arrived" from "SMS is malformed".
        console.log(`\n[gsa-login] sms code received (len=${code.length})`);
        child.stdin.write(`${code}\n`);
        return;
      }
      await sleep(4000);
    }
    console.error("\n[gsa-login] no sms code arrived");
    child.kill();
  };

  child.stdout.on("data", (c) => consume(c, false));
  child.stderr.on("data", (c) => consume(c, true));
  child.on("exit", (code, signal) => {
    console.log(`\n[gsa-login] exit=${code} signal=${signal}`);
    const major = profile && /Chrome\/(\d+)/.exec(profile.ua || "");
    observeProfile(path.resolve(dir), {
      engine: major ? `chrome-${major[1]}` : "",
      egress_ref: proxy ? "cli-proxy" : "",
      egress_ip: egressIP,
    });
    // A killed child (code=null) or a signal is never success.
    const ok = code === 0 && !signal;
    process.exit(ok ? 0 : 1);
  });
})();
