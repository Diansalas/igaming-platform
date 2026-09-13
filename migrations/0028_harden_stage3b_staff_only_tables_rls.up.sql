-- Stage 3B security review finding (both `security` and `architect`
-- specialists, independently): five tables that are supposed to be
-- staff/system-only carried a bare `tenant_id = app.tenant_id` policy
-- with no guard excluding a player-scoped connection - unlike every
-- other Stage 3B table, which correctly follows the two-policy pattern
-- (docs/decisions/0019: `tenant_staff_scope` requires
-- app.player_account_id to be UNSET, OR'd with a SELECT-only
-- `player_self_scope`). db.Pool.WithPlayerScope sets BOTH
-- app.tenant_id and app.player_account_id, so a player-scoped connection
-- satisfied these five tables' bare tenant-match policy completely -
-- full tenant-wide SELECT/INSERT/UPDATE/DELETE, not just a read of the
-- player's own rows.
--
-- Not exploitable by any code path that exists today (every query
-- against these five tables runs under db.Pool.WithTenant, never
-- WithPlayerScope - verified during review), which is exactly the
-- latent-trap character this fixes: the natural next feature (e.g. "show
-- me my transaction history" joining ledger_entries to
-- ledger_transactions under player scope) would have silently leaked
-- every player's data in the tenant, with the policy's own comments
-- asserting the opposite of what the SQL actually allowed.
--
-- None of these five tables has any legitimate player-self-service use
-- case (per their own docs: ledger_transactions/withdrawal_approvals are
-- explicitly staff/system-only by design; provider_capabilities is
-- tenant-administrative configuration; reconciliation_runs/mismatches
-- are a compliance/finance tool), so the fix is a straight tenant-
-- isolation tightening, not a new player_self_scope policy - the player
-- GUC being unset is now REQUIRED, not merely irrelevant.

DROP POLICY tenant_isolation ON ledger_transactions;
CREATE POLICY tenant_isolation ON ledger_transactions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

DROP POLICY tenant_isolation ON provider_capabilities;
CREATE POLICY tenant_isolation ON provider_capabilities
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

DROP POLICY tenant_isolation ON withdrawal_approvals;
CREATE POLICY tenant_isolation ON withdrawal_approvals
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

DROP POLICY tenant_isolation ON reconciliation_runs;
CREATE POLICY tenant_isolation ON reconciliation_runs
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

DROP POLICY tenant_isolation ON reconciliation_mismatches;
CREATE POLICY tenant_isolation ON reconciliation_mismatches
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- provider_capability_amount_limits is deliberately NOT touched here: its
-- own policy (migration 0024) is a subquery into provider_capabilities,
-- so it inherits this tightening transitively the moment
-- provider_capabilities' policy above excludes player scope. (Its
-- separate structural issues - no tenant_id column of its own, and a
-- subquery-shaped policy ADR 0019 otherwise forbids - are recorded as a
-- known follow-up in the Stage 3B completion report, not fixed here:
-- adding a column requires a backfill this migration does not attempt.)
