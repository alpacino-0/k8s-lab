-- One namespace never holds two tenants.
--
-- The repository claim in 0001 protects the write path: a commit that would
-- touch two tenants is refused before a worktree is opened. This one protects
-- the other end. A namespace is where the rendered manifest actually runs, and
-- it is what the ResourceQuota, the Pod Security Admission level and the
-- NetworkPolicy are attached to — so a tenant that could name a namespace
-- another tenant is already using would be deploying inside somebody else's
-- fence, with a perfectly well-formed request.
--
-- A table with a primary key rather than a SELECT before the INSERT, for the
-- reason 0001 wrote down at length and measured: PostgreSQL under READ
-- COMMITTED gives a read-then-write claim no protection at all, and SQLite only
-- gives it by accident of its single write lock.
--
-- Backfilled from the rows that already exist, because 0002 added the namespace
-- column and anything written between then and now holds one. INSERT OR IGNORE
-- rather than a plain INSERT: two placements of the same tenant legitimately
-- share a namespace, and only the first of them is the claim.
CREATE TABLE namespace_owner (
  namespace   TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  claimed_at  TEXT NOT NULL
) STRICT;

INSERT OR IGNORE INTO namespace_owner (namespace, tenant_id, claimed_at)
SELECT namespace, tenant_id, updated_at FROM placement;
