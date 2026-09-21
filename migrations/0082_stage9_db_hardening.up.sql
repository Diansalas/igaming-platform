-- Stage 9 (Production Readiness) database hardening, bundled per the
-- `architect` DB-hardening audit's own recommendation: several additive,
-- reversible, mechanically-similar fixes in ONE migration rather than one
-- migration each. Nothing here alters an existing column's type, an
-- existing RLS policy, or an existing CHECK constraint, and nothing here
-- rewrites historical data - every statement either adds a trigger, adds
-- an index, or tightens a foreign key from "some brand in this tenant" to
-- "this player's OWN brand".
--
-- Deliberately NOT in this migration (recorded here so the omissions are
-- visible rather than forgotten):
--   - The ledger_transactions.transaction_type / ledger_accounts.
--     account_type CHECK-widening NOT VALID/VALIDATE binding rule -
--     separately tracked for 0083+.
--   - ARCH-DB-2 (casino_games / sb_* RLS backstop) - cross-domain, not a
--     schema tweak; the architect explicitly ruled it out of this window.
--   - player_restrictions' composite brand pinning - see section 2's
--     closing note; it is NOT the same-shape fix as the other seven and
--     is already partly done (migration 0038).


-- ===========================================================================
-- 1. Immutability + TRUNCATE-deny triggers
-- ===========================================================================
--
-- Rationale (identical for every table in this section, stated once):
-- CLAUDE.md's "Financial / ledger rules" require append-only, auditable
-- evidence enforced by the DATABASE, not by application discipline. Every
-- one of these tables' RLS policies is `FOR ALL`, so RLS itself permits a
-- tenant-staff-scoped UPDATE. A row-level trigger (UPDATE/DELETE) and a
-- statement-level trigger (TRUNCATE, which row-level triggers never fire
-- on) are BOTH required - migration 0021's own "Append-only enforcement"
-- comment, and ADR 0013's Stage 2 correction for audit_log. REVOKE is not
-- an option: the application's runtime role owns these tables and a table
-- owner can re-GRANT itself any privilege.
--
-- The TRUNCATE-deny triggers below all bind the SHARED
-- ledger_deny_mutation() function (migration 0021), which is generic over
-- TG_TABLE_NAME/TG_OP - this codebase's established convention (migrations
-- 0022, 0026, 0044, 0053-0063, 0068, 0069 all reuse it rather than
-- defining a per-table copy). Only the column-level "these columns are
-- frozen, those are not" guards need a table-specific function.


-- 1.1 sportsbook_bets
--
-- Had NO immutability trigger and NO TRUNCATE deny at all, unlike every
-- structural sibling it was explicitly modeled on (casino_launch_sessions
-- migration 0036, withdrawal_requests migration 0026, casino_provider_
-- rounds migration 0080). Its tenant_staff_scope policy is FOR ALL, so an
-- UPDATE could retroactively rewrite stake_amount, the frozen-at-
-- acceptance odds, player_account_id/wallet_id, idempotency_key, or
-- ledger_transaction_id AFTER placement - silently falsifying a bet
-- against its own append-only ledger posting, which cannot be edited to
-- match. That is a bet record and a ledger record disagreeing with no
-- trace, i.e. exactly the class of thing the ledger's immutability exists
-- to make impossible.
--
-- `status` is deliberately left MUTABLE: migration 0078's own column
-- comment declares the full settlement lifecycle ('open' ->
-- 'settled_won'/'settled_lost'/'void') that a future settlement stage
-- will write. Freezing it here would block that stage with no benefit -
-- status carries no money, the stake and odds that determine the money do.
--
-- provider_id/provider_bet_reference get the "mutable until first set,
-- then frozen" treatment casino_provider_rounds.provider_session_id gets
-- (migration 0080): both are NULL on every row today (migration 0081's
-- own header), so a future adapter must be able to record them once, but
-- never to repoint an accepted bet at a different provider reference.
-- Migration 0081's symmetric-null CHECK means the pair moves together.
CREATE FUNCTION sportsbook_bets_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.selection_id IS DISTINCT FROM OLD.selection_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.stake_amount IS DISTINCT FROM OLD.stake_amount
        OR NEW.odds_numerator IS DISTINCT FROM OLD.odds_numerator
        OR NEW.odds_denominator IS DISTINCT FROM OLD.odds_denominator
        OR NEW.potential_return IS DISTINCT FROM OLD.potential_return
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.ledger_transaction_id IS DISTINCT FROM OLD.ledger_transaction_id
        OR NEW.placed_at IS DISTINCT FROM OLD.placed_at
    THEN
        RAISE EXCEPTION 'sportsbook_bets: identity/stake/odds/idempotency columns are immutable after insert';
    END IF;
    IF OLD.provider_id IS NOT NULL
        AND (NEW.provider_id IS DISTINCT FROM OLD.provider_id
             OR NEW.provider_bet_reference IS DISTINCT FROM OLD.provider_bet_reference)
    THEN
        RAISE EXCEPTION 'sportsbook_bets: the provider reference is immutable once set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER sportsbook_bets_immutable_fields
    BEFORE UPDATE ON sportsbook_bets
    FOR EACH ROW EXECUTE FUNCTION sportsbook_bets_enforce_immutable_fields();

