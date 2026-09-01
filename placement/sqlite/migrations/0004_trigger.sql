-- What turns a git push into a build.
--
-- Three columns on the placement row rather than a table of their own, because
-- the lifetime is identical: a trigger belongs to exactly one placement and
-- nothing else could ever delete it. A separate table would need its own
-- cascade, and a cascade that is written by hand is one that is forgotten once.
--
-- source_repo_url is NOT repo_url and the two must never be read as the same
-- thing. repo_url is the tenant's STATE repository, where damga commits
-- manifests. This is the repository a build clones, which until now no row
-- recorded at all — server/builds.go carried that gap in a comment and made
-- every caller send it in the request body, which is exactly why a push had
-- nowhere to arrive.
--
-- No claim table beside it, unlike repo_url and namespace. Those two are
-- claimed because a second tenant naming one would write into somebody else's
-- history or run inside their fence. A source repository is only read: two
-- tenants building the same public repository is a legitimate thing to want,
-- and neither can trigger the other's build without the other's secret.
--
-- Empty is the honest default for all three: every existing placement predates
-- webhooks and has no trigger, and Validate refuses an empty secret, so a row
-- left as it was cannot be matched by TriggersFor.
ALTER TABLE placement ADD COLUMN source_repo_url TEXT NOT NULL DEFAULT '';
ALTER TABLE placement ADD COLUMN trigger_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE placement ADD COLUMN trigger_secret TEXT NOT NULL DEFAULT '';

-- The lookup a webhook makes on every delivery, and the only unauthenticated
-- query in this schema. Not unique: one repository legitimately feeds several
-- environments, and which one a push is for is decided by which secret
-- verifies.
CREATE INDEX placement_trigger ON placement (trigger_provider, source_repo_url);
