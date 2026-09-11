-- Person: a real human, resolved across brands/tenants. Platform-level,
-- like jurisdictions/assets - NOT tenant-owned, so no tenant_id and no
-- RLS. See docs/architecture/05-identity-architecture.md.
--
-- person_key_hash is populated post-KYC (Stage 4) from a hash of
-- document number + date of birth, used to detect that two player
-- accounts on different brands belong to the same human (platform-level
-- self-exclusion, multi-accounting detection). Stage 2 creates one
-- unverified Person per registration; the Stage 4 KYC flow is what
-- actually resolves/merges persons across brands - this migration only
-- lays the column down.

CREATE TABLE persons (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status          TEXT NOT NULL DEFAULT 'unverified' CHECK (status IN ('unverified', 'verified', 'excluded')),
    person_key_hash TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial unique index (not a plain UNIQUE column) because most rows
-- have no hash yet (pre-KYC) and NULL <> NULL must not collide.
CREATE UNIQUE INDEX idx_persons_key_hash ON persons (person_key_hash) WHERE person_key_hash IS NOT NULL;

COMMENT ON TABLE persons IS 'Platform-wide person cluster, deliberately not tenant-owned - see docs/architecture/05-identity-architecture.md. Self-exclusion and AML case history attach here, never to player_accounts.';
