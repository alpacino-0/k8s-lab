-- Identity: who may act, in which tenant, and how they proved it.
--
-- Same portable subset as evidence/*/migrations/0001_init.sql, plus two rules
-- this schema found the hard way:
--
--   * No table or column named "user" or "group". Both are reserved words in
--     PostgreSQL. Unquoted DDL using them PARSES ON SQLITE and FAILS ON
--     POSTGRESQL, which is the worst split available to this product: SQLite is
--     what every install starts on, and PostgreSQL is what a larger one wants,
--     so it would migrate cleanly on every small install and fail at CREATE
--     TABLE on the first large one. Hence "account", and hence a role
--     column plus a team table rather than anything called a group.
--
--   * NULL is never "absent". Both engines treat NULLs as distinct inside a
--     UNIQUE, so a nullable column in a unique key is a constraint that has
--     quietly stopped constraining. Absence is the empty string, as in the
--     evidence schema.
--
-- Nothing here has a foreign key into the evidence schema and nothing here is
-- ever joined to it. That is what the copied actor name and email on an
-- evidence record buy: the two stores can live in different databases, and they
-- can only do that if nothing ever joined across them.
--
-- This sequence records itself in identity_schema_migration, not in the
-- evidence store's schema_migration. Sharing one table means one version read
-- with MAX(version), and a second sequence starting at 1 would be silently
-- skipped — see internal/sqlmigrate.

CREATE TABLE tenant (
  -- Opaque and permanent. evidence.record.tenant_id holds this value and the
  -- evidence store builds record ids out of it, so a renameable tenant id would
  -- orphan every record ever written about it. The slug is what changes.
  id            TEXT PRIMARY KEY,
  slug          TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,

  suspended     INTEGER NOT NULL CHECK (suspended IN (0, 1)),
  created_at    TEXT NOT NULL
) STRICT;

CREATE TABLE account (
  -- Never reused and never deleted. evidence.record.actor_id points here across
  -- a boundary that deliberately has no foreign key, so recycling an id
  -- silently reattributes somebody else's history.
  id            TEXT PRIMARY KEY,
  kind          TEXT NOT NULL CHECK (kind IN ('user', 'automation')),

  -- The login and contact address. Erasable.
  email         TEXT NOT NULL,
  -- Folded for lookup, so Orhan@example.test and orhan@example.test are one
  -- account rather than two. A separate column rather than an expression index,
  -- because the two engines spell that differently and this schema is written
  -- in what they share.
  email_folded  TEXT NOT NULL UNIQUE,

  -- What is written into git commits and copied into evidence records.
  -- Separate on purpose: those copies live inside a hash chain and inside
  -- history this platform does not own, so they can never be redacted. An
  -- instance-local alias here is what makes an erasure request answerable —
  -- what was published was never personal data.
  audit_email   TEXT NOT NULL,

  display_name  TEXT NOT NULL,
  disabled      INTEGER NOT NULL CHECK (disabled IN (0, 1)),
  created_at    TEXT NOT NULL
) STRICT;

-- One row per account, and only for local passwords. A federated account has no
-- row here, which is the honest representation of "this person's password lives
-- at their identity provider" rather than an empty hash something might compare
-- against.
CREATE TABLE credential (
  account_id    TEXT PRIMARY KEY REFERENCES account(id) ON DELETE CASCADE,
  -- The encoded hash, algorithm and parameters included, so the parameters can
  -- be raised later without a flag day.
  hash          TEXT NOT NULL,
  updated_at    TEXT NOT NULL
) STRICT;

-- What an account may do inside one tenant, and the ONLY source of that.
-- A subject assembled from anything the caller sent would let a stranger read
-- another tenant's deploy history, because the free authorizer treats an
-- unrecognised group as viewer and a viewer may read the evidence page.
CREATE TABLE membership (
  account_id    TEXT NOT NULL REFERENCES account(id) ON DELETE CASCADE,
  tenant_id     TEXT NOT NULL REFERENCES tenant(id)  ON DELETE CASCADE,
  role          TEXT NOT NULL CHECK (role IN ('owner', 'member', 'viewer')),
  created_at    TEXT NOT NULL,
  PRIMARY KEY (account_id, tenant_id)
) STRICT;

CREATE INDEX membership_by_tenant ON membership (tenant_id);

CREATE TABLE session (
  -- sha256 of the token, lowercase hex. The token itself is never stored, which
  -- is what makes a leaked dump useless for impersonation.
  digest        TEXT PRIMARY KEY,
  account_id    TEXT NOT NULL REFERENCES account(id) ON DELETE CASCADE,

  -- The Host this session was issued for, checked on every use. This is the
  -- anti-injection property the __Host- cookie prefix would have given,
  -- obtained a way that still works on http://localhost — where Chrome and
  -- Safari reject that prefix outright, and where this project's own documented
  -- first run lives.
  issued_for    TEXT NOT NULL,

  created_at    TEXT NOT NULL,
  expires_at    TEXT NOT NULL
) STRICT;

-- Deprovisioning terminates every session an account holds, now. That is the
-- whole promise SCIM makes, so the lookup it needs is indexed rather than a
-- scan.
CREATE INDEX session_by_account ON session (account_id);
-- The prune sweep walks by expiry.
CREATE INDEX session_by_expiry ON session (expires_at);
