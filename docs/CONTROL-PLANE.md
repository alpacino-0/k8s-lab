# Running the control plane

`cmd/damga` is the control plane: the panel, the API, the deploy write path and
the deploy history behind them. It is not the operator (`cmd/operator`, which
reconciles the `Workload`, `Database` and `Build` types) and it is not the
sample service in this repository.

It runs two ways. On its own it needs no cluster to start and keeps its data in
one database — which is what this document walks through, because it is the
fastest way to see it work. In a cluster it runs from `cluster/control-plane.yaml`,
which is what `make up` installs and what an actual install uses; that manifest
carries the ServiceAccount, and the identity it grants is deliberately lopsided
(cluster-wide reads, one namespace it may create a `Build` in, and no delete
anywhere).

This document is what a first run actually looks like, including the parts that
go wrong.

## First run, on a laptop

```bash
go build -o damga ./cmd/damga

# One account and one tenant. Runs once per install and refuses to run again.
./damga bootstrap -evidence-dsn ./damga.db -email you@example.com -tenant acme

./damga -evidence-dsn ./damga.db -listen-address 127.0.0.1:8080
```

`bootstrap` prints a generated password to the terminal once. It is stored only
as an argon2id hash, so there is no way to recover it — copy it before the
terminal scrolls. To choose your own instead:

```bash
printf 'a password you have chosen' | ./damga bootstrap \
  -evidence-dsn ./damga.db -email you@example.com -tenant acme -password-stdin
```

There is no `-password` flag on purpose. A password in `argv` is in the shell
history, in the process table for as long as the command runs, and in the audit
log of anything that records process execution — including `kubectl exec`.

Then open <http://127.0.0.1:8080> and sign in.

## Why bootstrap is a subcommand and not a page

There is no "create the first account" screen, and no setup token printed at
startup. Each of the alternatives gives something away:

- A first-run window, where whoever arrives first becomes the owner, is a race
  against the internet. Between the moment the service is reachable and the
  moment you open a browser there is a period — however short — in which the
  platform belongs to whoever finds it.
- A one-time token printed by the running server lands in the pod's stdout.
  This repository's own log pipeline ships all pod stdout to Loki unfiltered, so
  the token would be readable by everyone with log access — a wider group than
  everyone with install authority — for the whole retention period.
- A token written to a Secret needs the control plane to hold create permission
  on Secrets in its own namespace, permanently, in order to use it once.

A subcommand asks for an authority you already had to have to install anything:
reaching the database. In a cluster, `kubectl exec` streams through the CRI exec
channel, which is not the container log a collector tails, so the password is
shown to the person who ran the command and to nobody else.

```bash
kubectl -n damga exec -it deploy/damga -- \
  damga bootstrap -evidence-dsn "$DAMGA_DSN" -email you@example.com -tenant acme
```

Running it a second time exits with status 3 and changes nothing, so a
deployment script can call it unconditionally.

## The flags that matter

| Flag | Default | What it decides |
| --- | --- | --- |
| `-evidence-dsn` | *(empty)* | `postgres://…` for PostgreSQL, anything else is a SQLite path. **Empty keeps everything in memory and loses it on restart.** |
| `-listen-address` | `:8080` | Where the panel and the API listen. |
| `-secure-cookies` | `false` | Sets `Secure` on the session cookie. Turn it on behind TLS. |
| `-session-ttl` | `12h` | Absolute, not sliding. |
| `-retention-window` | `0` | How long non-current evidence is kept. `0` keeps it for ever. |
| `-observe-deploys` | `false` | Watch the cluster and close the evidence records the git write path opened. Needs a cluster. |
| `-leader-elect` | `false` | Run the observer and the sweep on one replica only. Required if you run more than one. |
| `-pending-timeout` | `30m` | How long an unobserved record may stay pending before it is marked unknown. Must exceed the cluster's progress deadline. |
| `-shutdown-timeout` | `15s` | How long in-flight requests get on SIGTERM. |

Two of these are easy to get wrong:

**`-secure-cookies` on plain HTTP loses the login silently.** The browser
accepts the response, discards the cookie, and the next request is anonymous —
with nothing in the logs to say so. The session cookie deliberately does not use
the `__Host-` prefix, which Chrome and Safari reject outright on
`http://localhost`; the session is bound to the host it was issued for instead,
which gives the same protection and survives the move to TLS without renaming
the cookie.

