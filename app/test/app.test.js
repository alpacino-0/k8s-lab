'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { createApp } = require('../src/app');
const { createMetrics } = require('../src/metrics');
const { loadConfig } = require('../src/config');
const { createLogger } = require('../src/logger');

const silentLogger = { error() {}, warn() {}, info() {}, debug() {} };

/** Start the app on an ephemeral port and return a fetch helper. */
async function withServer(overrides, fn) {
  const config = { ...loadConfig({}), ...overrides.config };
  const metrics = createMetrics();
  const app = createApp({
    config,
    logger: overrides.logger || silentLogger,
    metrics,
    db: overrides.db ?? null,
  });
  const server = app.listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${server.address().port}`;
  try {
    await fn(base, metrics);
  } finally {
    await new Promise((r) => server.close(r));
  }
}

test('liveness never depends on the database', async () => {
  const brokenDb = { ping: async () => { throw new Error('down'); } };
  await withServer({ db: brokenDb }, async (base) => {
    const res = await fetch(`${base}/healthz`);
    assert.strictEqual(res.status, 200, 'liveness must stay green when the DB is down');
  });
});

test('readiness fails when the database is unreachable', async () => {
  const brokenDb = { ping: async () => { throw new Error('connection refused'); } };
  await withServer({ db: brokenDb }, async (base) => {
    const res = await fetch(`${base}/readyz`);
    assert.strictEqual(res.status, 503);
    assert.match(await res.text(), /database unreachable/);
  });
});

test('readiness succeeds when the database answers', async () => {
  await withServer({ db: { ping: async () => {} } }, async (base) => {
    const res = await fetch(`${base}/readyz`);
    assert.strictEqual(res.status, 200);
  });
});

test('note creation validates input', async () => {
  const db = { createNote: async (text) => ({ id: 1, text, created_at: 'now' }) };
  await withServer({ db }, async (base) => {
    const empty = await fetch(`${base}/notes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: '   ' }),
    });
    assert.strictEqual(empty.status, 400);

    const tooLong = await fetch(`${base}/notes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: 'x'.repeat(501) }),
    });
    assert.strictEqual(tooLong.status, 400);

    const ok = await fetch(`${base}/notes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: 'hello' }),
    });
    assert.strictEqual(ok.status, 201);
    assert.strictEqual((await ok.json()).note.text, 'hello');
  });
});

test('endpoints return 503 rather than crashing when no database is configured', async () => {
  await withServer({ db: null }, async (base) => {
    assert.strictEqual((await fetch(`${base}/notes`)).status, 503);
    assert.strictEqual((await fetch(`${base}/healthz`)).status, 200);
  });
});

test('/config never leaks credentials', async () => {
  await withServer(
    { config: { db: { host: 'pg', password: 'super-secret' } }, db: { ping: async () => {} } },
    async (base) => {
      const body = await (await fetch(`${base}/config`)).text();
      assert.ok(!body.includes('super-secret'), '/config must not expose the password');
      assert.match(body, /"databaseConfigured":true/);
    },
  );
});

test('demo endpoint is unreachable unless enabled', async () => {
  await withServer({}, async (base) => {
    assert.strictEqual((await fetch(`${base}/burn`)).status, 404);
  });
  await withServer({ config: { demoEndpoints: true } }, async (base) => {
    assert.strictEqual((await fetch(`${base}/burn?ms=5`)).status, 200);
  });
});

test('metrics endpoint exposes the custom series', async () => {
  await withServer({ db: { countNotes: async () => 7 } }, async (base) => {
    await fetch(`${base}/`);
    const body = await (await fetch(`${base}/metrics`)).text();
    assert.match(body, /http_requests_total/);
    assert.match(body, /notes_total\{[^}]*\} 7/);
  });
});

test('unknown routes return a JSON 404', async () => {
  await withServer({}, async (base) => {
    const res = await fetch(`${base}/does-not-exist`);
    assert.strictEqual(res.status, 404);
    assert.deepStrictEqual(await res.json(), { error: 'not found' });
  });
});

test('logger writes one JSON object per line', () => {
  const written = [];
  const original = process.stdout.write.bind(process.stdout);
  process.stdout.write = (chunk) => { written.push(String(chunk)); return true; };
  try {
    createLogger({ level: 'info', base: { app: 't' } }).info('hello', { k: 1 });
  } finally {
    process.stdout.write = original;
  }
  const parsed = JSON.parse(written.join(''));
  assert.strictEqual(parsed.msg, 'hello');
  assert.strictEqual(parsed.app, 't');
  assert.strictEqual(parsed.k, 1);
});
