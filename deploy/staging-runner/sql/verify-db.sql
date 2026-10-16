-- staging-runner mode verify-db: READ-ONLY database verification pack (STAGING-PREP-3).
--
-- Proves, from inside the VPC and without writing anything, the database-side items of
-- docs/runbooks/aws-staging-deploy-plan.md sections 3.1-3.3 and 6.1/6.2 and the database-side
-- steps of plat-rolesplit-staging-verification.md (V4 ownership / roles / TEMP, V5 TEMP, V10 migration state):
-- role attributes and search_path, SECURITY DEFINER search_path pins, TEMPORARY revoked, the runtime
-- role unable to write schema_migrations, FORCE RLS on every tenant table, the runtime role neither
-- owner nor superuser nor BYPASSRLS, the migration head, the database_environment anchor, plus a
-- deterministic grant snapshot that a later run can diff.
--
-- Every statement is a plain SELECT (no temp object, no function, no DML/DDL): it runs as the
-- table-owner/master role and needs nothing the master does not have. Nothing here reads a row of a
-- tenant table (RLS would hide them anyway): only catalogs and the (non-tenant) schema_migrations and
-- database_environment tables. It prints no secret and no row data.
--
-- Output (machine readable):
--   RESULT|<name>|PASS|<detail>      a check that held
--   RESULT|<name>|FAIL|<detail>      a check that did not (the runner exits non-zero)
--   RESULT|<name>|INFO|<detail>      an observation recorded for the evidence file (not a verdict)
--   SNAPSHOT|grants|<grantee>|tables=N|columns=N|routines=N|sha256=<hex>     deterministic grant digest
--   GRANT|<grantee>|<line>           the full sorted listing (only when -v listing=1)
--
-- Variables (-v): owner_role (default igaming), expect_backup (present|absent|any), active_kid
-- (ACTOR_PROOF_ACTIVE_KID, informational), file_head / file_count (highest migration number and number of
-- .up.sql files baked into the image), listing (0|1), require_anchor (0|1: a blank environment anchor is a FAIL),
-- iam_only (0|1: igaming_backup is IAM-only, so a finite validity on it is a leftover FAIL).
\set ON_ERROR_STOP on

-- ---------------------------------------------------------------- 3.1 roles
SELECT 'RESULT|role_owner_attributes|' || CASE WHEN r.rolcanlogin AND NOT r.rolsuper AND NOT r.rolreplication THEN 'PASS' ELSE 'FAIL' END
       || '|role=' || r.rolname || ' login=' || r.rolcanlogin || ' super=' || r.rolsuper || ' createrole=' || r.rolcreaterole
       || ' createdb=' || r.rolcreatedb || ' bypassrls=' || r.rolbypassrls || ' replication=' || r.rolreplication
       || ' rds_superuser_member=' || COALESCE((SELECT pg_has_role(r.oid, g.oid, 'MEMBER')::text FROM pg_roles g WHERE g.rolname = 'rds_superuser'), 'no-such-role')
  FROM pg_roles r WHERE r.rolname = :'owner_role'
UNION ALL
SELECT 'RESULT|role_owner_attributes|FAIL|owner role ' || :'owner_role' || ' does not exist'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'owner_role');

SELECT 'RESULT|role_runtime_attributes|' || CASE WHEN r.rolcanlogin AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole
                                                   AND NOT r.rolbypassrls AND NOT r.rolreplication THEN 'PASS' ELSE 'FAIL' END
       || '|role=igaming_runtime login=' || r.rolcanlogin || ' super=' || r.rolsuper || ' createdb=' || r.rolcreatedb
       || ' createrole=' || r.rolcreaterole || ' bypassrls=' || r.rolbypassrls || ' replication=' || r.rolreplication
       || ' connlimit=' || r.rolconnlimit || ' validuntil=' || COALESCE(r.rolvaliduntil::text, 'none')
  FROM pg_roles r WHERE r.rolname = 'igaming_runtime'
