// Tests for the pre-fight probe (preflight.cjs).
//
// Run with:  node --test tools/capture/*.test.cjs
//
// docs/run-20261113.md §4.5: 31/31 /captcha requests answered 503 from the chosen
// residential exit, and the run only learned that after 16 paid solves. These
// tests cover the probe's decision logic and its transport against loopback
// fixtures -- no external network, no Apple host, no npm dependency.

"use strict";

const assert = require("node:assert/strict");
const http = require("node:http");
const net = require("node:net");
const { test } = require("node:test");

const preflight = require("./preflight.cjs");

// startProxy brings up a fake HTTP forward proxy on loopback and records what it
// was asked to fetch. The request line arrives in absolute form, exactly as a
// real proxy sees it.
async function startProxy(respond) {
  const seen = [];
  const server = http.createServer((req, res) => {
    seen.push({ method: req.method, url: req.url, headers: req.headers });
    respond(req, res);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    seen,
    close: () => {
      server.closeAllConnections();
      server.close();
    },
  };
}

// startSilentProxy accepts the TCP connection and never answers, which is what a
// wedged proxy looks like to the pipeline.
async function startSilentProxy() {
  const sockets = [];
  const server = net.createServer((socket) => {
    sockets.push(socket);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    close: () => {
      for (const socket of sockets) socket.destroy();
      server.close();
    },
  };
}

test("§4.5 a 200 through the proxy is ok and names the exit", async (t) => {
  const proxy = await startProxy((_req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end('{"status":200}');
  });
  t.after(proxy.close);

  const result = await preflight.probe({
    url: "http://captcha.test/captcha",
    proxy: proxy.url,
    proxyUser: "abc123xyz",
    proxyPass: "s3cret",
    timeoutMs: 3000,
  });

  assert.equal(result.ok, true);
  assert.equal(result.status, 200);
  assert.match(result.egress_hint, /^127\.0\.0\.1:\d+:abc\*\*\*$/);
  assert.ok(result.elapsed_ms >= 0);

  assert.equal(proxy.seen.length, 1);
  const [request] = proxy.seen;
  assert.equal(request.method, "POST");
  assert.equal(request.url, "http://captcha.test/captcha"); // absolute form
  assert.equal(request.headers["user-agent"], preflight.DEFAULT_USER_AGENT);
  assert.match(request.headers["user-agent"], /Mozilla\/5\.0.*Chrome\//);
  assert.equal(request.headers["content-type"], "application/json");
  assert.equal(
    request.headers["proxy-authorization"],
    `Basic ${Buffer.from("abc123xyz:s3cret").toString("base64")}`,
  );
});

test("§4.5 a 503 is not ok, and the status survives for the caller", async (t) => {
  const proxy = await startProxy((_req, res) => {
    res.writeHead(503, { Server: "Apple" });
    res.end("");
  });
  t.after(proxy.close);

  const result = await preflight.probe({ url: "http://captcha.test/captcha", proxy: proxy.url, timeoutMs: 3000 });
  assert.equal(result.ok, false);
  assert.equal(result.status, 503);
  assert.match(result.egress_hint, /^127\.0\.0\.1:\d+$/);
});

test("§4.5 a direct probe reaches a loopback target with the browser UA", async (t) => {
  let seenUA = "";
  const server = http.createServer((req, res) => {
    seenUA = req.headers["user-agent"];
    res.writeHead(201, { "Content-Type": "application/json" });
    res.end("{}");
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => {
    server.closeAllConnections();
    server.close();
  });

  const result = await preflight.probe({
    url: `http://127.0.0.1:${server.address().port}/captcha`,
    timeoutMs: 3000,
  });
  assert.equal(result.ok, true);
  assert.equal(result.status, 201);
  assert.equal(result.egress_hint, "direct");
  assert.equal(seenUA, preflight.DEFAULT_USER_AGENT);
});

test("§4.5 a proxy that refuses CONNECT is a transport failure, not a target status", async (t) => {
  const sockets = [];
  const server = net.createServer((socket) => {
    sockets.push(socket);
    socket.on("data", () => socket.write("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => {
    for (const socket of sockets) socket.destroy();
    server.close();
  });

  const result = await preflight.probe({
    url: "https://idmsa.apple.com/appleauth/static/img/captcha/image",
    proxy: `http://127.0.0.1:${server.address().port}`,
    method: "GET",
    timeoutMs: 3000,
  });
  assert.equal(result.ok, false);
  assert.equal(result.status, 0);
  assert.match(result.error, /CONNECT answered 502/);
});

test("§4.5 a proxy that never answers times out instead of hanging the run", async (t) => {
  const proxy = await startSilentProxy();
  t.after(proxy.close);

  const started = Date.now();
  const result = await preflight.probe({
    url: "http://captcha.test/captcha",
    proxy: proxy.url,
    timeoutMs: 250,
  });
  const elapsed = Date.now() - started;
  assert.equal(result.ok, false);
  assert.equal(result.status, 0);
  assert.equal(result.error, "timeout");
  assert.ok(elapsed < 2000, `probe took ${elapsed}ms, the timeout did not apply`);
});

test("§4.5 the response head parser keeps status and headers", () => {
  const head = preflight.parseHead(
    "HTTP/1.1 503 Service Temporarily Unavailable\r\nServer: Apple\r\nX-Cache: TCP_MISS\r\n\r\n",
  );
  assert.equal(head.status, 503);
  assert.equal(head.headers.server, "Apple");
  assert.equal(head.headers["x-cache"], "TCP_MISS");
  assert.equal(preflight.parseHead("not http"), null);
});

test("§4.5 the egress hint never carries a password", () => {
  assert.equal(preflight.egressHint(null), "direct");
  const proxy = new URL("http://acct:pw@gw.dataimpulse.com:10005");
  assert.equal(preflight.egressHint(proxy), "gw.dataimpulse.com:10005:acc***");
  assert.equal(preflight.egressHint(proxy, "zz"), "gw.dataimpulse.com:10005:zz***");
  assert.equal(preflight.egressHint(proxy).includes("pw"), false);
});

test("§4.5 the probe headers look like Chrome, and GET carries no body", () => {
  const post = preflight.browserHeaders({ host: "appleid.apple.com", body: '{"type":"IMAGE"}', method: "POST" });
  assert.match(post["User-Agent"], /Chrome\//);
  assert.equal(post["Content-Type"], "application/json");
  assert.equal(post["Content-Length"], String(Buffer.byteLength('{"type":"IMAGE"}')));
  assert.equal(post.Origin, "https://appleid.apple.com");
  const get = preflight.browserHeaders({ host: "appleid.apple.com", body: "", method: "GET" });
  assert.equal(get["Content-Length"], undefined);
  assert.equal(get["Content-Type"], undefined);
});

test("§4.5 --timeout is seconds and usage errors are refused", () => {
  const options = preflight.parseArgs(["--proxy", "http://127.0.0.1:1", "--timeout", "20"]);
  assert.equal(options.timeoutMs, 20000);
  assert.equal(options.proxy, "http://127.0.0.1:1");
  assert.throws(() => preflight.parseArgs(["--timeout", "0"]), /positive/);
  assert.throws(() => preflight.parseArgs(["--proxy"]), /missing value/);
  assert.throws(() => preflight.parseArgs(["--nonsense"]), /unknown argument/);
});

test("§4.5 the CLI exits 0 on a 2xx and 2 on anything else", async (t) => {
  let status = 200;
  const proxy = await startProxy((_req, res) => {
    res.writeHead(status, { Server: "Apple" });
    res.end("");
  });
  t.after(proxy.close);

  const lines = [];
  const original = console.log;
  console.log = (...args) => lines.push(args.join(" "));
  try {
    const args = ["--proxy", proxy.url, "--url", "http://captcha.test/captcha", "--timeout", "5"];
    assert.equal(await preflight.main(args), 0);
    status = 503;
    assert.equal(await preflight.main(args), 2);
  } finally {
    console.log = original;
  }
  const results = lines.map((line) => JSON.parse(line));
  assert.deepEqual(
    results.map((result) => [result.ok, result.status]),
    [
      [true, 200],
      [false, 503],
    ],
  );
});
