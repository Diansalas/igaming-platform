-- One more committed synthetic deposit in the SOURCE database AFTER the backup/clone was taken.
-- Purpose: make the RPO of a point-in-time snapshot concrete. The restored copy must NOT contain
-- this row; that difference (1 ledger transaction, 2 entries) is exactly the data a snapshot-only
-- strategy loses. RPO = 0 needs continuous WAL archiving / managed PITR (AWS-gated, NOT MET).
\set ON_ERROR_STOP on
BEGIN;
SELECT set_config('app.tenant_id', '00000000-0000-0000-0000-0000000000a1', true);
WITH w AS (
    SELECT wl.id AS wallet_id, wl.player_account_id AS pa, wl.asset_code AS asset
      FROM wallets wl WHERE wl.tenant_id = '00000000-0000-0000-0000-0000000000a1' ORDER BY wl.created_at, wl.id LIMIT 1
), acct AS (
    SELECT la.id AS player_acct, w.asset,
           (SELECT h.id FROM ledger_accounts h WHERE h.tenant_id = '00000000-0000-0000-0000-0000000000a1'
               AND h.account_type = 'psp_clearing' AND h.asset_code = w.asset AND h.wallet_id IS NULL) AS house_acct
      FROM w JOIN ledger_accounts la ON la.wallet_id = w.wallet_id AND la.account_type = 'player_cash'
), tx AS (
    INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 'deposit', 'drill-post-backup-1', 'drill-psp', 'drill-post-backup-1', gen_random_uuid())
    RETURNING id
)
INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
SELECT tx.id, x.acct, '00000000-0000-0000-0000-0000000000a1', acct.asset, x.dir, 12345
  FROM tx, acct, LATERAL (VALUES (acct.house_acct, 'debit'), (acct.player_acct, 'credit')) AS x(acct, dir);
COMMIT;