UNION ALL
SELECT 'RESULT|role_runtime_attributes|FAIL|role igaming_runtime does not exist (role-init has not run)'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime');

-- the runtime role owns nothing and holds no membership that would import privilege
SELECT 'RESULT|role_runtime_owns_nothing|' || CASE WHEN x.n = 0 THEN 'PASS' ELSE 'FAIL' END || '|owned relations/functions/types/schemas/databases=' || x.n
  FROM (SELECT (SELECT count(*) FROM pg_class WHERE relowner = r.oid)
              + (SELECT count(*) FROM pg_proc WHERE proowner = r.oid)
              + (SELECT count(*) FROM pg_type WHERE typowner = r.oid AND typnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace))
              + (SELECT count(*) FROM pg_namespace WHERE nspowner = r.oid)
              + (SELECT count(*) FROM pg_database WHERE datdba = r.oid) AS n
          FROM pg_roles r WHERE r.rolname = 'igaming_runtime') x;

SELECT 'RESULT|role_runtime_no_memberships|' || CASE WHEN x.n = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|member of ' || x.n || ' role(s)' || CASE WHEN x.n = 0 THEN '' ELSE ': ' || x.names END
  FROM (SELECT count(*) AS n, COALESCE(string_agg(g.rolname, ',' ORDER BY g.rolname), '') AS names
          FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
         WHERE u.rolname = 'igaming_runtime') x;

SELECT 'RESULT|role_runtime_not_owner_equivalent|' || CASE WHEN NOT pg_has_role('igaming_runtime', :'owner_role', 'MEMBER') THEN 'PASS' ELSE 'FAIL' END
       || '|runtime is member of owner role ' || :'owner_role' || ': ' || pg_has_role('igaming_runtime', :'owner_role', 'MEMBER')
 WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'owner_role');

SELECT 'RESULT|role_backup_attributes|' ||
       CASE WHEN :'expect_backup' = 'absent' THEN 'FAIL'
            WHEN r.rolcanlogin AND NOT r.rolsuper AND r.rolbypassrls AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND r.rolconnlimit = 2 THEN 'PASS'
            ELSE 'FAIL' END
       || '|role=igaming_backup login=' || r.rolcanlogin || ' super=' || r.rolsuper || ' bypassrls=' || r.rolbypassrls || ' createdb=' || r.rolcreatedb
       || ' createrole=' || r.rolcreaterole || ' replication=' || r.rolreplication || ' connlimit=' || r.rolconnlimit
       || ' memberof=' || COALESCE((SELECT string_agg(g.rolname, ',' ORDER BY g.rolname) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid), 'none')
  FROM pg_roles r WHERE r.rolname = 'igaming_backup'
UNION ALL
SELECT 'RESULT|role_backup_attributes|' || CASE WHEN :'expect_backup' = 'present' THEN 'FAIL' ELSE 'INFO' END
       || '|role igaming_backup does not exist (expected before the backup drill provisions it; EXPECT_BACKUP_ROLE=' || :'expect_backup' || ')'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup');

-- every other login role (V4 last query): the evidence file lists them; unexplained ones are reviewed by a human
SELECT 'RESULT|login_roles_inventory|INFO|' || COALESCE(string_agg(r.rolname || '(super=' || r.rolsuper::int || ',bypassrls=' || r.rolbypassrls::int || ',temp=' ||
       has_database_privilege(r.oid, current_database(), 'TEMPORARY')::int || ')', ' ' ORDER BY r.rolname), 'none')
  FROM pg_roles r WHERE r.rolcanlogin AND r.rolname !~ '^pg_';

