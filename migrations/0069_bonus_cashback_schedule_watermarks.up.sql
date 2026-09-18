-- Cashback scheduler watermark (Stage 4H-B1 Wave 3 Phase 1,
-- ledger-accounting-model.md §7.18.4). Schema only (Phase 2, backend) -
-- no scheduler, no "which players/campaigns are due" query, no
-- NetLossAmount computation. §7.18.4's own text is explicit that all of
-- that is Phase 3, bonus-engine's, business logic to write.
--
-- Granularity, decided here with reasoning (the dispatch's own
-- question): per (tenant_id, campaign_id, player_account_id,
-- asset_code) - NOT a single per-tenant-global cursor (§7.18.2's shape,
-- for a different reason - a deposit sweep scans every deposit
-- regardless of player) and NOT per-tenant-per-campaign-only. Cashback
-- eligibility and NetLossAmount are evaluated PER PLAYER (§7.18.4 item
-- 4, verbatim: "For a given (tenant_id, player_account_id, asset_code)
-- and window [start, end)"). A single per-campaign cursor could not
-- express "player A's next window starts at T1, player B's at T2" once
-- players enroll at different times or a cadence is anchored per-player
-- rather than campaign-globally - nothing in doc 10 or §7.18.4 requires
-- every player under one campaign to share one window boundary, and a
-- coarser cursor would force that assumption into the schema. asset_code
-- is included (matching bonus_wagering_progress's own denormalization
-- precedent, migration 0058) because NetLossAmount's own read is scoped
-- by asset; a campaign's current OfferVersion pins one
-- reward_asset_code today, but denormalizing here keeps a player's
-- cursor identity stable and unambiguous even across an OfferVersion
-- change that moves the reward asset, rather than making the cursor's
-- own identity implicitly depend on a live, mutable Offer-version
-- lookup.
--
-- This table stores ONLY a resume cursor (last_processed_window_end),
-- mirroring the deposit-sweep watermark's own purpose (migration 0068)
-- for the identical reason: §7.18.4 item 5's idempotency does NOT
-- depend on this table at all - it is already fully guaranteed by
-- bonus_grants' existing (tenant_id, campaign_id, offer_version_id,
-- player_account_id, trigger_reference) unique constraint (migration
-- 0057), keyed on the deterministic
-- "cashback:<campaign_id>:<player_account_id>:<window_start>:<window_end>"
-- reference §7.18.4 item 5 specifies. A retried/re-run scheduler tick
-- recomputes the identical trigger_reference and resolves to the
-- existing Grant via ErrAlreadyGranted regardless of whether this table
-- exists. This table exists purely so a scheduler tick can find "whose
-- window has elapsed" without rescanning a player's entire enrollment
-- history from scratch every tick - a performance/resumability concern,
-- not a correctness dependency, the same role migration 0068 plays for
-- the deposit sweep.
CREATE TABLE bonus_cashback_schedule_watermarks (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID NOT NULL,
    campaign_id                 UUID NOT NULL,
    player_account_id           UUID NOT NULL,
    asset_code                  TEXT NOT NULL REFERENCES assets (code),
    -- The end of the last window actually processed (issued, or found
    -- not-yet-due, depending on Phase 3's own retry semantics) for this
    -- (campaign, player, asset) - NULL means no window processed yet.
    -- The next window's own start is derivable from this value plus the
    -- Offer's own cadence configuration; not duplicated here.
    last_processed_window_end  TIMESTAMPTZ,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, campaign_id, player_account_id, asset_code),
    FOREIGN KEY (campaign_id, tenant_id) REFERENCES bonus_campaigns (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);

CREATE INDEX idx_bonus_cashback_schedule_watermarks_tenant ON bonus_cashback_schedule_watermarks (tenant_id);
-- The scheduler's own "which campaigns/players are due" scan pattern
-- (§7.18.4 item 2's "loop over every tenant with at least one active
-- Cashback campaign whose window has elapsed").
CREATE INDEX idx_bonus_cashback_schedule_watermarks_campaign ON bonus_cashback_schedule_watermarks (tenant_id, campaign_id);

-- Same posture as migration 0068: the cursor column itself
-- (last_processed_window_end/updated_at) is mutable by design; only the
-- row's own identity is frozen after insert.
CREATE FUNCTION bonus_cashback_schedule_watermarks_enforce_immutable_identity() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.campaign_id IS DISTINCT FROM OLD.campaign_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
    THEN
        RAISE EXCEPTION 'bonus_cashback_schedule_watermarks: tenant_id/campaign_id/player_account_id/asset_code are immutable after insert - create a new watermark row instead';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_cashback_schedule_watermarks_immutable_identity
    BEFORE UPDATE ON bonus_cashback_schedule_watermarks
    FOR EACH ROW EXECUTE FUNCTION bonus_cashback_schedule_watermarks_enforce_immutable_identity();

ALTER TABLE bonus_cashback_schedule_watermarks ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_cashback_schedule_watermarks FORCE ROW LEVEL SECURITY;

-- Staff/system-only, identical posture to migration 0068 - no
-- player-facing purpose exists for a scheduler cursor.
CREATE POLICY tenant_isolation_read ON bonus_cashback_schedule_watermarks
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_cashback_schedule_watermarks
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bonus_cashback_schedule_watermarks
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE TRIGGER bonus_cashback_schedule_watermarks_no_truncate
    BEFORE TRUNCATE ON bonus_cashback_schedule_watermarks
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
