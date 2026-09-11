DROP POLICY IF EXISTS tenant_isolation ON tenant_jurisdiction_configs;
ALTER TABLE tenant_jurisdiction_configs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenant_jurisdiction_configs DISABLE ROW LEVEL SECURITY;
