-- Defense-in-depth for migration 0007's licensing-model/licence
-- consistency fix, per Stage 2 architect review: tenants.licensing_model
-- already has a NOT NULL CHECK restricting it to exactly
-- 'under_platform_licence'/'own_licence' (migration 0001), so
-- expected_licensee's CASE expression can never actually produce NULL
-- today - but the CASE itself has no ELSE branch, and a composite
-- foreign key with a NULL referencing column (MATCH SIMPLE, the
-- Postgres default) is satisfied unconditionally, silently skipping
-- enforcement. If a future migration ever adds a third licensing_model
-- value without updating the CASE, tenants_licence_matches_model would
-- stop enforcing with no error. This CHECK makes that failure mode loud
-- instead of silent.

ALTER TABLE tenants ADD CONSTRAINT tenants_expected_licensee_not_null_when_linked
    CHECK (licence_id IS NULL OR expected_licensee IS NOT NULL);
