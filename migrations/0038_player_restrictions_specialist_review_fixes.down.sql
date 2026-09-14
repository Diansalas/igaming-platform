ALTER TABLE player_restrictions DROP CONSTRAINT player_restrictions_player_account_id_tenant_id_fkey;
ALTER TABLE player_accounts DROP CONSTRAINT player_accounts_id_tenant_id_key;

DROP TRIGGER IF EXISTS player_restrictions_immutable_statement ON player_restrictions;

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

DROP POLICY player_self_read ON player_restrictions;

CREATE POLICY player_self_read ON player_restrictions
    FOR SELECT
    USING (
        person_id = (
            SELECT person_id FROM player_accounts
            WHERE id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
    );
