-- Stage 8 provider-integration readiness (docs/decisions/0080 Decision 2).
-- Purely additive: no existing column, constraint, RLS policy, or index on
-- sportsbook_bets changes. Every row written by the current, same-process,
-- synchronous PlaceBet flow (internal/sportsbook/bets.go) - both existing
-- rows and every bet placed from this migration forward - has both new
-- columns NULL, since no external provider round-trip exists or is added
-- this stage (ADR 0080 Decision 2 / internal/sportsbook.Provider's own doc
-- comment). This is deliberately the minimum additive schema a future real
-- sportsbook adapter needs to record its own bet-acceptance reference,
-- without guessing that provider's actual bet-placement contract.

ALTER TABLE sportsbook_bets
    ADD COLUMN provider_id             TEXT,
    ADD COLUMN provider_bet_reference  TEXT,
    -- Mirrors ledger_transactions' identical symmetric-null constraint
    -- exactly (migration 0021_create_ledger_transactions.up.sql: "CHECK
    -- ((provider_id IS NULL) = (provider_tx_id IS NULL))") - a provider
    -- reference is either fully present (both columns set) or fully
    -- absent (both NULL), never half-recorded.
    ADD CONSTRAINT sportsbook_bets_provider_reference_symmetric_null
        CHECK ((provider_id IS NULL) = (provider_bet_reference IS NULL));

-- Mirrors ledger_transactions' idx_ledger_transactions_tenant_provider_tx
-- partial unique index exactly: a provider's own bet-acceptance reference
-- is unique within (tenant_id, provider_id) once it exists, but rows with
-- no provider reference (every row this stage) never collide with each
-- other on NULL.
CREATE UNIQUE INDEX idx_sportsbook_bets_tenant_provider_reference
    ON sportsbook_bets (tenant_id, provider_id, provider_bet_reference)
    WHERE provider_id IS NOT NULL;
