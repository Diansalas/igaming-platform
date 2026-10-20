-- Down of 0186 (DEV/STAGING ONLY): restores the 0183 exclusivity trigger function body byte for byte (and its invoker security) and drops the kyc_verifications guard.
SET LOCAL lock_timeout = '5s';

DROP TRIGGER kyc_verifications_transition_guard ON kyc_verifications;
DROP FUNCTION kyc_verifications_transition_guard();

CREATE OR REPLACE FUNCTION tenants_licence_basis_exclusive() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.licence_id IS NULL OR (TG_OP = 'UPDATE' AND NEW.licence_id IS NOT DISTINCT FROM OLD.licence_id) THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM public.licences l WHERE l.id = NEW.licence_id AND l.licensee = 'tenant') THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('operating_bases:licence:' || NEW.licence_id::text, 0));
        IF EXISTS (SELECT 1 FROM public.operating_bases b WHERE b.licence_id = NEW.licence_id AND b.tenant_id <> NEW.id) THEN
            RAISE EXCEPTION 'tenants: this tenant licence has been used by another tenant''s operating basis and cannot be bound to a different tenant (lic14)'
                USING ERRCODE = 'LC043';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
