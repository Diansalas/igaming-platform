-- Stage 8, docs/decisions/0080-provider-integration-readiness-without-
-- external-contracts.md, Decision 1 - resolves the read-model limitation
-- ADR 0048 ("Casino Play-Simulation Trust Boundary") disclosed and
-- deferred: internal/casino/history.go re-derives a round's
-- correlation_id from roundCorrelationID(tenantID, providerID, roundID),
-- a convention Stage 7's play-simulation seam introduced (it always sets
-- CallbackEvent.RoundID = session.ID.String()). A real provider declares
-- its own round id, never assumed to equal a session id
-- (CallbackEvent.RoundID's own doc comment) - so a real provider's round
-- previously had nowhere durable to be looked up from by provider
-- identifiers alone. This migration is purely additive: no existing
-- table, column, or RLS policy is altered.

-- Prerequisite composite unique constraint so the FK below can verify
-- launch_session_id really belongs to tenant_id - casino_launch_sessions
-- had no (id, tenant_id) unique constraint before this migration because
-- nothing needed to FK against it at that granularity yet (mirrors
-- migration 0019's identical player_accounts_id_tenant_brand_key
-- rationale, and migration 0079's re-use of that same established
-- pattern for casino_launch_sessions' own brand pinning).
ALTER TABLE casino_launch_sessions
    ADD CONSTRAINT casino_launch_sessions_id_tenant_key UNIQUE (id, tenant_id);

-- casino_provider_rounds: one row per (tenant_id, provider_id,
-- provider_round_id) first observed, binding it to the platform's own
-- launch_session_id, player_account_id, brand_id, game_id, and the
-- correlation_id its ledger transactions use - durable, provider-
-- neutral, queryable by provider identifiers alone (not only by session
-- id). A row belongs to exactly one (tenant, brand, player) - NOT to
-- exactly one launch_session_id, which may legitimately advance across a
-- row's lifetime (rounds.go's own BindProviderRound doc comment; see the
-- immutability trigger below for the precise column-level rule) - but it
-- still gets the identical tenant/player two-policy RLS shape
-- casino_launch_sessions (migration 0035) uses, since RLS is scoped by
-- tenant_id/player_account_id, not by session.
CREATE TABLE casino_provider_rounds (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    brand_id             UUID NOT NULL,
    player_account_id    UUID NOT NULL,
    launch_session_id    UUID NOT NULL,
    game_id              UUID NOT NULL REFERENCES casino_games (id),
    provider_id          TEXT NOT NULL,
    provider_round_id    TEXT NOT NULL,
    -- Nullable: not every provider has a distinct session id from round
    -- id (ADR 0080 Decision 1) - this is honest about what is not
    -- knowable from CallbackEvent today (it surfaces no separate
    -- provider-session-id concept at all).
    provider_session_id  TEXT,
    correlation_id       UUID NOT NULL,
    first_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Pins launch_session_id -> casino_launch_sessions to THIS SAME
    -- tenant_id - the same "composite FK, never a bare id reference on a
    -- tenant-partitioned table" discipline migration 0079 established for
    -- casino_launch_sessions itself.
    FOREIGN KEY (launch_session_id, tenant_id)
        REFERENCES casino_launch_sessions (id, tenant_id),
    -- Pins player_account_id -> tenant_id -> brand_id together, reusing
    -- migration 0019's player_accounts_id_tenant_brand_key exactly like
    -- migration 0079 does for casino_launch_sessions' own brand pinning -
    -- a row can never claim a (tenant, brand, player) triple that doesn't
    -- genuinely exist together.
    FOREIGN KEY (player_account_id, tenant_id, brand_id)
        REFERENCES player_accounts (id, tenant_id, brand_id),

    -- ADR 0080 Decision 1's own "Uniqueness scope" section: tenant +
    -- provider, mirroring ledger_transactions' existing
    -- idx_ledger_transactions_tenant_provider_tx scoping exactly (a round
    -- id is that provider's own namespace, never assumed to be shared
    -- across the tenants it serves). Because each row carries exactly
    -- one player_account_id, this single constraint is also what makes a
    -- provider round id belong to exactly one player - a second row
    -- attempting to claim the same (tenant_id, provider_id,
    -- provider_round_id) for a different player/brand is a
    -- uniqueness violation, not an application-level check. This is a
    -- documented ASSUMPTION, not a fact about any real provider's
    -- contract - revisit the moment a real provider's documented round-id
    -- scope is known (see the ADR's own "Removal / extension condition").
    UNIQUE (tenant_id, provider_id, provider_round_id)
);

CREATE INDEX idx_casino_provider_rounds_tenant ON casino_provider_rounds (tenant_id);
CREATE INDEX idx_casino_provider_rounds_session ON casino_provider_rounds (tenant_id, launch_session_id);

-- Immutability, enforced at the database level rather than by application
-- discipline (CLAUDE.md's "Financial / ledger rules" section; this repo's
-- own repeated precedent - withdrawal_requests migration 0026,
-- casino_launch_sessions_enforce_immutable_fields migration 0036, assets
-- migration 0044, bonus_grants migration 0057). Modeled directly on
-- casino_launch_sessions_enforce_immutable_fields (migration 0036): every
-- identity column is frozen after insert, with two narrow, deliberate
-- exceptions this table's own semantics require -
--
--   - last_seen_at: updated on every redelivery of an already-bound round
--     (BindProviderRound's own idempotent-upsert doc comment).
--   - launch_session_id: a provider round belongs to exactly one
--     (player_account_id, brand_id) - NOT to exactly one launch_session_id
--     (rounds.go's own BindProviderRound doc comment) - a round can
--     legitimately span more than one launch session for the SAME player
--     (e.g. a free-spins round continuing across a session timeout), so a
--     legitimate same-player/same-brand continuation is allowed to advance
--     this column to the newest session.
--
-- provider_session_id gets one additional narrow allowance: a NULL -> value
-- transition (this column is nullable - see its own column comment above -
-- so learning it for the first time on a later delivery is legitimate),
-- but never a value -> different-value or value -> NULL transition.
-- Every other column (tenant_id, brand_id, player_account_id, game_id,
-- provider_id, provider_round_id, correlation_id, first_seen_at) is
-- immutable, full stop - in particular player_account_id and
-- provider_round_id, which is exactly the cross-player-misattribution
-- property this table exists to enforce (ADR 0080 Decision 1) and which a
-- tenant-staff-scoped UPDATE could otherwise silently defeat.
CREATE FUNCTION casino_provider_rounds_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.game_id IS DISTINCT FROM OLD.game_id
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.provider_round_id IS DISTINCT FROM OLD.provider_round_id
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.first_seen_at IS DISTINCT FROM OLD.first_seen_at
    THEN
        RAISE EXCEPTION 'casino_provider_rounds: identity columns are immutable after insert';
    END IF;
    IF OLD.provider_session_id IS NOT NULL
        AND NEW.provider_session_id IS DISTINCT FROM OLD.provider_session_id
    THEN
        RAISE EXCEPTION 'casino_provider_rounds: provider_session_id is immutable once set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_provider_rounds_immutable_fields
    BEFORE UPDATE ON casino_provider_rounds
    FOR EACH ROW EXECUTE FUNCTION casino_provider_rounds_enforce_immutable_fields();

ALTER TABLE casino_provider_rounds ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_provider_rounds FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as casino_launch_sessions (migration 0035):
-- staff/system get full access under tenant-only scope (app.
-- player_account_id unset, per migration 0028's convention); a player
-- gets SELECT-only visibility into their OWN rounds.
CREATE POLICY tenant_staff_scope ON casino_provider_rounds
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON casino_provider_rounds
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
