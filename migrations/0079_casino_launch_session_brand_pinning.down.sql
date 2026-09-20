ALTER TABLE casino_launch_sessions DROP CONSTRAINT casino_launch_sessions_player_tenant_brand_fkey;
ALTER TABLE casino_launch_sessions
    ADD CONSTRAINT casino_launch_sessions_brand_id_tenant_id_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);