CREATE TRIGGER sportsbook_bets_no_truncate
    BEFORE TRUNCATE ON sportsbook_bets
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();


-- 1.2 deposit_intents
--
-- The mirror image of withdrawal_requests, which has had an immutability
-- trigger since migration 0026; deposit_intents never got one. Same
-- exposure: amount/asset/wallet/player/brand/idempotency_key are what the
-- posted LedgerTransaction was derived from, and the ledger cannot be
-- edited to follow a retroactive change here.
--
-- The mutable set was determined from internal/payments/orchestrator.go's
-- ACTUAL write paths, not assumed by analogy with sportsbook_bets:
--
--   - status, updated_at: the workflow's own state (pending -> succeeded/
--     declined/ambiguous/failed).
--   - provider_id AND provider_reference stay FULLY mutable - NOT
--     "immutable once set". This is the one place where copying
--     sportsbook_bets' treatment would have been wrong: handleDecline()
--     CASCADES a cascadable decline to the NEXT provider on the SAME
--     intent row (orchestrator.go's maxCascadeDepth loop), and the retry
--     calls setIntentAttempt() again with a DIFFERENT providerID and a
--     different reference. Freezing them on first write would break
--     payment failover outright. Cross-attempt integrity is instead
--     carried by migration 0025's existing partial unique index on
--     (tenant_id, provider_id, provider_reference).
--   - ledger_transaction_id IS frozen once set. It is written by exactly
--     one statement in the whole codebase (postDepositSuccess), and a
--     deposit that has posted to the ledger must never be repointed at a
--     different transaction - that would relabel which money this deposit
--     was. A same-value rewrite (idempotent replay) still passes, since
--     the guard uses IS DISTINCT FROM.
CREATE FUNCTION deposit_intents_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'deposit_intents: amount/asset/wallet/player/tenant/brand/payment_method/idempotency_key/created_at are immutable after insert';
    END IF;
    IF OLD.ledger_transaction_id IS NOT NULL
        AND NEW.ledger_transaction_id IS DISTINCT FROM OLD.ledger_transaction_id
    THEN
        RAISE EXCEPTION 'deposit_intents: ledger_transaction_id is immutable once set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deposit_intents_immutable_fields
    BEFORE UPDATE ON deposit_intents
    FOR EACH ROW EXECUTE FUNCTION deposit_intents_enforce_immutable_fields();

CREATE TRIGGER deposit_intents_no_truncate
    BEFORE TRUNCATE ON deposit_intents
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();


-- 1.3 reconciliation_runs
--
-- These rows are the evidence CLAUDE.md's "reconciliation-capable,
-- auditable" claim and its "any non-zero drift is a P1 incident" rule
-- rest on. A reconciliation result that can be edited after the fact is
-- not evidence: a clean/mismatches_found verdict, the period it covers,
-- and when it ran are the entire content of the claim "we checked".
--
-- internal/reconciliation writes reconciliation_runs with a single INSERT
-- and NEVER updates or deletes one (verified against reconciliation.go's
-- full write surface), so this table takes the STRONGEST available guard
-- - the same total append-only pair ledger_transactions itself uses,
-- not a per-column allowlist.
CREATE TRIGGER reconciliation_runs_immutable
    BEFORE UPDATE OR DELETE ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER reconciliation_runs_no_truncate
    BEFORE TRUNCATE ON reconciliation_runs
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();


-- 1.4 reconciliation_mismatches
--
-- Unlike reconciliation_runs this table has a genuine, legitimate update
-- path: ResolveMismatch() (reconciliation.go) sets investigation_status,
-- resolution_note, resolved_by, resolved_at and
-- correction_ledger_transaction_id. So the guard is column-level, not
-- total.
--
-- What is frozen is the EVIDENCE: which run found it, what key it was
-- found under, what was expected, what was actually there, what kind of
-- mismatch it is, and when it was recorded. Those five columns are the
-- drift finding itself. Being able to edit expected_value/actual_value
-- after the fact would let a P1 be made to look like it never happened,
-- which is worse than the drift.
--
-- correction_ledger_transaction_id is deliberately left mutable rather
-- than frozen-once-set: migration 0027's own column comment is explicit
-- that a resolution changes the ledger EXCLUSIVELY via a compensating
-- LedgerTransaction, so this column is a back-reference to an
-- independently immutable row, and an investigation that is reopened and
-- re-resolved (investigation_status is a free-moving enum) must be able
-- to name the second compensating transaction.
CREATE FUNCTION reconciliation_mismatches_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.reconciliation_run_id IS DISTINCT FROM OLD.reconciliation_run_id
        OR NEW.reconciliation_key IS DISTINCT FROM OLD.reconciliation_key
        OR NEW.expected_value IS DISTINCT FROM OLD.expected_value
        OR NEW.actual_value IS DISTINCT FROM OLD.actual_value
        OR NEW.mismatch_kind IS DISTINCT FROM OLD.mismatch_kind
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'reconciliation_mismatches: the recorded drift finding (run/key/expected/actual/kind/created_at) is immutable after insert';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER reconciliation_mismatches_immutable_fields
    BEFORE UPDATE ON reconciliation_mismatches
    FOR EACH ROW EXECUTE FUNCTION reconciliation_mismatches_enforce_immutable_fields();

CREATE TRIGGER reconciliation_mismatches_no_truncate
    BEFORE TRUNCATE ON reconciliation_mismatches
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();


-- 1.5 login_attempts
--
-- The brute-force / credential-stuffing forensic record, and the backing
-- store lockout decisions are computed from (internal/identity/
-- login_attempt.go counts failures since the last success). A mutable
-- login_attempts row is both an erasable attack trail AND a live lockout
-- bypass: flipping succeeded=false to true on one row resets the failure
-- window for the whole identifier.
--
-- Verified append-only in fact, not just in intent: internal/identity is
-- the only writer and issues exactly one INSERT plus two SELECTs; there
-- is no UPDATE, no DELETE and no retention/purge job anywhere in the tree
-- (a future retention policy will need its own migration to relax this,
-- which is the correct place for that decision to be visible).
CREATE TRIGGER login_attempts_immutable
    BEFORE UPDATE OR DELETE ON login_attempts
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER login_attempts_no_truncate
    BEFORE TRUNCATE ON login_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();


-- 1.6 ledger_accounts (HR-15, known debt, confirmed still open)
--
-- ledger_accounts is the one ledger table with NO update guard at all:
-- ledger_transactions and ledger_entries have been fully append-only
-- since migrations 0021/0022, but the ACCOUNTS those entries post to
-- could be repointed. Repointing a ledger_account's wallet_id or
-- account_type retroactively rewrites the meaning of every historical
-- entry against it - a player_cash account silently becoming
-- house_gaming, or moving to another player's wallet, without touching a
-- single (immutable) ledger_entries row. Every balance, every
-- reconciliation recompute and every invariant that aggregates by
-- account_type (B1's BONUS_SET, L1's locked-origin determinacy) would
-- then be computed over a falsified grouping while SUM(DEBITS) ==
-- SUM(CREDITS) still held perfectly.
--
-- `status` is the sole mutable column, deliberately: migration 0020's own
-- column comment defines it as the house-level account's lifecycle
-- (active/frozen/closed), NULL for player-owned accounts whose effective
-- status is always read from the owning Wallet. Freezing it would make a
-- house account impossible to freeze, which is a control we want to keep.
-- Note there is no Go writer for it today either way.
--
-- player_account_id is frozen along with wallet_id even though migration
-- 0020's BEFORE INSERT trigger populates it: that trigger is INSERT-only,
-- so without this guard the denormalized copy could be edited out of
-- agreement with its wallet, and it is an RLS key (player_self_scope
-- reads it directly).
CREATE FUNCTION ledger_accounts_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.account_type IS DISTINCT FROM OLD.account_type
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'ledger_accounts: tenant/wallet/player/account_type/asset_code/created_at are immutable after insert (only status is mutable)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_accounts_immutable_fields
    BEFORE UPDATE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_enforce_immutable_fields();

CREATE TRIGGER ledger_accounts_no_truncate
    BEFORE TRUNCATE ON ledger_accounts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();


-- ===========================================================================
-- 2. ARCH-DB-3: composite brand pinning
-- ===========================================================================
--
-- Same class of fix as Stage 6.1's sportsbook_bets fix (migration 0078)
-- and Stage 7's casino_launch_sessions fix (migration 0079), applied to
-- the remaining tenant-owned, player-owned tables that still pin brand_id
-- only to "some brand in this tenant" via
-- FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id) -
-- never to the player's OWN brand. Every one of them is replaced by the
-- composite FOREIGN KEY (player_account_id, tenant_id, brand_id)
-- REFERENCES player_accounts (id, tenant_id, brand_id), reusing the
-- player_accounts_id_tenant_brand_key unique key migration 0019 already
-- added, exactly as migrations 0079 and 0080 do.
--
-- Not reachable through the application today (every one of these write
-- paths derives brand_id server-side from the player's own account), but
-- CLAUDE.md's rule is that integrity is a database fact, not application
-- discipline - and these are the rows a misattributed brand would show up
-- in: a withdrawal, a deposit, a bonus disposition, a bonus progress
-- entry, a KYC verification/document, a jurisdiction resolution.

ALTER TABLE withdrawal_requests DROP CONSTRAINT withdrawal_requests_brand_id_tenant_id_fkey;
ALTER TABLE withdrawal_requests
    ADD CONSTRAINT withdrawal_requests_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

ALTER TABLE deposit_intents DROP CONSTRAINT deposit_intents_brand_id_tenant_id_fkey;
ALTER TABLE deposit_intents
    ADD CONSTRAINT deposit_intents_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

ALTER TABLE bonus_held_dispositions DROP CONSTRAINT bonus_held_dispositions_brand_id_tenant_id_fkey;
ALTER TABLE bonus_held_dispositions
    ADD CONSTRAINT bonus_held_dispositions_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

ALTER TABLE bonus_grant_progress DROP CONSTRAINT bonus_grant_progress_brand_id_tenant_id_fkey;
ALTER TABLE bonus_grant_progress
    ADD CONSTRAINT bonus_grant_progress_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

ALTER TABLE kyc_verifications DROP CONSTRAINT kyc_verifications_brand_id_tenant_id_fkey;
ALTER TABLE kyc_verifications
    ADD CONSTRAINT kyc_verifications_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

ALTER TABLE kyc_documents DROP CONSTRAINT kyc_documents_brand_id_tenant_id_fkey;
ALTER TABLE kyc_documents
    ADD CONSTRAINT kyc_documents_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

-- jurisdiction_resolutions is the ONE table in this section where the
-- loose brands FK is KEPT rather than replaced, and the composite is
-- ADDED alongside it. Both brand_id and player_account_id are NULLABLE
-- here by design (migration 0071: a resolution may be tenant-scoped,
-- brand-scoped, or player-scoped). Under Postgres's MATCH SIMPLE default
-- a composite FK is satisfied trivially the moment ANY of its columns is
-- NULL - so on a brand-scoped, player-NULL row the composite would not
-- fire at all, and dropping the brands FK would have SILENTLY REMOVED
-- that row's only brand->tenant check instead of strengthening it (the
-- same nullable-composite-FK trap migrations 0020 and 0022 both record).
-- Keeping both gives: brand->tenant always, plus brand == the player's
-- own brand whenever a row names both. Verified safe against both live
-- callers (internal/casino LaunchGame and internal/bonus eligibility),
-- which pass BrandID and PlayerAccountID derived from the same player
-- account.
ALTER TABLE jurisdiction_resolutions
    ADD CONSTRAINT jurisdiction_resolutions_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);

