// Throw-away smoke test for the native (patchright + real Chrome) arm.
// Verifies, in this order: launch, SOCKS5-with-auth through the residential
// exit, egress IP, and the UNPATCHED fingerprint the browser reports.
// Never touches a Microsoft host.
'use strict';

const { chromium } = require('D:/appleservices-family-private/.tools/js/node_modules/patchright-core');

// Chromium refuses socks5 credentials, so Chrome points at the local no-auth
// relay (tools/capture/socks5-relay.cjs) which carries the auth hop upstream.
const PROXY = {
  server: 'socks5://127.0.0.1:7891',
};
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const ARGS = ['--lang=en-US', '--no-first-run', '--no-default-browser-check'];

(async () => {
  const t0 = Date.now();
  let browser = null;
  let via = '';
  try {
    browser = await chromium.launch({ channel: 'chrome', headless: false, proxy: PROXY, args: ARGS, timeout: 120000 });
    via = 'channel:chrome';
  } catch (error) {
    console.log('LAUNCH channel=chrome FAILED ::', String(error).slice(0, 300));
    browser = await chromium.launch({ executablePath: CHROME, headless: false, proxy: PROXY, args: ARGS, timeout: 120000 });
    via = 'executablePath';
  }
  console.log('LAUNCH ok via', via, 'version', browser.version(), `( ${Date.now() - t0}ms )`);

  const context = await browser.newContext({ locale: 'en-US', timezoneId: 'America/New_York', viewport: null });
  const page = await context.newPage();

  const fp = await page.evaluate(() => {
    let webgl = null;
    try {
      const gl = document.createElement('canvas').getContext('webgl');
      const dbg = gl && gl.getExtension('WEBGL_debug_renderer_info');
      if (dbg) webgl = [gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL), gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL)];
    } catch { /* ignore */ }
    return {
      ua: navigator.userAgent,
      webdriver: navigator.webdriver,
      languages: navigator.languages,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      hardwareConcurrency: navigator.hardwareConcurrency,
      deviceMemory: navigator.deviceMemory,
      plugins: navigator.plugins.length,
      mimeTypes: navigator.mimeTypes.length,
      platform: navigator.platform,
      vendor: navigator.vendor,
      hasChromeObject: Boolean(window.chrome),
      screen: [screen.width, screen.height, screen.availWidth, screen.availHeight, screen.colorDepth, devicePixelRatio],
      webgl,
    };
  });
  console.log('FP', JSON.stringify(fp, null, 1));

  const probe = 'https://ipinfo.io/json';
  try {
    await page.goto(probe, { waitUntil: 'domcontentloaded', timeout: 60000 });
    const body = (await page.textContent('body') || '').trim();
    console.log('EGRESS', body.slice(0, 500));
  } catch (error) {
    console.log('EGRESS FAILED ::', String(error).slice(0, 300));
  }

  console.log('total_ms', Date.now() - t0);
  await browser.close();
  process.exit(0);
})().catch((error) => {
  console.error('SMOKE FAILED ::', String(error).slice(0, 600));
  process.exit(1);
});
