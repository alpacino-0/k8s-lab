'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { loadConfig } = require('../src/config');

test('applies defaults when the environment is empty', () => {
  const c = loadConfig({});
  assert.strictEqual(c.env, 'development');
  assert.strictEqual(c.port, 3000);
  assert.strictEqual(c.greeting, 'Hello');
  assert.strictEqual(c.demoEndpoints, false);
  assert.strictEqual(c.db, null, 'no DB_HOST means no database config');
});

test('reads database settings when DB_HOST is present', () => {
  const c = loadConfig({
    DB_HOST: 'pg-0.pg.svc',
    POSTGRES_USER: 'u',
    POSTGRES_PASSWORD: 'p',
    POSTGRES_DB: 'd',
  });
  assert.strictEqual(c.db.host, 'pg-0.pg.svc');
  assert.strictEqual(c.db.port, 5432);
  assert.strictEqual(c.db.user, 'u');
  assert.strictEqual(c.db.database, 'd');
});

test('demo endpoints stay off unless explicitly set to the string "true"', () => {
  assert.strictEqual(loadConfig({ DEMO_ENDPOINTS: '1' }).demoEndpoints, false);
  assert.strictEqual(loadConfig({ DEMO_ENDPOINTS: 'yes' }).demoEndpoints, false);
  assert.strictEqual(loadConfig({ DEMO_ENDPOINTS: 'true' }).demoEndpoints, true);
});

test('rejects invalid numeric settings instead of silently defaulting', () => {
  assert.throws(() => loadConfig({ PORT: 'abc' }), /PORT must be a positive number/);
  assert.throws(() => loadConfig({ PORT: '-1' }), /PORT must be a positive number/);
});
