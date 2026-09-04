'use strict';

// That the page actually loads the files it is made of.
//
// This exists because it did not, and nothing noticed. logs.js and catalog.js
// were both written, both tested, and neither was ever loaded by index.html or
// referenced by app.js — a log view and a catalogue that passed their tests and
// could not be reached in a browser. Their unit tests went on passing the whole
// time, because a module's tests require it directly and never ask whether the
// page does.
//
// The panel has no build step, which is what makes this checkable with a string
// search and also what makes it necessary: nothing resolves an import, so a
// missing script tag is a missing global at runtime and silence at every other
// time.

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const assets = path.join(__dirname, 'assets');
const read = (name) => fs.readFileSync(path.join(assets, name), 'utf8');

// Each view: the file, the global it publishes, and the word app.js reaches it
// by. Adding a view means adding a row, which is the point — the next one
// cannot be orphaned by being forgotten here, because forgetting it here is
// the same act as forgetting it in the page.
const views = [
  { file: 'metrics.js', global: 'damgaMetrics' },
  { file: 'logs.js', global: 'damgaLogs' },
  { file: 'catalog.js', global: 'damgaCatalog' },
];

test('every view is loaded by the page, before the script that uses it', () => {
  const html = read('index.html');
  const appAt = html.indexOf('/app.js');
  assert.ok(appAt > -1, 'index.html does not load app.js at all');

  for (const view of views) {
    const at = html.indexOf(`/${view.file}`);
    assert.ok(at > -1,
      `index.html never loads ${view.file}: it is written, it is tested, and in a ` +
      'browser it does not exist');
    assert.ok(at < appAt,
      `${view.file} is loaded after app.js, so the global it publishes is not there ` +
      'when app.js looks for it');
  }
});

test('every view publishes the global the page looks for', () => {
  for (const view of views) {
    const source = read(view.file);
    assert.ok(source.includes(`window.${view.global}`),
      `${view.file} does not publish window.${view.global}, so loading it achieves nothing`);
    // Both guards, or the file is a page and not a module — and its own tests
    // stop being able to require it.
    assert.ok(source.includes('module.exports'),
      `${view.file} does not export for node --test`);
  }
});

test('app.js reaches every view, and reaches it by the published name', () => {
  const app = read('app.js');
  for (const view of views) {
    assert.ok(app.includes(view.global),
      `app.js never mentions ${view.global}: ${view.file} would be loaded and never mounted, ` +
      'which is the state logs.js and catalog.js were both in');
  }
});

// The half that made "wired" not the same as "usable". Both views were mounted
// into a page with no rules for them at all: an unstyled <pre> with no lines
// collapses to nothing, so a stream that is connecting or stopped reads as an
// application that printed nothing.
test('the page has rules for what the views draw', () => {
  const css = read('style.css');
  for (const selector of ['.logs', '.catalog-entry', '.refusal', '#catalog-list']) {
    assert.ok(css.includes(selector),
      `style.css has no ${selector}: the view renders, and renders unstyled`);
  }
});

// The view that lives in app.js rather than in a file of its own, and can be
// orphaned the same way: a function that draws a form, and no button that
// reaches it. That is not hypothetical here — the panel had exactly one POST
// other than signing in, so the endpoint that registers an application was
// reachable from curl and from nowhere on the page.
test('the page offers a way to create an application, and reaches it', () => {
  const app = read('app.js');
  assert.ok(/function showNewApp\(/.test(app),
    'app.js has no showNewApp: the only way to create an app is the catalogue, which ' +
    'installs a template and cannot register something somebody builds themselves');
  // Two mentions, not one. The definition is a mention, which is why the first
  // version of this assertion passed with the button deleted: `includes` found
  // `showNewApp()` in `function showNewApp()` and reported the view as wired.
  const mentions = app.match(/showNewApp/g) || [];
  assert.ok(mentions.length >= 2,
    'showNewApp is defined and never called: a form nothing opens is a form nobody has');
});

test('the page can actually POST an application', () => {
  const app = read('app.js');
  assert.ok(app.includes('`${tenantBase()}/apps`'),
    "app.js never addresses the tenant's /apps: the form draws and submits nothing, or " +
    'submits somewhere else');
  assert.ok(app.includes('method: "POST"'),
    'nothing on this page POSTs anything but the sign-in form, which is the state that ' +
    'made every first app on every install a curl command');
});
