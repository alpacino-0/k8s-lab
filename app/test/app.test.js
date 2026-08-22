'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { createApp, createMetricsApp } = require('../src/app');
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
  const db = {
    countNotesFor: async () => 0,
    createNote: async (_owner, text) => ({ id: 1, text, created_at: 'now' }),
  };
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

test('the application port does not serve metrics', async () => {
  await withServer({ db: { countNotes: async () => 7 } }, async (base) => {
    const res = await fetch(`${base}/metrics`);
    assert.strictEqual(res.status, 404, 'metrics must live on the telemetry port only');
  });
});

test('the telemetry port serves the custom series', async () => {
  const metrics = createMetrics();
  const app = createMetricsApp({ metrics, db: { countNotes: async () => 7 } });
  metrics.httpRequests.inc({ route: '/', method: 'GET', status: 200 });
  const server = app.listen(0);
  await new Promise((r) => server.once('listening', r));
  try {
    const body = await (
      await fetch(`http://127.0.0.1:${server.address().port}/metrics`)
    ).text();
    assert.match(body, /http_requests_total/);
    assert.match(body, /notes_total\{[^}]*\} 7/);
  } finally {
    await new Promise((r) => server.close(r));
  }
});

test('the telemetry port serves nothing else', async () => {
  const app = createMetricsApp({ metrics: createMetrics(), db: null });
  const server = app.listen(0);
  await new Promise((r) => server.once('listening', r));
  try {
    const res = await fetch(`http://127.0.0.1:${server.address().port}/notes`);
    assert.strictEqual(res.status, 404);
  } finally {
    await new Promise((r) => server.close(r));
  }
});

test('responses identify the replica that served them', async () => {
  await withServer(
    { config: { pod: { name: 'app-abc', node: 'worker-2', ip: '10.0.0.9', namespace: 'k8s-lab' } },
      db: { countNotesFor: async () => 3 } },
    async (base) => {
      const body = await (await fetch(`${base}/stats`)).json();
      assert.strictEqual(body.servedBy.pod, 'app-abc');
      assert.strictEqual(body.servedBy.node, 'worker-2');
      assert.strictEqual(body.notes, 3);
      assert.strictEqual(body.databaseUp, true);
    },
  );
});

test('stats reports the database as down without failing the request', async () => {
  const db = { countNotesFor: async () => { throw new Error('unreachable'); } };
  await withServer({ db }, async (base) => {
    const res = await fetch(`${base}/stats`);
    assert.strictEqual(res.status, 200, 'stats must stay available when the DB is not');
    assert.strictEqual((await res.json()).databaseUp, false);
  });
});

test('deleting a note validates the id and reports a missing note', async () => {
  const db = {
    deleteNote: async (_owner, id) => (id === 1 ? { id: 1, text: 'gone', created_at: 'now' } : null),
  };
  await withServer({ db }, async (base) => {
    assert.strictEqual((await fetch(`${base}/notes/abc`, { method: 'DELETE' })).status, 400);
    assert.strictEqual((await fetch(`${base}/notes/-3`, { method: 'DELETE' })).status, 400);
    assert.strictEqual((await fetch(`${base}/notes/99`, { method: 'DELETE' })).status, 404);

    const ok = await fetch(`${base}/notes/1`, { method: 'DELETE' });
    assert.strictEqual(ok.status, 200);
    assert.strictEqual((await ok.json()).deleted.id, 1);
  });
});

test('unknown routes return a JSON 404', async () => {
  await withServer({}, async (base) => {
    const res = await fetch(`${base}/does-not-exist`);
    assert.strictEqual(res.status, 404);
    assert.deepStrictEqual(await res.json(), { error: 'not found' });
  });
});

test('every response assigns an anonymous visitor cookie', async () => {
  await withServer({}, async (base) => {
    const cookie = (await fetch(`${base}/`)).headers.get('set-cookie');
    assert.match(cookie, /^visitor=[0-9a-f]{32}/);
    assert.match(cookie, /HttpOnly/);
    assert.match(cookie, /SameSite=Lax/);
  });
});

test('notes are scoped to the visitor that created them', async () => {
  const store = [];
  const db = {
    listNotes: async (owner) => store.filter((n) => n.owner === owner),
    createNote: async (owner, text) => {
      const note = { id: store.length + 1, owner, text, created_at: 'now' };
      store.push(note);
      return note;
    },
    countNotesFor: async (owner) => store.filter((n) => n.owner === owner).length,
    deleteNote: async (owner, id) => {
      const i = store.findIndex((n) => n.id === id && n.owner === owner);
      return i === -1 ? null : store.splice(i, 1)[0];
    },
  };

  await withServer({ db }, async (base) => {
    const post = await fetch(`${base}/notes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: 'mine' }),
    });
    assert.strictEqual(post.status, 201);
    const mine = post.headers.get('set-cookie').split(';')[0];

    // The owner sees it.
    const own = await (await fetch(`${base}/notes`, { headers: { cookie: mine } })).json();
    assert.strictEqual(own.count, 1);

    // A different visitor does not.
    const other = await (await fetch(`${base}/notes`)).json();
    assert.strictEqual(other.count, 0, 'a visitor must not see other people\'s notes');

    // And cannot delete it.
    const steal = await fetch(`${base}/notes/1`, { method: 'DELETE' });
    assert.strictEqual(steal.status, 404, 'deleting someone else\'s note must not succeed');

    // The owner still can.
    const del = await fetch(`${base}/notes/1`, { method: 'DELETE', headers: { cookie: mine } });
    assert.strictEqual(del.status, 200);
  });
});

test('a visitor cannot store more than the configured number of notes', async () => {
  const db = {
    countNotesFor: async () => 20,
    createNote: async () => { throw new Error('should not be reached'); },
  };
  await withServer({ config: { limits: { ...loadConfig({}).limits, maxNotesPerVisitor: 20 } }, db },
    async (base) => {
      const res = await fetch(`${base}/notes`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: 'one too many' }),
      });
      assert.strictEqual(res.status, 409);
      assert.match((await res.json()).error, /20 notes/);
    });
});

test('control characters are stripped from stored text', async () => {
  let stored = null;
  const db = {
    countNotesFor: async () => 0,
    createNote: async (_owner, text) => { stored = text; return { id: 1, text, created_at: 'now' }; },
  };
  await withServer({ db }, async (base) => {
    await fetch(`${base}/notes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: 'clean\u0000text\u001b' }),
    });
    assert.strictEqual(stored, 'clean text');
  });
});

test('writes are rate limited per visitor', async () => {
  const db = {
    countNotesFor: async () => 0,
    createNote: async (_owner, text) => ({ id: 1, text, created_at: 'now' }),
  };
  await withServer(
    { config: { limits: { ...loadConfig({}).limits, writesPerMinute: 3 } }, db },
    async (base) => {
      const cookie = 'visitor=' + 'a'.repeat(32);
      const codes = [];
      for (let i = 0; i < 5; i += 1) {
        const res = await fetch(`${base}/notes`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', cookie },
          body: JSON.stringify({ text: `note ${i}` }),
        });
        codes.push(res.status);
        if (res.status === 429) {
          assert.ok(res.headers.get('retry-after'), '429 must say when to retry');
        }
      }
      assert.deepStrictEqual(codes, [201, 201, 201, 429, 429]);
    },
  );
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
