-- Stage 3C hardening (directive item 4): the reconciliation sweep must
-- detect "missing projection" as a condition distinct from "unexpected
-- projection balance" / "ledger/projection mismatch", per reconciliation-
-- model.md's own three-way distinction. Stage 3B's schema recorded every
-- discrepancy identically via expected_value/actual_value free text with
-- no machine-readable classification - this column makes the distinction
-- an explicit, queryable fact rather than something an operator has to
-- infer by parsing actual_value.

ALTER TABLE reconciliation_mismatches
    ADD COLUMN mismatch_kind TEXT NOT NULL DEFAULT 'balance_mismatch'
        CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch'));

CREATE INDEX idx_reconciliation_mismatches_kind ON reconciliation_mismatches (tenant_id, mismatch_kind);
