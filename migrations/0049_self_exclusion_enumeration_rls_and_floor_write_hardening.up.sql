-- Stage 4H-B0-R6 fix dispatch (identity-compliance, Workstream E
-- follow-up): closes two confirmed independent-review defects in
-- migration 0043, found by security + code-reviewer against this same
-- stage's own Workstream E commit (c5b6162).
--
-- Fix 2 (P2): self_exclusion_enumeration_runs' tenant_isolation policy
-- was missing the "app.player_account_id IS NULL" conjunct every sibling
-- table in migration 0043 (open_bet_self_exclusion_policies) and every
-- other newer staff/config table (player_restrictions, risk_rules,
-- asset_authorizations) correctly includes. Postgres ORs permissive
-- policies together, and db.WithPlayerScope sets BOTH app.tenant_id and
-- app.player_account_id - so without this conjunct, a player-scoped
-- transaction that happened to reach this table would satisfy
-- tenant_isolation on tenant_id alone and could read every self-excluded
-- person's compliance record in its own tenant, and forge a completion
-- record via the table's own trigger (which permits progress-field
-- updates before dispatch_status reaches 'completed'). No handler
-- reaches this table under player scope today - the fix closes the
-- structural gap regardless, per internal/db/tenant_rls.go's own
-- documented premise that the conjunct is what actually makes isolation
-- hold, not merely which handlers happen to call it today.
--
-- Fix 5 (P2/Medium-High): migration 0043's open_bet_self_exclusion_
-- policies write policies accepted a platform-wide (jurisdiction-floor)
-- row from any connection with app.tenant_id merely UNSET, with no
-- platform-admin GUC requirement - the exact weaker precedent migration
-- 0045 (Workstream A, asset_operation_eligibility) deliberately did NOT
-- repeat when it closed finding S-3's shape for its own platform-wide-
-- default write path. Aligns 0043 to require
-- app.platform_admin_principal_id (see internal/db/tenant_rls.go's
-- WithPlatformAdmin) for a jurisdiction-floor write too. Read remains
-- open (unchanged) - mirroring migration 0045's own read-open/write-
-- gated split for platform_products/asset_operation_eligibility.

-- ---------------------------------------------------------------------
-- Fix 2: self_exclusion_enumeration_runs tenant_isolation policy
-- ---------------------------------------------------------------------
DROP POLICY IF EXISTS tenant_isolation ON self_exclusion_enumeration_runs;

CREATE POLICY tenant_isolation ON self_exclusion_enumeration_runs
    FOR ALL
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- ---------------------------------------------------------------------
-- Fix 5: open_bet_self_exclusion_policies write policies require the
-- platform-admin GUC for a platform-wide (jurisdiction-floor) row.
-- ---------------------------------------------------------------------
DROP POLICY IF EXISTS tenant_and_platform_write ON open_bet_self_exclusion_policies;

CREATE POLICY tenant_and_platform_write ON open_bet_self_exclusion_policies
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

DROP POLICY IF EXISTS tenant_and_platform_update ON open_bet_self_exclusion_policies;

CREATE POLICY tenant_and_platform_update ON open_bet_self_exclusion_policies
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

DROP POLICY IF EXISTS tenant_and_platform_delete_visibility ON open_bet_self_exclusion_policies;

CREATE POLICY tenant_and_platform_delete_visibility ON open_bet_self_exclusion_policies
    FOR DELETE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

-- ---------------------------------------------------------------------
-- Fix 4: reject a backdated jurisdiction-floor effective_from AT WRITE
-- TIME, database-level backstop (identity-compliance's own conservative
-- technical-control decision - see internal/rg/self_exclusion_policy.go's
-- updated doc comment for the full reasoning). A jurisdiction floor is
-- the one write type deliberately exempt from the tighten-only trigger
-- above (it IS the floor) - but that exemption previously let an
-- unbounded backdated effective_from retroactively change which policy
-- value governed an instant that had already passed, including an
-- instant after a real self-exclusion had already occurred under the
-- old, stricter floor. This trigger makes a floor change take effect now
-- or later, never retroactively - a temporal-integrity rule, not a
-- compliance-value choice, so it closes the exploit without deciding
-- anything ADR 0034 §14.9 left to the jurisdiction. Tenant/brand override
-- rows are NOT in scope here - they remain governed by the tighten-only
-- trigger above, which already prevents this exact exploit shape for
-- them (a tenant/brand row can never resolve to a LOOSER value than its
-- floor at any as-of instant, backdated or not).
--
-- Tolerance rationale: this package's own "effective now" default
-- (rg.SetOpenBetSelfExclusionPolicy, EffectiveFrom == nil) resolves
-- effective_from via one `SELECT clock_timestamp()` round trip, then
-- inserts that already-fetched value in a SEPARATE, later statement.
-- clock_timestamp() strictly advances on every call (unlike now()/
-- transaction_timestamp()), so comparing that earlier-fetched value
-- against a bare, zero-tolerance clock_timestamp() call inside THIS
-- trigger would reject every legitimate default "now" floor write
-- purely from ordinary round-trip latency between the two statements -
-- not a real backdating attempt. A small fixed tolerance
-- (FLOOR_BACKDATING_TOLERANCE, matching
-- rg.floorBackdatingTolerance in Go exactly - see
-- TestFloorBackdatingTolerance_MatchesDatabase for the parity check)
-- absorbs that ordinary latency while still rejecting the exploit this
-- fix exists for, which backdates by a materially longer interval (the
-- gap between an operator's later config-change action and the earlier
-- self-exclusion instant it is trying to retroactively cover).
CREATE FUNCTION open_bet_self_exclusion_policies_enforce_floor_no_backdating() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS NULL AND NEW.effective_from < clock_timestamp() - INTERVAL '5 seconds' THEN
        RAISE EXCEPTION 'open_bet_self_exclusion_policies: a jurisdiction-floor row may not be backdated - effective_from (%) is before the current instant; a floor change may only take effect now or later',
            NEW.effective_from;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER open_bet_self_exclusion_policies_floor_no_backdating
    BEFORE INSERT ON open_bet_self_exclusion_policies
    FOR EACH ROW EXECUTE FUNCTION open_bet_self_exclusion_policies_enforce_floor_no_backdating();
