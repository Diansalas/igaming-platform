-- Reverts 0153 (MANUAL-ADJ-LINK-1). Removes the two session-independent
-- manual_adjustment link triggers and their functions, restoring the 0151
-- schema exactly. No row is created, changed or deleted by either direction:
-- the fence only refuses future inserts, so the down cannot strand or
-- corrupt data (every manual_adjustment already on the ledger stays, and the
-- detective reconciliation kind ledger_unlinked_manual_adjustment keeps
-- covering the period without the fence). Idempotent.
--
-- WARNING (operational, not enforced): after this down, a non-acting session
-- can again post an unlinked manual_adjustment. Run it only together with a
-- rollback of the code that relies on the fence, and keep the detector's P1
-- routing live.

DROP TRIGGER IF EXISTS ledger_entries_manual_adjustment_link ON ledger_entries;
DROP TRIGGER IF EXISTS ledger_transactions_manual_adjustment_link ON ledger_transactions;
DROP FUNCTION IF EXISTS ledger_manual_adjustment_entry_check();
DROP FUNCTION IF EXISTS ledger_manual_adjustment_link_check();
