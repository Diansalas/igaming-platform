-- Synthetic seed for the local backup/restore drill (deploy/drill/backup-restore-drill.sh).
--
-- SYNTHETIC DATA ONLY: fake tenants and players, fixed ids, unusable password hashes, no real
-- customer data. Runs as the table-owner role against a PRIVATE scratch database only (the
-- driver script refuses any other database name). The owner provisions tenants/brands directly
-- as 'active' (the ADR 0112 section 4.6 owner_provisioned path); that is legitimate only on a
-- synthetic database such as this one.
--
-- Volume: psql variable seed_deposits (deposits per wallet, default 50) via
--   psql -v seed_deposits=200 -f seed.sql
-- Everything is posted through the real triggers (ledger_entries_balanced, the balance
-- projection trigger, immutability and guard triggers), not by writing the projection.
\set ON_ERROR_STOP on
\if :{?seed_deposits}
\else
\set seed_deposits 50
\endif
SELECT set_config('drill.seed_deposits', :'seed_deposits', false);

-- Session-local helper (pg_temp; the owner role holds TEMPORARY): get-or-create a ledger account.
CREATE FUNCTION pg_temp.drill_acct(t uuid, w uuid, p uuid, atype text, asset text) RETURNS uuid
LANGUAGE plpgsql AS $f$
DECLARE a uuid;
BEGIN
    SELECT id INTO a FROM ledger_accounts
     WHERE tenant_id = t AND account_type = atype AND asset_code = asset
       AND wallet_id IS NOT DISTINCT FROM w;
    IF a IS NULL THEN
        INSERT INTO ledger_accounts (tenant_id, wallet_id, player_account_id, account_type, asset_code, status)
        VALUES (t, w, p, atype, asset, CASE WHEN w IS NULL THEN 'active' END) RETURNING id INTO a;
    END IF;
    RETURN a;
END
$f$;

-- One call == one transaction == one tenant scope: the deferred commit-time ledger triggers
-- check the session's tenant scope AT COMMIT, so a tenant must not be seeded across a scope switch.
CREATE FUNCTION pg_temp.drill_seed_tenant(ti int) RETURNS void
LANGUAGE plpgsql AS $seed$
DECLARE
    n_dep    int := current_setting('drill.seed_deposits')::int;
    t        uuid := ('00000000-0000-0000-0000-0000000000a' || ti::text)::uuid;
    b        uuid;
    p        uuid;
    person   uuid;
    w        uuid;
    acct     uuid;
    house    uuid;
    tx       uuid;
    pi       int;
    asset    text;
    di       int;
    amount   numeric;
BEGIN
    BEGIN

        -- tenant rows are platform scope
        PERFORM set_config('app.tenant_id', '', true);
        PERFORM set_config('app.platform_admin_principal_id', '00000000-0000-0000-0000-00000000d001', true);
        INSERT INTO tenants (id, name, slug, licensing_model, status)
        VALUES (t, 'Drill Tenant ' || ti, 'drill-' || ti, 'under_platform_licence', 'active')
        ON CONFLICT (id) DO NOTHING;

        -- everything below is tenant scope
        PERFORM set_config('app.platform_admin_principal_id', '', true);
        PERFORM set_config('app.tenant_id', t::text, true);

        b := ('00000000-0000-0000-0000-0000000001' || lpad(ti::text, 2, '0'))::uuid;
        INSERT INTO brands (id, tenant_id, name, slug, status)
        VALUES (b, t, 'Drill Brand ' || ti, 'drill-brand-' || ti, 'active')
        ON CONFLICT (id) DO NOTHING;

        FOR pi IN 1..3 LOOP
            p := ('00000000-0000-0000-0000-0000000002' || lpad(ti::text, 1, '0') || lpad(pi::text, 1, '0'))::uuid;
            person := gen_random_uuid();
            INSERT INTO persons (id) VALUES (person);
            INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash)
            VALUES (p, t, b, person, 'drill-player-' || ti || '-' || pi || '@example.invalid', 'drill-not-a-real-hash');

            FOREACH asset IN ARRAY (CASE WHEN pi = 1 THEN ARRAY['EUR','USD'] ELSE ARRAY['EUR'] END) LOOP
                INSERT INTO wallets (tenant_id, brand_id, player_account_id, asset_code)
                VALUES (t, b, p, asset) RETURNING id INTO w;

                acct  := pg_temp.drill_acct(t, w, p, 'player_cash', asset);
                house := pg_temp.drill_acct(t, NULL, NULL, 'psp_clearing', asset);

                FOR di IN 1..n_dep LOOP
                    amount := 1000 + (di * 37 % 9000);
                    INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key,
                                                     provider_id, provider_tx_id, correlation_id)
                    VALUES (t, 'deposit', 'drill-' || w || '-' || di, 'drill-psp', 'drill-' || w || '-' || di, gen_random_uuid())
                    RETURNING id INTO tx;
                    INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
                    VALUES (tx, house, t, asset, 'debit',  amount),
                           (tx, acct,  t, asset, 'credit', amount);
                END LOOP;
            END LOOP;
        END LOOP;
    END;
END
$seed$;

SELECT pg_temp.drill_seed_tenant(1);
SELECT pg_temp.drill_seed_tenant(2);
