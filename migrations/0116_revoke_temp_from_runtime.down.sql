-- Reverses 0116: GRANT TEMPORARY on the current database back to PUBLIC, which
-- restores the PostgreSQL default.
--
-- WARNING (DEV/CI ONLY): applying this re-opens TRIGGER-SEARCH-PATH-1 - the
-- runtime role can again create TEMP objects and shadow unqualified table
-- names used by the unpinned 0026..0113 guard functions (ADR 0108). Never run
-- it against a database that serves real traffic.
--
-- igaming_runtime is not granted anything explicitly: it inherits TEMPORARY
-- through PUBLIC again, exactly as before 0116.

DO $$
BEGIN
    EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', current_database());
END
$$;
