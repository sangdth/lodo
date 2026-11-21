'use strict';
// Run by TestBind with lodo's hook for flowy.test in NODE_OPTIONS. LODO_LISTED is the listed port and LODO_ADDR
// its address, LODO_UNLISTED a port the hook doesn't list, LODO_OTHER a loopback address outside the hook's local
// hosts, or empty when there is none, and LODO_BIND the path of bind.cjs.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const http = require('node:http');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');

const P = Number(process.env.LODO_LISTED);
const Q = Number(process.env.LODO_UNLISTED);
const ADDR = process.env.LODO_ADDR;
const OTHER = process.env.LODO_OTHER;
const WILDCARD = ['::', '0.0.0.0'];
const VITE_HOSTS = '__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS';

// listen resolves with where server bound, and whether a callback in args ran. The server is unref'd, so one a
// failed assertion leaves open doesn't keep node running.
function listen(server, ...args) {
  server.unref();
  return new Promise((resolve, reject) => {
    let cbCalled = false;
    if (typeof args.at(-1) === 'function') args[args.length - 1] = () => { cbCalled = true; };
    server.once('error', reject);
    server.once('listening', () => setImmediate(() => resolve({ addr: server.address(), cbCalled })));
    server.listen(...args);
  });
}
const close = (s) => new Promise((r) => s.close(r));

test('NODE_OPTIONS keeps the options before the hook', () => {
  assert.strictEqual(process.noDeprecation, true);
});

test("the hook adds the project to Vite's allowed hosts", () => {
  assert.strictEqual(process.env[VITE_HOSTS], '.flowy.test');
});

// setEnv sets VITE_HOSTS to value, or unsets it when value is undefined.
function setEnv(value) {
  if (value === undefined) delete process.env[VITE_HOSTS];
  else process.env[VITE_HOSTS] = value;
}

// [name, VITE_HOSTS before, hosts, VITE_HOSTS after]
const viteHosts = [
  ['set from none', undefined, '.flowy.test', '.flowy.test'],
  ['appended to an existing value', 'app.example', '.flowy.test', 'app.example,.flowy.test'],
  ['not added twice', 'app.example,.flowy.test', '.flowy.test', 'app.example,.flowy.test'],
  ['appended next to a longer name', 'admin.flowy.test', '.flowy.test', 'admin.flowy.test,.flowy.test'],
  ['untouched when hosts is empty', 'app.example', '', 'app.example'],
  ['left unset when hosts is missing', undefined, undefined, undefined],
];
for (const [name, before, hosts, after] of viteHosts) {
  test(`Vite's allowed hosts: ${name}`, () => {
    const listen = net.Server.prototype.listen;
    const saved = process.env[VITE_HOSTS];
    try {
      setEnv(before);
      require(process.env.LODO_BIND)({}, hosts);
      assert.strictEqual(process.env[VITE_HOSTS], after);
    } finally {
      net.Server.prototype.listen = listen; // drop the extra wrapper
      setEnv(saved);
    }
  });
}

const rewritten = [
  ['listen(P)', [P]],
  ['listen("P")', [String(P)]],
  ['listen(P, cb)', [P, () => {}]],
  ['listen(P, "localhost", cb)', [P, 'localhost', () => {}]],
  ['listen(P, "127.0.0.1", 511, cb)', [P, '127.0.0.1', 511, () => {}]],
  ['listen(P, "::1")', [P, '::1']],
  ['listen(P, "0.0.0.0")', [P, '0.0.0.0']],
  ['listen(P, "::")', [P, '::']],
  ['listen(P, null, 511, cb)', [P, null, 511, () => {}]],
  ['listen(P, 511, cb)', [P, 511, () => {}]],
  ['listen({port: P}, cb)', [{ port: P }, () => {}]],
  ['listen({port: "P", host: "localhost"})', [{ port: String(P), host: 'localhost' }]],
  ['listen({port: P, host: "::", backlog: 10})', [{ port: P, host: '::', backlog: 10 }]],
];
for (const [name, args] of rewritten) {
  test(`rewrites ${name}`, async () => {
    const s = net.createServer();
    const { addr, cbCalled } = await listen(s, ...args);
    assert.deepStrictEqual([addr.address, addr.port], [ADDR, P]);
    if (typeof args.at(-1) === 'function') assert.ok(cbCalled, 'callback ran');
    await close(s);
  });
}

test('rewrites http.Server too', async () => {
  const s = http.createServer((_, res) => res.end('ok'));
  const { addr } = await listen(s, P);
  assert.strictEqual(addr.address, ADDR);
  await close(s);
});

test('keeps an explicit non-loopback host on a listed port', { skip: !OTHER && 'no second loopback address' }, async () => {
  const s = net.createServer();
  const { addr } = await listen(s, P, OTHER);
  assert.strictEqual(addr.address, OTHER);
  await close(s);
});

test('keeps an unlisted port', async () => {
  const s = net.createServer();
  const { addr } = await listen(s, Q);
  assert.ok(WILDCARD.includes(addr.address), addr.address);
  assert.strictEqual(addr.port, Q);
  await close(s);
});

test('keeps port 0 and no-arg listen', async () => {
  for (const args of [[0], [], [{ port: 0 }]]) {
    const s = net.createServer();
    const { addr } = await listen(s, ...args);
    assert.ok(WILDCARD.includes(addr.address), `${JSON.stringify(args)}: ${addr.address}`);
    await close(s);
  }
});

test('keeps unix socket paths', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lodo-bind-'));
  try {
    for (const p of [path.join(dir, 't1.sock'), { path: path.join(dir, 't2.sock') }]) {
      const s = net.createServer();
      const { addr } = await listen(s, p);
      assert.strictEqual(addr, typeof p === 'string' ? p : p.path);
      await close(s);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('keeps a handle and {handle}', async () => {
  const s1 = net.createServer();
  await listen(s1, Q, '127.0.0.1');
  for (const args of [[s1._handle], [{ handle: s1._handle }]]) {
    const s2 = net.createServer();
    const { addr } = await listen(s2, ...args);
    assert.deepStrictEqual([addr.address, addr.port], ['127.0.0.1', Q]);
    s2._handle = null; // the handle belongs to s1
  }
  await close(s1);
});
