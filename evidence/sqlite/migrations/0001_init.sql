-- The evidence record, persisted.
--
-- Written in the subset of SQL that is correct on both SQLite and PostgreSQL,
-- because the second engine is not hypothetical: SQLite has no roles and no
-- REVOKE, so it can support "we do not modify evidence" but never "we cannot",
-- and the paid archive is sold on the second sentence. The rules that follow
-- from that, and that are cheap now and expensive later:
--
--   * TEXT ids and lowercase-hex digests, never a uuid or bytea column type.
--   * TEXT + CHECK instead of ENUM.
--   * Timestamps as fixed-width RFC3339 UTC TEXT, so lexicographic order is
--     chronological order on both engines and matches the form the hash chain
--     already hashes.
--   * No ALTER COLUMN TYPE, ever. SQLite cannot parse it; every type change
--     becomes create-copy-drop-rename.
--   * No SELECT ... FOR UPDATE. SQLite cannot parse it, and BEGIN IMMEDIATE
--     already serialises writers there.
--
-- STRICT is SQLite-only and is the reason a column typed TEXT actually holds
-- text rather than whatever was handed to it.

CREATE TABLE record (
  id                 TEXT PRIMARY KEY,
  -- What makes Append retry-safe and replay-safe. The git writer uses
  -- "commit:<sha>:<path>"; an observer uses "argocd:<appUID>:<historyID>".
  idempotency_key    TEXT NOT NULL UNIQUE,

  -- Ref. The tenant is a column and never a path segment, so the layout of the
  -- tenant repositories can change without rewriting a single record.
  tenant_id          TEXT NOT NULL,
  app                TEXT NOT NULL,
  env                TEXT NOT NULL,
  seq                INTEGER NOT NULL,

  tier               TEXT NOT NULL CHECK (tier IN ('free', 'enterprise')),

  -- Actor. DisplayName and Email are copied rather than joined: an audit
  -- record has to stay readable after the account is gone.
  actor_id           TEXT NOT NULL,
  actor_kind         TEXT NOT NULL,
  actor_name         TEXT NOT NULL,
  actor_email        TEXT NOT NULL,

  -- Source. author is the human, committer is the platform; the split is what
  -- keeps a name on the evidence page instead of "the platform did it".
  repo_url           TEXT NOT NULL,
  git_ref            TEXT NOT NULL,
  repo_path          TEXT NOT NULL,
  commit_sha         TEXT NOT NULL,
  author_email       TEXT NOT NULL,
  committer_email    TEXT NOT NULL,

  -- Image. requested is what git said, admitted is what ran after Kyverno's
  -- mutateDigest rewrote the reference. Both are kept because they disagree
  -- exactly when something interesting happened.
  image_requested    TEXT NOT NULL,
  image_admitted     TEXT NOT NULL,

  -- The supply-chain answer at admission time, frozen. Re-running the check
  -- later answers a different question: a policy can be edited and a signature
  -- cannot be revoked.
  sig_verified       INTEGER NOT NULL CHECK (sig_verified IN (0, 1)),
  sig_issuer         TEXT NOT NULL,
  sig_subject        TEXT NOT NULL,
  sig_digest         TEXT NOT NULL,
  sig_message        TEXT NOT NULL,

  -- JSON. Deliberately opaque to the database: these are quoted back verbatim
  -- and never filtered on, and a schema for them would have to track two
  -- policy engines that do not agree about what a result is.
  policies           TEXT NOT NULL,

  adm_allowed        INTEGER NOT NULL CHECK (adm_allowed IN (0, 1)),
  adm_reason         TEXT NOT NULL,
  adm_message        TEXT NOT NULL,

  note               TEXT NOT NULL,

  state              TEXT NOT NULL,
  -- What the record was appended as. This one is chained; state is not,
  -- because it changes.
  initial_state      TEXT NOT NULL,
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,

  prev_hash          TEXT NOT NULL,
  hash               TEXT NOT NULL,

  -- Seq is per Ref and gapless: "the 41st deploy of api/prod" has to mean
  -- that, and a busy staging must not renumber production.
  UNIQUE (tenant_id, app, env, seq)
) STRICT;

-- The evidence page reads one Ref at a time, newest first.
CREATE INDEX record_by_ref ON record (tenant_id, app, env, seq DESC);
-- The observer arrives knowing a revision and nothing else.
CREATE INDEX record_by_commit ON record (repo_url, commit_sha);
-- The retention sweep walks by age.
CREATE INDEX record_by_age ON record (tenant_id, created_at);

-- One state change. Append-only: the record's state is a projection of these,
-- which is what lets the log be immutable while the page still reads one row.
--
-- ON DELETE CASCADE here is correct and is not the defect measured in
-- PolicyReport and in Dokploy's deployments table. Those cascade evidence off
-- the *application*, so deleting an app silently erases its history. This
-- cascades events off the record they are events of, and the record is removed
-- only by a retention sweep that is separately forbidden from touching a live
-- one.
CREATE TABLE record_event (
  record_id      TEXT NOT NULL REFERENCES record(id) ON DELETE CASCADE,
  seq            INTEGER NOT NULL,
  from_state     TEXT NOT NULL,
  to_state       TEXT NOT NULL,
  at             TEXT NOT NULL,
  reason         TEXT NOT NULL,

  obs_source     TEXT NOT NULL,
  obs_app_uid    TEXT NOT NULL,
  -- Nullable on purpose: Argo CD appends to .status.history only on a
  -- successful, non-dry-run, non-selective sync, so the absence of a history
  -- id is information rather than a missing value.
  obs_history_id INTEGER,
  obs_revision   TEXT NOT NULL,
  obs_phase      TEXT NOT NULL,
  obs_at         TEXT NOT NULL,

  prev_hash      TEXT NOT NULL,
  hash           TEXT NOT NULL,

  PRIMARY KEY (record_id, seq)
) STRICT;
