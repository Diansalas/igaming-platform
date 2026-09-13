-- Stage 3C hardening (directive item 5): the configuration boundary for
-- withdrawal approval policy. Stage 3B's four-eyes threshold was a
-- single process-wide Go constant (httpserver.defaultWithdrawalApproval
-- Threshold, now removed) applied identically to every tenant, brand,
-- jurisdiction, and asset - with no asset-precision awareness (the same
-- raw minor-unit number compared against a EUR withdrawal and a BTC
-- withdrawal alike). This table is the boundary future business policy
-- configuration writes into; it does NOT itself contain a final business
-- policy - no row is seeded here, and
-- internal/withdrawal.ResolveApprovalPolicy's own fallback (used when no
-- row matches) is explicitly documented in code as a test/development
-- default, not a production policy decision. See
-- docs/architecture/withdrawal-policy-configuration.md.

CREATE TABLE withdrawal_policies (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id),
    -- NULL = applies to every brand under tenant_id; a non-null value
    -- narrows the rule to one brand.
    brand_id                  UUID REFERENCES brands (id),
    -- NULL = applies regardless of jurisdiction. Not resolvable from a
    -- real per-withdrawal jurisdiction today - no player-jurisdiction
    -- assignment exists in the platform as of Stage 3C (a player_account
    -- belongs to a brand; a tenant, not a brand, resolves to a set of
    -- jurisdictions via tenant_jurisdiction_configs, and can serve
    -- several at once). Present so the schema does not need another
    -- migration once that assignment exists - see
    -- docs/architecture/withdrawal-policy-configuration.md's open
    -- decisions.
    jurisdiction_code         TEXT,
    -- Required: a policy row always belongs to exactly one asset, so a
    -- threshold can never be compared against the wrong asset's minor
    -- units - the precision bug this migration exists to close.
    asset_code                TEXT NOT NULL REFERENCES assets (code),
    approval_threshold_minor_units BIGINT NOT NULL CHECK (approval_threshold_minor_units >= 0),
    required_approvals        SMALLINT NOT NULL DEFAULT 2 CHECK (required_approvals >= 1),
    -- Present for future use (directive item 5's "approver role
    -- requirements") - not yet enforced by internal/withdrawal.Approve;
    -- see that package's policy.go doc comment.
    required_approver_roles   TEXT[],
    -- Directive item 6's step-up/MFA enforcement boundary. MFA itself
    -- (ADR 0017) is not implemented - see withdrawal.ErrStepUpRequired's
    -- doc. A tenant that sets this true before MFA exists gets a safe,
    -- documented failure (fail closed) from Approve, never a silently
    -- ignored flag.
    require_step_up           BOOLEAN NOT NULL DEFAULT false,
    policy_version            INT NOT NULL DEFAULT 1 CHECK (policy_version >= 1),
    effective_from            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_withdrawal_policies_lookup ON withdrawal_policies (tenant_id, asset_code, effective_from DESC);

ALTER TABLE withdrawal_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_policies FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON withdrawal_policies
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
