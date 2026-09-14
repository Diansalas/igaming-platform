-- Stage 4D-RG: the authoritative Responsible Gaming restriction record.
-- See docs/decisions/0026-responsible-gaming-player-status-enforcement-
-- foundation.md for full design rationale. This table adds exactly ONE
-- new concept to the existing identity model (Person/PlayerAccount/
-- Tenant/Brand/Wallet, unchanged) - a restriction that can bind at the
-- PERSON level (platform-wide, surviving a new brand registration) as
-- well as at tenant/brand level, closing the gap ADR 0011's Stage-0
-- proposal already anticipated ("self-exclusion at both brand and
-- platform level... requires the cross-brand person cluster").
--
-- restriction_type is deliberately CHECK-limited to 'self_exclusion'
-- only - the one restriction type this stage actually implements
-- end-to-end (CLAUDE.md's "no fake completion": a type nothing enforces
-- is not added just to look complete). Deposit/loss/wagering/session
-- limits, reality checks, and time-outs/cooling-off (docs/decisions/0026
-- §15) are documented EXTENSION POINTS for a future stage, not schema
-- placeholders here - extending this CHECK constraint is an additive
-- future migration.
--
-- Append-only, like ledger_transactions/withdrawal_approvals/audit_log:
-- self-exclusion is a player-protective control that must not be
-- editable or revocable by anyone (including the row's own author) once
-- created - see ADR 0026 §4 for why no "end restriction early" mutation
-- exists at all yet.

CREATE TABLE player_restrictions (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The enforcement anchor - always set, always resolved server-side.
    -- Person carries no tenant_id/RLS of its own (it is platform-wide by
    -- design, see persons table) - this FK is a plain single-column
    -- reference, not a composite one, for exactly that reason.
    person_id              UUID NOT NULL REFERENCES persons (id),
    -- tenant_id/brand_id are the ADMINISTRATIVE scope of this specific
    -- restriction row - NULL tenant_id = platform-wide (applies in every
    -- tenant context, closing the cross-brand-evasion gap); tenant_id set
    -- + brand_id NULL = applies across that one tenant's brands only;
    -- both set = applies to that one brand only. Never brand_id set with
    -- tenant_id NULL (see CHECK below) - a brand always belongs to some
    -- tenant, so a "platform-wide but single-brand" restriction is not a
    -- coherent scope.
    tenant_id              UUID,
    brand_id               UUID,
    -- The specific account the restriction was administered/requested
    -- through - context/provenance only, never itself the enforcement
    -- key (person_id is). Nullable in the schema for a hypothetical
    -- future path with no anchor account, but every path this stage
    -- implements always populates it.
    player_account_id      UUID REFERENCES player_accounts (id),
    restriction_type       TEXT NOT NULL CHECK (restriction_type IN ('self_exclusion')),
    starts_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL = indefinite (ADR 0026 §4). A time-bound exclusion simply
    -- stops matching the "currently active" query once ends_at passes -
    -- there is no separate mutable status column to keep in sync.
    ends_at                TIMESTAMPTZ,
    source                 TEXT NOT NULL CHECK (source IN ('player_self_service', 'staff')),
    reason_code            TEXT,
    created_by_actor_type  TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff')),
    created_by_actor_id    UUID NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at IS NULL OR ends_at > starts_at),
    CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    CHECK ((source = 'player_self_service') = (created_by_actor_type = 'player')),
    -- Guarantees brand_id really belongs to tenant_id when both are set -
    -- a database-level fact (docs/decisions/0012's precedent), not an
    -- application-trusted one. MATCH SIMPLE (Postgres default) means this
    -- is not checked at all when brand_id IS NULL, which is exactly the
    -- platform-wide/tenant-wide case - never reachable with a mismatched
    -- pair since the CHECK above forbids brand_id without tenant_id.
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE INDEX idx_player_restrictions_person ON player_restrictions (person_id);
CREATE INDEX idx_player_restrictions_tenant ON player_restrictions (tenant_id);
CREATE INDEX idx_player_restrictions_player_account ON player_restrictions (player_account_id);

ALTER TABLE player_restrictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE player_restrictions FORCE ROW LEVEL SECURITY;

-- READ: a staff/system context (app.player_account_id unset - i.e. every
-- WithTenant/WithoutTenant caller, including the casino orchestrator's
-- own eligibility check) sees its own tenant's rows PLUS every
-- platform-wide row, regardless of which tenant minted it - this is what
-- actually makes cross-brand/cross-tenant enforcement possible: a bet
-- evaluated inside tenant B's own WithTenant transaction must still see a
-- platform-wide self-exclusion a completely different tenant A's player
-- self-service call created.
CREATE POLICY staff_and_system_read ON player_restrictions
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            tenant_id IS NULL
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- READ: a player-scoped context (app.player_account_id set, via
-- db.WithPlayerScope) sees only rows naming THEIR OWN person_id -
-- resolved server-side from their own authenticated player_account_id,
-- never player-supplied. A player is allowed to see they are restricted
-- (including a staff-created, tenant/brand/platform-scoped restriction
-- against them) - this is the player-facing "am I restricted" self-check.
CREATE POLICY player_self_read ON player_restrictions
    FOR SELECT
    USING (
        person_id = (
            SELECT person_id FROM player_accounts
            WHERE id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
    );

-- WRITE (staff): a tenant-scoped staff context may only insert a row
-- scoped to ITS OWN tenant (tenant_id = app.tenant_id) - a platform-wide
-- row (tenant_id NULL) may only be inserted by a genuinely
-- platform-scoped context (db.WithoutTenant - app.tenant_id itself
-- unset), mirroring PermCasinoCatalogueManage's identical
-- platform-only-for-platform-wide-effect precedent (ADR 0025 §4). Never
-- both branches at once - a tenant-scoped connection cannot smuggle in a
-- platform-wide row by simply setting the tenant_id COLUMN to NULL in its
-- INSERT, since the WITH CHECK below still requires app.tenant_id itself
-- to be unset for that branch to match.
CREATE POLICY staff_insert ON player_restrictions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND source = 'staff'
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

-- WRITE (player self-service): always platform-wide scope (tenant_id AND
-- brand_id NULL) - a self-service self-exclusion protects the player
-- everywhere on the platform, not just the one brand they happened to
-- click the button from (ADR 0026 §4's cross-brand rationale). The row's
-- person_id/player_account_id/created_by_actor_id are all pinned to the
-- CALLER's own server-resolved identity via the same GUC
-- db.WithPlayerScope already sets for every other player-self-service
-- financial write in this codebase - never client-supplied.
CREATE POLICY player_self_insert ON player_restrictions
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND brand_id IS NULL
        AND restriction_type = 'self_exclusion'
        AND source = 'player_self_service'
        AND created_by_actor_type = 'player'
        AND created_by_actor_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND person_id = (
            SELECT person_id FROM player_accounts
            WHERE id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
    );

-- UPDATE/DELETE visibility (scope only, mirroring staff_and_system_read -
-- NOT a grant of mutation). Immutability itself is enforced by the
-- trigger below, not by withholding row visibility here - matching the
-- established pattern audit_log/ledger_entries/ledger_transactions/
-- withdrawal_approvals all already use (a FOR ALL-style scope policy plus
-- a deny-mutation trigger), so an UPDATE/DELETE against a row this role
-- can otherwise see fails LOUDLY with the trigger's own exception, never
-- silently as "0 rows matched" (which would be indistinguishable from a
-- caller simply naming a nonexistent id).
CREATE POLICY staff_and_system_update_visibility ON player_restrictions
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    );

CREATE POLICY staff_and_system_delete_visibility ON player_restrictions
    FOR DELETE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    );

CREATE FUNCTION player_restrictions_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'player_restrictions is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER player_restrictions_immutable
    BEFORE UPDATE OR DELETE ON player_restrictions
    FOR EACH ROW EXECUTE FUNCTION player_restrictions_deny_mutation();

CREATE TRIGGER player_restrictions_no_truncate
    BEFORE TRUNCATE ON player_restrictions
    FOR EACH STATEMENT EXECUTE FUNCTION player_restrictions_deny_mutation();
