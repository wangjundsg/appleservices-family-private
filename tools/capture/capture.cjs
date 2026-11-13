// Apple Account creation capture harness.
//
// Drives a real installed Chrome through playwright-core (no bundled browser
// download) and records every XHR/fetch the creation flow performs: URL,
// method, request headers, post body, status and the first slice of the
// response. Screenshots land next to the JSONL so a human can see which step
// produced which call.
//
// Usage:
//   node tools/capture/capture.cjs --outdir .cache/capture            (headless)
//   node tools/capture/capture.cjs --headless=false                    (visible Chrome)
//   node tools/capture/capture.cjs --email you@yourdomain.tld          (fill email step)

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

const outdir = path.resolve(arg("outdir", ".cache/capture"));
const headless = String(arg("headless", "true")) !== "false";
const startURL = arg("url", "https://appleid.apple.com/account");
const email = arg("email", "");
const proxy = arg("proxy", "");

fs.mkdirSync(outdir, { recursive: true });

const events = [];
let step = 0;

function record(kind, data) {
  events.push({ t: Date.now(), kind, ...data });
}

async function shot(page, name) {
  step += 1;
  const file = path.join(outdir, `${String(step).padStart(2, "0")}-${name}.png`);
  await page.screenshot({ path: file, fullPage: false }).catch(() => {});
  const text = await page.evaluate(() => document.body?.innerText?.slice(0, 3000) || "").catch(() => "");
  fs.writeFileSync(file.replace(/\.png$/, ".txt"), text, "utf8");
  return file;
}

