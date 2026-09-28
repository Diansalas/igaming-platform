-- PRH-2 G1 (KS-AUDIT-TENANT-1). ADR 0104 §3: a first-class
-- audit_log.subject_tenant_id gives a tenant read-only visibility into
-- platform-scope audit rows that concern it (starting with kill-switch
-- actions on that tenant), without any new write power into tenant scope
-- and without touching the existing dual_scope_isolation policy or the
-- append-only triggers (migration 0014/0016), which remain byte-identical.
--
-- NUMBERING NOTE: migration 0108 is allocated to another, unmerged
-- workstream and is not present on this branch. `migrate verify` will
-- therefore report a version gap (107 -> 109) until 0108 lands; this is
-- expected and the orchestrator resolves final numbering at merge
-- (see the G1 task instructions).

ALTER TABLE audit_log ADD COLUMN subject_tenant_id UUID NULL REFERENCES tenants (id);

ALTER TABLE audit_log ADD CONSTRAINT audit_log_subject_tenant_platform_only
    CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL);

-- Partial index: only subject rows are ever looked up by
-- (subject_tenant_id, created_at) - the tenant-projection read path
-- (internal/httpserver's newListPlatformActionsAuditHandler).
CREATE INDEX idx_audit_log_subject_tenant_time
    ON audit_log (subject_tenant_id, created_at DESC) WHERE subject_tenant_id IS NOT NULL;

-- Read-only additional policy (permissive policies OR together with the
-- existing dual_scope_isolation FOR ALL policy - this ADDS visibility, it
-- never narrows anything). SA-2/C-104-1: excludes player-scope,
-- platform-admin, platform-service and both app.acting_* sessions - only
-- a genuine, single-tenant staff session sees its own subject rows.
CREATE POLICY subject_tenant_read ON audit_log
    FOR SELECT
    USING (
        tenant_id IS NULL
        AND subject_tenant_id IS NOT NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- BEFORE INSERT trigger (S-10(4), C-104-1, SA-2): the ONLY gate on ever
-- setting subject_tenant_id. It does not check staff status (Q7 - the
-- audit records what happened; authorization is enforced elsewhere).
CREATE FUNCTION audit_log_subject_actor_guard() RETURNS TRIGGER AS $$
DECLARE
    v_platform TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
BEGIN
    IF NEW.subject_tenant_id IS NULL THEN
        RETURN NEW;
    END IF;

    IF v_platform IS NULL THEN
        RAISE EXCEPTION 'audit_log: subject_tenant_id requires a validated platform_admin_principal_id session';
    END IF;

    IF NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL
        OR NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
        OR NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        OR NULLIF(current_setting('app.platform_service_id', true), '') IS NOT NULL
        OR NULLIF(current_setting('app.acting_tenant_id', true), '') IS NOT NULL
        OR NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL
    THEN
        RAISE EXCEPTION 'audit_log: subject_tenant_id requires a pure platform-admin session (no tenant/player/principal/service/acting GUC set)';
    END IF;

    IF NEW.actor_type <> 'staff' OR NEW.actor_id IS DISTINCT FROM v_platform::uuid THEN
        RAISE EXCEPTION 'audit_log: subject_tenant_id requires actor_type=staff and actor_id equal to the session platform principal';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM staff_users WHERE id = v_platform::uuid AND tenant_id IS NULL) THEN
        RAISE EXCEPTION 'audit_log: platform principal % is not a platform-scoped staff_users row', v_platform;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_subject_actor_guard
    BEFORE INSERT ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_subject_actor_guard();

-- C-104-4 / orchestrator decision (option a): a nullable display name for
-- staff, hygiene-checked. N-3 (security confirmation): the CHECK also
-- refuses Unicode bidi-override and zero-width format characters, not
-- just [:cntrl:] - a name containing them could visually misrepresent
-- another actor's identity in the tenant projection (ADR 0104 §5.4).
ALTER TABLE staff_users ADD COLUMN display_name TEXT NULL
    CONSTRAINT staff_users_display_name_hygiene
    CHECK (
        display_name IS NULL
        OR (
            char_length(display_name) BETWEEN 1 AND 100
            AND display_name !~ '[[:cntrl:]]'
            -- Bidi overrides U+202A-U+202E, isolates U+2066-U+2069, and
            -- zero-width characters U+200B-U+200F.
            AND display_name !~ '[‪-‮⁦-⁩​-‏]'
        )
    );
