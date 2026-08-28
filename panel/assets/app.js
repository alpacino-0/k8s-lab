// The panel. It renders what the API says and decides nothing itself: no
// verdict here is computed locally, because a panel that can reach a different
// conclusion from the CLI about the same deploy is worse than no panel.

const state = { account: null, memberships: [], tenant: null, ref: null };

// The 401 case, given a name. Without one, "the session ended" and "the page
// has a bug" are the same rejected promise, and every failure renders as the
// sign-in form — which says the one thing guaranteed to send somebody looking
// in the wrong place. This cost an hour the first time.
class NotSignedIn extends Error {}

const $ = (id) => document.getElementById(id);

// Whether anything actually recorded an admission outcome for this record.
//
// The panel decides nothing, and this is the smallest amount of deciding that
// still honours that: it does not work out whether a deploy was admitted, only
// whether anybody said. A record carrying no reason, not allowed, and not in
// the rejected state is one nothing has observed — and saying "refused" about
// it would be the panel reaching a conclusion the API never gave it.
const admissionKnown = (r) =>
  Boolean(r.admission.allowed || r.admission.reason || r.state === "rejected");

// One place that talks to the API, so the signed-out case is handled once.
// Anything can 401 at any time — a session expires, an account is disabled
// while somebody has the page open — and the answer is always the same.
async function api(path, options = {}) {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Accept": "application/json", ...(options.headers || {}) },
    ...options,
  });
  if (res.status === 401) {
    throw new NotSignedIn("not signed in");
  }
  const body = res.headers.get("content-type")?.includes("json")
    ? await res.json()
    : null;
  if (!res.ok) {
    throw Object.assign(new Error(body?.detail || `HTTP ${res.status}`), { status: res.status });
  }
  return body;
}

// ---------------------------------------------------------------- sign in

function showSignIn() {
  $("app").hidden = true;
  $("signin").hidden = false;
}

$("signin-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  const button = form.querySelector("button");
  const error = $("signin-error");
  error.hidden = true;
  button.disabled = true;
  try {
    await api("/api/v1/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        email: form.email.value,
        password: form.password.value,
      }),
    });
    form.password.value = "";
    await start();
  } catch (err) {
    // Whatever the server said, verbatim. It deliberately says the same thing
    // for a wrong password and an address that does not exist, and rewording
    // it here could undo that.
    error.textContent = err.message;
    error.hidden = false;
    if (!(err instanceof NotSignedIn)) {
      console.error("damga: sign-in failed after the credentials were accepted", err);
    }
  } finally {
    button.disabled = false;
  }
});

$("signout").addEventListener("click", async () => {
  try {
    await fetch("/api/v1/logout", { method: "POST", credentials: "same-origin" });
  } finally {
    location.reload();
  }
});

// ---------------------------------------------------------------- shell

async function start() {
  const me = await api("/api/v1/me");
  state.account = me.account;
  state.memberships = me.memberships;

  $("signin").hidden = true;
  $("app").hidden = false;
  $("who").textContent = me.account.displayName || me.account.email;

  const select = $("tenant");
  select.replaceChildren();
  for (const m of me.memberships) {
    const option = document.createElement("option");
    option.value = m.tenantId;
    option.textContent = `${m.tenantName} · ${m.role}`;
    select.append(option);
  }
  if (!me.memberships.length) {
    $("apps").replaceChildren();
    render(el("p", { class: "muted empty" },
      "This account is not a member of any tenant yet."));
    return;
  }
  select.value = me.memberships[0].tenantId;
  await pickTenant(select.value);
}

$("tenant").addEventListener("change", (e) => pickTenant(e.target.value));

async function pickTenant(tenantId) {
  state.tenant = tenantId;
  state.ref = null;
  const { apps } = await api(`/api/v1/tenants/${encodeURIComponent(tenantId)}/apps`);

  const nav = $("apps");
  nav.replaceChildren();
  if (!apps.length) {
    nav.append(el("p", { class: "muted" }, "Nothing has been deployed here yet."));
    render(el("p", { class: "muted empty" },
      "Once something is deployed, it appears here with the evidence for it."));
    return;
  }
  for (const app of apps) {
    const button = el("button", { type: "button" },
      el("span", {}, app.app), " ", el("span", { class: "env" }, app.env));
    button.addEventListener("click", () => pickApp(app));
    nav.append(button);
  }
  await pickApp(apps[0]);
}

