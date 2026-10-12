-- Reverses 0132 (ADR 0113 slice 1; DEV/STAGING ONLY, per the migrate-down policy).
--
-- WARNING: after this, the withdrawal state machine is enforced by the Go layer only again: the
-- runtime credential can INSERT a request directly as approved / submitted / completed, revive a
-- terminal request (requested -> cancelled -> requested, ADR 0112 14(b2), LAUNCH-BLOCKING), and post
-- a standalone second release leg against a hold (double pay) or a second hold leg (strand). No row is
-- touched (0132 changed no data and replaced no existing function); the schema returns to 0131 byte
-- for byte (whole-schema snapshot test).

DROP INDEX idx_withdrawal_requests_open_wallet_asset;
DROP TRIGGER ledger_transactions_withdrawal_hold_leg_at_commit ON ledger_transactions;
DROP FUNCTION ledger_transactions_withdrawal_hold_leg_at_commit();
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_player_types_have_wallet;
DROP TRIGGER ledger_entries_withdrawal_hold_link_at_commit ON ledger_entries;
DROP FUNCTION ledger_entries_withdrawal_hold_link_at_commit();
DROP TRIGGER ledger_entries_withdrawal_hold_guard ON ledger_entries;
DROP FUNCTION ledger_entries_withdrawal_hold_guard();
DROP TRIGGER withdrawal_requests_ledger_link_at_commit ON withdrawal_requests;
DROP FUNCTION withdrawal_requests_ledger_link_at_commit();
DROP FUNCTION withdrawal_request_ledger_link_violation(uuid, uuid);
DROP TRIGGER withdrawal_requests_aa_state_transition_guard ON withdrawal_requests;
DROP FUNCTION withdrawal_requests_state_transition_guard();
