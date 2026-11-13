// Preflight: can this egress reach the Apple captcha resource at all?
//
// docs/run-20261113.md §4.5: every /captcha request from the residential exit
// answered 503, the challenge page rendered a broken image, and the run only
// found out after 16 paid solves. One cheap request before the form turns that
// into a five-second refusal.
//
// Usage:
//   node tools/capture/preflight.cjs --proxy <url> [--proxy-user <u> --proxy-pass <p>] [--timeout 20]
//   node tools/capture/preflight.cjs                       # direct, for a control run
//
// --proxy takes an http:// or https:// forward proxy (CONNECT for the https
// target). A socks5h proxy needs a SOCKS client the repository does not ship for
// Node, so it is refused by name instead of failing obscurely.
// Output: one JSON object on stdout
//   {"ok":true,"status":200,"elapsed_ms":412,"egress_hint":"gw.dataimpulse.com:10005:abc***"}
// Exit codes: 0 = the resource answered 2xx, 2 = anything else (including usage
// errors and transport failures), which is what a caller script branches on.
//
// Target selection: the resource that answered 503 in the live run was recorded
// by path only (`/captcha`, 31/31 503s), which is appleid.apple.com's captcha
// endpoint as documented in docs/signup-protocol.md §3.1:
//
//   POST https://appleid.apple.com/captcha  {"type":"IMAGE"}  -> 201 + base64
//
// so that is the default. The idmsa image resource the run document also names
// is reachable with `--url https://idmsa.apple.com/appleauth/static/img/captcha/image --method GET`.
//
// No npm dependency: the repository ships playwright-core only, so the proxy
// transport is node's own net/tls plus a CONNECT for https targets. Only the
// response head is read -- a preflight needs the status and the server header,
// not the captcha itself.

"use strict";

const net = require("node:net");
const tls = require("node:tls");
const { URL } = require("node:url");

// Apple's edge answers 403 to a non-browser client (run-20261113 §4.2), so the
// probe has to look like the browser the creation run drives.
const DEFAULT_USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

const DEFAULT_TARGET = "https://appleid.apple.com/captcha";
const DEFAULT_METHOD = "POST";
const DEFAULT_BODY = '{"type":"IMAGE"}';
const DEFAULT_TIMEOUT_MS = 20_000;

const MAX_HEAD_BYTES = 64 * 1024;

// browserHeaders mirrors the headers the driven Chrome sends for the widget, so
// a 200 here means "this exit can carry the real request", not "curl works".
function browserHeaders({ host, body, method }) {
  const headers = {
    "User-Agent": DEFAULT_USER_AGENT,
    Accept: "*/*",
    "Accept-Language": "en-US,en;q=0.9",
    "Accept-Encoding": "identity",
    "Sec-CH-UA": '"Chromium";v="154", "Google Chrome";v="154", "Not?A_Brand";v="99"',
    "Sec-CH-UA-Mobile": "?0",
    "Sec-CH-UA-Platform": '"Windows"',
    Origin: `https://${host.replace(/^www\./, "")}`,
    Referer: `https://${host.replace(/^www\./, "")}/`,
  };
  if (String(method).toUpperCase() !== "GET" && String(method).toUpperCase() !== "HEAD") {
    headers["Content-Type"] = "application/json";
    headers["Content-Length"] = String(Buffer.byteLength(body || ""));
  }
  return headers;
}

// parseHead reads one response head. latin1 keeps header bytes byte-exact,
// which matters for a UA comparison test and costs nothing for ASCII.
function parseHead(text) {
  const lines = String(text).split("\r\n");
  const match = /^HTTP\/\d(?:\.\d)?\s+(\d{3})/.exec(lines[0] || "");
  if (!match) return null;
  const headers = {};
  for (const line of lines.slice(1)) {
    const colon = line.indexOf(":");
    if (colon <= 0) continue;
    const name = line.slice(0, colon).trim().toLowerCase();
    if (name) headers[name] = line.slice(colon + 1).trim();
  }
  return { status: Number(match[1]), headers };
}

// egressHint names the exit the probe went through, without credentials: the
// proxy's host:port plus a three-character username prefix. "direct" means no
// proxy was configured, which is itself the hint an operator needs.
function egressHint(proxyURL, proxyUser) {
  if (!proxyURL) return "direct";
  const user = String(proxyUser || decodeURIComponent(proxyURL.username || ""));
  const prefix = user ? `:${user.slice(0, 3)}***` : "";
  const port = proxyURL.port || (proxyURL.protocol === "https:" ? "443" : "80");
  return `${proxyURL.hostname}:${port}${prefix}`;
}

