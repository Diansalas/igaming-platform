-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). Run by hand under
-- psql autocommit, a refusal below would leave FORCE ROW LEVEL SECURITY lifted for the
-- owner on the three tables. By hand use `psql --single-transaction -v ON_ERROR_STOP=1 -f`.
--
-- Reverses 0128 (ADR 0112 slice 1, S13). The FIRST checking statement refuses (LA099)
-- while ANY governed transition exists, before anything is dropped; it also refuses
-- while any launch request or approval row exists (a recorded decision is never
-- silently dropped) or any tenant / brand holds pending_launch (the narrowed CHECK
-- could not hold it). legacy_baseline and owner_provisioned rows carry no decision and
-- are dropped with the tables. Otherwise: drop the guards, restore the defaults to
-- 'active', narrow the CHECKs, drop the tables and functions.

-- The owner is bound by FORCE ROW LEVEL SECURITY; lift it (transactionally) so the
-- refusal check below sees every row. Restored by the DROP TABLE that follows, or by the
-- rollback if the check refuses.
ALTER TABLE launch_authorisation_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE launch_authorisation_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE launch_status_transitions NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM launch_status_transitions WHERE kind = 'governed') THEN
        RAISE EXCEPTION '0128 down refused: governed launch transitions exist' USING ERRCODE = 'LA099';
    END IF;
    IF EXISTS (SELECT 1 FROM launch_authorisation_requests) OR EXISTS (SELECT 1 FROM launch_authorisation_approvals) THEN
        RAISE EXCEPTION '0128 down refused: launch authorisation requests or approvals exist' USING ERRCODE = 'LA099';
    END IF;
    IF EXISTS (SELECT 1 FROM tenants WHERE status = 'pending_launch')
       OR EXISTS (SELECT 1 FROM brands WHERE status = 'pending_launch') THEN
        RAISE EXCEPTION '0128 down refused: a tenant or brand is pending_launch' USING ERRCODE = 'LA099';
    END IF;
END $$;

DROP TRIGGER zz_launch_status_governed ON brands;
DROP TRIGGER zz_launch_status_governed ON tenants;
DROP TRIGGER zz_launch_owner_provisioned ON brands;
DROP TRIGGER zz_launch_owner_provisioned ON tenants;
DROP TRIGGER zz_launch_status_insert_guard ON brands;
DROP TRIGGER zz_launch_status_insert_guard ON tenants;

ALTER TABLE tenants ALTER COLUMN status SET DEFAULT 'active';
ALTER TABLE brands ALTER COLUMN status SET DEFAULT 'active';

ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended', 'closed'));
ALTER TABLE brands DROP CONSTRAINT brands_status_check;
ALTER TABLE brands ADD CONSTRAINT brands_status_check CHECK (status IN ('active', 'suspended', 'closed'));

DROP FUNCTION launch_request_payload_hash(launch_authorisation_requests);
DROP TABLE launch_status_transitions;
DROP TABLE launch_authorisation_approvals;
DROP TABLE launch_authorisation_requests;

DROP FUNCTION launch_subject_status_guard();
DROP FUNCTION launch_subject_owner_provisioned();
DROP FUNCTION launch_subject_insert_guard();
DROP FUNCTION launch_transitions_match_subject_at_commit();
DROP FUNCTION launch_transitions_guard();
DROP FUNCTION launch_approvals_guard();
DROP FUNCTION launch_requests_not_executing_at_commit();
DROP FUNCTION launch_requests_guard();
DROP FUNCTION launch_actor_session();
DROP FUNCTION launch_deny_mutation();
DROP FUNCTION launch_action_move_legal(text, text, text);
DROP FUNCTION launch_status_move_legal(text, text);
DROP FUNCTION launch_is_table_owner();
DROP FUNCTION launch_table_owner();
DROP FUNCTION launch_uuid_array_distinct(uuid[]);
