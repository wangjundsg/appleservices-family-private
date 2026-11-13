// Sign in to icloud.com inside the Roxy/Forge browser and clear the iCloud
// terms gate, so `loginDelegates` stops returning status=1.
//
// The portal puts its form inside an idmsa.apple.com iframe
// (`#account_name_text_field` / `#password_text_field`), which is why plain
// outer-document selectors never matched.
//
// node tools/capture/roxy-terms.cjs --port 55053 --email a@b.c --password p \
//   --activation <id> --hero-key K --previous <lastcode> --outdir .cache/roxy-terms

const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("D:/appleservices-family-private/.tools/js/node_modules/playwright-core");

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

const port = String(arg("port", ""));
const outdir = path.resolve(arg("outdir", ".cache/roxy-terms"));
const email = String(arg("email", ""));
const password = String(arg("password", ""));
const activation = String(arg("activation", ""));
const heroKey = String(arg("hero-key", ""));
const previous = String(arg("previous", ""));
fs.mkdirSync(outdir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function smsCode() {
  const res = await fetch(
    `https://hero-sms.com/stubs/handler_api.php?api_key=${heroKey}&action=getStatus&id=${activation}`,
  );
  const t = (await res.text()).trim();
  if (!t.startsWith("STATUS_OK")) return "";
  const c = t.split(":")[1] || "";
  return c && c !== previous ? c : "";
}

(async () => {
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`);
  const context = browser.contexts()[0] || (await browser.newContext());
  const page = context.pages()[0] || (await context.newPage());

  const shot = async (name) => {
    await page.screenshot({ path: path.join(outdir, `${name}.png`) }).catch(() => {});
    const text = await page.evaluate(() => document.body?.innerText?.slice(0, 1200) || "").catch(() => "");
    fs.writeFileSync(path.join(outdir, `${name}.txt`), text, "utf8");
    return text;
  };

  await page.goto("https://www.icloud.com/", { waitUntil: "domcontentloaded", timeout: 90000 });
  await sleep(4000);

  const signIn = page.getByRole("button", { name: /sign in/i }).or(page.getByRole("link", { name: /sign in/i })).first();
  if (await signIn.count().catch(() => 0)) {
    await signIn.click().catch(() => {});
    await sleep(7000);
  }
  await shot("01-signin");

  const auth = page.frameLocator('iframe[src*="idmsa.apple.com"]');
  const userField = auth.locator("#account_name_text_field");
  await userField.waitFor({ timeout: 60000 });
  await userField.fill(email);
  await sleep(400);
  await auth.getByRole("button", { name: /continue|next|→/i }).first().click().catch(() => {});
  await sleep(7000);
  await shot("02-password");

  const pwField = auth.locator("#password_text_field");
  await pwField.waitFor({ timeout: 60000 });
  await pwField.fill(password);
  await sleep(400);
  await auth.getByRole("button", { name: /continue|sign in/i }).first().click().catch(() => {});
  await sleep(10000);
  await shot("03-after-password");

  // 2FA: wait for the rented number to receive a new code.
  const deadline = Date.now() + 240000;
  let code = "";
  while (Date.now() < deadline) {
    code = await smsCode().catch(() => "");
    if (code) break;
    await sleep(4000);
  }
  fs.writeFileSync(path.join(outdir, "sms-code.txt"), code, "utf8");
  await shot("04-2fa");

  if (code) {
    const boxes = auth.locator('input[type="tel"]');
    const n = await boxes.count().catch(() => 0);
    if (n >= 6) {
      for (let i = 0; i < 6; i += 1) await boxes.nth(i).fill(code[i]).catch(() => {});
    } else {
      const single = auth.locator("input").first();
      await single.fill(code).catch(() => {});
    }
    await sleep(800);
    await auth.getByRole("button", { name: /continue|verify|sign in/i }).first().click().catch(() => {});
    await sleep(14000);
    await shot("05-after-2fa");
  }

  // Terms sheet, wherever it renders (portal or iframe).
  for (let i = 0; i < 4; i += 1) {
    const text = await page.evaluate(() => document.body?.innerText || "").catch(() => "");
    const inFrame = await auth
      .locator("body")
      .innerText()
      .catch(() => "");
    const combined = `${text}\n${inFrame}`;
    if (!/terms|conditions|agree|条款/i.test(combined)) break;
    const agreeOuter = page.getByRole("button", { name: /agree|accept|continue|ok/i }).first();
    if (await agreeOuter.count().catch(() => 0)) {
      await agreeOuter.click().catch(() => {});
    } else {
      await auth.getByRole("button", { name: /agree|accept|continue|ok/i }).first().click().catch(() => {});
    }
    await sleep(8000);
    await shot(`06-accepted-${i + 1}`);
  }

  await shot("07-final");
  // Disconnect only; the Roxy window keeps running.
  await browser.close().catch(() => {});
  console.log(`roxy terms done code=${code || "none"}`);
})().catch((e) => {
  console.error("roxy terms failed:", e);
  process.exit(1);
});
