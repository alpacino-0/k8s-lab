-- The namespace the rendered manifest says it runs in.
--
-- Added rather than derived from the tenant and the environment: a derived
-- namespace makes the name a parse of an identity, and the day a customer
-- renames a tenant or has an existing namespace to deploy into, a convention
-- has become a rewrite.
--
-- Backfilled with the app name for any row written before this column existed.
-- There are none outside a test at the time of writing, and a default of ''
-- would make every one of them fail Validate with no way to see why.
ALTER TABLE placement ADD COLUMN namespace TEXT NOT NULL DEFAULT '';
UPDATE placement SET namespace = app WHERE namespace = '';
