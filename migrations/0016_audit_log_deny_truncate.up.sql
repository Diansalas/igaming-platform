-- Fix: migration 0014's audit_log_immutable trigger is BEFORE UPDATE OR
-- DELETE FOR EACH ROW. Row-level triggers do not fire on TRUNCATE -
-- Postgres requires a separate statement-level trigger for that. Since
-- the application's own database role owns audit_log (the whole premise
-- of using a trigger instead of REVOKE - see docs/decisions/0013), a
-- compromised application credential or a careless future migration
-- could otherwise erase the entire append-only audit trail with one
-- TRUNCATE, defeating the guarantee this table exists to provide. Caught
-- in Stage 2 security review.
--
-- audit_log_deny_mutation() already unconditionally raises regardless of
-- TG_OP, so it's reused as-is; only the trigger registration is new.

CREATE TRIGGER audit_log_deny_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_deny_mutation();
