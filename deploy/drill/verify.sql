-- Read-only verification for the local backup/restore drill (deploy/drill/backup-restore-drill.sh).
--
-- Run with:  psql -X -At -v ON_ERROR_STOP=1 -f verify.sql <database-url>
-- Output is line oriented and machine compared by the driver:
--   CHK|<name>|PASS|<detail>      an invariant that must hold in EVERY database checked
--   CHK|<name>|FAIL|<detail>      the invariant is violated (the driver exits non-zero)
--   FP|<name>|<value>             a fingerprint; the SOURCE and the RESTORED database must be equal
--   ROW|<table>|<scope>|<count>   non-zero row counts visible per RLS scope (diffed source vs restored)
--
-- It writes nothing to the database (only session-local pg_temp functions, which vanish at
-- disconnect). It runs as the table-owner role, which is subject to FORCE ROW LEVEL SECURITY, so
-- counts and sums are taken once per tenant scope (set_config app.tenant_id, transaction-local)
-- plus once with no scope (platform / un-tenanted rows). A table whose rows are visible only
-- under some other scope (a staff or platform GUC) shows 0 here in BOTH databases; that
-- limitation is the price of not using a superuser, and is stated in the runbook.
\set ON_ERROR_STOP on

-- ---------------------------------------------------------------- helpers
CREATE FUNCTION pg_temp.drill_scopes() RETURNS TABLE (scope text) LANGUAGE sql AS $$
    SELECT id::text FROM tenants UNION ALL SELECT ''
$$;

-- Row counts of every public base table, per scope.
CREATE FUNCTION pg_temp.drill_counts() RETURNS TABLE (tbl text, scope text, n bigint)
LANGUAGE plpgsql AS $$
DECLARE
    s text;
    r record;
    c bigint;
BEGIN
    FOR s IN SELECT x.scope FROM pg_temp.drill_scopes() x LOOP
        PERFORM set_config('app.tenant_id', s, true);
        FOR r IN SELECT c2.relname FROM pg_class c2 JOIN pg_namespace n ON n.oid = c2.relnamespace
                  WHERE n.nspname = 'public' AND c2.relkind IN ('r', 'p') ORDER BY 1 LOOP
            EXECUTE format('SELECT count(*) FROM public.%I', r.relname) INTO c;
            tbl := r.relname; scope := s; n := c;
            RETURN NEXT;
        END LOOP;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;

-- Ledger invariant: per tenant and asset, SUM(debits) = SUM(credits).
CREATE FUNCTION pg_temp.drill_ledger_balance() RETURNS TABLE (tenant text, asset text, debits numeric, credits numeric)
LANGUAGE plpgsql AS $$
DECLARE
    s text;
BEGIN
    FOR s IN SELECT x.scope FROM pg_temp.drill_scopes() x WHERE x.scope <> '' LOOP
        PERFORM set_config('app.tenant_id', s, true);
        RETURN QUERY
        SELECT s, e.asset_code,
               COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
               COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)
          FROM ledger_entries e WHERE e.tenant_id = s::uuid GROUP BY e.asset_code;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;

-- Balance projection versus a recompute from ledger_entries, per tenant scope.
-- Returns one row per ledger account that exists in either side, with the mismatch flag.
CREATE FUNCTION pg_temp.drill_projection_diff() RETURNS TABLE (account uuid, matches boolean)
LANGUAGE plpgsql AS $$
DECLARE
    s text;
BEGIN
    FOR s IN SELECT x.scope FROM pg_temp.drill_scopes() x WHERE x.scope <> '' LOOP
        PERFORM set_config('app.tenant_id', s, true);
        RETURN QUERY
        WITH re AS (
            SELECT e.ledger_account_id AS a,
                   COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0)  AS d,
                   COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0) AS c
              FROM ledger_entries e WHERE e.tenant_id = s::uuid GROUP BY e.ledger_account_id
        ), pr AS (
            SELECT p.ledger_account_id AS a, p.debit_total AS d, p.credit_total AS c
              FROM wallet_balance_projection p WHERE p.tenant_id = s::uuid
        )
        SELECT COALESCE(re.a, pr.a),
               (re.d IS NOT DISTINCT FROM pr.d AND re.c IS NOT DISTINCT FROM pr.c)
          FROM re FULL JOIN pr ON re.a = pr.a;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END