(async () => {
  const launch = { channel: "chrome", headless, args: ["--disable-blink-features=AutomationControlled"] };
  if (proxy) { launch.proxy = { server: proxy }; if (arg("proxy-user", "")) { launch.proxy.username = String(arg("proxy-user", "")); launch.proxy.password = String(arg("proxy-pass", "")); } }

  const browser = await chromium.launch(launch);
  const context = await browser.newContext({
    locale: "en-US",
    timezoneId: "America/New_York",
    viewport: { width: 1280, height: 900 },
    userAgent:
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
  });

  // Hide the most obvious automation flag; the page also ships its own checks.
  await context.addInitScript(() => {
    Object.defineProperty(navigator, "webdriver", { get: () => undefined });
  });

  const page = await context.newPage();

  page.on("request", (req) => {
    const type = req.resourceType();
    if (!["xhr", "fetch", "document"].includes(type)) return;
    record("request", {
      method: req.method(),
      url: req.url(),
      headers: req.headers(),
      postData: (req.postData() || "").slice(0, 4000),
    });
  });

  page.on("response", async (res) => {
    const type = res.request().resourceType();
    if (!["xhr", "fetch"].includes(type)) return;
    let body = "";
    try {
      body = (await res.text()).slice(0, 4000);
    } catch {
      body = "<unreadable>";
    }
    record("response", { url: res.url(), status: res.status(), body });
  });

  page.on("framenavigated", (frame) => {
    if (frame === page.mainFrame()) record("navigate", { url: frame.url() });
  });

  await page.goto(startURL, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForTimeout(4000);
  await shot(page, "landing");

  // Find the entry into the creation flow.
  const candidates = await page.evaluate(() => {
    const out = [];
    for (const el of document.querySelectorAll("a,button,[role=link],[role=button]")) {
      const text = (el.innerText || el.textContent || "").trim();
      const href = el.getAttribute("href") || "";
      if (/create/i.test(text) || /create/i.test(href)) {
        out.push({ tag: el.tagName, text: text.slice(0, 80), href: href.slice(0, 200) });
      }
    }
    return out;
  });
  record("candidates", { list: candidates });

  const target = page.getByText(/Create Your Apple\s*Account/i).first();
  if (await target.count().catch(() => 0)) {
    await target.click({ timeout: 15000 }).catch((e) => record("click-error", { error: String(e) }));
    await page.waitForTimeout(5000);
    await shot(page, "create-step1");
  }

  // The creation form lives inside the AuthKit widget iframe
  // (appleid.apple.com/widget/account/?...&widgetKey=...), not in the outer
  // document. Operate on that frame.
  await page.waitForTimeout(3000);
  const widget = page.frames().find((f) => f.url().includes("/widget/account/"));
  if (!widget) {
    record("no-widget-frame", { frames: page.frames().map((f) => f.url()) });
  } else {
    record("widget-frame", { url: widget.url() });
  }

  // Dump every control on the form so the fill step can be written against
  // the real DOM instead of guesses.
  const form = await (widget || page.mainFrame()).evaluate(() => {
    const describe = (el) => {
      const attrs = {};
      for (const a of el.attributes) attrs[a.name] = a.value;
      return {
        tag: el.tagName,
        type: attrs.type || "",
        id: attrs.id || "",
        name: attrs.name || "",
        placeholder: attrs.placeholder || "",
        ariaLabel: attrs["aria-label"] || "",
        autocomplete: attrs.autocomplete || "",
        options: el.tagName === "SELECT" ? [...el.options].map((o) => `${o.value}:${o.text}`).slice(0, 8) : undefined,
      };
    };
    return {
      inputs: [...document.querySelectorAll("input,select,textarea")].map(describe),
      buttons: [...document.querySelectorAll("button,[role=button]")].map((b) => ({
        text: (b.innerText || "").trim().slice(0, 40),
        type: b.getAttribute("type") || "",
        disabled: b.disabled,
      })),
      iframes: [...document.querySelectorAll("iframe")].map((f) => f.src),
      captcha: {
        recaptcha: Boolean(window.grecaptcha),
        hcaptcha: Boolean(window.hcaptcha),
        arkose: Boolean(window.Arkose),
      },
    };
  });
  fs.writeFileSync(path.join(outdir, "form.json"), JSON.stringify(form, null, 2), "utf8");
  record("form", { count: form.inputs.length });

  // Captcha image, if the widget rendered one. Screenshot it so a human (or a
  // vision pass) can read the code before the form is submitted.
  if (widget) {
    const captchaImg = widget.locator("img");
    const count = await captchaImg.count().catch(() => 0);
    const imgs = [];
    for (let i = 0; i < count; i += 1) {
      const el = captchaImg.nth(i);
      const src = (await el.getAttribute("src").catch(() => "")) || "";
      const box = await el.boundingBox().catch(() => null);
      imgs.push({ i, src: src.slice(0, 120), box });
    }
    record("images", { imgs });
    for (const img of imgs) {
      if (img.box) {
        const el = captchaImg.nth(img.i);
        await el.screenshot({ path: path.join(outdir, `img-${img.i}.png`) }).catch(() => {});
      }
    }
  }

  if (email && widget) {
    const f = widget;
    const fill = async (selector, value, label) => {
      if (value === "" || value === undefined) return;
      await f.fill(selector, String(value)).catch((e) => record("fill-error", { label, error: String(e) }));
    };
    await fill('input[name="firstName"]', arg("first", "James"));
    await fill('input[name="lastName"]', arg("last", "Carter"));
    await f.selectOption('select[name="countrySelect"]', arg("country3", "USA")).catch((e) =>
      record("select-error", { label: "country", error: String(e) }),
    );
    const selects = f.locator("select");
    await selects.nth(1).selectOption(arg("month", "07")).catch((e) => record("select-error", { label: "month", error: String(e) }));
    await selects.nth(2).selectOption(arg("day", "18")).catch((e) => record("select-error", { label: "day", error: String(e) }));
    await selects.nth(3).selectOption(arg("year", "1992")).catch((e) => record("select-error", { label: "year", error: String(e) }));
    await fill('input[name="appleId"]', email);
    await fill('input[name="password"]', arg("password", "Kq7#mZp2vLt9"));
    await fill('input[name="confirmPassword"]', arg("password", "Kq7#mZp2vLt9"));
    await f.selectOption('select[name="phoneCode"]', arg("phonecc", "US")).catch((e) =>
      record("select-error", { label: "phonecc", error: String(e) }),
    );
    await fill('input[name="phoneNumber"]', arg("phone", ""));
    await fill('input[name="captcha"]', arg("captcha", ""));
    await page.waitForTimeout(800);
    await shot(page, "form-filled");

    // Cooperative captcha loop: the harness parks here until an answer file
    // appears, so a human (or a vision pass) can read 03-captcha.png and drop
    // the characters in without restarting the flow and changing the nonce.
    const awaitPath = arg("await-captcha", "");
    if (awaitPath) {
      const answerFile = path.resolve(String(awaitPath));
      fs.rmSync(answerFile, { force: true });
      const imgs = widget.locator("img");
      if (await imgs.count().catch(() => 0)) {
        await imgs.first().screenshot({ path: path.join(outdir, "03-captcha.png") }).catch(() => {});
      }
      const deadline = Date.now() + 240000;
      let answer = "";
      while (Date.now() < deadline) {
        if (fs.existsSync(answerFile)) {
          answer = fs.readFileSync(answerFile, "utf8").trim();
          if (answer) break;
        }
        await page.waitForTimeout(2000);
      }
      record("captcha-answer", { answer, waited: 240000 - (deadline - Date.now()) });
      if (answer) {
        await fill('input[name="captcha"]', answer);
        await f.locator('input[name="captcha"]').press("Tab").catch(() => {});
        await page.waitForTimeout(2000);
      }
    }

    // Report what the widget thinks of the form right before submitting: a
    // disabled Continue plus its inline errors is the difference between a
    // captcha rejection and a required field we never filled.
    const state = await f
      .evaluate(() => {
        const btn = document.querySelector('button[type="submit"]');
        const errs = [...document.querySelectorAll('[class*="error" i],[role="alert"]')]
          .map((e) => (e.innerText || "").trim())
          .filter(Boolean)
          .slice(0, 6);
        const val = (sel) => document.querySelector(sel)?.value ?? null;
        return {
          disabled: btn ? btn.disabled : null,
          errs,
          phone: val('input[name="phoneNumber"]'),
          captcha: val('input[name="captcha"]'),
          email: val('input[name="appleId"]'),
        };
      })
      .catch((e) => ({ error: String(e) }));
    record("form-state", state);

    if (arg("submit", "") === "1") {
      const submit = f.locator('button[type="submit"]').first();
      await submit.click({ timeout: 15000 }).catch((e) => record("submit-error", { error: String(e) }));
      await page.waitForTimeout(10000);
      await shot(page, "after-submit");
      // Re-dump the widget so the next step (SMS code / email code) can be
      // driven without guessing its field names.
      const next = await (page.frames().find((fr) => fr.url().includes("/widget/account/")) || f)
        .evaluate(() => ({
          inputs: [...document.querySelectorAll("input,select")].map((el) => ({
            name: el.name || "",
            type: el.type || "",
            id: el.id || "",
          })),
          buttons: [...document.querySelectorAll("button")].map((b) => (b.innerText || "").trim()).filter(Boolean),
          text: (document.body?.innerText || "").slice(0, 800),
        }))
        .catch((e) => ({ error: String(e) }));
      fs.writeFileSync(path.join(outdir, "after.json"), JSON.stringify(next, null, 2), "utf8");
      record("after-state", next);
    }
  }

  fs.writeFileSync(path.join(outdir, "network.jsonl"), events.map((e) => JSON.stringify(e)).join("\n"), "utf8");
  console.log(`events=${events.length} outdir=${outdir}`);
  await browser.close();
})().catch((e) => {
  fs.writeFileSync(path.join(outdir, "network.jsonl"), events.map((x) => JSON.stringify(x)).join("\n"), "utf8");
  console.error("capture failed:", e);
  process.exit(1);
});