async function pickApp(app) {
  state.ref = app;
  for (const button of $("apps").children) {
    const match = button.textContent === `${app.app} ${app.env}`;
    button.setAttribute("aria-current", String(match));
  }
  await showEvidence();
}

// ---------------------------------------------------------------- evidence

function base() {
  const { tenant, ref } = state;
  return `/api/v1/tenants/${encodeURIComponent(tenant)}` +
    `/apps/${encodeURIComponent(ref.app)}/envs/${encodeURIComponent(ref.env)}`;
}

async function showEvidence() {
  const prefix = base();
  render(el("p", { class: "muted empty" }, "Loading…"));

  // Fetched together rather than in sequence. They are independent reads and
  // the page is not useful until all of them are in.
  const [current, history, proof, retention] = await Promise.all([
    api(`${prefix}/evidence`).catch((err) => (err.status === 404 ? null : Promise.reject(err))),
    api(`${prefix}/history?limit=20`),
    api(`${prefix}/verify`),
    api(`${prefix}/retention`),
  ]);

  const { app, env } = state.ref;

  // What to show when nothing is running. A deploy that admission refused
  // never becomes current, so this page would otherwise say "nothing is
  // running" and stop — with the reason it is not running sitting in the very
  // next record down. On a product whose entire claim is that it can tell you
  // why, that is the wrong place to go quiet.
  const latest = history.records[0] || null;
  // Read from the state, not inferred from a boolean nobody sets.
  //
  // This was `!latest.admission.allowed`, and nothing in the product writes
  // that field — so it was false on every record ever created, and every
  // deploy that had not finished syncing yet rendered as "was refused" with a
  // banner explaining the refusal. On the page whose entire claim is that it
  // can tell you what happened, the absence of an observation was being shown
  // as the worst thing that could have happened.
  //
  // "rejected" is a state the record reaches because something moved it there.
  // A record that is merely pending has not been refused; it has not been
  // looked at, and those are different sentences.
  const blocked = !current && latest && latest.state === "rejected";
  const shown = current || (blocked ? latest : null);

  const parts = [
    el("h2", {}, `${app} · ${env}`),
    el("p", { class: "sub muted" }, current
      ? `Deploy ${current.seq}, ${current.state}, ${when(current.updatedAt)}`
      : blocked
        ? `Nothing is running. Deploy ${latest.seq} was refused, ${when(latest.createdAt)}.`
        : latest
          ? `Nothing is running. Deploy ${latest.seq} is ${latest.state}, ${when(latest.createdAt)}.`
          : "Nothing has been deployed here yet."),
  ];

  if (blocked) {
    parts.push(el("div", { class: "box banner" },
      el("h3", {}, "Refused"),
      el("p", {}, latest.admission.reason || "Admission refused this deploy."),
      latest.signature.message
        ? el("p", { class: "muted" }, latest.signature.message)
        : []));
  }

  if (shown) {
    parts.push(el("div", { class: "grid" },
      box("Image", dl([
        ["Requested", el("span", { class: "mono" }, shown.image.requestedRef || "—")],
        ["Admitted digest", el("span", { class: "mono" }, shown.image.admittedDigest || "—")],
        ["Signature", verdict(shown.signature.verified,
          shown.signature.verified ? "verified" : "not verified")],
        ["Issuer", el("span", { class: "mono" }, shown.signature.issuer || "—")],
      ])),
      box("Source", dl([
        ["Repository", shown.source.repoUrl || "—"],
        ["Commit", el("span", { class: "mono" }, shown.source.commitSha || "—")],
        ["Path", el("span", { class: "mono" }, shown.source.path || "—")],
        ["Deployed by", shown.actor.displayName || shown.actor.email || "—"],
      ])),
      box("Admission", dl([
        // Three answers and not two. Nothing records an admission outcome yet,
        // so a page with only "admitted" and "refused" has to call every deploy
        // in existence refused — which is a claim, made out of a zero value,
        // about the one thing this page exists to be trusted on.
        ["Outcome", admissionKnown(shown)
          ? verdict(shown.admission.allowed,
              shown.admission.allowed ? "admitted" : "refused")
          : el("span", { class: "muted" }, "not observed")],
        ["Reason", shown.admission.reason || "—"],
      ])),
    ));

    parts.push(el("div", { class: "box", style: "margin-top:1rem" },
      el("h3", {}, "Policies"),
      shown.policies.length
        ? table(["Policy", "Source", "Result", "Severity"],
            shown.policies.map((p) => [
              p.name, p.source, verdict(p.result === "pass", p.result), p.severity || "—",
            ]))
        : el("p", { class: "muted" }, "No policy results were recorded for this deploy.")));
  }

  parts.push(el("div", { class: "box", style: "margin-top:1rem" },
    el("h3", {}, "Chain"),
    dl([
      // Reported, never recomputed here. The server verified it; this says so.
      ["Verified", verdict(proof.valid,
        proof.valid
          ? `${proof.records} ${proof.records === 1 ? "record" : "records"} intact`
          : `broken at ${proof.brokenAt}`)],
      ["Root hash", el("span", { class: "mono" }, short(proof.rootHash))],
      ["Checked", when(proof.checkedAt)],
      ["Retention", retention.windowSeconds
        ? `${Math.round(retention.windowSeconds / 86400)} days, current deploy kept`
        : "kept indefinitely"],
    ])));

  parts.push(el("div", { class: "box", style: "margin-top:1rem" },
    el("h3", {}, "History"),
    history.records.length
      ? table(["#", "State", "Image", "Commit", "When"],
          history.records.map((r) => [
            String(r.seq), r.state, r.image.requestedRef || "—",
            el("span", { class: "mono" }, short(r.source.commitSha)), when(r.createdAt),
          ]))
      : el("p", { class: "muted" }, "No deploys recorded yet.")));

  parts.push(el("div", { class: "actions" },
    el("a", { href: `${prefix}/export`, download: "" }, "Download the full log (JSONL)"),
    el("span", { class: "muted" }, "Every record, oldest first, in the form the chain was computed over.")));

  render(...parts);
}