$$;

-- ---------------------------------------------------------------- row counts
SELECT 'ROW|' || tbl || '|' || scope || '|' || n FROM pg_temp.drill_counts() WHERE n > 0 ORDER BY tbl, scope;
SELECT 'FP|rowcounts_md5|' || md5(COALESCE(string_agg(tbl || '|' || scope || '|' || n, ',' ORDER BY tbl, scope), ''))
  FROM pg_temp.drill_counts() WHERE n > 0;
SELECT 'CHK|rowcounts_nonempty|' || CASE WHEN count(*) > 0 THEN 'PASS' ELSE 'FAIL' END
       || '|tables_with_rows=' || count(DISTINCT tbl) || ' total_rows=' || COALESCE(sum(n), 0)
  FROM pg_temp.drill_counts() WHERE n > 0;

-- ---------------------------------------------------------------- ledger invariant
SELECT 'CHK|ledger_debits_eq_credits_per_tenant_asset|'
       || CASE WHEN count(*) > 0 AND count(*) FILTER (WHERE debits <> credits) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|tenant_asset_groups=' || count(*) || ' unbalanced=' || count(*) FILTER (WHERE debits <> credits)
  FROM pg_temp.drill_ledger_balance();
SELECT 'FP|ledger_sums_md5|' || md5(COALESCE(string_agg(tenant || '|' || asset || '|' || debits || '|' || credits, ',' ORDER BY tenant, asset), ''))
  FROM pg_temp.drill_ledger_balance();

-- ---------------------------------------------------------------- projection == recompute
SELECT 'CHK|balance_projection_equals_ledger_recompute|'
       || CASE WHEN count(*) > 0 AND count(*) FILTER (WHERE NOT matches) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|accounts=' || count(*) || ' mismatched=' || count(*) FILTER (WHERE NOT matches)
  FROM pg_temp.drill_projection_diff();

-- ---------------------------------------------------------------- RLS on every tenant table
SELECT 'CHK|rls_enabled_and_forced_on_all_tenant_tables|'
       || CASE WHEN count(*) > 0 AND count(*) FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity)) = 0
               THEN 'PASS' ELSE 'FAIL' END
       || '|tenant_tables=' || count(*) || ' not_enabled_and_forced='
       || count(*) FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity))
       || COALESCE(' [' || string_agg(c.relname, ',' ORDER BY c.relname)
                    FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity)) || ']', '')
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
 WHERE c.relkind IN ('r', 'p');

-- ---------------------------------------------------------------- migrations head
SELECT 'FP|migration_head|' || max(version) || '|count=' || count(*) FROM schema_migrations;
SELECT 'FP|migration_checksums_md5|' || md5(string_agg(version || ':' || checksum, ',' ORDER BY version)) FROM schema_migrations;

-- ---------------------------------------------------------------- runtime role pins (docs/security/runtime-role-separation.md)
-- The cluster-level role must exist; its database-level and object-level privileges live in THIS
-- database and are what a restore can silently lose.
SELECT 'CHK|runtime_role_exists_nonprivileged|'
       || CASE WHEN count(*) = 1 AND bool_and(NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
                                             AND NOT rolbypassrls AND NOT rolreplication)
               THEN 'PASS' ELSE 'FAIL' END || '|roles=' || count(*)
  FROM pg_roles WHERE rolname = 'igaming_runtime';
SELECT 'CHK|runtime_role_owns_nothing|'
       || CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END || '|owned_objects=' || count(*)
  FROM pg_class c WHERE c.relowner = (SELECT oid FROM pg_roles WHERE rolname = 'igaming_runtime');
