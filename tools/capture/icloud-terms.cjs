// Sign an Apple Account in at icloud.com and accept the iCloud terms there.
//
// The GSA session alone leaves a fresh account with `delegate refused status=1`;
// Apple only enables the iCloud delegate once the iCloud web app has recorded
// terms acceptance for that account. This is the cheapest way to do that
// without the native ADI profile.
//
// node tools/capture/icloud-terms.cjs --email a@b.c --password p \
//   --activation <id> --hero-key K --previous <lastcode> --outdir .cache/terms-01

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

const outdir = path.resolve(arg("outdir", ".cache/terms"));
const email = String(arg("email", ""));
const password = String(arg("password", ""));
const activation = String(arg("activation", ""));
const heroKey = String(arg("hero-key", ""));
const previous = String(arg("previous", ""));
fs.mkdirSync(outdir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const frameOf = (page) => {
  const widget = page.frames().find((f) => f.url().includes("/widget/account/"));
  return widget || page.mainFrame();
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
  const launchOpts = { channel: "chrome", headless: true, args: ["--disable-blink-features=AutomationControlled"] };
  const proxy = String(arg("proxy", ""));
  if (proxy) {
    launchOpts.proxy = { server: proxy };
    if (arg("proxy-user", "")) {
      launchOpts.proxy.username = String(arg("proxy-user", ""));
      launchOpts.proxy.password = String(arg("proxy-pass", ""));
    }
  }
  const browser = await chromium.launch(launchOpts);
  const context = await browser.newContext({
    locale: "en-US",
    timezoneId: "America/New_York",
    viewport: { width: 1280, height: 1000 },
    userAgent:
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
  });
  let page = await context.newPage();
  const shot = async (n) => {
    await page.screenshot({ path: path.join(outdir, `${n}.png`) }).catch(() => {});
    const text = await page.evaluate(() => document.body?.innerText?.slice(0, 2000) || "").catch(() => "");
    fs.writeFileSync(path.join(outdir, `${n}.txt`), text, "utf8");
  };

  await page.goto("https://www.icloud.com/", { waitUntil: "domcontentloaded", timeout: 60000 });
  await sleep(4000);
  await shot("01-icloud");

  // The landing page uses a button; older variants used a link. The click can
  // open a popup window, so follow whichever surface appears.
  let active = page;
  const entry = page.getByRole("button", { name: /sign in/i }).or(page.getByRole("link", { name: /sign in/i })).first();
  if (await entry.count().catch(() => 0)) {
    const popupPromise = page.waitForEvent("popup", { timeout: 20000 }).catch(() => null);
    await entry.click().catch(() => {});
    const popup = await popupPromise;
    if (popup) {
      active = popup;
      await popup.waitForLoadState("domcontentloaded").catch(() => {});
    }
    await sleep(6000);
  }
  page = active;
  await active.screenshot({ path: path.join(outdir, "02-signin-page.png") }).catch(() => {});
  fs.writeFileSync(
    path.join(outdir, "02-signin-page.txt"),
    await active.evaluate(() => document.body?.innerText?.slice(0, 1500) || "").catch(() => ""),
    "utf8",
  );

  let f = frameOf(page);
  const user = f.locator('input[type="email"], input[name="accountName"], input[autocomplete="username"], input[placeholder*="Email" i], input[placeholder*="Phone" i]').first();
  if (await user.count().catch(() => 0)) {
    await user.fill(email);
    await sleep(400);
    await f.locator('button[type="submit"]').first().click().catch(() => {});
    await sleep(8000);
    await shot("03-password-page");
  }

  f = frameOf(page);
  const pass = f.locator('input[type="password"]').first();
  if (await pass.count().catch(() => 0)) {
    await pass.fill(password);
    await sleep(400);
    await f.locator('button[type="submit"]').first().click().catch(() => {});
    await sleep(9000);
    await shot("04-after-password");
  }

  // 2FA: six boxes (or one field) and a code from the rented number.
  const deadline = Date.now() + 180000;
  let code = "";
  while (Date.now() < deadline) {
    const boxes = frameOf(page).locator('input[type="tel"], input[inputmode="numeric"]');
    if ((await boxes.count().catch(() => 0)) > 0) break;
    await sleep(3000);
  }
  code = await smsCode().catch(() => "");
  fs.writeFileSync(path.join(outdir, "sms-code.txt"), code, "utf8");
  await shot("05-2fa");

  if (code) {
    f = frameOf(page);
    const boxes = f.locator('input[type="tel"], input[inputmode="numeric"]');
    const n = await boxes.count().catch(() => 0);
    if (n >= 6) {
      for (let i = 0; i < 6; i += 1) await boxes.nth(i).fill(code[i]).catch(() => {});
    } else if (n >= 1) {
      await boxes.first().fill(code).catch(() => {});
    }
    await sleep(600);
    await f.locator('button[type="submit"]').first().click().catch(() => {});
    await sleep(12000);
    await shot("06-after-2fa");
  }

  // Terms: iCloud shows an acceptance sheet on first use.
  for (let i = 0; i < 3; i += 1) {
    const text = await page.evaluate(() => document.body?.innerText || "").catch(() => "");
    if (!/terms|conditions|agree/i.test(text)) break;
    const agree = page.getByRole("button", { name: /agree|accept|continue|ok/i }).first();
    if (await agree.count().catch(() => 0)) {
      await agree.click().catch(() => {});
      await sleep(6000);
      await shot(`07-agreed-${i + 1}`);
    } else {
      break;
    }
  }

  await context.storageState({ path: path.join(outdir, "storage.json") });
  await shot("08-final");
  await browser.close();
  console.log(`terms run done code=${code || "none"}`);
})().catch((e) => {
  console.error("terms run failed:", e);
  process.exit(1);
});