-- ---------------------------------------------------------------- 3.2 search_path
SELECT 'RESULT|search_path_role_settings|INFO|' || COALESCE(string_agg(COALESCE(r.rolname, '<all roles>') || ': ' || array_to_string(s.setconfig, ';'), ' | ' ORDER BY r.rolname), 'no role/database-level settings')
  FROM pg_db_role_setting s LEFT JOIN pg_roles r ON r.oid = s.setrole
 WHERE (s.setrole = 0 OR r.rolname IN (:'owner_role', 'igaming_runtime', 'igaming_backup')) AND (s.setdatabase = 0 OR s.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database()));

SELECT 'RESULT|search_path_session_owner|' || CASE WHEN current_setting('search_path') = '"$user", public' THEN 'PASS' ELSE 'FAIL' END
       || '|current_user=' || current_user || ' search_path=' || current_setting('search_path');

-- every SECURITY DEFINER function pins search_path (TRIGGER-SEARCH-PATH-1 / migration 0116 class)
SELECT 'RESULT|secdef_functions_pin_search_path|' || CASE WHEN count(*) FILTER (WHERE NOT pinned) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|security definer functions=' || count(*) || ' without a search_path pin=' || count(*) FILTER (WHERE NOT pinned)
       || COALESCE(' first: ' || (SELECT string_agg(f, ', ') FROM (SELECT x.f FROM (SELECT n2.nspname || '.' || p2.proname AS f FROM pg_proc p2 JOIN pg_namespace n2 ON n2.oid = p2.pronamespace
            WHERE p2.prosecdef AND n2.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
              AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p2.oid AND d.deptype = 'e')
              AND NOT EXISTS (SELECT 1 FROM unnest(COALESCE(p2.proconfig, '{}'::text[])) c WHERE c LIKE 'search_path=%') ORDER BY 1 LIMIT 5) x) y), '')
  FROM (SELECT EXISTS (SELECT 1 FROM unnest(COALESCE(p.proconfig, '{}'::text[])) c WHERE c LIKE 'search_path=%') AS pinned
          FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE p.prosecdef AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
           AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')) q;

-- ---------------------------------------------------------------- 3.3 TEMPORARY (migration 0116)
SELECT 'RESULT|temp_revoked_runtime|' || CASE WHEN NOT has_database_privilege('igaming_runtime', current_database(), 'TEMPORARY') THEN 'PASS' ELSE 'FAIL' END
       || '|igaming_runtime TEMPORARY on ' || current_database() || '=' || has_database_privilege('igaming_runtime', current_database(), 'TEMPORARY')
 WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime');

SELECT 'RESULT|temp_revoked_public|' || CASE WHEN NOT EXISTS (
         SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
          WHERE d.datname = current_database() AND a.grantee = 0 AND a.privilege_type = 'TEMPORARY') THEN 'PASS' ELSE 'FAIL' END
       || '|PUBLIC holds TEMPORARY on ' || current_database() || ': ' || EXISTS (
         SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
          WHERE d.datname = current_database() AND a.grantee = 0 AND a.privilege_type = 'TEMPORARY');

SELECT 'RESULT|temp_backup_role|' || CASE WHEN NOT has_database_privilege('igaming_backup', current_database(), 'TEMPORARY') THEN 'PASS' ELSE 'FAIL' END
       || '|igaming_backup TEMPORARY=' || has_database_privilege('igaming_backup', current_database(), 'TEMPORARY')
 WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup');

SELECT 'RESULT|runtime_cannot_create_in_schemas|' || CASE WHEN count(*) FILTER (WHERE has_schema_privilege('igaming_runtime', n.nspname, 'CREATE')) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|schemas checked=' || count(*) || ' with CREATE for the runtime role=' || count(*) FILTER (WHERE has_schema_privilege('igaming_runtime', n.nspname, 'CREATE'))
  FROM pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
   AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime');

-- ---------------------------------------------------------------- 6.1 schema_migrations is not writable by the runtime role
SELECT 'RESULT|runtime_cannot_write_schema_migrations|' ||
       CASE WHEN NOT (has_table_privilege('igaming_runtime', 'public.schema_migrations', 'INSERT') OR has_table_privilege('igaming_runtime', 'public.schema_migrations', 'UPDATE')
                      OR has_table_privilege('igaming_runtime', 'public.schema_migrations', 'DELETE') OR has_table_privilege('igaming_runtime', 'public.schema_migrations', 'TRUNCATE'))
            THEN 'PASS' ELSE 'FAIL' END
       || '|INSERT=' || has_table_privilege('igaming_runtime', 'public.schema_migrations', 'INSERT') || ' UPDATE=' || has_table_privilege('igaming_runtime', 'public.schema_migrations', 'UPDATE')
       || ' DELETE=' || has_table_privilege('igaming_runtime', 'public.schema_migrations', 'DELETE') || ' TRUNCATE=' || has_table_privilege('igaming_runtime', 'public.schema_migrations', 'TRUNCATE')
 WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') AND to_regclass('public.schema_migrations') IS NOT NULL;

-- the owner-only actor proof tables (migration 0120) and the owner-only key schema (migration 0166, F1): the runtime role holds no privilege
-- at all on the nonce table, the schema actorproof_private or the key table in it, and the backup role (when it exists) none on the schema or
-- the key table (privileges read by OID: a name lookup in a schema without USAGE would itself be refused)
SELECT 'RESULT|runtime_has_no_actor_proof_table_access|' ||
       CASE WHEN NOT (has_schema_privilege('igaming_runtime', n.oid, 'USAGE') OR has_schema_privilege('igaming_runtime', n.oid, 'CREATE')
                      OR has_table_privilege('igaming_runtime', c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
                      OR has_any_column_privilege('igaming_runtime', c.oid, 'SELECT,INSERT,UPDATE,REFERENCES')
                      OR has_table_privilege('igaming_runtime', 'public.actor_proof_nonces', 'DELETE') OR has_table_privilege('igaming_runtime', 'public.actor_proof_nonces', 'SELECT'))
            THEN 'PASS' ELSE 'FAIL' END
       || '|actorproof_private (schema and actor_proof_keys) and actor_proof_nonces are owner-only'
  FROM pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys'
 WHERE n.nspname = 'actorproof_private' AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') AND to_regclass('public.actor_proof_nonces') IS NOT NULL;
SELECT 'RESULT|backup_role_has_no_actor_proof_key_access|' ||
       CASE WHEN NOT (has_schema_privilege('igaming_backup', n.oid, 'USAGE') OR has_schema_privilege('igaming_backup', n.oid, 'CREATE')
                      OR has_table_privilege('igaming_backup', c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
                      OR has_any_column_privilege('igaming_backup', c.oid, 'SELECT,INSERT,UPDATE,REFERENCES'))
            THEN 'PASS' ELSE 'FAIL' END
       || '|the backup credential cannot read the actor-proof signing key (owner decision 5, migration 0166)'
  FROM pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys'
 WHERE n.nspname = 'actorproof_private' AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup');
SELECT 'RESULT|actor_proof_key_isolated|' || CASE WHEN to_regclass('public.actor_proof_keys') IS NULL AND to_regnamespace('actorproof_private') IS NOT NULL THEN 'PASS' ELSE 'FAIL' END
       || '|the key table lives only in the owner-only schema actorproof_private (migration 0166 applied; no public.actor_proof_keys)';

-- ---------------------------------------------------------------- V4 ownership
SELECT 'RESULT|database_owner_is_owner_role|' || CASE WHEN pg_get_userbyid(datdba) = :'owner_role' THEN 'PASS' ELSE 'FAIL' END
       || '|database ' || datname || ' owner=' || pg_get_userbyid(datdba)
  FROM pg_database WHERE datname = current_database();

SELECT 'RESULT|public_relations_owned_only_by_owner_role|' || CASE WHEN count(*) FILTER (WHERE pg_get_userbyid(c.relowner) <> :'owner_role') = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|relations=' || count(*) || ' owned by another role=' || count(*) FILTER (WHERE pg_get_userbyid(c.relowner) <> :'owner_role')
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public' WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f');

-- ---------------------------------------------------------------- FORCE RLS on every tenant table
SELECT 'RESULT|tenant_tables_force_rls|' || CASE WHEN count(*) FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity)) = 0 AND count(*) > 0 THEN 'PASS' ELSE 'FAIL' END
       || '|tables with tenant_id=' || count(*) || ' lacking FORCE RLS=' || count(*) FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity))
       || COALESCE(' first: ' || (SELECT string_agg(c2.relname, ', ' ORDER BY c2.relname) FROM (SELECT c3.relname FROM pg_class c3 JOIN pg_namespace n3 ON n3.oid = c3.relnamespace AND n3.nspname = 'public'
            JOIN pg_attribute a3 ON a3.attrelid = c3.oid AND a3.attname = 'tenant_id' AND NOT a3.attisdropped
           WHERE c3.relkind IN ('r', 'p') AND NOT (c3.relrowsecurity AND c3.relforcerowsecurity) ORDER BY c3.relname LIMIT 5) c2), '')
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
 WHERE c.relkind IN ('r', 'p');

