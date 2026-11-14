// Press-and-hold probe: does the Microsoft signup entry hand us a HUMAN
// (PerimeterX) "press & hold" challenge, and does a real mouse press clear it?
//
// Scope discipline: this never types an email, never submits the signup form
// and never buys a solve. It stops at the challenge: detect -> press -> observe.
//
// Usage:
//   node tools/capture/px-hold-probe.cjs --proxy http://gw.example.com:10010 \
//        --proxy-user U --proxy-pass P [--url https://signup.live.com/signup] \
//        [--hold-ms 3000] [--dwell-ms 0] [--wait 45000] [--after-wait 10000] \
//        [--headless 1] [--advance] [--out .cache/px-hold/run-1.json] \
//        [--shots .cache/px-hold/run-1]
//
// Output: one JSON line (+ the same object in --out). Exit 0 = probe ran,
// 1 = launch/navigation failure, 3 = no challenge on this exit.
//
// Credentials are never echoed: proxy_user is redacted in the report and the
// password is only ever handed to Playwright.

const path = require("node:path");
const fs = require("node:fs");

const argv = process.argv.slice(2);
function arg(name, fallback = "") {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}
function flag(name) {
  return argv.includes(`--${name}`);
}
const rand = (min, max) => min + Math.random() * (max - min);
const round = (n, d = 1) => Number(Number(n).toFixed(d));

const target = arg("url", "https://signup.live.com/signup");
const proxy = arg("proxy", process.env.PX_PROBE_PROXY || "");
const proxyUser = arg("proxy-user", process.env.PX_PROBE_PROXY_USER || "");
const proxyPass = arg("proxy-pass", process.env.PX_PROBE_PROXY_PASS || "");
const direct = flag("direct");
const headless = arg("headless", "0") === "1" || flag("headless");
const waitMs = Number(arg("wait", "45000"));
const afterWaitMs = Number(arg("after-wait", "10000"));
const dwellMs = Number(arg("dwell-ms", "0"));
const holdMsArg = Number(arg("hold-ms", "0"));
const advance = flag("advance");
const noPress = flag("no-press");
const forceRender = flag("force-render");
const outPath = arg("out", "");
const shotDir = arg("shots", "");
const navTimeout = Number(arg("timeout", "60")) * 1000;

function loadPlaywright() {
  const candidates = [
    path.resolve(__dirname, "..", "..", ".tools", "js", "node_modules", "playwright-core"),
    "playwright-core",
    "playwright",
  ];
  for (const candidate of candidates) {
    try {
      return require(candidate);
    } catch {
      // next candidate
    }
  }
  throw new Error("playwright not found");
}

// --- mouse helpers ---------------------------------------------------------
// Human approach: cubic bezier with lateral bow, ease-out at the end, small
// per-sample noise. Never a straight teleport to the target.
function bezierPath(from, to, samples = 28) {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const dist = Math.hypot(dx, dy) || 1;
  const nx = -dy / dist;
  const ny = dx / dist;
  const bow = Math.min(180, Math.max(40, dist * 0.35)) * (Math.random() < 0.5 ? -1 : 1);
  const p1 = { x: from.x + dx * 0.28 + nx * bow, y: from.y + dy * 0.28 + ny * bow };
  const p2 = { x: from.x + dx * 0.66 - nx * bow * 0.45, y: from.y + dy * 0.66 - ny * bow * 0.45 };
  const points = [];
  for (let i = 1; i <= samples; i += 1) {
    const raw = i / samples;
    // ease-out quint: fast in the middle, decelerating into the target
    const t = 1 - Math.pow(1 - raw, 4);
    const mt = 1 - t;
    const x =
      mt * mt * mt * from.x + 3 * mt * mt * t * p1.x + 3 * mt * t * t * p2.x + t * t * t * to.x;
    const y =
      mt * mt * mt * from.y + 3 * mt * mt * t * p1.y + 3 * mt * t * t * p2.y + t * t * t * to.y;
    const wobble = i < samples ? rand(-0.8, 0.8) : 0;
    points.push({ x: x + wobble, y: y + wobble });
  }
  return points;
}

