// The catalogue: the list a user picks an application from, and the one button.
//
// It decides nothing about whether an entry can be installed, and that is the
// rule this file is under rather than a simplification. Whether n8n installs
// depends on what the converter produced, what the template asks the platform
// to invent, and how many objects the write path can hold — three answers that
// live in the API. A page that worked them out from the entry it can see would
// eventually disagree with the endpoint that refuses, and a catalogue that
// greys out what the API would accept is worse than one that offers everything.
//
// So: the list comes from GET /catalog, and the moment a user picks an entry
// the page asks the install endpoint itself, with dryRun, and shows the answer
// it gets back word for word.

// Turns the filter form into a query string.
//
// Its own function because the parameter names are a contract with the handler
// and getting one wrong fails silently in the worst direction: the endpoint
// ignores what it does not recognise and answers the whole catalogue, which
// reads as a search box that does not work rather than as a bug.
//
// Tags repeat rather than joining with a comma. Two tags narrow, and one
// parameter carrying "a,b" is a single tag nothing has.
function catalogQuery({ text = '', category = '', tags = [] } = {}) {
  const params = new URLSearchParams();
  if (text.trim()) params.set('q', text.trim());
  if (category.trim()) params.set('category', category.trim());
  for (const tag of tags) {
    if (String(tag).trim()) params.append('tag', String(tag).trim());
  }
  const query = params.toString();
  return query ? `?${query}` : '';
}

// Whether the Install button may be offered for this plan.
//
// Strictly true and nothing else. Every other answer — a plan that has not been
// asked for yet, a 503 because no catalogue is mounted, a problem document, a
// response from an older build with no such field — arrives here as something
// that is not true, and all of them mean the same thing: the page does not know
// that this would work, so it must not offer a button that says it will.
function canInstall(plan) {
  return plan !== null && typeof plan === 'object' && plan.installable === true;
}

// The reasons an install was refused, as the API gave them.
//
// Returns an empty list rather than inventing a sentence. The endpoint names
// which of three limits stopped it — a value nothing mints, an image the API
// refuses, more objects than an environment holds — and a page that summarises
// those as "this cannot be installed" throws away the only part a person can
// act on.
function refusals(plan) {
  if (!plan || !Array.isArray(plan.refusals)) return [];
  return plan.refusals;
}

// --- the page ---------------------------------------------------------------

const catalogState = { entries: [], categories: [], selected: null, plan: null };

async function catalogApi(path, options = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  const body = await res.text();
  let parsed = null;
  try {
    parsed = body ? JSON.parse(body) : null;
  } catch {
    parsed = null;
  }
  return { status: res.status, body: parsed, text: body };
}

function catalogEl(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else node.setAttribute(k, v);
  }
  for (const child of children) node.append(child);
  return node;
}

// Mounts a section into the page the panel already renders.
//
// It appends rather than expecting markup, so that turning the catalogue on is
// one import in app.js and no edit to index.html. The base path comes from the
// same place the rest of the panel's does — a tenant is in the URL of every
// endpoint here — and is passed in rather than read, because this file does not
// own the panel's state.
function mountCatalog(root, base) {
  const search = catalogEl('input', { id: 'catalog-search', placeholder: 'search applications' });
  const list = catalogEl('div', { id: 'catalog-list' });
  const detail = catalogEl('div', { id: 'catalog-detail' });
  root.append(catalogEl('h2', { text: 'Catalogue' }), search, list, detail);

  const load = async () => {
    const { status, body } = await catalogApi(`${base}/catalog${catalogQuery({ text: search.value })}`);
    list.replaceChildren();
    if (status !== 200) {
      // The 503 case says which flag is unset, and it is shown as it arrived:
      // "no applications" and "nobody mounted the catalogue" are different
      // sentences and only the endpoint knows which one is true.
      list.append(catalogEl('p', { class: 'empty', text: (body && body.detail) || 'the catalogue is unavailable' }));
      return;
    }
    catalogState.entries = body.entries || [];
    catalogState.categories = body.categories || [];
    for (const entry of catalogState.entries) {
      const button = catalogEl('button', { class: 'catalog-entry', type: 'button' },
        catalogEl('strong', { text: entry.name }),
        catalogEl('span', { text: entry.slogan || '' }),
      );
      button.addEventListener('click', () => select(entry));
      list.append(button);
    }
    if (catalogState.entries.length === 0) {
      list.append(catalogEl('p', { class: 'empty', text: 'nothing matches that search' }));
    }
  };

  const select = async (entry) => {
    catalogState.selected = entry;
    catalogState.plan = null;
    detail.replaceChildren(catalogEl('p', { text: `planning ${entry.name}…` }));

    // The dry run is the panel asking the endpoint that would refuse. Nothing
    // below is computed from the entry.
    const app = entry.name;
    const { body } = await catalogApi(`${base}/apps/${app}/envs/prod/from-catalog`, {
      method: 'POST',
      body: JSON.stringify({
        template: entry.name, dryRun: true,
        namespace: `${app}-prod`, repoUrl: '', path: `apps/${app}/prod`,
      }),
    });
    catalogState.plan = body;
    render(entry, body);
  };

  const render = (entry, plan) => {
    const nodes = [catalogEl('h3', { text: entry.name })];
    if (entry.documentation) {
      nodes.push(catalogEl('a', { href: entry.documentation, text: 'documentation' }));
    }
    for (const reason of refusals(plan)) {
      nodes.push(catalogEl('p', { class: 'refusal', text: reason }));
    }
    if (canInstall(plan)) {
      nodes.push(catalogEl('button', { class: 'install', type: 'button', text: `install ${entry.name}` }));
    }
    detail.replaceChildren(...nodes);
  };

  search.addEventListener('input', () => { load().catch(() => {}); });
  return load;
}

// The two guards that let this file be both a page and a module, for the reason
// app.js gives: the browser has document and no module, node --test has the
// reverse, and the panel has no build step to reconcile them.
if (typeof module !== 'undefined') {
  module.exports = { catalogQuery, canInstall, refusals, mountCatalog };
}
