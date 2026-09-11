DROP POLICY IF EXISTS persons_platform_scope_delete ON persons;
DROP POLICY IF EXISTS persons_platform_scope_update ON persons;
DROP POLICY IF EXISTS persons_platform_scope_read_write ON persons;
DROP POLICY IF EXISTS persons_insert_any_scope ON persons;

ALTER TABLE persons NO FORCE ROW LEVEL SECURITY;
ALTER TABLE persons DISABLE ROW LEVEL SECURITY;
