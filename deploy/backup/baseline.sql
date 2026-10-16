-- Backup baseline / post-restore verification queries (A12, ADR 0122). READ-ONLY, no temp objects.
--
-- Run by deploy/backup/backup.sh inside the SAME REPEATABLE READ snapshot as pg_dump (so the baseline describes
-- exactly what the dump contains even while the source is taking writes) and by deploy/backup/verify.sh against
-- a restored database. It must run as the BYPASSRLS backup role (igaming_backup): under FORCE ROW LEVEL SECURITY
-- any other role sees only its tenant's rows and the counts/hashes below would be partial. It deliberately creates
-- nothing (the backup role has no TEMPORARY privilege and no CREATE).
--
-- Output is line oriented: <KIND>|<key>|<value...>
--   TABLE|<table>|<rows>|<md5 of the sorted row texts>    exact content equality, every public base table
--   INV|<name>|<violations>                                invariants: every value MUST be 0
--   SEQ|<sequence>|<last_value>
--   ROLE|, MEMBER|, DBACL|, SCHEMAACL|, ACL|, COLACL|, FUNCACL|, DEFACL|   role attributes and grants by NAME
--   TRG|, POL|, RLS|                                       triggers (with enabled state), policies, RLS flags
--   FP|<name>|<md5>                                        catalog fingerprints (same definitions as deploy/drill/verify.sql)
--   META|...                                               informational, excluded from comparisons
\set ON_ERROR_STOP on

SELECT 'META|server_version|' || current_setting('server_version');
SELECT 'META|database|' || current_database();
SELECT 'META|environment_anchor|' || COALESCE((SELECT environment FROM database_environment LIMIT 1), 'unrecorded');
SELECT 'META|dbsettings|encoding=' || pg_encoding_to_char(encoding) || ' collate=' || datcollate || ' ctype=' || datctype || ' owner=' || pg_get_userbyid(datdba)
  FROM pg_database WHERE datname = current_database();
SELECT 'META|migration_head|' || COALESCE(max(version)::text, 'none') || '|count=' || count(*) FROM schema_migrations;