async function glideTo(page, to, samples = 28) {
  const from = page.__lastPointer || { x: rand(180, 520), y: rand(160, 420) };
  const points = bezierPath(from, to, samples);
  for (const point of points) {
    await page.mouse.move(point.x, point.y);
    await page.waitForTimeout(rand(8, 22));
  }
  page.__lastPointer = to;
}

// --- challenge detection ---------------------------------------------------
// Runs inside every frame. Deliberately over-broad: we would rather call a
// near-miss a candidate than miss a press widget inside a nested iframe.
function probeFrame() {
  const rn = (n) => Math.round(n * 10) / 10;
  const box = (el) => {
    if (!el) return null;
    const r = el.getBoundingClientRect();
    return { x: rn(r.x), y: rn(r.y), w: rn(r.width), h: rn(r.height) };
  };

  const pxCaptcha = document.querySelector("#px-captcha");
  const enforcement = document.querySelector("iframe#enforcementFrame, #enforcementFrame");
  const bodyText = (document.body && document.body.innerText) || "";

  const pressLabels = [];
  for (const el of document.querySelectorAll("[aria-label]")) {
    const aria = el.getAttribute("aria-label") || "";
    if (/press|hold|按下|按住|可访问|accessib/i.test(aria)) {
      const r = el.getBoundingClientRect();
      pressLabels.push({
        tag: el.tagName,
        aria: aria.slice(0, 120),
        box: box(el),
        visible: r.width > 0 && r.height > 0,
      });
    }
  }

  const clickables = [];
  for (const el of document.querySelectorAll('div[role="button"], button, [role="button"]')) {
    const r = el.getBoundingClientRect();
    if (r.width < 8 || r.height < 8) continue;
    clickables.push({
      tag: el.tagName,
      role: el.getAttribute("role") || "",
      aria: (el.getAttribute("aria-label") || "").slice(0, 80),
      text: ((el.innerText || "").trim() || "").slice(0, 60),
      box: box(el),
      hasSvg: Boolean(el.querySelector("svg")),
    });
    if (clickables.length >= 8) break;
  }

  return {
    url: location.href,
    origin: location.origin,
    hasPxCaptcha: Boolean(pxCaptcha),
    pxBox: box(pxCaptcha),
    pxClass: pxCaptcha ? String(pxCaptcha.className || "").slice(0, 80) : "",
    hasEnforcementFrame: Boolean(enforcement),
    pressLabels,
    clickables,
    textHit: /press\s*&?\s*hold|robot or human|按下|按住|可访问性|human challenge/i.test(
      bodyText
    ),
    blockPage: /access to this page has been denied|px-captcha-error|incapsula|perimeterx/i.test(
      bodyText
    ),
    textSample: bodyText.replace(/\s+/g, " ").slice(0, 400),
    title: document.title,
  };
}

async function scanFrames(page) {
  const frames = page.frames();
  const rows = [];
  for (const frame of frames) {
    let row;
    try {
      row = await frame.evaluate(probeFrame);
    } catch (error) {
      row = { url: frame.url(), error: String(error).slice(0, 120) };
    }
    rows.push({ frameUrl: frame.url(), ...row });
  }
  // A rendered press widget must have real geometry. The HUMAN iframe ships an
  // empty #px-captcha div on every load; treating that as "challenged" would
  // make every run look like a challenge.
  const sized = (b) => Boolean(b && b.w > 8 && b.h > 8);
  const visiblePress = rows.find(
    (r) =>
      (r.hasPxCaptcha && sized(r.pxBox)) ||
      (r.pressLabels || []).some((l) => l.visible && sized(l.box))
  );
  const idleFrame = rows.find((r) => r.hasPxCaptcha);
  const enforcementFrame = rows.find((r) => r.hasEnforcementFrame);
  const textFrame = rows.find((r) => r.textHit);
  let kind = "none";
  if (visiblePress) kind = "press-visible";
  else if (enforcementFrame) kind = "arkose-enforcement-frame";
  else if (textFrame) kind = "press-text";
  else if (rows.some((r) => r.blockPage)) kind = "px-block-page";
  else if (idleFrame) kind = "px-idle";
  return {
    kind,
    pressFrame: visiblePress ? visiblePress.frameUrl : "",
    pressBox: visiblePress
      ? visiblePress.pxBox || (visiblePress.pressLabels[0] || {}).box || null
      : null,
    idleFrame: idleFrame ? idleFrame.frameUrl : "",
    idleBox: idleFrame ? idleFrame.pxBox : null,
    enforcementFrame: enforcementFrame ? enforcementFrame.frameUrl : "",
    rows,
  };
}

