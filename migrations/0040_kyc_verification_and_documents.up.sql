-- Stage 4F: platform-owned player verification (KYC) and document
-- management foundation, plus email-verification/password-reset
-- credential tokens. See docs/decisions/0028-kyc-provider-abstraction.md,
-- 0029-document-storage-and-security.md, and 0030-email-verification-and-
-- password-reset.md for full design rationale.
--
-- Deliberate scope decision (recorded here, not just in docs): both
-- kyc_verifications and kyc_documents are TENANT-OWNED (tenant_id NOT
-- NULL, standard tenant_isolation RLS), NOT platform-wide like persons -
-- unlike Stage 4D-RG's player_restrictions, this stage does not attempt
-- cross-tenant/cross-brand REUSE of a verification (e.g. "verified at
-- Brand A therefore automatically verified at Brand B"). Whether an
-- approved verification should ever apply platform-wide to the Person is
-- an explicit OPEN DECISION (ADR 0028 §7), not resolved by this schema -
-- person_id is carried on both tables only as a denormalized anchor for a
-- future platform-scoped read, never as an enforcement or RLS key today.
-- This is a narrower, safer, and much simpler starting point than
-- reintroducing player_restrictions' dual-scope RLS model for a reuse
-- policy nobody has decided yet.
--
-- Never store document binaries here - kyc_documents holds authoritative
-- METADATA and an opaque storage_reference a DocumentStorageProvider
-- implementation understands (internal/kyc), never raw content.

-- Lets kyc_verifications/kyc_documents/player_credential_tokens enforce
-- "my player_account_id/reviewed_by really belongs to my own tenant_id"
-- via a composite foreign key instead of trusting application code to
-- keep the two consistent - same rationale as brands' own UNIQUE (id,
-- tenant_id) (migration 0008). Closes a PostgreSQL/RLS specialist review
-- P1 finding: without this, a row could be inserted naming a DIFFERENT
-- tenant's player_account_id/reviewed_by while carrying this table's own
-- tenant_id, relying entirely on application code never doing that.
ALTER TABLE player_accounts ADD CONSTRAINT player_accounts_id_tenant_unique UNIQUE (id, tenant_id);
ALTER TABLE staff_users ADD CONSTRAINT staff_users_id_tenant_unique UNIQUE (id, tenant_id);

