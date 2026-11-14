// Shared in-page probe. Evaluated as an expression in BOTH clients:
//   Playwright : page.evaluate(<this file>)   (awaits the promise)
//   Selenium   : execute_script(<this file>)  (W3C execute script awaits promise)
// Keep it dependency-free and side-effect-free: it only reads.
(async () => {
  const out = {};

  // ---- navigator basics --------------------------------------------------
  const nav = navigator;
  out.webdriver = nav.webdriver;                       // undefined | false | true
  out.hasWebdriverProp = 'webdriver' in nav;
  out.userAgent = nav.userAgent;
  out.appVersion = nav.appVersion;
  out.platform = nav.platform;
  out.language = nav.language;
  out.languages = (nav.languages || []).slice(0, 4);
  out.hardwareConcurrency = nav.hardwareConcurrency;
  out.deviceMemory = nav.deviceMemory;
  out.maxTouchPoints = nav.maxTouchPoints;
  out.pluginsLength = nav.plugins ? nav.plugins.length : null;
  out.mimeTypesLength = nav.mimeTypes ? nav.mimeTypes.length : null;
  out.pluginsIsPluginArray = nav.plugins instanceof PluginArray;
  out.vendor = nav.vendor;
  out.pdfViewerEnabled = nav.pdfViewerEnabled;
  out.timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  out.screen = `${screen.width}x${screen.height}@${window.devicePixelRatio}`;
  out.innerSize = `${innerWidth}x${innerHeight}`;

  // ---- window.chrome -----------------------------------------------------
  out.chromeType = typeof window.chrome;               // undefined | object
  out.chromeKeys = window.chrome ? Object.keys(window.chrome) : null;
  out.hasChromeRuntime = Boolean(window.chrome && window.chrome.runtime);
  out.hasChromeLoadTimes = Boolean(window.chrome && window.chrome.loadTimes);

  // ---- permissions -------------------------------------------------------
  try {
    const p = await navigator.permissions.query({ name: 'notifications' });
    out.permissionNotifications = p.state;
    out.permissionNotificationsOwn = Object.getOwnPropertyNames(p).join(',');
  } catch (e) {
    out.permissionNotifications = `ERR:${String(e).slice(0, 80)}`;
  }
  try {
    const p2 = await navigator.permissions.query({ name: 'clipboard-read' });
    out.permissionClipboard = p2.state;
  } catch (e) {
    out.permissionClipboard = `ERR:${String(e).slice(0, 60)}`;
  }

  // ---- native-ness of the things that get patched ------------------------
  const NATIVE = 'function %s() { [native code] }';
  const isNative = (fn) => {
    try { return Function.prototype.toString.call(fn) === 'function ' + fn.name + '() { [native code] }' || /\[native code\]/.test(Function.prototype.toString.call(fn)); }
    catch { return false; }
  };
  out.toStrNative = (() => { try { return /\[native code\]/.test(Function.prototype.toString.toString()); } catch { return false; } })();
  out.tostrOfWebdriverGetter = (() => {
    try {
      const d = Object.getOwnPropertyDescriptor(Navigator.prototype, 'webdriver');
      return d && d.get ? Function.prototype.toString.call(d.get) : 'n/a';
    } catch { return 'ERR'; }
  })();
  out.nativeChecks = {
    querySelectorAll: isNative(Document.prototype.querySelectorAll),
    addEventListener: isNative(EventTarget.prototype.addEventListener),
    getImageData: isNative(CanvasRenderingContext2D.prototype.getImageData),
    toDataURL: isNative(HTMLCanvasElement.prototype.toDataURL),
    getParameter: isNative(WebGLRenderingContext.prototype.getParameter),
  };

  // ---- injected-global / driver fingerprint scan -------------------------
  // Anything a driver or an automation library leaves behind in the main world.
  const SUSPECT = /^(cdc_|_?selenium|__playwright|__pw|webdriver|__webdriver|__cdp|_phantom|domAutomation|__nightmare|__puppeteer|__devtools)/i;
  const globals = [];
  try {
    for (const k of Object.getOwnPropertyNames(window)) {
      if (SUSPECT.test(k)) globals.push(k);
    }
  } catch { /* ignore */ }
  out.suspectGlobals = globals.slice(0, 40);
  out.suspectGlobalCount = globals.length;

  const docKeys = [];
  try {
    for (const k of Object.getOwnPropertyNames(document)) {
      if (SUSPECT.test(k)) docKeys.push(k);
    }
  } catch { /* ignore */ }
  out.suspectDocumentKeys = docKeys.slice(0, 20);

  // ---- injection traces in the DOM / error objects ------------------------
  out.domTrace = (() => {
    try {
      const hits = [];
      const all = document.querySelectorAll('*');
      for (let i = 0; i < all.length && i < 4000; i += 1) {
        const el = all[i];
        for (const a of el.attributes) {
          if (/cdc_|selenium|webdriver|__pw|__playwright/i.test(a.name) || /cdc_|__playwright/i.test(a.value)) {
            hits.push(`${el.tagName}#${el.id}.${a.name}`);
          }
        }
      }
      return hits.slice(0, 10);
    } catch { return ['ERR']; }
  })();

  // ---- stack-trace shape (CDP-driven scripts show up here) ---------------
  out.stackHead = (() => {
    try {
      return String(new Error('probe').stack || '').split('\n').slice(0, 3).join(' | ');
    } catch { return 'ERR'; }
  })();

  return out;
})()