// Locate a pressable target and return main-frame coordinates.
async function locatePressTarget(page) {
  for (const frame of page.frames()) {
    let box = null;
    const selectors = [
      "#px-captcha",
      '[aria-label*="Press" i]',
      '[aria-label*="press" i]',
      '[aria-label*="Hold" i]',
      '[aria-label*="按下"]',
      '[aria-label*="可访�?]',
      'div[role="button"]:has(svg)',
      'button:has(svg)',
      'div[role="button"]',
      "button",
    ];
    for (const selector of selectors) {
      try {
        const locator = frame.locator(selector).first();
        if ((await locator.count()) === 0) continue;
        if (!(await locator.isVisible().catch(() => false))) continue;
        box = await locator.boundingBox();
        if (box && box.width > 8 && box.height > 8) {
          return {
            selector,
            frameUrl: frame.url(),
            box,
            center: { x: box.x + box.width / 2, y: box.y + box.height / 2 },
          };
        }
      } catch {
        // next selector
      }
    }
  }
  return null;
}

const CHALLENGE_KINDS = new Set([
  "press-visible",
  "press-text",
  "arkose-enforcement-frame",
  "px-block-page",
]);

async function challengePresent(page) {
  const scan = await scanFrames(page);
  return CHALLENGE_KINDS.has(scan.kind);
}

// Diagnostic only. The HUMAN iframe renders nothing until the host page tells
// it a request was blocked; this posts that message with a synthetic session so
// the widget's own JS draws, letting us measure the press mechanics. A widget
// rendered this way can never be accepted server-side - it is a mechanics
// probe, not a solve.
async function forceRenderWidget(page) {
  return page.evaluate(() => {
    const iframe = [...document.querySelectorAll("iframe")].find((f) =>
      /hsprotect\.net\/index\.html/.test(f.src || "")
    );
    if (!iframe || !iframe.contentWindow) return { ok: false, reason: "no human iframe" };
    const uuid = (crypto.randomUUID && crypto.randomUUID()) || String(Date.now());
    iframe.contentWindow.postMessage(
      { type: "block", jsonResponse: { vid: uuid, uuid }, requestUrl: location.href },
      "*"
    );
    return { ok: true, uuid };
  });
}