SELECT 'RESULT|tenant_tables_have_a_policy|' || CASE WHEN count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid)) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|tenant tables without any policy=' || count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid))
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
 WHERE c.relkind IN ('r', 'p');

-- ---------------------------------------------------------------- migration state (V10)
SELECT 'RESULT|migration_head_equals_image|' || CASE WHEN m.head = :'file_head'::bigint THEN 'PASS' ELSE 'FAIL' END
       || '|database head=' || m.head || ' applied=' || m.n || ' image highest migration file=' || :'file_head' || ' (files=' || :'file_count' || ', reserved unused versions explain applied < files)'
  FROM (SELECT COALESCE(max(version), -1) AS head, count(*) AS n FROM schema_migrations) m;

SELECT 'RESULT|migration_checksums_recorded|' || CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|applied versions with a NULL checksum=' || count(*)
  FROM schema_migrations WHERE checksum IS NULL;

-- ---------------------------------------------------------------- 6.2 environment anchor
-- require_anchor=1 (SRP3-1): the seeding commands (seed-market, seed-world) refuse a database whose anchor is not staging / development / test
-- (no row = production). 0: a blank anchor is acceptable (nothing is about to be seeded).
SELECT 'RESULT|database_environment_anchor|' ||
       CASE WHEN a.n = 0 AND :'require_anchor' = '0' THEN 'PASS'
            WHEN a.n = 0 THEN 'FAIL'
            WHEN a.n = 1 AND a.env = 'staging' THEN 'PASS'
            WHEN a.n = 1 AND a.env IN ('development', 'test') AND :'require_anchor' = '1' THEN 'PASS'
            ELSE 'FAIL' END
       || '|rows=' || a.n || ' environment=' || COALESCE(a.env, 'unrecorded (blank = production for the seeding commands: run the record-anchor mode, plan 6.2)')
       || ' require_anchor=' || :'require_anchor'
  FROM (SELECT count(*) AS n, min(environment) AS env FROM database_environment) a;

