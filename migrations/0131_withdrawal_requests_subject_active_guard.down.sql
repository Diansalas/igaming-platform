-- Reverses 0131 (ADR 0112 slice 4; DEV/STAGING ONLY, per the migrate-down policy).
--
-- WARNING: after this, the creation of a withdrawal request on a non-active tenant or brand is again
-- enforced by the Go layer only (H-SEC-11) and a withdrawal request can be deleted again by any role
-- holding DELETE (slice-3 residuals 14(b) and 14(b2) return). No row is touched; withdrawal requests
-- created while the guards were applied are unaffected.

DROP TRIGGER withdrawal_requests_aa_subject_active_guard ON withdrawal_requests;
DROP FUNCTION withdrawal_requests_subject_active_guard();
DROP TRIGGER withdrawal_requests_deny_delete ON withdrawal_requests;
DROP TRIGGER withdrawal_requests_deny_truncate ON withdrawal_requests;
DROP FUNCTION withdrawal_requests_deny_delete();
