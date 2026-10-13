-- Reverses 0142 (ADR 0116 S3 part 2; DEV/STAGING ONLY, per the migrate-down policy).
--
-- REFUSES (AM099) while any AML case row exists (alerts, cases, case alerts, notes, events): case history is compliance
-- history and is never deleted to make a down pass. It restores 0141's aml_hold_lifts_guard() body verbatim, drops the
-- approval-event FK, and removes exactly what 0142 created. FORCE RLS is lifted on the case tables only to count their
-- rows (a raise rolls the whole transaction back; on success the tables are dropped).
-- Returns the schema to 0141 exactly (whole-schema snapshot test: internal/aml/migration_0142_integration_test.go).

DO $$
BEGIN
    ALTER TABLE aml_alerts NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_cases NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_case_alerts NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_case_notes NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_case_events NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM aml_alerts) OR EXISTS (SELECT 1 FROM aml_cases) OR EXISTS (SELECT 1 FROM aml_case_alerts)
       OR EXISTS (SELECT 1 FROM aml_case_notes) OR EXISTS (SELECT 1 FROM aml_case_events) THEN
        RAISE EXCEPTION 'migration 0142 down refused: AML case history exists (append-only compliance history)' USING ERRCODE = 'AM099';
    END IF;
END
$$;

ALTER TABLE aml_hold_lifts DROP CONSTRAINT aml_hold_lifts_approval_event_fkey;
CREATE OR REPLACE FUNCTION aml_hold_lifts_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hold  record;
    v_actor record;
BEGIN
    SELECT h.tenant_id, h.owner_scope, h.reason_class, h.placed_at INTO v_hold FROM public.aml_holds h WHERE h.id = NEW.hold_id;
    IF NOT FOUND OR v_hold.tenant_id <> NEW.tenant_id THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    IF NEW.owner_scope <> v_hold.owner_scope THEN
        RAISE EXCEPTION 'aml: only the hold''s owner scope may lift it' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class = 'confirmed_sanctions' AND NEW.approval_event_id IS NULL THEN
        RAISE EXCEPTION 'aml: lifting a confirmed sanctions hold needs a proof-bound four-eyes approval' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class <> 'confirmed_sanctions' AND NEW.approval_event_id IS NOT NULL THEN
        RAISE EXCEPTION 'aml: an approval event is only valid for a confirmed sanctions lift' USING ERRCODE = 'AM006';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.lifted_by IS DISTINCT FROM v_actor.actor OR NEW.lifted_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.lifted_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the lifting actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;

DROP FUNCTION aml_person_has_records(uuid, uuid);
DROP FUNCTION aml_case_queue(uuid, integer);
DROP FUNCTION aml_case_notes_list(uuid, uuid);
DROP FUNCTION aml_case_events_list(uuid, uuid);
DROP FUNCTION aml_case_read(uuid, uuid);
DROP FUNCTION aml_case_approve(uuid, uuid, integer, text, text);
DROP FUNCTION aml_case_propose(uuid, uuid, integer, text, uuid, text, text);
DROP FUNCTION aml_case_flag_sar(uuid, uuid, integer, text);
DROP FUNCTION aml_case_add_note(uuid, uuid, integer, text, text);
DROP FUNCTION aml_case_transition(uuid, uuid, integer, text, text);
DROP FUNCTION aml_case_lock_for_write(uuid, uuid, text, uuid, integer);
DROP FUNCTION aml_case_open(uuid, uuid, text, text, uuid[]);
DROP FUNCTION aml_alert_raise(uuid, uuid, text, text);
DROP TABLE aml_case_events;
DROP TABLE aml_case_alerts;
DROP TABLE aml_case_notes;
DROP TABLE aml_cases;
DROP TABLE aml_alerts;
DROP FUNCTION aml_case_events_guard();
DROP FUNCTION aml_case_notes_guard();
DROP FUNCTION aml_case_alerts_guard();
DROP FUNCTION aml_cases_guard();
DROP FUNCTION aml_alerts_guard();
DROP FUNCTION aml_proposal_content_hash(uuid, uuid, integer, text, uuid, text, text, uuid);
DROP FUNCTION aml_case_transition_allowed(text, text);
DROP FUNCTION aml_case_sar_flagged(uuid);
DROP FUNCTION aml_case_last_seq(uuid);
DROP FUNCTION aml_case_status(uuid);