**An empty `-evidence-dsn` is a demo, not an installation.** The server says so
at startup. `bootstrap` refuses to run without one at all, because it would
report an owner into a database that stops existing when the command returns.

## PostgreSQL

```bash
./damga -evidence-dsn 'postgres://damga:…@db:5432/damga?sslmode=require'
```

Both engines are supported from day one and chosen by the DSN scheme. SQLite is
what a single node runs; PostgreSQL is what an install wants once it needs more
than one, and it is also the only one that can back the stronger sentence —
SQLite has no roles and no `REVOKE`, so "we do not modify the history" is a
promise it can make and "we cannot" is not. Migrations are applied at startup by
the binary itself.

## What is on the API

Everything the panel shows, it gets from these. The CLI and the panel use the
same endpoints; there is no private API.

| Endpoint | What it answers |
| --- | --- |
| `POST /api/v1/login`, `POST /api/v1/logout` | A session cookie. |
| `GET /api/v1/me` | Who you are and which tenants you belong to. |
| `GET /api/v1/tenants/{tenant}/apps` | Every app, from both stores, saying which one knew: deployed, never deployed, or record removed. |
| `POST …/apps` | Registers an app and its environments. Until this existed nothing wrote to the placement store at all. |
| `DELETE …/apps/{app}` | Removes the registration. Not the data, not the history. |
| `POST …/apps/{app}/builds` | Asks for one commit to be built into one image. |
| `…/apps/{app}/envs/{env}/evidence` | What is running now. |
| `…/history` | The deploy log, newest first, keyset-paged. |
| `…/verify` | Recomputes the hash chain and reports whether it holds. |
| `…/retention` | What the store promises to keep. |
| `…/backup` | The app's database, and when its backup was last restored. |
| `…/export` | Every record, oldest first, as JSONL. |
| `POST …/deploys` | Writes a commit. Argo CD applies it; this endpoint touches no cluster. |

Four of these write, and only one of them writes to the cluster. `deploys` and
the two `apps` endpoints write to git and to the control plane's own database;
`builds` is the single exception, because a build is an action rather than a
desired state and has to happen before there is a digest to commit. That
exception is why the ServiceAccount in `cluster/control-plane.yaml` can create a
`Build` in one namespace and nothing else anywhere.

Every endpoint under `/tenants/{tenant}` resolves the caller from the session
cookie and the membership row, and from nothing the request carries. A tenant
you are not a member of answers `403` with the same message as a tenant that
does not exist, so the endpoint cannot be used to find out which tenants exist.

`export` returns the store's own encoding rather than the panel's, because an
export exists to be checked later: it has to carry the form the hash chain was
computed over. A truncated download fails to verify at the point it was cut.

## Verifying the chain

Each record is hashed over the previous one, per application and environment.
`/verify` recomputes the whole range and reports where it breaks, if anywhere.
The panel prints the result; it does not compute it, so the panel and a script
cannot reach different conclusions about the same deploy.

This is tamper-**evident**, not tamper-proof. You are your own cluster
administrator, and anyone who can write to the database can rewrite a record —
what they cannot do is rewrite it without the chain from that point on ceasing
to verify. On PostgreSQL, connecting the application as a role without `UPDATE`
or `DELETE` is what turns that into something stronger, and that is a deployment
decision this binary cannot make or observe. It reports `immutable: false`
either way rather than claiming otherwise.

## What is not here yet

- **The build endpoint answers `501` on an install with no cluster.** It is a
  seam, and a control plane started without one has nothing to fill it with.
  Said plainly rather than answered with an empty success, which would read as
  a build that was started.
- **No account management in the panel.** After `bootstrap` there is no screen
  for inviting anyone.
- **No move.** An app is registered against one repository and branch; changing
  them means deleting the registration and making it again.
- **CSV export.** Declared as a format and deliberately not implemented —
  flattening the transitions loses exactly what makes an export re-verifiable.
  Asking for it returns `400` rather than quietly handing back JSONL.

## Shutting down

SIGTERM drains in-flight requests within `-shutdown-timeout` and exits `0`. A
control plane that returns an error on SIGTERM makes every rollout look failed,
so this is treated as part of the contract and tested as one.