-- player_restrictions is deliberately NOT changed here. Two findings from
-- reading it (migration 0037) rather than assuming:
--
--   1. The audit's premise that it "uses a bare player_account_id UUID
--      REFERENCES player_accounts (id) with no tenant composite at all"
--      is stale. Migration 0038 already added
--      player_restrictions_player_account_id_tenant_id_fkey for exactly
--      this reason, with the identical MATCH SIMPLE reasoning. The bare
--      single-column FK survives alongside it, redundantly but harmlessly.
--
--   2. Widening it to (player_account_id, tenant_id, brand_id) would be
--      semantically WRONG, not merely bigger. On this table brand_id is
--      the ADMINISTRATIVE SCOPE of the restriction (migration 0037's own
--      column comment: NULL tenant = platform-wide, tenant set + brand
--      NULL = tenant-wide, both set = that one brand), while
--      player_account_id is provenance only - "the account the
--      restriction was administered THROUGH", explicitly "never itself
--      the enforcement key" (person_id is). The two are not required to
--      agree: a self-exclusion scoped to brand B2 may legitimately be
--      administered through the person's account on brand B1. Pinning
--      them together would reject valid rows.
--
-- Recorded as a separate follow-up rather than forced into this
-- migration's shape.


-- ===========================================================================
-- 3. Pagination indexes (audit §26 item 2)
-- ===========================================================================
--
-- All three tables carried only a single-column (tenant_id) index, which
-- is useless for the ORDER BY <time> DESC pagination the back office and
-- player history actually issue - and in a single-tenant-dominant B2C
-- model that index is near-zero-selectivity anyway, so the planner sorts
-- the whole partition. Mirrors idx_audit_log_tenant_time (migration 0014),
-- with an id tiebreak on the two tables whose timestamp is not unique
-- enough to give a stable keyset-pagination order on its own.
--
-- Plain CREATE INDEX, not CONCURRENTLY: CONCURRENTLY cannot run inside a
-- transaction block, and this repo's migration runner is transactional.
-- These tables are small at current (pre-production, synthetic-data)
-- scale, so a brief ACCESS SHARE-blocking build is fine here; if that
-- ever stops being true the answer is a non-transactional migration
-- runner, not a CONCURRENTLY statement this runner cannot execute.
CREATE INDEX idx_casino_launch_sessions_tenant_time
    ON casino_launch_sessions (tenant_id, created_at DESC, id DESC);

CREATE INDEX idx_sportsbook_bets_tenant_time
    ON sportsbook_bets (tenant_id, placed_at DESC, id DESC);

CREATE INDEX idx_ledger_transactions_tenant_time
    ON ledger_transactions (tenant_id, created_at DESC);
