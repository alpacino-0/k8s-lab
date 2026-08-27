-- Placement: which repository, branch and path each of a tenant's environments
-- is written to.
--
-- Same portable subset as the other two schemas, and one table that exists
-- purely to turn an invariant into a constraint.

CREATE TABLE placement (
  tenant_id   TEXT NOT NULL,
  app         TEXT NOT NULL,
  env         TEXT NOT NULL,

  repo_url    TEXT NOT NULL,
  branch      TEXT NOT NULL,
  -- Relative to the repository root, never with a leading slash and never
  -- containing "..". Checked in Go rather than here because the message has to
  -- explain which of those it was.
  path        TEXT NOT NULL,

  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,

  PRIMARY KEY (tenant_id, app, env)
);

-- The list the panel builds, and the "is this repository still in use" check
-- that a delete makes.
CREATE INDEX placement_by_repo ON placement (repo_url);

-- One commit never touches two tenants.
--
-- This could be a SELECT before the INSERT instead, and that is what it was
-- first. Two tenants onboarding at the same moment, both pointed at the same
-- repository by a copy-pasted value, each read "unclaimed" and each wrote —
-- PostgreSQL under READ COMMITTED gives no protection, and SQLite only gives
-- it by accident of its single write lock. A primary key gives it on both,
-- always, and the check becomes "insert, then read back who is actually
-- there".
--
-- The row is deleted when the last placement pointing at the repository is,
-- or a tenant that moves repositories leaves one nobody can ever claim again.
CREATE TABLE repo_owner (
  repo_url    TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claimed_at  TEXT NOT NULL
);
