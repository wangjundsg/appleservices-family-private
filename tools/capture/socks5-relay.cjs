// Local SOCKS5 relay for the native (patchright + real Chrome) arm.
//
// WHY THIS EXISTS: Chromium -- and therefore playwright/patchright -- refuses a
// socks5:// proxy that carries credentials ("Browser does not support socks5
// proxy authentication"). The Roxy build sidestepped this by baking the proxy
// into the Forge profile. This build keeps the browser stack untouched and puts
// the credential hop in a local relay instead:
//
//   Chrome --(socks5, NO auth, 127.0.0.1)--> this relay --(socks5, user/pass)--> upstream
//
// Everything the target site can observe about the browser is still produced by
// the real Chrome process itself; no JS is injected and no CDP attach happens.
//
// Usage:  RELAY_PORT=7891 UP_PORT=10046 node tools/capture/socks5-relay.cjs
'use strict';

const net = require('node:net');

const UP_HOST = process.env.UP_HOST || 'gw.dataimpulse.com';
const UP_PORT = Number(process.env.UP_PORT || 10046);
const UP_USER = process.env.UP_USER || '3ddce6da3add2226e7d2__cr.us;sessttl.30';
const UP_PASS = process.env.UP_PASS || '2805d82cb2a633dc';
const LISTEN_PORT = Number(process.env.RELAY_PORT || 7891);
const VERBOSE = process.env.VERBOSE === '1';

let conns = 0;
function note(...args) { if (VERBOSE) console.log(new Date().toISOString().slice(11, 19), ...args); }

// Buffered exact-length reader over a socket.
class Reader {
  constructor(sock) {
    this.sock = sock;
    this.buf = Buffer.alloc(0);
    this.pending = null;
    this.err = null;
    this.ended = false;
    this._onData = (d) => { this.buf = Buffer.concat([this.buf, d]); this._pump(); };
    this._onEnd = () => { this.ended = true; this._pump(); };
    this._onErr = (e) => { this.err = e; this._pump(); };
    sock.on('data', this._onData);
    sock.on('end', this._onEnd);
    sock.on('error', this._onErr);
  }
  _pump() {
    if (!this.pending) return;
    const p = this.pending;
    if (this.buf.length >= p.n) {
      this.pending = null;
      const out = this.buf.subarray(0, p.n);
      this.buf = this.buf.subarray(p.n);
      p.resolve(out);
      this._pump();
      return;
    }
    if (this.err || this.ended) {
      this.pending = null;
      p.reject(this.err || new Error('eof'));
    }
  }
  read(n) {
    return new Promise((resolve, reject) => { this.pending = { n, resolve, reject }; this._pump(); });
  }
  // Hand the socket over to raw piping, returning whatever was already buffered.
  detach() {
    this.sock.removeListener('data', this._onData);
    this.sock.removeListener('end', this._onEnd);
    this.sock.removeListener('error', this._onErr);
    const rest = this.buf;
    this.buf = Buffer.alloc(0);
    return rest;
  }
}

async function upstreamConnect(host, port) {
  const sock = await new Promise((resolve, reject) => {
    const s = net.connect({ host: UP_HOST, port: UP_PORT }, () => { s.setTimeout(0); resolve(s); });
    s.setTimeout(25000, () => reject(new Error('upstream connect timeout')));
    s.once('error', reject);
  });
  sock.removeAllListeners('error');
  sock.on('error', () => {});
  const rd = new Reader(sock);

  // 1. greeting: username/password only
  sock.write(Buffer.from([0x05, 0x01, 0x02]));
  const hello = await rd.read(2);
  if (hello[0] !== 0x05) throw new Error(`upstream bad ver ${hello[0]}`);
  if (hello[1] !== 0x02) throw new Error(`upstream wants method ${hello[1]}`);

  // 2. RFC1929 user/pass
  const user = Buffer.from(UP_USER);
  const pass = Buffer.from(UP_PASS);
  sock.write(Buffer.concat([Buffer.from([0x01, user.length]), user, Buffer.from([pass.length]), pass]));
  const auth = await rd.read(2);
  if (auth[1] !== 0x00) throw new Error(`upstream auth rejected status ${auth[1]}`);

  // 3. CONNECT to the client's target, addressed the same way the client asked
  const name = Buffer.from(host);
  const req = Buffer.concat([
    Buffer.from([0x05, 0x01, 0x00, 0x03, name.length]), name,
    Buffer.from([(port >> 8) & 0xff, port & 0xff]),
  ]);
  sock.write(req);
  const rep = await rd.read(4);
  if (rep[1] !== 0x00) throw new Error(`upstream connect refused rep=${rep[1]}`);
  if (rep[3] === 0x01) await rd.read(4);
  else if (rep[3] === 0x03) { const l = (await rd.read(1))[0]; await rd.read(l); }
  else if (rep[3] === 0x04) await rd.read(16);
  await rd.read(2);

  return { sock, rest: rd.detach() };
}

const server = net.createServer((client) => {
  const id = ++conns;
  client.on('error', () => {});
  client.setTimeout(0);
  const rd = new Reader(client);

  (async () => {
    const head = await rd.read(2);
    if (head[0] !== 0x05) throw new Error(`client not socks5 (ver ${head[0]})`);
    await rd.read(head[1]);
    client.write(Buffer.from([0x05, 0x00])); // no auth required, none offered

    const req = await rd.read(4);
    if (req[1] !== 0x01) throw new Error(`only CONNECT supported (cmd ${req[1]})`);
    const atyp = req[3];
    let host = '';
    if (atyp === 0x01) { host = [...await rd.read(4)].join('.'); }
    else if (atyp === 0x03) { const l = (await rd.read(1))[0]; host = (await rd.read(l)).toString('latin1'); }
    else if (atyp === 0x04) {
      const b = await rd.read(16);
      const parts = [];
      for (let i = 0; i < 16; i += 2) parts.push(b.readUInt16BE(i).toString(16));
      host = parts.join(':');
    } else throw new Error(`bad atyp ${atyp}`);
    const port = (await rd.read(2)).readUInt16BE(0);
    const rest = rd.detach();

    let up;
    try {
      up = await upstreamConnect(host, port);
    } catch (error) {
      note(`#${id} ${host}:${port} FAIL ${error.message}`);
      client.write(Buffer.from([0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
      client.destroy();
      return;
    }

    client.write(Buffer.from([0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
    note(`#${id} ${host}:${port} OK`);
    if (rest.length) up.sock.write(rest);
    if (up.rest.length) client.write(up.rest);
    client.pipe(up.sock);
    up.sock.pipe(client);
    const kill = () => { try { client.destroy(); } catch { /* ignore */ } try { up.sock.destroy(); } catch { /* ignore */ } };
    client.on('close', kill);
    up.sock.on('close', kill);
  })().catch((error) => {
    note(`#${id} handshake error ${String(error).slice(0, 120)}`);
    try { client.destroy(); } catch { /* ignore */ }
  });
});

server.on('error', (error) => { console.error('relay server error', String(error).slice(0, 200)); });
server.listen(LISTEN_PORT, '127.0.0.1', () => {
  console.log(`socks5 relay listening on 127.0.0.1:${LISTEN_PORT} -> ${UP_HOST}:${UP_PORT} as ${UP_USER.slice(0, 8)}...`);
});