-- Owner decision 5 (ADR 0122 section 10) and F1 (migration 0166): actor_proof_keys.secret is signing material. The baseline must never read it,
-- so no hash derived from it (even an md5 of a row text) is written into baseline.txt. Since migration 0166 the table lives in the schema
-- actorproof_private, on which THIS role holds no privilege at all: the baseline then carries no key row line and no key ids (verify.sh asserts the
-- key table's state as the OWNER role) - only the schema's STRUCTURE fingerprint (FP|actorproof_private_md5) and the privilege invariant
-- (INV|actor_proof_key_reachable_by_non_owner). A database below 0166 (legacy layout) still has public.actor_proof_keys: its key ids and its
-- row line (loop below) cover the NON-SECRET columns only (kid, status, created_at, retired_at).
SELECT 'META|actor_proof_kids|' || COALESCE((xpath('/row/k/text()', query_to_xml(
         CASE WHEN to_regclass('public.actor_proof_keys') IS NOT NULL
              THEN 'SELECT string_agg(kid || '':'' || status, '','' ORDER BY kid) AS k FROM public.actor_proof_keys'
              ELSE 'SELECT NULL::text AS k' END, false, true, '')))[1]::text, '');

-- ---------------------------------------------------------------- exact row counts and content hashes
SELECT 'TABLE|' || c.relname || '|' || COALESCE((xpath('/row/n/text()', x.r))[1]::text, 'ERR') || '|' || COALESCE((xpath('/row/h/text()', x.r))[1]::text, 'empty')
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 CROSS JOIN LATERAL (
       SELECT query_to_xml(format('SELECT count(*) AS n, COALESCE(md5(string_agg(t::text, E''\n'' ORDER BY t::text)), '''') AS h FROM %s t',
                                  CASE WHEN c.relname = 'actor_proof_keys'
                                       THEN '(SELECT kid, status, created_at, retired_at FROM public.actor_proof_keys)'
                                       ELSE format('public.%I', c.relname) END), false, true, '') AS r
 ) x
 WHERE c.relkind IN ('r', 'p')
 ORDER BY c.relname;

-- ---------------------------------------------------------------- invariants (all must be 0)
-- per transaction and asset: debits = credits
SELECT 'INV|ledger_transaction_asset_unbalanced|' || count(*) FROM (
    SELECT ledger_transaction_id, asset_code FROM ledger_entries GROUP BY 1, 2
    HAVING sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END) <> 0) q;
-- per tenant and asset: debits = credits
SELECT 'INV|ledger_tenant_asset_unbalanced|' || count(*) FROM (
    SELECT tenant_id, asset_code FROM ledger_entries GROUP BY 1, 2
    HAVING sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END) <> 0) q;
SELECT 'INV|ledger_entries_without_transaction|' || count(*)
  FROM ledger_entries e LEFT JOIN ledger_transactions t ON t.id = e.ledger_transaction_id WHERE t.id IS NULL;
-- the balance projection must equal a recompute from ledger_entries, per account (drift 0)
SELECT 'INV|projection_differs_from_recompute|' || count(*) FROM (
    SELECT re.a, pr.a FROM (
        SELECT ledger_account_id AS a,
               COALESCE(sum(amount) FILTER (WHERE direction = 'debit'), 0) AS d,
               COALESCE(sum(amount) FILTER (WHERE direction = 'credit'), 0) AS c
          FROM ledger_entries GROUP BY ledger_account_id) re
    FULL JOIN (SELECT ledger_account_id AS a, debit_total AS d, credit_total AS c FROM wallet_balance_projection) pr ON pr.a = re.a
    WHERE re.d IS DISTINCT FROM pr.d OR re.c IS DISTINCT FROM pr.c) q;
-- a sequence must never be behind the maximum of the column it feeds
SELECT 'INV|sequence_behind_column_max|' || count(*) FROM (
    SELECT ps.last_value,
           (xpath('/row/m/text()', query_to_xml(format('SELECT max(%I)::numeric AS m FROM %s', a.attname, d.refobjid::regclass), false, true, '')))[1]::text::numeric AS mx
      FROM pg_depend d
      JOIN pg_class seq ON seq.oid = d.objid AND seq.relkind = 'S'
      JOIN pg_namespace sn ON sn.oid = seq.relnamespace AND sn.nspname = 'public'
      JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
      JOIN pg_sequences ps ON ps.schemaname = 'public' AND ps.sequencename = seq.relname
     WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')) q
 WHERE mx IS NOT NULL AND COALESCE(last_value, 0) < mx;
-- every tenant table: RLS enabled AND forced, and at least one policy
SELECT 'INV|tenant_tables_without_forced_rls_or_policy|' || count(*) FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
 WHERE c.relkind IN ('r', 'p')
   AND (NOT (c.relrowsecurity AND c.relforcerowsecurity)
        OR NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid));
-- F1 (migration 0166): nobody but the owner (and its members / superusers) can reach the actor-proof key schema: no direct grant to anyone else
-- (PUBLIC included) on actorproof_private or on a relation in it, and no other role with effective USAGE/CREATE on the schema or SELECT on a
-- relation in it (e.g. through pg_read_all_data). Read by OID from the catalog and the has_*_privilege functions (they need no access to the
-- schema itself). 0 on a database below 0166.
SELECT 'INV|actor_proof_key_reachable_by_non_owner|' || (
    (SELECT count(*) FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a WHERE n.nspname = 'actorproof_private' AND a.grantee <> n.nspowner)
  + (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private', LATERAL aclexplode(c.relacl) a
      WHERE a.grantee <> c.relowner)
  + (SELECT count(*) FROM pg_attribute at JOIN pg_class c ON c.oid = at.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
      WHERE at.attacl IS NOT NULL)
  + (SELECT count(*) FROM pg_namespace n JOIN pg_roles r ON NOT r.rolsuper AND r.oid <> n.nspowner AND r.rolname !~ '^pg_' AND NOT pg_has_role(r.oid, n.nspowner, 'MEMBER')
      WHERE n.nspname = 'actorproof_private' AND (has_schema_privilege(r.oid, n.oid, 'USAGE') OR has_schema_privilege(r.oid, n.oid, 'CREATE')))
  + (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
       JOIN pg_roles r ON NOT r.rolsuper AND r.oid <> c.relowner AND r.rolname !~ '^pg_' AND NOT pg_has_role(r.oid, c.relowner, 'MEMBER')
      WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND has_table_privilege(r.oid, c.oid, 'SELECT')));
-- every non-internal trigger is enabled for ordinary sessions (origin mode 'O' or 'A'), none disabled
SELECT 'INV|triggers_disabled|' || count(*) FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE NOT t.tgisinternal AND t.tgenabled = 'D';

-- ---------------------------------------------------------------- sequences
SELECT 'SEQ|' || sequencename || '|' || COALESCE(last_value::text, 'null') FROM pg_sequences WHERE schemaname = 'public' ORDER BY sequencename;

-- ---------------------------------------------------------------- roles and grants, by NAME (OIDs differ across clusters)
SELECT 'ROLE|' || rolname || '|super=' || rolsuper::int || ' createrole=' || rolcreaterole::int || ' createdb=' || rolcreatedb::int
       || ' login=' || rolcanlogin::int || ' repl=' || rolreplication::int || ' bypassrls=' || rolbypassrls::int
       || ' inherit=' || rolinherit::int || ' connlimit=' || rolconnlimit
       || ' config=' || COALESCE(array_to_string(ARRAY(SELECT x FROM unnest(rolconfig) x ORDER BY x), ';'), '')
  FROM pg_roles WHERE rolname IN ('igaming', 'igaming_runtime', 'igaming_backup', 'igaming_test_admin') ORDER BY rolname;
SELECT 'MEMBER|' || r.rolname || '|' || m.rolname
  FROM pg_auth_members am JOIN pg_roles r ON r.oid = am.roleid JOIN pg_roles m ON m.oid = am.member
 WHERE m.rolname IN ('igaming', 'igaming_runtime', 'igaming_backup', 'igaming_test_admin') ORDER BY 1;  -- memberships OF the platform roles; operator/restore roles are environment-specific
SELECT 'DBACL|' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '|' || a.privilege_type || '|grant=' || a.is_grantable::int
  FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
 WHERE d.datname = current_database() ORDER BY 1;
SELECT 'SCHEMAACL|' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '|' || a.privilege_type
  FROM pg_namespace n, LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a
 WHERE n.nspname = 'public' ORDER BY 1;
SELECT 'ACL|' || c.relname || '|' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '|' || a.privilege_type
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public',
       LATERAL aclexplode(COALESCE(c.relacl, acldefault((CASE c.relkind WHEN 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) a
 WHERE c.relkind IN ('r', 'p', 'S') ORDER BY 1;
SELECT 'COLACL|' || c.relname || '.' || at.attname || '|' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '|' || a.privilege_type
  FROM pg_attribute at JOIN pg_class c ON c.oid = at.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public',
       LATERAL aclexplode(at.attacl) a
 WHERE at.attacl IS NOT NULL AND NOT at.attisdropped ORDER BY 1;
SELECT 'FUNCACL|' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|'
       || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '|' || a.privilege_type
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'public',
       LATERAL aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a
 WHERE p.prokind IN ('f', 'p') ORDER BY 1;
SELECT 'DEFACL|' || pg_get_userbyid(defaclrole) || '|' || defaclobjtype::text || '|' || defaclacl::text FROM pg_default_acl ORDER BY 1;

-- ---------------------------------------------------------------- append-only / guard triggers, policies, RLS flags (named)
SELECT 'TRG|' || c.relname || '|' || t.tgname || '|enabled=' || t.tgenabled::text
  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE NOT t.tgisinternal ORDER BY c.relname, t.tgname;
SELECT 'POL|' || tablename || '|' || policyname || '|' || cmd || '|' || permissive FROM pg_policies WHERE schemaname = 'public' ORDER BY tablename, policyname;
SELECT 'RLS|' || c.relname || '|enabled=' || c.relrowsecurity::int || ' forced=' || c.relforcerowsecurity::int || ' owner=' || pg_get_userbyid(c.relowner)
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public' WHERE c.relkind IN ('r', 'p') ORDER BY c.relname;

-- ---------------------------------------------------------------- catalog fingerprints (definitions as in deploy/drill/verify.sql)
SELECT 'FP|migration_checksums_md5|' || md5(string_agg(version || ':' || checksum, ',' ORDER BY version)) FROM schema_migrations;
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
SELECT 'FP|constraints_md5|' || md5(string_agg(conrelid::regclass::text || ':' || conname || ':' || regexp_replace(pg_get_constraintdef(oid), '[()]', '', 'g'), ',' ORDER BY conrelid::regclass::text, conname))
       || '|count=' || count(*)
  FROM pg_constraint WHERE connamespace = 'public'::regnamespace;
-- F1 (migration 0166): the STRUCTURE of the actor-proof key schema (never its rows): owner, ACLs by role name, relations, columns, defaults,
-- constraints, indexes, triggers, policies, functions and types. After a logical restore it must equal the source's: the schema is recreated
-- from deploy/backup/actorproof-private-schema.sql, so this line proves that DDL matches the migrations. count=0 below 0166.
SELECT 'FP|actorproof_private_md5|' || md5(COALESCE(string_agg(x, ',' ORDER BY x), '')) || '|count=' || count(*) FROM (
    SELECT 'schema:' || pg_get_userbyid(n.nspowner) || ':'
           || COALESCE((SELECT string_agg(CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '=' || a.privilege_type, ';'
                                          ORDER BY CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END, a.privilege_type)
                          FROM aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a), '') AS x
      FROM pg_namespace n WHERE n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'rel:' || c.relname || ':' || c.relkind::text || ':' || c.relrowsecurity || ':' || c.relforcerowsecurity || ':' || pg_get_userbyid(c.relowner) || ':'
           || COALESCE((SELECT string_agg(CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '=' || a.privilege_type, ';'
                                          ORDER BY CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END, a.privilege_type)
                          FROM aclexplode(COALESCE(c.relacl, acldefault((CASE c.relkind WHEN 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) a), '')
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'col:' || c.relname || '.' || at.attname || ':' || at.attnum || ':' || format_type(at.atttypid, at.atttypmod) || ':' || at.attnotnull || ':'
           || COALESCE(pg_get_expr(d.adbin, d.adrelid), '') || ':' || COALESCE(at.attacl::text, '')
      FROM pg_attribute at JOIN pg_class c ON c.oid = at.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
      LEFT JOIN pg_attrdef d ON d.adrelid = at.attrelid AND d.adnum = at.attnum
     WHERE at.attnum > 0 AND NOT at.attisdropped
    UNION ALL
    SELECT 'con:' || c.relname || ':' || co.conname || ':' || pg_get_constraintdef(co.oid)
      FROM pg_constraint co JOIN pg_class c ON c.oid = co.conrelid JOIN pg_namespace n ON n.oid = co.connamespace AND n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'idx:' || pg_get_indexdef(i.indexrelid)
      FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'trg:' || c.relname || ':' || t.tgname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid)
      FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'pol:' || c.relname || ':' || po.polname || ':' || po.polcmd::text || ':' || po.polpermissive
      FROM pg_policy po JOIN pg_class c ON c.oid = po.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'fn:' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
      FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'actorproof_private'
    UNION ALL
    SELECT 'type:' || t.typname || ':' || t.typtype::text
      FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace AND n.nspname = 'actorproof_private') q;
SELECT 'FP|indexes_md5|' || md5(string_agg(tablename || ':' || indexname || ':' || indexdef, ',' ORDER BY tablename, indexname))
       || '|count=' || count(*)
  FROM pg_indexes WHERE schemaname = 'public';
