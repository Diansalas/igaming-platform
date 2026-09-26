-- Reverses migration 0092. Always safe at any time: this only drops the
-- backstop index, never any data, and the primary control (the L2 lock
-- and re-check in internal/payments/orchestrator.go) is unaffected by
-- whether this index exists.
DROP INDEX IF EXISTS ledger_transactions_one_deposit_reversal;
