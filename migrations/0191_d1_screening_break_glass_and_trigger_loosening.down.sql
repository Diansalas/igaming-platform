-- Down of 0191 (DEV/STAGING ONLY): restores the 0154 grant of the break-glass setter to the runtime role (NOT recommended anywhere else).
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'GRANT EXECUTE ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) TO igaming_runtime';
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) IS NULL;
