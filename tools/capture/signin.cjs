// Sign an existing Apple Account in through the account portal and keep the
// resulting browser state. Used right after creation so the account is not
// left without a live session.
//
// node tools/capture/signin.cjs --email a@b.c --password p --activity <id> \
//   --hero-key K --previous 810914 --outdir .cache/signin-01

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

const outdir = path.resolve(arg("outdir", ".cache/signin"));
const email = String(arg("email", ""));
const password = String(arg("password", ""));
const activation = String(arg("activation", ""));
const heroKey = String(arg("hero-key", ""));
const previous = String(arg("previous", ""));
fs.mkdirSync(outdir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// The create flow lives in the AuthKit iframe; the sign-in form is rendered by
// the portal itself, so fall back to the main frame.
const widget = (page) => page.frames().find((f) => f.url().includes("/widget/account/")) || page.mainFrame();
const textOf = async (page) => {
  const f = widget(page);
  return f ? f.evaluate(() => document.body?.innerText || "").catch(() => "") : "";
};

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
  const browser = await chromium.launch({ channel: "chrome", headless: true, args: ["--disable-blink-features=AutomationControlled"] });
  const context = await browser.newContext({
    locale: "en-US",
    timezoneId: "America/New_York",
    viewport: { width: 1280, height: 1000 },
    userAgent:
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
  });
  await context.addInitScript(() => Object.defineProperty(navigator, "webdriver", { get: () => undefined }));
  const page = await context.newPage();
  const shot = async (n) => {
    await page.screenshot({ path: path.join(outdir, `${n}.png`) }).catch(() => {});
    fs.writeFileSync(path.join(outdir, `${n}.txt`), await textOf(page), "utf8");
  };

  await page.goto("https://account.apple.com/account", { waitUntil: "domcontentloaded", timeout: 60000 });
  await sleep(5000);
  await page.getByText(/^Sign In$/).first().click({ timeout: 10000 }).catch(() => {});
  await sleep(4000);
  await shot("01-signin-form");

  let f = widget(page);
  const emailBox = f
    .locator('input[name="accountName"], input[type="email"], input[id*="account_name"], input[autocomplete="username"]')
    .first();
  if (await emailBox.count()) {
    await emailBox.fill(email);
    await sleep(500);
    await f.locator('button[type="submit"]').first().click({ timeout: 10000 }).catch(() => {});
    await sleep(8000);
    await shot("02-password");
  }

  f = widget(page);
  const passBox = f.locator('input[type="password"]').first();
  if (await passBox.count().catch(() => 0)) {
    await passBox.fill(password);
    await sleep(500);
    await f.locator('button[type="submit"]').first().click({ timeout: 10000 }).catch(() => {});
    await sleep(10000);
    await shot("03-after-password");
  }

  // 2FA: wait up to 4 minutes for the number to receive a fresh code.
  const deadline = Date.now() + 240000;
  let code = "";
  while (Date.now() < deadline) {
    const t = await textOf(page);
    if (/verification code|Verify|two-factor|trusted/i.test(t) || /Enter the code/i.test(t)) break;
    code = await smsCode().catch(() => "");
    if (code) break;
    await sleep(3000);
  }
  if (!code) code = await smsCode().catch(() => "");
  fs.writeFileSync(path.join(outdir, "sms-code.txt"), code, "utf8");
  await shot("04-2fa");

  if (code) {
    f = widget(page);
    const boxes = f.locator('input[type="tel"]');
    const n = await boxes.count().catch(() => 0);
    if (n >= 6) {
      for (let i = 0; i < 6; i += 1) await boxes.nth(i).fill(code[i]).catch(() => {});
    } else {
      await f.locator('input').first().fill(code).catch(() => {});
    }
    await sleep(800);
    await f.locator('button[type="submit"]').first().click({ timeout: 10000 }).catch(() => {});
    await sleep(12000);
    await shot("05-after-2fa");
  }

  await context.storageState({ path: path.join(outdir, "storage.json") });
  await browser.close();
  console.log("signin done", code ? `code=${code}` : "no code");
})().catch(async (e) => {
  console.error("signin failed:", e);
  process.exit(1);
});
