'use strict';

// The panel's first tests.
//
// It had none, and the one place it decides anything decided wrongly for as
// long as the page existed: every deploy that had not finished syncing rendered
// as "was refused", under a banner explaining a refusal that had not happened.
// Nothing could have caught it. The page is 550 lines and this file covers the
// four functions that are not rendering — which is the whole of what it decides.
//
// node --test, no npm, nothing installed. The package's argument against a
// build step holds; it does not extend to having no tests.

const test = require('node:test');
const assert = require('node:assert');

const { whatToShow, admissionKnown, short, when } = require('./assets/app.js');

const rec = (state, extra = {}) => ({
  seq: 1, state, admission: { allowed: false, reason: '' }, ...extra,
});

test('a running deploy is shown and is not blocked', () => {
  const current = rec('running');
  const got = whatToShow(current, rec('running'));
  assert.strictEqual(got.blocked, false);
  assert.strictEqual(got.shown, current);
});

// The bug, as a test. A deploy on its way is not a deploy that was refused, and
// the page said it was — at the exact moment somebody is most likely to be
// watching it.
test('a deploy still on its way is not reported as refused', () => {
  for (const state of ['pending', 'syncing', 'applied']) {
    const got = whatToShow(null, rec(state));
    assert.strictEqual(got.blocked, false,
      `a ${state} deploy was shown as refused; nothing refused it, it has not ` +
      'finished — and those are different sentences');
  }
});

// And the case the page exists for: when something WAS refused, the reason is
// in the very next record down and the page has to reach for it rather than
// saying "nothing is running" and stopping.
test('a refused deploy is shown so the reason can be', () => {
  const latest = rec('rejected', {
    admission: { allowed: false, reason: 'PolicyViolation', message: 'not signed' },
  });
  const got = whatToShow(null, latest);
  assert.strictEqual(got.blocked, true);
  assert.strictEqual(got.shown, latest,
    'the refused record was not shown, so the reason it was refused is nowhere');
});

test('nothing deployed shows nothing rather than inventing a refusal', () => {
  const got = whatToShow(null, null);
  assert.strictEqual(got.blocked, false);
  assert.strictEqual(got.shown, null);
});

// A running deploy wins over a rejected later one. The rejected record belongs
// to a different attempt; the app is still up, and a page that says otherwise
// is wrong about the thing the reader can see for themselves.
test('a later refusal does not hide what is running', () => {
  const current = rec('running');
  const got = whatToShow(current, rec('rejected'));
  assert.strictEqual(got.blocked, false);
  assert.strictEqual(got.shown, current);
});

// Three answers, not two. With nothing writing admission outcomes, a page that
// can only say admitted or refused has to call every deploy in existence
// refused — which is a claim, made out of a zero value.
test('an unobserved admission is not an admitted or refused one', () => {
  assert.strictEqual(admissionKnown(rec('syncing')), false,
    'a record nothing observed was treated as carrying an outcome');

  assert.strictEqual(admissionKnown(rec('running', {
    admission: { allowed: true, reason: 'Admitted' },
  })), true);

  assert.strictEqual(admissionKnown(rec('rejected', {
    admission: { allowed: false, reason: 'FailedCreate', message: 'denied' },
  })), true, 'a refusal with a reason was treated as unobserved');

  // The state alone is enough. A record moved to rejected was refused whether
  // or not anything filled in the reason.
  assert.strictEqual(admissionKnown(rec('rejected')), true);
});

// Both of these were written against what the functions were assumed to do and
// both were wrong, which is its own small argument for the file existing: short
// truncates past sixteen characters rather than at seven, and when formats a
// date rather than saying "two hours ago". The tests now say what the code does.
test('a long commit is shortened and a short one is left alone', () => {
  // A full SHA-1 is forty characters and gets cut; anything already short
  // enough is not, because an ellipsis on a value that fits is noise.
  assert.strictEqual(short('a07ef31b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f'), 'a07ef31b2c3d…');
  assert.strictEqual(short('a07ef31b2c3d4e5f'), 'a07ef31b2c3d4e5f');

  // The em dash is the point: an absent commit must not render as a real one.
  assert.strictEqual(short(''), '—', 'an absent commit rendered as a value');
  assert.strictEqual(short(null), '—');
  assert.strictEqual(short(undefined), '—');
});

test('a timestamp renders and an absent one does not pretend', () => {
  const stamp = '2026-08-28T09:00:00Z';
  const rendered = when(stamp);
  assert.notStrictEqual(rendered, '—');
  assert.match(rendered, /2026/, 'the date did not survive formatting');

  // A value the browser cannot parse is shown as it arrived rather than as
  // "Invalid Date", which reads like the record is corrupt when the formatter
  // is the thing that failed.
  assert.strictEqual(when('not a date'), 'not a date');

  assert.strictEqual(when(''), '—');
  assert.strictEqual(when(null), '—');
});

// The line the restore rehearsal exists to put on a page, and the three answers
// it has to be able to give.
const { backupView } = require('./assets/app.js');

test('a restored backup says when and how many out of how many', () => {
  const v = backupView({
    database: 'shop-db', state: 'restored',
    finishedAt: '2026-08-29T02:04:11Z', rows: 1284, sourceRows: 1284, tables: 7,
  });
  assert.match(v.lastRun.text, /restored/);
  assert.strictEqual(v.lastRun.ok, true);
  // Against the source, not alone. "1,284 came back" and "1,284 came back out
  // of 1,284" are different claims and only the second was measured.
  assert.match(v.verified.text, /1284 of 1284/);
  assert.match(v.verified.text, /7 tables/);
});

test('an archive nothing restored is not called restored', () => {
  const v = backupView({
    database: 'shop-db', state: 'backed up', finishedAt: '2026-08-29T02:00:00Z',
  });
  assert.doesNotMatch(v.lastRun.text, /restored/,
    'a backup nobody restored was reported as restored, which is the claim the ' +
    'whole rehearsal exists to make honestly');
  assert.match(v.verified.text, /not restored/);
  assert.strictEqual(v.lastRun.ok, false);
});

test('a database whose first backup has not run says exactly that', () => {
  const v = backupView({ database: 'shop-db', state: 'none yet' });
  assert.match(v.lastRun.text, /no backup has run yet/);
  assert.strictEqual(v.lastRun.ok, null, 'a run that has not happened was given a verdict');
  // And no count, because there is nothing to count. A zero here reads as a
  // restore that produced nothing.
  assert.strictEqual(v.verified, null);
});

test('one table is a table', () => {
  const v = backupView({
    database: 'db', state: 'restored', finishedAt: '2026-08-29T02:00:00Z',
    rows: 3, sourceRows: 3, tables: 1,
  });
  assert.match(v.verified.text, /1 table\b/);
});