// ---------------------------------------------------------------- rendering

function render(...nodes) {
  $("detail").replaceChildren(...nodes);
}

// Builds elements without innerHTML, so a repository URL or a policy message
// that happens to contain markup is text and stays text.
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  node.append(...children.flat());
  return node;
}

function box(title, ...body) {
  return el("div", { class: "box" }, el("h3", {}, title), ...body);
}

function dl(rows) {
  return el("dl", {}, rows.flatMap(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]));
}

function table(headers, rows) {
  return el("table", {},
    el("thead", {}, el("tr", {}, headers.map((h) => el("th", {}, h)))),
    el("tbody", {}, rows.map((cells) => el("tr", {}, cells.map((c) => el("td", {}, c))))));
}

function verdict(ok, text) {
  return el("span", { class: `pill ${ok ? "ok" : "bad"}` }, text);
}

function short(hex) {
  if (!hex) return "—";
  return hex.length > 16 ? `${hex.slice(0, 12)}…` : hex;
}

function when(stamp) {
  if (!stamp) return "—";
  const date = new Date(stamp);
  return Number.isNaN(date.valueOf()) ? stamp : date.toLocaleString();
}

// ---------------------------------------------------------------- boot

// No cookie check first: whether the session is good is the server's answer,
// and asking is one request either way.
//
// Only a 401 becomes the sign-in form. Anything else is shown as what it is,
// because a bug that renders as "please sign in" is a bug nobody reports
// accurately — they say the login is broken, and it is not.
start().catch(fail);

function fail(err) {
  if (err instanceof NotSignedIn) {
    showSignIn();
    return;
  }
  console.error("damga:", err);
  $("signin").hidden = true;
  $("app").hidden = false;
  render(
    el("h2", {}, "This page could not be loaded"),
    el("p", { class: "error" }, String(err && err.message ? err.message : err)),
    el("p", { class: "muted" }, "The session is fine. This is a fault in the panel or the API behind it."),
  );
}