CREATE TABLE kyc_verifications (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    brand_id           UUID NOT NULL,
    player_account_id  UUID NOT NULL,
    -- Denormalized anchor to the platform-wide Person - not an RLS or
    -- enforcement key this stage (see header comment's OPEN DECISION).
    person_id          UUID NOT NULL REFERENCES persons (id),
    status             TEXT NOT NULL DEFAULT 'unverified'
                           CHECK (status IN ('unverified', 'pending', 'review_required', 'approved', 'rejected', 'expired')),
    -- Which registered KYCProvider adapter is handling this verification
    -- (e.g. 'mock') - a plain identifier, not a foreign key, mirroring
    -- payments/casino's own provider_id convention (ADR 0022/0025):
    -- providers are registered in code (internal/kyc.Orchestrator), not
    -- as database rows.
    provider_id        TEXT NOT NULL,
    -- Opaque reference the PROVIDER assigns - never a raw provider
    -- payload or vendor-specific structure (ADR 0028 §4/§10).
    provider_reference TEXT,
    -- Machine-readable reason for the current status (e.g. a rejection
    -- code) - must never contain raw KYC evidence (directive §10/§17).
    reason             TEXT,
    submitted_at       TIMESTAMPTZ,
    reviewed_at        TIMESTAMPTZ,
    reviewed_by        UUID,
    expires_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((reviewed_at IS NULL) = (reviewed_by IS NULL)),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    FOREIGN KEY (reviewed_by, tenant_id) REFERENCES staff_users (id, tenant_id),
    -- Lets kyc_documents.verification_id enforce the same composite,
    -- tenant-matching FK below.
    UNIQUE (id, tenant_id)
);

CREATE INDEX idx_kyc_verifications_tenant ON kyc_verifications (tenant_id);
CREATE INDEX idx_kyc_verifications_player_account ON kyc_verifications (player_account_id);
CREATE INDEX idx_kyc_verifications_person ON kyc_verifications (person_id);
-- Callback dispatch (Orchestrator.ReceiveCallback) looks a verification up
-- by (provider_id, provider_reference) inside an already tenant-scoped
-- transaction (kyc_admin_handlers.go) - scoped to tenant_id, not global,
-- so one tenant's own reference namespace can never collide with or be
-- probed via another's (PostgreSQL/RLS specialist review P2: a real
-- B2B tenant may hold its own vendor account under the same provider_id
-- string, with a reference namespace that need not be globally disjoint
-- from another tenant's).
CREATE UNIQUE INDEX idx_kyc_verifications_provider_ref ON kyc_verifications (tenant_id, provider_id, provider_reference) WHERE provider_reference IS NOT NULL;

ALTER TABLE kyc_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_verifications FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON kyc_verifications
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- kyc_verifications' own status/reason/reviewed_at/reviewed_by/etc are
-- deliberately mutable in place (the verification state machine itself -
-- internal/kyc.updateVerificationStatus/ReviewVerification), unlike
-- kyc_documents' evidence fields. But the ROW itself must never disappear
-- - deleting a verification would silently destroy the audit trail of
-- what was ever submitted/decided (directive §5's "never silently delete
-- verification evidence" applies to the verification record, not just
-- its attached documents). Closes a PostgreSQL/RLS specialist review P1
-- finding: this table previously had zero append-only protection at all.
CREATE FUNCTION kyc_verifications_deny_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'kyc_verifications is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_verifications_no_delete
    BEFORE DELETE ON kyc_verifications
    FOR EACH ROW EXECUTE FUNCTION kyc_verifications_deny_delete();

CREATE TRIGGER kyc_verifications_no_truncate
    BEFORE TRUNCATE ON kyc_verifications
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_verifications_deny_delete();

CREATE TABLE kyc_documents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    brand_id            UUID NOT NULL,
    player_account_id   UUID NOT NULL,
    person_id           UUID NOT NULL REFERENCES persons (id),
    verification_id     UUID NOT NULL,
    document_type       TEXT NOT NULL CHECK (document_type IN ('passport', 'national_id', 'drivers_license', 'proof_of_address', 'selfie', 'other')),
    issuing_country     TEXT,
    -- Versioning: a REUPLOAD of the same document_type for the same
    -- account is a NEW row with version+1, never an UPDATE of the
    -- existing row - directive §5's "do not overwrite historical
    -- documents" / "documents must be immutable after submission".
    version             INT NOT NULL DEFAULT 1 CHECK (version > 0),
    status              TEXT NOT NULL DEFAULT 'pending_review' CHECK (status IN ('pending_review', 'approved', 'rejected')),
    -- Which registered DocumentStorageProvider holds the actual bytes,
    -- and the opaque reference/key it understands - the database is
    -- authoritative METADATA only (directive §6).
    storage_provider    TEXT NOT NULL,
    storage_reference   TEXT NOT NULL,
    content_type        TEXT NOT NULL,
    size_bytes          BIGINT NOT NULL CHECK (size_bytes > 0),
    original_filename   TEXT NOT NULL,
    checksum_sha256     TEXT NOT NULL,
    -- The DOCUMENT's own expiry (e.g. a passport's expiry date) - not
    -- this row's own retention/TTL, which is a future storage-lifecycle
    -- concern (ADR 0029 §6).
    document_expires_at TIMESTAMPTZ,
    rejection_reason    TEXT,
    uploaded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_at         TIMESTAMPTZ,
    reviewed_by         UUID,
    CHECK ((reviewed_at IS NULL) = (reviewed_by IS NULL)),
    UNIQUE (player_account_id, document_type, version),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    FOREIGN KEY (verification_id, tenant_id) REFERENCES kyc_verifications (id, tenant_id),
    FOREIGN KEY (reviewed_by, tenant_id) REFERENCES staff_users (id, tenant_id)
);

CREATE INDEX idx_kyc_documents_tenant ON kyc_documents (tenant_id);
CREATE INDEX idx_kyc_documents_player_account ON kyc_documents (player_account_id);
CREATE INDEX idx_kyc_documents_verification ON kyc_documents (verification_id);

ALTER TABLE kyc_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_documents FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON kyc_documents
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Immutability: only status/reviewed_at/reviewed_by/rejection_reason may
-- ever change (the review outcome) - every other column, once inserted,
-- is fixed for that row's lifetime, and DELETE/TRUNCATE are refused
-- outright (directive §5's "do not silently delete verification
-- evidence"). Mirrors casino_launch_sessions_enforce_immutable_fields'
-- (migration 0036) and player_restrictions_deny_mutation's (migration
-- 0037) established pattern.
CREATE FUNCTION kyc_documents_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'kyc_documents is append-only: % is not permitted', TG_OP;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.tenant_id <> OLD.tenant_id
        OR NEW.brand_id <> OLD.brand_id
        OR NEW.player_account_id <> OLD.player_account_id
        OR NEW.person_id <> OLD.person_id
        OR NEW.verification_id <> OLD.verification_id
        OR NEW.document_type <> OLD.document_type
        OR NEW.version <> OLD.version
        OR NEW.storage_provider <> OLD.storage_provider
        OR NEW.storage_reference <> OLD.storage_reference
        OR NEW.content_type <> OLD.content_type
        OR NEW.size_bytes <> OLD.size_bytes
        OR NEW.original_filename <> OLD.original_filename
        OR NEW.checksum_sha256 <> OLD.checksum_sha256
        OR NEW.uploaded_at <> OLD.uploaded_at
        OR COALESCE(NEW.issuing_country, '') <> COALESCE(OLD.issuing_country, '')
        OR COALESCE(NEW.document_expires_at, 'epoch'::timestamptz) <> COALESCE(OLD.document_expires_at, 'epoch'::timestamptz)
    THEN
        RAISE EXCEPTION 'kyc_documents: core document fields are immutable after upload - only status/reviewed_at/reviewed_by/rejection_reason may change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_documents_immutable_core
    BEFORE UPDATE ON kyc_documents
    FOR EACH ROW EXECUTE FUNCTION kyc_documents_enforce_immutability();

