'use strict';

// The log view's decisions, which are the four things in it that are not
// rendering.
//
// Its own file rather than cases inside app_test.js: that file covers app.js,
// this covers logs.js, and the two are read separately. node --test, no npm,
// nothing installed — the same terms panel/panel.go argues for.
//
// CI runs `node --test panel/*_test.js`, a glob, so this runs on every push.
// That line is in .github/workflows/ci.yml, which this change does not own; the
// report says so.

const test = require('node:test');
const assert = require('node:assert');

const {
  logsUrl, lineText, appendLine, streamStatus, nextRetry, MAX_LINES,
} = require('./assets/logs.js');

const base = '/api/v1/tenants/t_home/apps/api/envs/prod';

test('the stream is asked for the window the page says it is showing', () => {
  const url = new URL(logsUrl(base, { tail: 50, follow: true }), 'http://x');
  assert.strictEqual(url.pathname, `${base}/logs`);
  assert.strictEqual(url.searchParams.get('tail'), '50');
  assert.strictEqual(url.searchParams.get('follow'), 'true');
});

test('follow=false is sent as a word the server parses', () => {
  const url = new URL(logsUrl(base, { follow: false }), 'http://x');
  assert.strictEqual(url.searchParams.get('follow'), 'false',
    'the server refuses a follow it cannot parse, so this cannot be sent as an empty string');
});

test('a line without a timestamp is still a line', () => {
  const got = lineText({ pod: 'api-1', container: 'app', at: '', text: 'starting' });
  assert.ok(got.includes('starting'), `the text was dropped: ${got}`);
  assert.ok(got.includes('api-1/app'), `the container was dropped: ${got}`);
});

// The kubelet stamps every line it is asked to, and a stamp that arrives
// unparseable must not take the line down with it.
test('a timestamp that is not one does not swallow the line', () => {
  const got = lineText({ pod: 'api-1', container: 'app', at: 'not a date', text: 'still here' });
  assert.ok(got.includes('still here'), `the text was dropped: ${got}`);
});

test('the oldest lines go when the ceiling is reached, not the newest', () => {
  let lines = [];
  for (let i = 0; i < 5; i++) lines = appendLine(lines, { text: `line ${i}` }, 3);
  assert.strictEqual(lines.length, 3);
  assert.deepStrictEqual(lines.map((l) => l.text), ['line 2', 'line 3', 'line 4'],
    'a followed stream never ends, so the view has to drop from the end nobody is reading');
});

test('the ceiling has a default, because a stream with none is a memory leak', () => {
  assert.ok(MAX_LINES > 0 && Number.isFinite(MAX_LINES));
});

// The decision EventSource does not make for itself. It reconnects for ever,
// and a stream the server is refusing — no cluster to read, a session that
// ended — is indistinguishable to it from a network blip. Left alone, one
// forgotten tab asks a broken endpoint every few seconds until it is closed.
test('reconnection backs off and then stops', () => {
  assert.strictEqual(nextRetry(1), 1000);
  assert.strictEqual(nextRetry(2), 2000);
  assert.ok(nextRetry(5) <= 30000, 'the wait grew past the cap');
  assert.strictEqual(nextRetry(99), null,
    'retrying never stops, so a page that cannot stream keeps asking for ever');
});

// A stream that finished and a stream that broke are different sentences, and
// an app scaled to zero produces the first one. Saying "disconnected" about it
// sends somebody looking for a fault that is not there.
test('an ended stream is not reported as a failure', () => {
  const ended = streamStatus('ended', 'nothing is running for this app');
  assert.strictEqual(ended.ok, null);
  assert.strictEqual(ended.text, 'nothing is running for this app',
    'the reason the server gave was replaced with a generic sentence');

  const failed = streamStatus('failed');
  assert.strictEqual(failed.ok, false);
  assert.ok(failed.text.length > 0);
});

test('a live stream says so', () => {
  assert.strictEqual(streamStatus('live').ok, true);
  assert.strictEqual(streamStatus('connecting').ok, null);
});
