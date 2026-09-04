// One click, and a Postgres or a Redis exists.
//
// The engine was here the whole time: the Database CRD carries both, with
// backups and a restore rehearsal Coolify does not have. What was missing was
// a way to ask for one that is not kubectl. docs/PLAN.md §7 counted it as the
// highest-return item for that reason.
//
// Nothing here runs on load. app.js calls mountDatabases; the guards at the
// bottom are the ones every view in this directory carries.

function databasesUrl(base) {
  return `${base}/databases`;
}

// The credentials row, and it is the settings page's secret row wearing a
// different hat.
//
// A database password is in exactly the class that decision was about: the
// operator mints it into a Secret the tenant's pods read, git carries the name
// of that Secret and never the value. So it gets the same sentence, for the
// same two reasons — there is no rollback to a value git never held, and a
// Secret deleted by hand is restored by nothing, unlike a manifest that Argo CD
// puts back in about ten seconds.
//
// The endpoint cannot send the password even by accident: its response shape
// has no field for one. That is worth saying on the screen rather than only in
// a comment, because "not shown" and "not there" look identical to a reader
// and only one of them is a promise.
const CREDENTIALS_NOTE =
  "the username and password are generated into a Secret your application reads. " +
  "They are not in git and this page never sees them: there is no rollback to a " +
  "value git never held, and if the Secret is deleted nothing puts it back";

function credentialsView(db) {
  return {
    state: "secret",
    shown: false,
    secretName: (db && db.secretName) || "",
    text: db && db.secretName
      ? `in the Secret ${db.secretName}`
      : "in a Secret this page does not read",
    warning: CREDENTIALS_NOTE,
    inGit: false,
  };
}

// What one database costs, said as a fact rather than as a prediction.
//
// The endpoint explains at length why it cannot forecast the refusal — it may
// not read persistent volume claims or quotas, a namespace may hold several
// apps whose manifests live in other directories, and the quota is applied when
// Argo CD syncs rather than during the request. So the page says the same thing
// the endpoint says: what this takes, what the ceiling is, and that the ceiling
// is shared.
//
// Three screens, because "you have room", "this is the last one" and "this will
// not fit" are three different things to be told and the middle one is the one
// worth seeing before pressing the button.
function claimsView(claims, wanted = 1) {
  const budget = Number((claims && claims.budget) || 0);
  const used = Number((claims && claims.usedByTheseHere) || 0);
  const left = budget - used;
  if (budget === 0) {
    return { state: "unknown", text: "", ok: null, left: null };
  }
  if (wanted > left) {
    return {
      state: "over",
      text: `this would take ${wanted} of the ${left} volume ${left === 1 ? "claim" : "claims"} ` +
        `left here, and the namespace allows ${budget} in total. The commit will be made ` +
        "and the cluster will refuse it when Argo CD syncs",
      ok: false,
      left,
    };
  }
  if (wanted === left) {
    return {
      state: "last",
      text: `this takes the last of the ${budget} volume claims this namespace allows`,
      ok: null,
      left,
    };
  }
  return {
    state: "room",
    text: `this takes ${wanted} of ${budget} volume claims; ${left} are free here`,
    ok: true,
    left,
  };
}

// Backups are two claims, not one, and the page says so before the choice is
// made rather than after the quota refuses.
function claimsWanted(form) {
  return form && form.backups && form.engine !== "redis" ? 2 : 1;
}

// Redis has no backups here, and the reason is not a limitation to apologise
// for: the schedule would run pg_dump against a server that has never heard of
// it. The API refuses the combination, so the form does not offer it.
function backupsOffered(engine) {
  return engine !== "redis";
}

// What a refusal says, in the server's words.
function refusalText(status, body) {
  const detail = body && typeof body.detail === "string" ? body.detail : "";
  if (detail) return detail;
  switch (status) {
    case 403:
      return "removing a database is owner-only, and this account is not an owner";
    case 404:
      return "this app and environment have no repository configured yet";
    default:
      return `the request was refused with ${status} and no detail`;
  }
}

// What a removal says afterwards.
//
// The endpoint sends the sentence, because every clause in it was measured
// against the operator: which objects are owned and go, which volumes stay, and
// that the claims stay spent. Printing it rather than keeping a second copy is
// the rule the catalogue screen set.
//
// A removal that said only "removed" would be the failure this whole response
// exists to prevent: somebody deleting a database to make room, and getting
// none.
function removedText(body) {
  const note = body && typeof body.note === "string" ? body.note : "";
  return note || "the database was withdrawn. Its volumes are not deleted by this.";
}