CREATE TRIGGER kyc_documents_deny_delete
    BEFORE DELETE ON kyc_documents
    FOR EACH ROW EXECUTE FUNCTION kyc_documents_enforce_immutability();

CREATE TRIGGER kyc_documents_no_truncate
    BEFORE TRUNCATE ON kyc_documents
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_documents_enforce_immutability();

-- Email-verification and password-reset one-time tokens. One shared
-- table (purpose-discriminated) rather than two near-identical tables -
-- both are "a random, hashed, short-lived, one-time-use credential proof
-- tied to a player_account", differing only in what they authorize.
CREATE TABLE player_credential_tokens (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    player_account_id  UUID NOT NULL,
    purpose            TEXT NOT NULL CHECK (purpose IN ('email_verification', 'password_reset')),
    -- SHA-256 hex of the actual, unguessable token - never the raw value
    -- (identical never-store-the-secret principle as sessions.
    -- refresh_token_hash / migration 0018's own rationale). The raw token
    -- exists only in the confirmation email/link and the request/response
    -- bodies at issuance time, never persisted or logged.
    token_hash         TEXT NOT NULL,
    expires_at         TIMESTAMPTZ NOT NULL,
    consumed_at        TIMESTAMPTZ,
    requested_ip       INET,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (token_hash),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);

CREATE INDEX idx_player_credential_tokens_account_purpose ON player_credential_tokens (player_account_id, purpose, created_at);

ALTER TABLE player_credential_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE player_credential_tokens FORCE ROW LEVEL SECURITY;

-- Ordinary tenant-scoped visibility - used by the REQUEST side (already
-- running inside a WithTenant transaction that resolved the brand) to
-- insert a new token, supersede prior outstanding ones, and count recent
-- requests for rate-limiting.
CREATE POLICY tenant_isolation ON player_credential_tokens
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Lookup-by-hash visibility for the CONFIRM side, mirroring sessions'
-- own app.session_lookup_hash pattern exactly (migration 0018/ADR 0016):
-- a presented token carries no tenant hint, so there is no tenant scope
-- to query with before reading the one row that would reveal it. Grants
-- visibility to at most the single row whose token_hash exactly equals
-- the presented value - possessing the exact, unguessable raw token is
-- the only thing that ever satisfies this policy. The trailing "AND no
-- tenant is set" conjunct mirrors migration 0018's own
-- session_select_internal_op reasoning exactly (a one-line future
-- footgun closed pre-emptively, PostgreSQL/RLS specialist review P2):
-- WithCredentialTokenLookup itself never sets app.tenant_id today, but
-- without this conjunct a future caller that combined an ordinary
-- WithTenant scope with this GUC would unintentionally see a DIFFERENT
-- tenant's token row purely by knowing its hash.
CREATE POLICY token_lookup ON player_credential_tokens
    FOR SELECT
    USING (
        token_hash = NULLIF(current_setting('app.credential_token_lookup_hash', true), '')
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
    );

-- Deleting a credential token would destroy the audit trail of a
-- requested email-verification/password-reset action (directive §17's
-- "audit every... password reset requested/completed" implies the
-- request record itself must survive). Consumption is an UPDATE
-- (consumed_at), which remains fully allowed - only DELETE/TRUNCATE are
-- refused. Closes a PostgreSQL/RLS specialist review P1 finding: this
-- table previously had zero append-only protection at all.
CREATE FUNCTION player_credential_tokens_deny_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'player_credential_tokens is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER player_credential_tokens_no_delete
    BEFORE DELETE ON player_credential_tokens
    FOR EACH ROW EXECUTE FUNCTION player_credential_tokens_deny_delete();

CREATE TRIGGER player_credential_tokens_no_truncate
    BEFORE TRUNCATE ON player_credential_tokens
    FOR EACH STATEMENT EXECUTE FUNCTION player_credential_tokens_deny_delete();

COMMENT ON COLUMN player_accounts.verified_at IS 'Stage 4F: set when this specific brand relationship''s EMAIL is confirmed via the player_credential_tokens email_verification flow (internal/auth, internal/httpserver credential handlers). Distinct from platform-wide KYC/identity verification, which lives entirely in kyc_verifications/kyc_documents and is never written here - see docs/decisions/0030.';
