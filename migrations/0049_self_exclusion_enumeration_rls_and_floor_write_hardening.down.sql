-- Reverses migration 0049.

DROP TRIGGER IF EXISTS open_bet_self_exclusion_policies_floor_no_backdating ON open_bet_self_exclusion_policies;
DROP FUNCTION IF EXISTS open_bet_self_exclusion_policies_enforce_floor_no_backdating();

DROP POLICY IF EXISTS tenant_and_platform_delete_visibility ON open_bet_self_exclusion_policies;
CREATE POLICY tenant_and_platform_delete_visibility ON open_bet_self_exclusion_policies
    FOR DELETE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

DROP POLICY IF EXISTS tenant_and_platform_update ON open_bet_self_exclusion_policies;
CREATE POLICY tenant_and_platform_update ON open_bet_self_exclusion_policies
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

DROP POLICY IF EXISTS tenant_and_platform_write ON open_bet_self_exclusion_policies;
CREATE POLICY tenant_and_platform_write ON open_bet_self_exclusion_policies
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

DROP POLICY IF EXISTS tenant_isolation ON self_exclusion_enumeration_runs;
CREATE POLICY tenant_isolation ON self_exclusion_enumeration_runs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