// proxyCredentials prefers the explicit flags (the CLI shape the pipeline uses)
// and falls back to credentials embedded in the proxy URL.
function proxyCredentials(proxyURL, proxyUser, proxyPass) {
  if (!proxyURL) return "";
  const user = String(proxyUser || decodeURIComponent(proxyURL.username || ""));
  if (!user) return "";
  const pass = String(proxyPass || decodeURIComponent(proxyURL.password || ""));
  return `Basic ${Buffer.from(`${user}:${pass}`, "utf8").toString("base64")}`;
}

// readHead resolves with the first non-1xx response head on the socket.
function readHead(socket, resolve, reject) {
  let buffer = Buffer.alloc(0);
  const onData = (chunk) => {
    buffer = Buffer.concat([buffer, chunk]);
    if (buffer.length > MAX_HEAD_BYTES) {
      socket.removeListener("data", onData);
      reject(new Error("response head too large"));
      return;
    }
    for (;;) {
      const end = buffer.indexOf("\r\n\r\n");
      if (end < 0) return;
      const head = parseHead(buffer.subarray(0, end).toString("latin1"));
      buffer = buffer.subarray(end + 4);
      if (!head) {
        socket.removeListener("data", onData);
        reject(new Error("malformed response head"));
        return;
      }
      if (head.status < 200) continue; // 100 Continue and friends
      socket.removeListener("data", onData);
      resolve(head);
      return;
    }
  };
  socket.on("data", onData);
}

// requestHead performs one request and returns {status, headers}. It reads only
// the head and then closes: the body of a 503 is not worth metering.
function requestHead({ target, method, headers, body, proxyURL, proxyAuth, timeoutMs }) {
  return new Promise((resolve, reject) => {
    const secure = target.protocol === "https:";
    const host = target.hostname;
    const port = Number(target.port || (secure ? 443 : 80));
    const payload = body == null ? "" : String(body);
    // Origin-form for a direct request or a CONNECT tunnel, absolute-form when
    // the request itself is handed to an http proxy.
    const path = secure || !proxyURL ? `${target.pathname || "/"}${target.search || ""}` : target.href;
    const lines = [
      `${method} ${path} HTTP/1.1`,
      `Host: ${target.host}`,
      ...Object.entries(headers).map(([name, value]) => `${name}: ${value}`),
    ];
    if (proxyURL && !secure && proxyAuth) lines.push(`Proxy-Authorization: ${proxyAuth}`);
    lines.push("Connection: close", "", "");
    const requestBytes = Buffer.from(`${lines.join("\r\n")}${payload}`, "utf8");

    let settled = false;
    const open = [];
    const timer = setTimeout(() => fail(new Error("timeout")), timeoutMs);
    function finish(err, head) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      for (const socket of open) socket.destroy();
      if (err) reject(err);
      else resolve(head);
    }
    const fail = (err) => finish(err);
    function guard(socket) {
      open.push(socket);
      socket.setNoDelay(true);
      socket.on("error", fail);
      return socket;
    }
    function send(socket) {
      readHead(socket, (head) => finish(null, head), fail);
      socket.write(requestBytes);
    }

    if (proxyURL && secure) {
      const tunnel = guard(net.connect({ host: proxyURL.hostname, port: Number(proxyURL.port || 80) }));
      tunnel.on("connect", () => {
        const lines = [`CONNECT ${host}:${port} HTTP/1.1`, `Host: ${host}:${port}`];
        if (proxyAuth) lines.push(`Proxy-Authorization: ${proxyAuth}`);
        tunnel.write(`${lines.join("\r\n")}\r\n\r\n`);
      });
      readHead(tunnel, (head) => {
        if (head.status < 200 || head.status >= 300) {
          fail(new Error(`proxy CONNECT answered ${head.status}`));
          return;
        }
        const secureSocket = tls.connect({ socket: tunnel, servername: host });
        guard(secureSocket);
        secureSocket.on("secureConnect", () => send(secureSocket));
      }, fail);
      return;
    }

    if (proxyURL) {
      // Plain http through a proxy: absolute-form request line, no tunnel, and
      // Proxy-Authorization rides on the request itself.
      const socket = guard(net.connect({ host: proxyURL.hostname, port: Number(proxyURL.port || 80) }));
      socket.on("connect", () => send(socket));
      return;
    }

    const socket = secure
      ? tls.connect({ host, port, servername: host, ALPNProtocols: ["http/1.1"] })
      : net.connect({ host, port });
    guard(socket);
    if (secure) socket.on("secureConnect", () => send(socket));
    else socket.on("connect", () => send(socket));
  });
}