SELECT 'CHK|runtime_role_has_connect|'
       || CASE WHEN has_database_privilege('igaming_runtime', current_database(), 'CONNECT') THEN 'PASS' ELSE 'FAIL' END
       || '|';
-- migration 0116: TEMPORARY revoked from PUBLIC and the runtime role. A pg_dump/pg_restore does
-- NOT carry database-level ACLs without --create, so this is the check that catches a restore
-- that "worked" but left the TEMP shadowing hole open.
SELECT 'CHK|runtime_role_cannot_create_temp_0116|'
       || CASE WHEN NOT has_database_privilege('igaming_runtime', current_database(), 'TEMPORARY')
                    AND NOT EXISTS (SELECT 1 FROM pg_database d,
                                          LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
                                     WHERE d.datname = current_database() AND a.grantee = 0
                                       AND a.privilege_type = 'TEMPORARY')
               THEN 'PASS' ELSE 'FAIL' END || '|';
SELECT 'CHK|runtime_role_no_schema_create|'
       || CASE WHEN NOT has_schema_privilege('igaming_runtime', 'public', 'CREATE')
                    AND has_schema_privilege('igaming_runtime', 'public', 'USAGE') THEN 'PASS' ELSE 'FAIL' END || '|';
SELECT 'CHK|runtime_role_no_truncate_trigger_references_ddl|'
       || CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END || '|offending_tables=' || count(*)
       || COALESCE(' [' || string_agg(c.relname, ',' ORDER BY c.relname) || ']', '')
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE c.relkind IN ('r', 'p')
   AND (has_table_privilege('igaming_runtime', c.oid, 'TRUNCATE')
        OR has_table_privilege('igaming_runtime', c.oid, 'TRIGGER')
        OR has_table_privilege('igaming_runtime', c.oid, 'REFERENCES'));
-- the explicit revokes the migrations (0091, 0097) and the CI harness apply
SELECT 'CHK|runtime_role_ledger_append_only_grants|'
       || CASE WHEN NOT has_table_privilege('igaming_runtime', 'sportsbook_bet_settlements', 'UPDATE')
                    AND NOT has_table_privilege('igaming_runtime', 'sportsbook_bet_settlements', 'DELETE')
                    AND NOT has_table_privilege('igaming_runtime', 'casino_callback_rejections', 'UPDATE')
                    AND NOT has_table_privilege('igaming_runtime', 'casino_callback_rejections', 'DELETE')
                    AND has_table_privilege('igaming_runtime', 'ledger_entries', 'INSERT')
               THEN 'PASS' ELSE 'FAIL' END || '|';
SELECT 'CHK|runtime_role_narrowed_grants_insert_only_tables|'
       || CASE WHEN NOT has_table_privilege('igaming_runtime', 'alert_occurrences', 'UPDATE')
                    AND NOT has_table_privilege('igaming_runtime', 'alert_occurrences', 'DELETE')
                    AND NOT has_table_privilege('igaming_runtime', 'payment_statement_lines', 'UPDATE')
                    AND NOT has_table_privilege('igaming_runtime', 'payment_statement_lines', 'DELETE')
                    AND NOT has_table_privilege('igaming_runtime', 'provider_credential_handles', 'UPDATE')
                    AND NOT has_table_privilege('igaming_runtime', 'provider_credential_handles', 'DELETE')
               THEN 'PASS' ELSE 'FAIL' END || '|';