-- ---------------------------------------------------------------- leftovers of a drill (SRP3-9)
-- Nothing a drill creates may remain: the disposable restore-loader login, a restore-check database (any database that is not the application
-- database or a system database), and, on RDS (iam_only=1), a finite validity on igaming_backup (it is IAM-only there; the 2-hour password
-- credential exists only on a disposable PITR clone). Fix with the cleanup mode.
SELECT 'RESULT|leftover_restore_loader_role|' || CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|igaming_restore_loader roles=' || count(*) || CASE WHEN count(*) = 0 THEN '' ELSE ' (run the cleanup mode)' END
  FROM pg_roles WHERE rolname = 'igaming_restore_loader';
SELECT 'RESULT|leftover_restore_check_database|' || CASE WHEN count(*) = 0 THEN 'PASS' ELSE 'FAIL' END
       || '|databases other than ' || current_database() || ' and the system databases=' || count(*)
       || CASE WHEN count(*) = 0 THEN '' ELSE ': ' || string_agg(datname, ',' ORDER BY datname) || ' (run the cleanup mode)' END
  FROM pg_database WHERE datname NOT IN (current_database(), 'postgres', 'template0', 'template1', 'rdsadmin');
SELECT 'RESULT|leftover_backup_credential|' ||
       CASE WHEN r.rolvaliduntil IS NULL OR r.rolvaliduntil = 'infinity' THEN 'PASS'
            WHEN :'iam_only' = '1' THEN 'FAIL' ELSE 'INFO' END
       || '|igaming_backup valid until ' || COALESCE(r.rolvaliduntil::text, 'none')
       || CASE WHEN :'iam_only' = '1' THEN ' (IAM-only on RDS: a finite validity is a leftover; run the cleanup mode)' ELSE ' (local harness: password credential by design)' END
  FROM pg_roles r WHERE r.rolname = 'igaming_backup';

