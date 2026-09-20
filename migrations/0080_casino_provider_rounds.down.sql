-- DROP TABLE cascades away casino_provider_rounds_immutable_fields (a
-- trigger belongs to its table), but the trigger FUNCTION is independent
-- and must be dropped explicitly.
DROP TABLE casino_provider_rounds;
DROP FUNCTION casino_provider_rounds_enforce_immutable_fields();

ALTER TABLE casino_launch_sessions
    DROP CONSTRAINT casino_launch_sessions_id_tenant_key;