-- schema_migrations is owner-written only (the CI/dev harness revokes the runtime role's writes)
SELECT 'CHK|runtime_role_cannot_write_schema_migrations|'
       || CASE WHEN NOT has_table_privilege('igaming_runtime', 'schema_migrations', 'INSERT')
                    AND NOT has_table_privilege('igaming_runtime', 'schema_migrations', 'UPDATE')
                    AND NOT has_table_privilege('igaming_runtime', 'schema_migrations', 'DELETE')
               THEN 'PASS' ELSE 'FAIL' END || '|';
-- Function definitions (incl. search_path pins in proconfig and the SECURITY DEFINER flag) are compared
-- as a fingerprint below; here the count of definer functions without a search_path pin is reported
-- (informational: any non-zero number is the TRIGGER-SEARCH-PATH-1 residual, and must be identical on both sides).
SELECT 'FP|secdef_functions_without_search_path_pin|'
       || count(*) FILTER (WHERE p.prosecdef
                             AND NOT EXISTS (SELECT 1 FROM unnest(COALESCE(p.proconfig, '{}')) cfg WHERE cfg LIKE 'search_path=%'))
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'public';

-- ---------------------------------------------------------------- catalog fingerprints (source must equal restored)
SELECT 'FP|tables_md5|' || md5(string_agg(c.relname || ':' || c.relkind::text || ':' || c.relrowsecurity || ':' || c.relforcerowsecurity
                                          || ':' || pg_get_userbyid(c.relowner), ',' ORDER BY c.relname))
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'S', 'v', 'm');
SELECT 'FP|columns_md5|' || md5(string_agg(table_name || '.' || column_name || ':' || data_type || ':' || is_nullable
                                           || ':' || COALESCE(column_default, '') || ':' || COALESCE(generation_expression, ''),
                                           ',' ORDER BY table_name, column_name))
  FROM information_schema.columns WHERE table_schema = 'public';
SELECT 'FP|triggers_md5|' || md5(string_agg(c.relname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid), ',' ORDER BY c.relname, t.tgname))
       || '|count=' || count(*)
  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND NOT t.tgisinternal;
SELECT 'FP|functions_md5|'
       || md5(string_agg(p.proname || '(' || pg_get_function_identity_arguments(p.oid) || '):' || md5(pg_get_functiondef(p.oid))
                         || ':' || p.prosecdef::text || ':' || COALESCE(array_to_string(p.proconfig, ';'), '') || ':' || pg_get_userbyid(p.proowner),
                         ',' ORDER BY p.proname, pg_get_function_identity_arguments(p.oid)))
       || '|count=' || count(*)
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p');
SELECT 'FP|policies_md5|' || md5(string_agg(tablename || ':' || policyname || ':' || permissive || ':' || array_to_string(roles, ',') || ':' || cmd
                                            || ':' || COALESCE(qual, '') || ':' || COALESCE(with_check, ''), ',' ORDER BY tablename, policyname))
       || '|count=' || count(*)
  FROM pg_policies WHERE schemaname = 'public';
-- pg_get_constraintdef re-parenthesises nested AND/OR after a dump/restore round trip (same
-- expression, different bracket depth), so brackets are stripped before hashing. Names, types, columns
-- and operators are still compared; bracket-only regrouping is the one thing this fingerprint ignores.
SELECT 'FP|constraints_md5|' || md5(string_agg(conrelid::regclass::text || ':' || conname || ':' || regexp_replace(pg_get_constraintdef(oid), '[()]', '', 'g'), ',' ORDER BY conrelid::regclass::text, conname))
       || '|count=' || count(*)
  FROM pg_constraint WHERE connamespace = 'public'::regnamespace;
SELECT 'FP|indexes_md5|' || md5(string_agg(tablename || ':' || indexname || ':' || indexdef, ',' ORDER BY tablename, indexname))
       || '|count=' || count(*)
  FROM pg_indexes WHERE schemaname = 'public';
SELECT 'FP|object_acls_md5|' || md5(string_agg(c.relname || ':' || COALESCE(c.relacl, acldefault((CASE c.relkind WHEN 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))::text, ',' ORDER BY c.relname))
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'S');
SELECT 'FP|default_acls_md5|' || COALESCE(md5(string_agg(pg_get_userbyid(defaclrole) || ':' || defaclobjtype::text || ':' || defaclacl::text, ',' ORDER BY defaclobjtype, pg_get_userbyid(defaclrole))), 'none')
  FROM pg_default_acl;
SELECT 'FP|schema_acl|' || COALESCE(nspacl::text, '') FROM pg_namespace WHERE nspname = 'public';
