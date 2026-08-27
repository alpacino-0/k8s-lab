-- Forge: which source repository each of a tenant's apps builds and signs from.
--
-- Same portable subset as the other three schemas, and the same arrangement:
-- one table for the rows, one table that exists purely to turn an invariant
-- into a constraint.

CREATE TABLE forge_connection (
  tenant_id        TEXT NOT NULL,
  app              TEXT NOT NULL,

  host             TEXT NOT NULL,
  owner            TEXT NOT NULL,
  repo             TEXT NOT NULL,
  branch           TEXT NOT NULL,
  -- Under .github/workflows, checked in Go so the message can say which rule
  -- was broken.
  workflow_path    TEXT NOT NULL,
  image_repository TEXT NOT NULL,

  -- The certificate subject the tenant's workflow will present, derived from
  -- the five columns above and stored anyway.
  --
  -- Denormalisation on purpose. It is the key forge_identity joins on, and the
  -- alternative is concatenating those five parts in SQL — a second copy of
  -- Connection.Identity in another language, whose divergence is silent: change
  -- the format in Go and the release query stops matching, so every claim ever
  -- made leaks and the next tenant is refused on behalf of a connection that no
  -- longer exists.
  identity         TEXT NOT NULL,

  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,

  -- One source repository per app. Not (tenant, app, env): an app has one
  -- repository and one signing identity, and deploys to several environments
  -- out of it.
  PRIMARY KEY (tenant_id, app)
) STRICT;

CREATE INDEX forge_connection_by_identity ON forge_connection (identity);

-- One certificate subject never belongs to two tenants.
--
-- Two tenants connecting the same repository and branch render the same
-- subject, and a policy cannot tell which tenant's build produced a signature
-- carrying it — each would accept the other's images. A PRIMARY KEY is what
-- makes that impossible on both engines; a SELECT before an INSERT is not,
-- because PostgreSQL under READ COMMITTED gives no protection and SQLite gives
-- it only by accident of its single write lock.
--
-- The row is deleted when the last connection holding it is, or an app that
-- moves repositories leaves an identity nobody can ever claim again.
CREATE TABLE forge_identity (
  identity    TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claimed_at  TEXT NOT NULL
) STRICT;