-- SRP3R-2: on RDS (iam_only=1) igaming_backup must be an rds_iam member (IAM-only credential); anything else is a password login on the all-tenant role.
SELECT 'RESULT|backup_role_iam_member|' || CASE WHEN m.is_member THEN 'PASS' WHEN :'iam_only' = '1' THEN 'FAIL' ELSE 'INFO' END
       || '|igaming_backup is a member of rds_iam: ' || m.is_member || CASE WHEN m.is_member OR :'iam_only' <> '1' THEN '' ELSE ' (IAM-only expected on RDS; run the cleanup mode)' END
  FROM (SELECT EXISTS (SELECT 1 FROM pg_auth_members am JOIN pg_roles g ON g.oid = am.roleid JOIN pg_roles u ON u.oid = am.member
                        WHERE g.rolname = 'rds_iam' AND u.rolname = 'igaming_backup') AS is_member) m
 WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup');

-- ---------------------------------------------------------------- server posture (observations)
SELECT 'RESULT|server_posture|INFO|version=' || current_setting('server_version') || ' ssl=' || COALESCE((SELECT ssl::text FROM pg_stat_ssl WHERE pid = pg_backend_pid()), 'n/a')
       || ' rds.force_ssl=' || COALESCE(current_setting('rds.force_ssl', true), 'n/a') || ' log_connections=' || current_setting('log_connections')
       || ' log_statement=' || current_setting('log_statement') || ' pgaudit.log=' || COALESCE((SELECT setting FROM pg_settings WHERE name = 'pgaudit.log'), 'not loaded')
       || ' preload=' || COALESCE((SELECT setting FROM pg_settings WHERE name = 'shared_preload_libraries'), 'restricted');

SELECT 'RESULT|extensions|INFO|' || COALESCE(string_agg(extname || ' ' || extversion, ', ' ORDER BY extname), 'none') FROM pg_extension;

SELECT 'RESULT|actor_proof_keys_rows|INFO|active kids (no key material): ' || COALESCE(string_agg(kid || ':' || status, ', ' ORDER BY kid), 'none')
       || ' ; ACTOR_PROOF_ACTIVE_KID=' || :'active_kid'
  FROM actorproof_private.actor_proof_keys;

-- ---------------------------------------------------------------- fingerprints comparable with the local drill (same definitions as deploy/backup/baseline.sql)
SELECT 'RESULT|fingerprint_functions_md5|INFO|' || md5(string_agg(p.proname || '(' || pg_get_function_identity_arguments(p.oid) || '):' || md5(pg_get_functiondef(p.oid))
                         || ':' || p.prosecdef::text || ':' || COALESCE(array_to_string(p.proconfig, ';'), '') || ':' || pg_get_userbyid(p.proowner),
                         ',' ORDER BY p.proname, pg_get_function_identity_arguments(p.oid))) || ' count=' || count(*)
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p');
SELECT 'RESULT|fingerprint_migration_checksums_md5|INFO|' || md5(string_agg(version || ':' || checksum, ',' ORDER BY version)) FROM schema_migrations;

