-- PRH-I5 security condition C2 (docs/plans/payment-readiness/
-- rv-prh-i5-security.md; ADR 0095 §12.7.1): the database half of the
-- statement-line charset rule the Go fetch validation already enforces.
--
--   payment_statement_lines.merchant_reference: the providerref rule (no C0,
--     DEL or C1 control character - PostgreSQL text cannot hold NUL at all)
--     and 1..64 bytes;
--   payment_statement_lines.asset_code: ^[A-Z0-9]{1,16}$ (the Asset registry's
--     code shape).
--
-- Migration number 0104 is allocated by the orchestrator (after 0103, ADR
-- 0096's KYC supersession).
--
-- Pre-flight: counts violating rows per column and aborts, naming the
-- counts, BEFORE any constraint is added. It never modifies a row (the
-- table is append-only anyway). payment_statement_lines carries FORCE ROW
-- LEVEL SECURITY and the migration connection has no app.tenant_id, so a
-- plain count(*) would see zero rows: the pre-flight iterates tenants and
-- sets app.tenant_id for each (migration 0099's pattern), never toggling
-- FORCE and never bypassing RLS.
DO $$
DECLARE
    tenant_rec RECORD;
    n_merchant BIGINT;
    n_asset BIGINT;
    total_merchant BIGINT := 0;
    total_asset BIGINT := 0;
BEGIN
    PERFORM set_config('app.player_account_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) FILTER (WHERE merchant_reference IS NOT NULL
                                  AND (octet_length(merchant_reference) NOT BETWEEN 1 AND 64
                                       OR merchant_reference ~ '[\x01-\x1F\x7F-\x9F]')),
               count(*) FILTER (WHERE asset_code !~ '^[A-Z0-9]{1,16}$')
          INTO n_merchant, n_asset
          FROM payment_statement_lines WHERE tenant_id = tenant_rec.id;
        total_merchant := total_merchant + n_merchant;
        total_asset := total_asset + n_asset;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF total_merchant + total_asset > 0 THEN
        RAISE EXCEPTION 'migration 0104 pre-flight: existing payment_statement_lines rows violate the statement-line charset rule: merchant_reference=% asset_code=%. Nothing was changed. The table is append-only: resolve under a reviewed data-correction procedure, then re-run.',
            total_merchant, total_asset;
    END IF;
END $$;

ALTER TABLE payment_statement_lines
    ADD CONSTRAINT payment_statement_lines_merchant_reference_charset
        CHECK (merchant_reference IS NULL
               OR (octet_length(merchant_reference) BETWEEN 1 AND 64
                   AND merchant_reference !~ '[\x01-\x1F\x7F-\x9F]')),
    ADD CONSTRAINT payment_statement_lines_asset_code_shape
        CHECK (asset_code ~ '^[A-Z0-9]{1,16}$');
