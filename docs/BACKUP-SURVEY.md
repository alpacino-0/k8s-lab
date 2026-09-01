# Does any platform prove its backups restore?

A survey of eighteen products, read on 2026-09-01.

## Why this document exists

This repository claims that **no platform you deploy with proves its own backups
restore**. That is a strong claim about other people's software, so it is written
down here with its sources, and it is dated because it will go stale.

An earlier version of the claim said *"of eighteen platforms surveyed, none does
this."* It was wrong twice, and the correction is the reason this file exists:

1. **The number had no survey behind it.** It was borrowed from an unrelated
   count of platforms that sign container images.
2. **"None" is false.** Some products do verify restores. They are simply never
   the platform you deployed with.

Publishing the first version would have been indefensible. The corrected claim is
narrower, true, and — as it turns out — a stronger argument.

## Method

Each product's own documentation was read, not its marketing page. Three
questions, in order:

1. Does it take backups?
2. Does it verify **anything** about them?
3. Does it **restore one** and check what came back out?

Only the third separates *"we have backups"* from *"the backup works"*. A
checksum proves an archive is the archive that was written. It cannot tell you
the archive holds a schema and no rows, which is the failure that matters and the
one `gzip -t` cannot see.

## A. Platforms you deploy with

| Product | Backs up | Proves a restore | Notes |
|---|---|---|---|
| [Coolify](https://coolify.io/docs/knowledge-base/how-to/backup-restore-coolify) | Yes — `pg_dump` on a cron | **No** | Documentation asks the *user* to schedule restore tests |
| [Dokploy](https://docs.dokploy.com/docs/core/backups) | Yes — S3 on a cron | **No** | *"Test the restore process at least once"* — the user's job |
| [CapRover](https://caprover.com/docs/backup-and-restore.html) | Config only | **No** | Persistent directories and databases are **not in the backup at all** |
| [Cloudron](https://docs.cloudron.io/backups/) | Yes | **No** — closest | "Check integrity" validates the signature, checksums and sizes. Restore is a manual dry run. It verifies the *archive*, not the *restore* |
| [Northflank](https://northflank.com/docs/v1/application/databases-and-persistence/backup-restore-and-import-data) | Yes | **No** | Create, import, restore — no verification step |
| [Heroku](https://devcenter.heroku.com/articles/remora-backup) | Yes | **No** | Stated in the first person: *"Remora Backup does not test the decryption or restoration of your backup files, and you are responsible for ensuring a backup file can be successfully downloaded, decrypted, and restored."* |
| [Fly.io](https://fly.io/docs/postgres/managing/backup-and-restore/) | Yes — volume snapshots | **No** | Restoring and verifying is the user's procedure |
| [Neon](https://neon.com/docs/manage/backups) | Yes | **No** | |
| [PlanetScale](https://planetscale.com/docs/postgres/imports/neon) | Yes | **No** | |

## B. Database operators and backup tools

This is where the claim is most likely to be attacked, so it was checked hardest.

| Tool | Proves a restore | What it actually does |
|---|---|---|
| [pgBackRest `verify`](https://pgbackrest.org/command.html) | **No** | Validates **repository** checksums against the manifest. The documentation is explicit that it does not restore |
| [Barman `verify-backup`](https://manpages.debian.org/unstable/barman-cli/barman-verify-backup.1.en.html) | **No** — closest | Runs `pg_verifybackup` against the backup manifest. Verifies the backup *files*; nothing is restored into a running database and nothing is counted |
| [CloudNativePG](https://cloudnative-pg.io/documentation/1.20/recovery/) | **No** | Recommends testing restores regularly, as a practice |
| [Crunchy PGO](https://access.crunchydata.com/documentation/postgres-operator/4.7.3/) | **No** | Documented plainly: *"The restore workflow does not perform a backup after the restore nor does it verify…"* |
| [Velero](https://velero.io/docs/v1.8/manual-testing/) | **No** | Verification is a pattern users build themselves — a CronJob that runs a restore into a scratch namespace — not a feature of the tool |

## C. Products that *do* verify restores

| Product | What it does | Category |
|---|---|---|
| [Veeam SureBackup](https://helpcenter.veeam.com/docs/vbr/userguide/surebackup_tests.html) | Boots the backup in an isolated virtual lab, runs heartbeat, ping and application-level tests, and produces a report | Enterprise **VM backup appliance**. Not a developer platform |
| [BackProve](https://backprove.com/) | *"restore-tests every backup"* | Third-party service **bolted onto** Supabase |
| [ReviveDB](https://revivedb.dev/) | *"every database backup is restore-tested"* | Same |
| [railway-postgres-backups](https://github.com/Kjudeh/railway-postgres-backups) | *"daily restore verification"* | A script somebody **had to write** because Railway does not |

**Table C is the evidence, not the counter-evidence.** A practice that is both
mature — Veeam has been booting and verifying backups for years — and has grown a
market of bolt-on services is a practice the underlying platforms do not perform.
BackProve can only exist if Supabase does not do this. The Railway script can only
exist if Railway does not do this.

## What this survey supports, and what it does not

| Not supported | Supported |
|---|---|
| *"Eighteen platforms surveyed, none does this"* | *"No platform you deploy with proves its own backups restore"* |
| *"We invented this"* | *"The practice is mature and proven; it is absent from developer platforms"* |
| *"Nobody verifies anything"* | *"The closest are Cloudron's checksum check and Barman's manifest verification. Neither restores anything"* |
| *"This cannot be done"* | *"Table C shows it can, which is why its absence is a choice"* |

## How to challenge this

If you maintain one of these products and a row is wrong, open an issue with a
link to the documentation and the row changes. The claim is about what platforms
do **today**, not about what is possible — table C already settles that.

The claim will also be extended only by adding a row, with its source. It will not
be widened by adjective.
