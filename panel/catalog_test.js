'use strict';

// The catalogue page's three decisions.
//
// The page renders what the API says and works nothing out for itself, so there
// are only three functions here that decide anything: what the filter form
// becomes, whether the install button may be offered, and what a refusal says.
// The third is the one that matters — the endpoint names which of three limits
// stopped an install, and a page that replaces that with "cannot be installed"
// throws away the only part a person can act on.

const test = require('node:test');
const assert = require('node:assert');

const { catalogQuery, canInstall, refusals } = require('./assets/catalog.js');

test('an empty filter asks for the whole catalogue', () => {
  assert.strictEqual(catalogQuery(), '');
  assert.strictEqual(catalogQuery({ text: '   ' }), '',
    'whitespace became a search for whitespace, which matches nothing');
});

// The parameter names are a contract with the handler, and the handler ignores
// what it does not recognise: a name spelled wrong here answers the whole
// catalogue, which reads as a search box that does not work.
test('the filter uses the names the endpoint reads', () => {
  const query = catalogQuery({ text: 'n8n', category: 'automation' });
  assert.match(query, /(^|[?&])q=n8n(&|$)/, `the search text is not in ${query}`);
  assert.match(query, /(^|[?&])category=automation(&|$)/, `the category is not in ${query}`);
});

// Two tags narrow. One parameter carrying "a,b" is a single tag nothing has,
// and the list would come back empty with no way to tell why.
test('two tags are two parameters and not one joined string', () => {
  const query = catalogQuery({ tags: ['self-hosted', 'analytics'] });
  const tags = new URLSearchParams(query.slice(1)).getAll('tag');
  assert.deepStrictEqual(tags, ['self-hosted', 'analytics'],
    `two tags arrived as ${JSON.stringify(tags)}`);
});

// The button is a promise that pressing it will work. Anything other than the
// API saying so is the page not knowing.
test('the install button is offered only when the API said installable', () => {
  assert.strictEqual(canInstall({ installable: true }), true);
  for (const plan of [null, undefined, {}, { installable: false },
    { detail: 'this installation has no catalogue', status: 503 }]) {
    assert.strictEqual(canInstall(plan), false,
      `${JSON.stringify(plan)} offered an install button for something nothing said would work`);
  }
});

test('a refusal is shown in the words the endpoint used', () => {
  const plan = {
    installable: false,
    refusals: ['this template asks the platform to invent 1 value(s) — SERVICE_PASSWORD_N8N'],
  };
  assert.deepStrictEqual(refusals(plan), plan.refusals,
    'the reason was rewritten; the endpoint names which of three limits stopped it and that ' +
    'is the only part a person can act on');
});

test('no reason from the API is no reason invented', () => {
  assert.deepStrictEqual(refusals({ installable: false }), []);
  assert.deepStrictEqual(refusals(null), []);
});
