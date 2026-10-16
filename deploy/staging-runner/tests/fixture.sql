-- Synthetic fixture for the local harness (superuser of the THROWAWAY cluster, bypasses RLS): one tenant with a licence, one brand and one
-- platform admin, so seed-market can be exercised. Brands are created pending_launch (launch_subject_insert_guard).
INSERT INTO jurisdictions (id, code, name) VALUES ('00000000-0000-0000-0000-0000000000a1', 'SV-LOCAL', 'Local Jurisdiction');
INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', 'platform', 'LOCAL-1');
INSERT INTO tenants (id, slug, name, licensing_model, licence_id) VALUES ('00000000-0000-0000-0000-0000000000c1', 'demo-casino', 'Demo Casino', 'under_platform_licence', '00000000-0000-0000-0000-0000000000b1');
INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000c1', 'demo-brand', 'Demo Brand', 'pending_launch');
INSERT INTO persons (id) VALUES ('00000000-0000-0000-0000-0000000000e1');
INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ('00000000-0000-0000-0000-0000000000f1', NULL, 'admin@example.test', 'x', 'platform_admin', '00000000-0000-0000-0000-0000000000e1', 'active');