// mountDatabases renders the view into el and returns a stop function.
function mountDatabases(el, base, options = {}) {
  const { fetcher = globalThis.fetch } = options;
  const doc = el.ownerDocument;
  let stopped = false;
  let claims = null;

  const node = (tag, attrs = {}, ...kids) => {
    const n = doc.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
    n.append(...kids.flat());
    return n;
  };

  const status = doc.createElement("p");
  status.className = "muted";
  const list = doc.createElement("div");
  list.className = "db-list";
  const form = doc.createElement("form");
  form.className = "db-form";
  const errors = doc.createElement("ul");
  errors.className = "refusal";
  errors.hidden = true;
  el.replaceChildren(status, list, form, errors);

  const showRefusal = (text) => {
    errors.replaceChildren(...(text ? [node("li", {}, text)] : []));
    errors.hidden = !text;
  };

  const field = (name, label, value, placeholder = "") => {
    const input = doc.createElement("input");
    input.type = "text";
    input.name = name;
    input.value = value;
    input.placeholder = placeholder;
    input.setAttribute("aria-label", label);
    input.setAttribute("autocomplete", "off");
    return input;
  };

  const engine = doc.createElement("select");
  engine.setAttribute("aria-label", "Engine");
  for (const value of ["postgres", "redis"]) {
    const option = doc.createElement("option");
    option.value = value;
    option.textContent = value;
    engine.append(option);
  }
  const name = field("name", "Name", "", "main");
  const image = field("image", "Image", "postgres:17.2");
  const storage = field("storage", "Storage", "5Gi");
  const backups = doc.createElement("input");
  backups.type = "checkbox";
  backups.setAttribute("aria-label", "Take backups");
  const backupStorage = field("backupStorage", "Backup volume", "2Gi");
  const cost = doc.createElement("p");
  cost.className = "db-cost";
  const create = doc.createElement("button");
  create.type = "submit";
  create.textContent = "Create";

  const backupsLabel = node("label", {}, backups, "backups, with a restore rehearsal");
  form.replaceChildren(engine, name, image, storage, backupsLabel, backupStorage, create, cost);

  const paintCost = () => {
    // Redis is not offered backups, rather than offered them and refused.
    const offered = backupsOffered(engine.value);
    backupsLabel.hidden = !offered;
    backupStorage.hidden = !offered || !backups.checked;
    const view = claimsView(claims, claimsWanted({ engine: engine.value, backups: backups.checked }));
    cost.textContent = view.text;
    cost.dataset.ok = String(view.ok);
    cost.dataset.state = view.state;
  };
  engine.addEventListener("change", paintCost);
  backups.addEventListener("change", paintCost);

  const draw = (databases) => {
    list.replaceChildren(...databases.map((db) => {
      const creds = credentialsView(db);
      const row = node("div", { class: "db-row", "data-engine": db.engine });
      const remove = doc.createElement("button");
      remove.type = "button";
      remove.className = "link";
      remove.textContent = "Remove";
      remove.addEventListener("click", () => void withdraw(db.name));

      row.append(
        node("strong", {}, db.name),
        node("span", { class: "muted" }, `${db.engine} · ${db.image} · ${db.storage}`),
        node("span", { class: "muted" },
          `${db.claims} volume ${db.claims === 1 ? "claim" : "claims"}`),
        remove,
        node("p", { class: "db-credentials", "data-state": creds.state },
          `${creds.text} — ${creds.warning}`),
      );
      if (db.backups) {
        row.append(node("p", { class: "muted" },
          `backups ${db.backups.schedule}, kept ${db.backups.retainDays} days` +
          (db.backups.rehearse ? ", restored and counted to prove they work" : "")));
      }
      return row;
    }));
  };

  async function load() {
    status.textContent = "loading…";
    status.dataset.ok = "null";
    try {
      const response = await fetcher(databasesUrl(base), { method: "GET" });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        showRefusal(refusalText(response.status, body));
        status.textContent = "these databases could not be read";
        status.dataset.ok = "false";
        return;
      }
      const databases = (body && body.databases) || [];
      claims = (body && body.claims) || null;
      status.textContent = databases.length
        ? `${databases.length} ${databases.length === 1 ? "database" : "databases"}`
        : "no databases yet";
      status.dataset.ok = "true";
      draw(databases);
      paintCost();
    } catch (err) {
      showRefusal(String((err && err.message) || err));
      status.textContent = "these databases could not be read";
      status.dataset.ok = "false";
    }
  }

  async function withdraw(dbName) {
    if (stopped) return;
    showRefusal("");
    try {
      const response = await fetcher(`${databasesUrl(base)}/${encodeURIComponent(dbName)}`,
        { method: "DELETE" });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        showRefusal(refusalText(response.status, body));
        return;
      }
      // The server's sentence about what became of the data, kept on screen
      // rather than flashed: it is the answer to a question the reader has not
      // asked yet.
      status.textContent = removedText(body);
      status.dataset.ok = "null";
      await load();
    } catch (err) {
      showRefusal(String((err && err.message) || err));
    }
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (stopped) return;
    create.disabled = true;
    showRefusal("");
    try {
      const wantBackups = backupsOffered(engine.value) && backups.checked;
      const response = await fetcher(databasesUrl(base), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: name.value, engine: engine.value, image: image.value,
          storage: storage.value,
          ...(wantBackups ? { backups: { storage: backupStorage.value, rehearse: true } } : {}),
        }),
      });
      const body = await response.json().catch(() => null);
      if (!response.ok) {
        showRefusal(refusalText(response.status, body));
        return;
      }
      name.value = "";
      await load();
    } catch (err) {
      showRefusal(String((err && err.message) || err));
    } finally {
      create.disabled = false;
    }
  });

  load();

  return function stop() {
    stopped = true;
  };
}

// The page's entry point:
//
//   index.html:  <script src="/databases.js"></script>   before app.js
//   app.js:      window.damgaDatabases.mountDatabases(box, base(), {});
if (typeof window !== "undefined") {
  window.damgaDatabases = {
    mountDatabases, databasesUrl, credentialsView, claimsView, claimsWanted,
    backupsOffered, refusalText, removedText,
  };
}

if (typeof module !== "undefined") {
  module.exports = {
    mountDatabases, databasesUrl, credentialsView, claimsView, claimsWanted,
    backupsOffered, refusalText, removedText, CREDENTIALS_NOTE,
  };
}