(async () => {
  const started = Date.now();
  const { chromium } = loadPlaywright();
  if (!proxy && !direct) {
    console.log(JSON.stringify({ ok: false, stage: "config", error: "no --proxy (or --direct)" }));
    process.exit(1);
  }

  const launchOpts = {
    channel: "chrome",
    headless,
    ignoreDefaultArgs: ["--enable-automation"],
    args: [
      "--disable-blink-features=AutomationControlled",
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-features=Translate,OptimizationHints",
    ],
    viewport: { width: 1366, height: 860 },
    locale: "en-US",
    timezoneId: "America/New_York",
  };
  if (proxy) {
    launchOpts.proxy = { server: proxy };
    if (proxyUser) {
      launchOpts.proxy.username = proxyUser;
      launchOpts.proxy.password = proxyPass;
    }
  }

  const netLog = { px: [], arkose: [], doc: [] };
  let browser = null;
  const result = {
    ok: false,
    stage: "launch",
    url: target,
    via: proxy ? "proxy" : "direct",
    headless,
    proxy_ref: proxy ? proxy.replace(/\/\/[^@]*@/, "//***@") : "direct",
    started_at: new Date().toISOString(),
  };

  try {
    browser = await chromium.launch(launchOpts);
  } catch (error) {
    result.error = String(error).slice(0, 240);
    console.log(JSON.stringify(result));
    process.exit(1);
  }

  const context = await browser.newContext({
    viewport: { width: 1366, height: 860 },
    locale: "en-US",
    timezoneId: "America/New_York",
    userAgent:
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36",
  });
  const page = await context.newPage();

  page.on("request", (request) => {
    const url = request.url();
    if (/px-cloud\.net|px-cdn\.net|perimeterx|human-security|pxchk|_px/i.test(url)) {
      if (netLog.px.length < 40) netLog.px.push(url.slice(0, 180));
    } else if (/arkoselabs|funcaptcha|arkose/i.test(url)) {
      if (netLog.arkose.length < 40) netLog.arkose.push(url.slice(0, 180));
    }
  });
  page.on("response", (response) => {
    if (response.request().resourceType() === "document" && netLog.doc.length < 20) {
      netLog.doc.push({ status: response.status(), url: response.url().slice(0, 160) });
    }
  });

  const shot = async (name) => {
    if (!shotDir) return;
    try {
      fs.mkdirSync(shotDir, { recursive: true });
      await page.screenshot({ path: path.join(shotDir, `${name}.png`), fullPage: false });
      const text = await page.evaluate(() => (document.body && document.body.innerText) || "");
      fs.writeFileSync(path.join(shotDir, `${name}.txt`), text, "utf8");
    } catch {
      /* best effort */
    }
  };

  try {
    let navError = "";
    try {
      await page.goto(target, { waitUntil: "domcontentloaded", timeout: navTimeout });
    } catch (error) {
      navError = String(error).slice(0, 200);
    }
    result.stage = "loaded";
    result.nav_error = navError;
    result.final_url = page.url();

    // Benign in-flow advance: reveal the email step without typing anything.
    if (advance) {
      for (const label of [
        /get a new email address/i,
        /get a new email/i,
        /create (a )?(free )?account/i,
        /sign up/i,
      ]) {
        const link = page.getByText(label).first();
        if ((await link.count().catch(() => 0)) > 0 && (await link.isVisible().catch(() => false))) {
          await link.click({ timeout: 8000 }).catch(() => {});
          await page.waitForTimeout(2500);
          break;
        }
      }
    }
    await shot("01-entry");

    // Wait for a *visible* challenge. A loaded-but-idle HUMAN iframe (the usual
    // state on the email step) is recorded, not mistaken for a challenge.
    let scan = await scanFrames(page);
    const waitedFrom = Date.now();
    while (!CHALLENGE_KINDS.has(scan.kind) && Date.now() - waitedFrom < waitMs) {
      await page.waitForTimeout(1500);
      scan = await scanFrames(page);
    }
    result.idle_widget_seen = result.idle_widget_seen || scan.kind === "px-idle";
    result.idle_box = scan.idleBox || null;

    // Diagnostic path: draw the widget ourselves, then measure the press.
    if (forceRender && !CHALLENGE_KINDS.has(scan.kind)) {
      result.force_render = await forceRenderWidget(page).catch((e) => ({
        ok: false,
        reason: String(e).slice(0, 160),
      }));
      const renderFrom = Date.now();
      while (!CHALLENGE_KINDS.has(scan.kind) && Date.now() - renderFrom < 20000) {
        await page.waitForTimeout(1000);
        scan = await scanFrames(page);
      }
      result.force_render_kind = scan.kind;
      await shot("02b-forced");
    }

    result.challenge_kind = scan.kind;
    result.challenge_frame = scan.pressFrame || scan.enforcementFrame || "";
    result.challenge_box = scan.pressBox || null;
    result.frames = scan.rows.map((r) => ({
      frameUrl: String(r.frameUrl || "").slice(0, 160),
      hasPxCaptcha: Boolean(r.hasPxCaptcha),
      hasEnforcementFrame: Boolean(r.hasEnforcementFrame),
      pressLabels: (r.pressLabels || []).length,
      textHit: Boolean(r.textHit),
      title: r.title || "",
      textSample: (r.textSample || "").slice(0, 200),
    }));
    result.network = netLog;
    result.wait_ms = Date.now() - waitedFrom;
    await shot("02-challenge");

    if (!CHALLENGE_KINDS.has(scan.kind)) {
      result.ok = true;
      result.stage = scan.kind === "px-idle" ? "px-idle-no-challenge" : "no-challenge";
      result.pressed = false;
      result.page_state = (scan.rows[0] && scan.rows[0].textSample) || "";
      result.elapsed_ms = Date.now() - started;
      throw Object.assign(new Error("no-challenge"), { __soft: true });
    }

    if (noPress) {
      result.ok = true;
      result.stage = "challenge-not-pressed";
      result.pressed = false;
      result.elapsed_ms = Date.now() - started;
      throw Object.assign(new Error("no-press"), { __soft: true });
    }

    // Natural dwell before touching anything (opt-in via --dwell-ms).
    if (dwellMs > 0) {
      const dwellMoves = Math.max(2, Math.round(dwellMs / 1200));
      for (let i = 0; i < dwellMoves; i += 1) {
        await page.mouse.move(rand(200, 1100), rand(200, 700));
        await page.waitForTimeout(rand(900, 1600));
      }
      await page.waitForTimeout(dwellMs);
      await shot("03-dwell");
    }

    const pressTarget = await locatePressTarget(page);
    result.press_target = pressTarget
      ? {
          selector: pressTarget.selector,
          frameUrl: String(pressTarget.frameUrl).slice(0, 160),
          center: { x: round(pressTarget.center.x), y: round(pressTarget.center.y) },
          box: {
            x: round(pressTarget.box.x),
            y: round(pressTarget.box.y),
            w: round(pressTarget.box.width),
            h: round(pressTarget.box.height),
          },
        }
      : null;

    if (!pressTarget) {
      result.ok = true;
      result.stage = "challenge-found-no-target";
      result.pressed = false;
      result.elapsed_ms = Date.now() - started;
      throw Object.assign(new Error("no-target"), { __soft: true });
    }

    // Approach along a curve, settle, then press.
    const start = { x: rand(150, 700), y: rand(140, 500) };
    await page.mouse.move(start.x, start.y);
    page.__lastPointer = start;
    await glideTo(page, pressTarget.center, 30);
    await page.waitForTimeout(rand(120, 320));

    const requestedHold = holdMsArg > 0 ? holdMsArg : Math.round(rand(2000, 4000));
    await page.mouse.down();
    const holdStart = Date.now();
    let clearedDuringHold = false;
    while (Date.now() - holdStart < requestedHold) {
      await page.waitForTimeout(rand(100, 190));
      const jx = pressTarget.center.x + rand(-1.6, 1.6);
      const jy = pressTarget.center.y + rand(-1.6, 1.6);
      await page.mouse.move(jx, jy);
      if (Date.now() - holdStart > 900 && !(await challengePresent(page))) {
        clearedDuringHold = true;
        break;
      }
    }
    const holdMs = Date.now() - holdStart;
    await page.mouse.up();
    result.pressed = true;
    result.press_executed = true;
    result.hold_ms = holdMs;
    result.cleared_during_hold = clearedDuringHold;
    await shot("04-after-press");

    // Did it clear?
    const clearedAt = Date.now();
    let stillThere = await challengePresent(page);
    while (stillThere && Date.now() - clearedAt < afterWaitMs) {
      await page.waitForTimeout(700);
      stillThere = await challengePresent(page);
    }
    const clearedAfter = !stillThere;
    result.cleared_after_press = clearedAfter;
    result.cleared_ms = Date.now() - clearedAt;

    await page.waitForTimeout(1500);
    const post = await scanFrames(page);
    result.post_kind = post.kind;
    result.post_url = page.url();
    result.post_text = (post.rows[0] && post.rows[0].textSample) || "";
    result.network = netLog;
    await shot("05-after");

    // "Passed" = the press widget is gone and we were not dropped onto a block
    // page. We never submit a form, so this is the furthest honest signal.
    result.passed = clearedAfter && post.kind !== "px-block-page";
    result.stage = result.passed ? "passed" : clearedAfter ? "blocked-page" : "still-challenged";
    result.ok = true;
    result.elapsed_ms = Date.now() - started;
  } catch (error) {
    if (!error.__soft) {
      result.ok = false;
      result.error = String(error).slice(0, 240);
      result.elapsed_ms = Date.now() - started;
    }
  } finally {
    await context.close().catch(() => {});
    await browser.close().catch(() => {});
  }

  const line = JSON.stringify(result);
  if (outPath) {
    try {
      fs.mkdirSync(path.dirname(path.resolve(outPath)), { recursive: true });
      fs.writeFileSync(path.resolve(outPath), `${line}\n`, "utf8");
    } catch {
      /* best effort */
    }
  }
  console.log(line);
  process.exit(result.stage === "no-challenge" || result.stage === "px-idle-no-challenge" ? 3 : result.ok === false ? 1 : 0);
})();
