-- Stage 4I security review (SEC-4I-F4): replace migration 0071's single
-- FOR ALL policy on jurisdiction_resolution_active with per-command
-- SELECT/INSERT/UPDATE policies and NO DELETE policy.
--
-- The canonical model's §6.1 ("Universal schema rules", adopted verbatim
-- from the Stage 4I security model §S-3.1) is binding on BOTH
-- jurisdiction_resolutions AND jurisdiction_resolution_active, and says
-- exactly: "Per-command policies. No `FOR ALL` policy. No DELETE policy.
-- No UPDATE policy on `jurisdiction_resolutions`." Migration 0071
-- satisfied that for jurisdiction_resolutions but wrote a `FOR ALL`
-- policy for jurisdiction_resolution_active, which silently grants
-- DELETE as well.
--
-- Why that matters concretely, beyond the letter of the rule: there is
-- no application DELETE path for this table, and the absence of a DELETE
-- policy is exactly the backstop for that fact. With `FOR ALL`, ANY
-- tenant-scoped transaction (db.Pool.WithTenant - every ordinary admin
-- handler in the platform) can `DELETE FROM jurisdiction_resolution_active`
-- and remove its own tenant's recorded resolution-active facts. That is
-- NOT equivalent to setting `active = false`: SetResolutionActive writes
-- an `audit_log` "jurisdiction_resolution_active.changed" row carrying
-- before/after in the same transaction, whereas a DELETE writes nothing.
-- The result is an UNAUDITED way to change a control's state, plus a
-- lasting divergence between audit_log (which still says the fact was set
-- active) and the table (where the row no longer exists) - and, once
-- RISK §2.4b's CreateRule precondition (R-2b) consumes IsActive, an
-- unaudited way to change that precondition's answer.
--
-- The shape below is the one migration 0071's own comment claimed to be
-- following: asset_authorizations (migration 0045) really does use
-- per-command tenant_isolation_read / tenant_isolation_insert /
-- tenant_isolation_update policies with no DELETE policy. The predicate
-- itself is unchanged from 0071 - same tenant match, same leading
-- "app.player_account_id IS NULL" conjunct that keeps a player-scoped
-- connection (which sets BOTH GUCs) out of this table entirely.

DROP POLICY jurisdiction_resolution_active_tenant_isolation ON jurisdiction_resolution_active;

CREATE POLICY jurisdiction_resolution_active_tenant_isolation_read ON jurisdiction_resolution_active
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY jurisdiction_resolution_active_tenant_isolation_insert ON jurisdiction_resolution_active
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- UPDATE is permitted (unlike jurisdiction_resolutions): this is a
-- mutable CURRENT-STATE fact, and SetResolutionActive's own
-- INSERT ... ON CONFLICT DO UPDATE upsert needs it. Both USING and
-- WITH CHECK are required so a row can neither be updated out of this
-- tenant's scope nor updated from outside it.
CREATE POLICY jurisdiction_resolution_active_tenant_isolation_update ON jurisdiction_resolution_active
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- Deliberately NO DELETE policy. A resolution-active fact is turned off
-- by SetResolutionActive(active=false), which is audited; it is never
-- removed.
