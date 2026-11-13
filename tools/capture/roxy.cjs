// Attach to a Roxy/Forge browser over CDP and report what Apple would see.
//
// Forge (50100) returns `debuggerAddress`, so Playwright connects to the real
// fingerprinted Chrome instead of launching its own. That is the whole point:
// locale, timezone, GPU strings and proxy all come from the Roxy profile.
//
// node tools/capture/roxy.cjs --port 63352 --outdir .cache/roxy-probe

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
const outdir = path.resolve(arg("outdir", ".cache/roxy"));
fs.mkdirSync(outdir, { recursive: true });

(async () => {
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`);
  const context = browser.contexts()[0] || (await browser.newContext());
  const page = context.pages()[0] || (await context.newPage());

  await page.goto("http://ip-api.com/json/?fields=status,country,countryCode,city,isp,query", {
    waitUntil: "domcontentloaded",
    timeout: 60000,
  });
  const ip = (await page.textContent("body")) || "";
  const fp = await page.evaluate(() => ({
    language: navigator.language,
    languages: navigator.languages,
    tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
    ua: navigator.userAgent,
    cores: navigator.hardwareConcurrency,
    memory: navigator.deviceMemory,
    screen: `${screen.width}x${screen.height}@${window.devicePixelRatio}`,
    webdriver: navigator.webdriver,
  }));
  console.log(JSON.stringify({ ip: ip.trim(), fp }, null, 2));
  fs.writeFileSync(path.join(outdir, "probe.json"), JSON.stringify({ ip: ip.trim(), fp }, null, 2));
  await browser.close().catch(() => {});
})();
