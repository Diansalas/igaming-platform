-- lic23 (security SN-4 and SN-6, final-tree confirmation review). Software capability only.
--
--   SN-4  tenants_licence_basis_exclusive (0183) read operating_bases and licences with the WRITING session's row-level security,
--         so a platform session carrying app.platform_service_id / acting settings (or a tenant or player session) saw no other
--         tenant's basis and the exclusivity check (LC043) passed unchecked. It is REPLACED by the same check run in a SECURITY
--         DEFINER function (owner, search_path pinned) that reads both tables in the platform-admin read shape and restores the
--         session settings. The down restores the 0183 function body byte for byte.
--   SN-6  kyc_verifications could be rewritten by the runtime role in ways that change what counts as residence evidence:
--         status moved backwards or out of a terminal status, provider_reference changed (an approved row turned into an
--         "orphan" the evidence read ignores), expires_at / created_at rewritten. A BEFORE UPDATE guard makes the status machine
--         the one the writers already follow (forward-only by rank unverified < pending < review_required < terminal; a terminal
--         status is final) and makes created_at, provider_reference (once set) and expires_at (once set) immutable. The future-dated
--         verified_residence_set_at half of SN-6 is in migration 0181 (unreleased, edited in place).
--
-- SQLSTATE: KV001 (illegal status transition), KV002 (immutable column).

SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION tenants_licence_basis_exclusive() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    -- Every session setting a row-level-security policy of licences / operating_bases keys on.
    v_names    constant text[] := ARRAY['app.platform_admin_principal_id', 'app.tenant_id', 'app.player_account_id', 'app.platform_service_id',
                                        'app.acting_tenant_id', 'app.acting_platform_principal_id', 'app.principal_id'];
    v_saved    text[] := ARRAY[]::text[];
    v_conflict boolean := false;
    i          integer;
BEGIN
    IF NEW.licence_id IS NULL OR (TG_OP = 'UPDATE' AND NEW.licence_id IS NOT DISTINCT FROM OLD.licence_id) THEN
        RETURN NEW;
    END IF;
    -- lic23 SN-4: the check must read operating_bases and licences as the PLATFORM sees them, whatever session writes the tenant row.
    -- Under a tenant, player, platform-service or acting session those tables are filtered by row-level security and the check would
    -- see no other tenant's basis and pass unchecked. This function is owned by the owner (SECURITY DEFINER, search_path pinned) and
    -- returns only the verdict; the session settings are saved, set to the platform-admin read shape for the two reads, and
    -- restored before it returns or raises.
    FOR i IN 1 .. array_length(v_names, 1) LOOP
        v_saved := v_saved || COALESCE(pg_catalog.current_setting(v_names[i], true), '');
    END LOOP;
    FOR i IN 1 .. array_length(v_names, 1) LOOP
        PERFORM pg_catalog.set_config(v_names[i], CASE WHEN v_names[i] = 'app.platform_admin_principal_id'
                                                       THEN '00000000-0000-0000-0000-000000000186' ELSE '' END, true);
    END LOOP;
    IF EXISTS (SELECT 1 FROM public.licences l WHERE l.id = NEW.licence_id AND l.licensee = 'tenant') THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('operating_bases:licence:' || NEW.licence_id::text, 0));
        v_conflict := EXISTS (SELECT 1 FROM public.operating_bases b WHERE b.licence_id = NEW.licence_id AND b.tenant_id <> NEW.id);
    END IF;
    FOR i IN 1 .. array_length(v_names, 1) LOOP
        PERFORM pg_catalog.set_config(v_names[i], v_saved[i], true);
    END LOOP;
    IF v_conflict THEN
        RAISE EXCEPTION 'tenants: this tenant licence has been used by another tenant''s operating basis and cannot be bound to a different tenant (lic14)'
            USING ERRCODE = 'LC043';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION kyc_verifications_transition_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_old integer;
    v_new integer;
BEGIN
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'kyc_verifications: created_at is immutable' USING ERRCODE = 'KV002';
    END IF;
    IF OLD.provider_reference IS NOT NULL AND NEW.provider_reference IS DISTINCT FROM OLD.provider_reference THEN
        RAISE EXCEPTION 'kyc_verifications: provider_reference is immutable once set' USING ERRCODE = 'KV002';
    END IF;
    IF OLD.expires_at IS NOT NULL AND NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'kyc_verifications: expires_at is immutable once set' USING ERRCODE = 'KV002';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        v_old := CASE OLD.status WHEN 'unverified' THEN 0 WHEN 'pending' THEN 1 WHEN 'review_required' THEN 2 ELSE 3 END;
        v_new := CASE NEW.status WHEN 'unverified' THEN 0 WHEN 'pending' THEN 1 WHEN 'review_required' THEN 2 ELSE 3 END;
        IF OLD.status IN ('approved', 'rejected', 'expired') OR v_new <= v_old THEN
            RAISE EXCEPTION 'kyc_verifications: illegal status transition % -> % (forward only; a terminal status is final)', OLD.status, NEW.status
                USING ERRCODE = 'KV001';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER kyc_verifications_transition_guard BEFORE UPDATE ON kyc_verifications
    FOR EACH ROW WHEN (NEW.status IS DISTINCT FROM OLD.status OR NEW.created_at IS DISTINCT FROM OLD.created_at
                       OR NEW.provider_reference IS DISTINCT FROM OLD.provider_reference OR NEW.expires_at IS DISTINCT FROM OLD.expires_at)
    EXECUTE FUNCTION kyc_verifications_transition_guard();

REVOKE ALL ON FUNCTION kyc_verifications_transition_guard() FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON FUNCTION kyc_verifications_transition_guard() FROM igaming_runtime;
    END IF;
END
$$;