// probe runs one preflight. It resolves (never rejects) with
// {ok, status, elapsed_ms, egress_hint} plus `error` when the request could not
// be made at all -- a caller can branch on ok alone.
async function probe(options = {}) {
  const started = Date.now();
  const timeoutMs = Number(options.timeoutMs) > 0 ? Number(options.timeoutMs) : DEFAULT_TIMEOUT_MS;
  let target;
  try {
    target = new URL(String(options.url || DEFAULT_TARGET));
  } catch {
    return { ok: false, status: 0, elapsed_ms: Date.now() - started, egress_hint: "", error: "bad url" };
  }
  if (target.protocol !== "http:" && target.protocol !== "https:") {
    return { ok: false, status: 0, elapsed_ms: Date.now() - started, egress_hint: "", error: "url is not http(s)" };
  }

  let proxyURL = null;
  if (options.proxy) {
    try {
      proxyURL = new URL(String(options.proxy));
    } catch {
      return { ok: false, status: 0, elapsed_ms: Date.now() - started, egress_hint: "", error: "bad proxy url" };
    }
    if (proxyURL.protocol !== "http:" && proxyURL.protocol !== "https:") {
      return {
        ok: false,
        status: 0,
        elapsed_ms: Date.now() - started,
        egress_hint: "",
        error: `proxy scheme ${proxyURL.protocol} is not http(s)`,
      };
    }
  }

  const method = String(options.method || DEFAULT_METHOD).toUpperCase();
  const body = options.body !== undefined ? String(options.body) : method === "GET" ? "" : DEFAULT_BODY;
  const egress = egressHint(proxyURL, options.proxyUser);
  try {
    const head = await requestHead({
      target,
      method,
      body,
      headers: browserHeaders({ host: target.host, body, method }),
      proxyURL,
      proxyAuth: proxyCredentials(proxyURL, options.proxyUser, options.proxyPass),
      timeoutMs,
    });
    const ok = head.status >= 200 && head.status < 300;
    return { ok, status: head.status, elapsed_ms: Date.now() - started, egress_hint: egress };
  } catch (e) {
    return {
      ok: false,
      status: 0,
      elapsed_ms: Date.now() - started,
      egress_hint: egress,
      error: String(e && e.message ? e.message : e),
    };
  }
}

// ---- CLI ------------------------------------------------------------------

function parseArgs(argv) {
  const options = {
    url: DEFAULT_TARGET,
    method: DEFAULT_METHOD,
    proxy: "",
    proxyUser: "",
    proxyPass: "",
    timeoutMs: DEFAULT_TIMEOUT_MS,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i];
    const value = () => {
      const next = argv[i + 1];
      if (next === undefined || String(next).startsWith("--")) throw new Error(`missing value for ${flag}`);
      i += 1;
      return next;
    };
    switch (flag) {
      case "--proxy":
        options.proxy = value();
        break;
      case "--proxy-user":
        options.proxyUser = value();
        break;
      case "--proxy-pass":
        options.proxyPass = value();
        break;
      case "--timeout": {
        const seconds = Number(value());
        if (!Number.isFinite(seconds) || seconds <= 0) throw new Error("--timeout needs a positive number of seconds");
        options.timeoutMs = seconds * 1000;
        break;
      }
      case "--url":
        options.url = value();
        break;
      case "--method":
        options.method = value().toUpperCase();
        break;
      case "--body":
        options.body = value();
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
    console.log(JSON.stringify({ ok: false, status: 0, elapsed_ms: 0, egress_hint: "", error: String(e.message || e) }));
    return 2;
  }
  if (options.help) {
    console.log(
      "usage: node tools/capture/preflight.cjs [--proxy <url>] [--proxy-user <u>] [--proxy-pass <p>] " +
        "[--timeout <seconds>] [--url <captcha url>] [--method POST|GET]",
    );
    return 0;
  }
  const result = await probe(options);
  console.log(JSON.stringify(result));
  return result.ok ? 0 : 2;
}

if (require.main === module) {
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (e) => {
      console.log(JSON.stringify({ ok: false, status: 0, elapsed_ms: 0, egress_hint: "", error: String(e && e.message ? e.message : e) }));
      process.exit(2);
    },
  );
}

module.exports = {
  DEFAULT_TARGET,
  DEFAULT_USER_AGENT,
  browserHeaders,
  egressHint,
  main,
  parseHead,
  parseArgs,
  probe,
  requestHead,
};
