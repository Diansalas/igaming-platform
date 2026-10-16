-- staging-runner mode verify-db, second half: the checks that only mean something from a session that is
-- really logged in as the application's runtime role (docs/runbooks/plat-rolesplit-staging-verification.md
-- V4/V5 "live" evidence: the application never runs as the owner). READ-ONLY plain SELECTs; prints no secret.
-- Output: RESULT|<name>|PASS|FAIL|INFO|<detail> (same format as verify-db.sql). Variable: require_tls (1 on RDS; 0 only for the local test harness).
\set ON_ERROR_STOP on

SELECT 'RESULT|runtime_session_identity|' || CASE WHEN current_user = 'igaming_runtime' AND session_user = 'igaming_runtime' THEN 'PASS' ELSE 'FAIL' END
       || '|current_user=' || current_user || ' session_user=' || session_user || ' database=' || current_database();

SELECT 'RESULT|runtime_session_not_privileged|' || CASE WHEN NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND NOT r.rolreplication THEN 'PASS' ELSE 'FAIL' END
       || '|super=' || r.rolsuper || ' bypassrls=' || r.rolbypassrls || ' createrole=' || r.rolcreaterole || ' createdb=' || r.rolcreatedb || ' replication=' || r.rolreplication
  FROM pg_roles r WHERE r.rolname = current_user;

SELECT 'RESULT|search_path_session_runtime|' || CASE WHEN current_setting('search_path') = '"$user", public' THEN 'PASS' ELSE 'FAIL' END
       || '|search_path=' || current_setting('search_path');

SELECT 'RESULT|runtime_session_uses_tls|' || CASE WHEN COALESCE(s.ssl, false) THEN 'PASS' WHEN :'require_tls' = '0' THEN 'INFO' ELSE 'FAIL' END
       || '|ssl=' || COALESCE(s.ssl::text, 'unknown') || ' version=' || COALESCE(s.version, 'n/a')
  FROM pg_stat_ssl s WHERE s.pid = pg_backend_pid();

SELECT 'RESULT|temp_denied_for_runtime_session|' || CASE WHEN NOT has_database_privilege(current_user, current_database(), 'TEMPORARY') AND NOT has_database_privilege(current_user, current_database(), 'CREATE') THEN 'PASS' ELSE 'FAIL' END
       || '|TEMPORARY=' || has_database_privilege(current_user, current_database(), 'TEMPORARY') || ' CREATE=' || has_database_privilege(current_user, current_database(), 'CREATE');

SELECT 'RESULT|schema_migrations_denied_for_runtime_session|' ||
       CASE WHEN NOT (has_table_privilege(current_user, 'public.schema_migrations', 'INSERT') OR has_table_privilege(current_user, 'public.schema_migrations', 'UPDATE')
                      OR has_table_privilege(current_user, 'public.schema_migrations', 'DELETE') OR has_table_privilege(current_user, 'public.schema_migrations', 'TRUNCATE')) THEN 'PASS' ELSE 'FAIL' END
       || '|runtime session INSERT=' || has_table_privilege(current_user, 'public.schema_migrations', 'INSERT') || ' UPDATE=' || has_table_privilege(current_user, 'public.schema_migrations', 'UPDATE')
       || ' DELETE=' || has_table_privilege(current_user, 'public.schema_migrations', 'DELETE') || ' TRUNCATE=' || has_table_privilege(current_user, 'public.schema_migrations', 'TRUNCATE');

SELECT 'RESULT|schema_create_denied_for_runtime_session|' || CASE WHEN NOT has_schema_privilege(current_user, 'public', 'CREATE') THEN 'PASS' ELSE 'FAIL' END
       || '|CREATE on schema public=' || has_schema_privilege(current_user, 'public', 'CREATE');

-- RLS really bites for this role: with no tenant scope, a tenant table shows nothing, and the role is not exempt
SELECT 'RESULT|runtime_session_rls_not_bypassed|' || CASE WHEN NOT r.rolbypassrls AND current_setting('row_security') = 'on' THEN 'PASS' ELSE 'FAIL' END
       || '|bypassrls=' || r.rolbypassrls || ' row_security=' || current_setting('row_security')
  FROM pg_roles r WHERE r.rolname = current_user;