-- ---------------------------------------------------------------- grant snapshot (deterministic; diff two runs by comparing the sha256)
WITH grantees AS (
    SELECT 'igaming_runtime' AS name, (SELECT oid FROM pg_roles WHERE rolname = 'igaming_runtime') AS oid
    UNION ALL SELECT 'igaming_backup', (SELECT oid FROM pg_roles WHERE rolname = 'igaming_backup')
    UNION ALL SELECT 'PUBLIC', 0::oid
), g AS (
    SELECT gr.name, 'T' AS k, 'T|' || c.relname || '|' || a.privilege_type AS l
      FROM grantees gr, pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public',
           LATERAL aclexplode(COALESCE(c.relacl, acldefault((CASE c.relkind WHEN 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) a
     WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND gr.oid IS NOT NULL AND a.grantee = gr.oid
    UNION ALL
    SELECT gr.name, 'C', 'C|' || c.relname || '.' || at.attname || '|' || a.privilege_type
      FROM grantees gr, pg_attribute at JOIN pg_class c ON c.oid = at.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public',
           LATERAL aclexplode(at.attacl) a
     WHERE at.attacl IS NOT NULL AND NOT at.attisdropped AND gr.oid IS NOT NULL AND a.grantee = gr.oid
    UNION ALL
    SELECT gr.name, 'F', 'F|' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|' || a.privilege_type
      FROM grantees gr, pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'public',
           LATERAL aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a
     WHERE p.prokind IN ('f', 'p') AND gr.oid IS NOT NULL AND a.grantee = gr.oid
)
SELECT 'SNAPSHOT|grants|' || gr.name || '|tables=' || count(*) FILTER (WHERE g.k = 'T') || '|columns=' || count(*) FILTER (WHERE g.k = 'C')
       || '|routines=' || count(*) FILTER (WHERE g.k = 'F')
       || '|sha256=' || encode(sha256(convert_to(COALESCE(string_agg(g.l, E'\n' ORDER BY g.l), ''), 'UTF8')), 'hex')
  FROM grantees gr LEFT JOIN g ON g.name = gr.name
 WHERE gr.oid IS NOT NULL
 GROUP BY gr.name ORDER BY gr.name;

\if :listing
WITH grantees AS (
    SELECT 'igaming_runtime' AS name, (SELECT oid FROM pg_roles WHERE rolname = 'igaming_runtime') AS oid
    UNION ALL SELECT 'PUBLIC', 0::oid
)
SELECT 'GRANT|' || gr.name || '|T|' || c.relname || '|' || a.privilege_type
  FROM grantees gr, pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public',
       LATERAL aclexplode(COALESCE(c.relacl, acldefault((CASE c.relkind WHEN 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) a
 WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND gr.oid IS NOT NULL AND a.grantee = gr.oid
UNION ALL
SELECT 'GRANT|' || gr.name || '|C|' || c.relname || '.' || at.attname || '|' || a.privilege_type
  FROM grantees gr, pg_attribute at JOIN pg_class c ON c.oid = at.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public',
       LATERAL aclexplode(at.attacl) a
 WHERE at.attacl IS NOT NULL AND NOT at.attisdropped AND gr.oid IS NOT NULL AND a.grantee = gr.oid
UNION ALL
SELECT 'GRANT|' || gr.name || '|F|' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|' || a.privilege_type
  FROM grantees gr, pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'public',
       LATERAL aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a
 WHERE p.prokind IN ('f', 'p') AND gr.oid IS NOT NULL AND a.grantee = gr.oid
ORDER BY 1;
\endif
