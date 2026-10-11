-- Reverses 0131 (ADR 0112 slice 4; DEV/STAGING ONLY, per the migrate-down policy).
--
-- WARNING: after this, the creation of a withdrawal request on a non-active tenant or brand is again
-- enforced by the Go layer only (H-SEC-11): a raw INSERT by the runtime credential is accepted by the
-- database (slice-3 residual 14(b) returns). No row is touched; withdrawal requests created while the
-- guard was applied are unaffected.

DROP TRIGGER withdrawal_requests_aa_subject_active_guard ON withdrawal_requests;
DROP FUNCTION withdrawal_requests_subject_active_guard();
