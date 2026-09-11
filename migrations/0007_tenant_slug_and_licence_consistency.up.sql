-- Enforces that tenants.licensing_model matches the licensee of
-- whatever licence it points to - the Stage 1 technical debt item
-- tracked in docs/architecture/15-jurisdiction-and-licensing-model.md
-- ("Known gap"). Uses the standard Postgres pattern for a cross-table
-- CHECK: a generated column expressing what the licence's `licensee`
-- value must be, paired with a composite foreign key against a composite
-- unique constraint on licences.
--
-- (tenants.slug already exists from migration 0001 - it's reused as-is
-- for staff/partner login tenant resolution, see
-- docs/decisions/0012-brand-distinct-from-tenant.md - no schema change
-- needed for that part.)

ALTER TABLE licences ADD CONSTRAINT licences_id_licensee_key UNIQUE (id, licensee);

ALTER TABLE tenants ADD COLUMN expected_licensee TEXT
    GENERATED ALWAYS AS (
        CASE licensing_model
            WHEN 'under_platform_licence' THEN 'platform'
            WHEN 'own_licence' THEN 'tenant'
        END
    ) STORED;

-- NULL licence_id short-circuits the FK check (a tenant may exist before
-- a licence is assigned), so this only bites once a licence is actually
-- linked - and from then on, licensing_model and the linked licence's
-- licensee can never disagree.
ALTER TABLE tenants ADD CONSTRAINT tenants_licence_matches_model
    FOREIGN KEY (licence_id, expected_licensee) REFERENCES licences (id, licensee);
