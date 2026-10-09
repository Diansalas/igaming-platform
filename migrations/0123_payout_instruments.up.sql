-- B13 (ADR 0111 section 2, revision 2; owner decisions ADR 0095 section 44
-- decisions 1-8): provider-neutral PAYOUT INSTRUMENTS and payout destination
-- binding. Migration 0123 (0122 = PAY-RECEIPT-ANOMALY-APPLIED-1 and 0124 =
-- HSEC are other workstreams; a `migrate verify` of this branch alone reports a
-- version gap at 0122 until 0122 merges - merge strictly in number order).
--
-- WHAT THIS MIGRATION HOLDS. ADR 0111 section 7.2 keeps B13 as ONE migration
-- ("none merged, none split further") and section 7.1 says B13-B "uses 0123", so
-- the whole B13 SCHEMA is here: the entity tables (B13-A), AND the binding
-- columns on withdrawal_requests, the write-once destination snapshot table and
-- the payment_attempts snapshot constraint (used by B13-B). The Go side of
-- B13-A is internal/payoutinstrument; the Go integration of the binding into
-- the withdrawal / payments paths is B13-B and is NOT part of this change.
--
-- TRANSITIONAL (recorded in ADR 0111 appendix "B13-A implementation notes",
-- ambiguity B13A-2): the section 2.1 BEFORE INSERT trigger on withdrawal_requests
-- "requires both NOT NULL". Until B13-B wires the request path no code supplies a
-- binding, so enforcing NOT NULL now would break every existing withdrawal
-- insert. This migration therefore VALIDATES a binding whenever one is supplied
-- (instrument exists, same tenant/brand/player, verified with an in-force
-- latest verification, no revoke, no later suspend, asset listed, fingerprint
-- equal) and tolerates NULL/NULL. B13-B ships its OWN migration that replaces
-- withdrawal_requests_payout_binding_guard() so it refuses NULL/NULL on INSERT, in
-- the same change that makes the request path always bind (0123 is not amended). A NULL binding is dispatchable only to
-- Synthetic adapters (A-11; tiering predicate in Go).
--
-- KEYS NEVER IN THE DATABASE. The detail ciphertext, the seals and the
-- fingerprints are produced in Go with keys held in configuration only. The
-- database stores ciphertext + nonce + key ids + seals and enforces STRUCTURE
-- (state machine, same-transaction verification, immutability, append-only,
-- composite FKs). It cannot verify a MAC and does not try to.

-- =========================================================================
-- 0. Up-time refusal (ADR 0111 2.1 "Migration-time assertion", L-6 / A-11):
--    a non-terminal pre-0123 withdrawal that already carries a provider id
--    outside the literal MOCK provider-id set would be dispatched without a
--    binding to a possibly non-Synthetic adapter. The tables are FORCE RLS and
--    this session has no app.tenant_id, so the scan iterates tenants (the
--    0099/0115 pattern). The literal set is pinned equal to the ids of the
--    providerkind.Synthetic payment adapters by a Go test.
-- =========================================================================

-- C-4: block concurrent writers for the rest of this transaction so no row can gain
-- a non-MOCK provider_id between the scan below and the ALTER TABLE further down.
-- (Scale note, ADR 0111 15.5: no lock_timeout / NOT VALID + VALIDATE here.)
LOCK TABLE withdrawal_requests IN SHARE ROW EXCLUSIVE MODE;

DO $$
DECLARE
    mock_ids CONSTANT TEXT[] := ARRAY['mock-payments'];
    tenant_rec RECORD;
    n BIGINT;
    total BIGINT := 0;
BEGIN
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) INTO n FROM withdrawal_requests
         WHERE tenant_id = tenant_rec.id
           AND state IN ('requested', 'pending_review', 'approved', 'submitted')
           AND provider_id IS NOT NULL
           AND NOT (provider_id = ANY (mock_ids));
        total := total + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF total > 0 THEN
        RAISE EXCEPTION 'migration 0123 pre-flight: % non-terminal pre-0123 withdrawal(s) carry a provider_id outside the MOCK set; they have no payout destination binding. Nothing was changed. Never delete rows; escalate to the human (ADR 0111 L-6).', total
            USING ERRCODE = 'PI098';
    END IF;
END $$;

-- =========================================================================
-- 1. payout_instrument_kinds: reference, SELECT-only for the runtime role.
-- =========================================================================

CREATE TABLE payout_instrument_kinds (
    code                  TEXT PRIMARY KEY CHECK (code ~ '^[a-z][a-z0-9_]{1,31}$'),
    detail_schema_version INT NOT NULL CHECK (detail_schema_version >= 1),
    allowed_rails         TEXT[] NOT NULL CHECK (cardinality(allowed_rails) >= 1),
    non_synthetic_enabled BOOLEAN NOT NULL,
    -- C-3: the asset type every listed asset must have (assets.asset_type); NULL = any
    -- (synthetic_test). A bank account cannot list BTC; a crypto address cannot list EUR.
    allowed_asset_type    TEXT NULL CHECK (allowed_asset_type IN ('fiat', 'crypto'))
);

INSERT INTO payout_instrument_kinds (code, detail_schema_version, allowed_rails, non_synthetic_enabled, allowed_asset_type) VALUES
    ('bank_account',     1, ARRAY['bank_transfer', 'sepa', 'faster_payments', 'pix', 'spei'], true,  'fiat'),
    ('card_token',       1, ARRAY['card'],                                                  true,  'fiat'),
    ('ewallet_account',  1, ARRAY['ewallet'],                                               true,  'fiat'),
    -- ADR 0008 / HD-R15-4: crypto destinations are not enabled for real use.
    ('crypto_address',   1, ARRAY['crypto'],                                                false, 'crypto'),
    ('synthetic_test',   1, ARRAY['synthetic', 'card', 'bank_transfer', 'ewallet'],         false, NULL);

ALTER TABLE payout_instrument_kinds ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_instrument_kinds FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON payout_instrument_kinds FOR SELECT USING (true);
CREATE TRIGGER payout_instrument_kinds_immutable
    BEFORE UPDATE OR DELETE ON payout_instrument_kinds FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_instrument_kinds_no_truncate
    BEFORE TRUNCATE ON payout_instrument_kinds FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- Per-jurisdiction verification max age. Runtime role: SELECT only. Rows are
-- written by the migration/owner role until a governed four-eyes writer exists
-- (ADR 0110 T8; ADR 0111 2.1). With no row for the tenant's jurisdiction no
-- non-synthetic verification can be recorded (A-6, HD-R15-1).
CREATE TABLE payout_instrument_verification_max_age (
    jurisdiction_id UUID PRIMARY KEY REFERENCES jurisdictions (id),
    max_age         INTERVAL NOT NULL CHECK (max_age > interval '0' AND max_age <= interval '3650 days')
);
-- RLS is ENABLEd but deliberately NOT FORCEd: the table is platform-wide (no
-- tenant_id) and its only writer is the owner/migration role, which a FORCEd
-- table with a SELECT-only policy would lock out of its own table. The runtime
-- role is not the owner, so it is bound by RLS (read policy only) AND has no
-- write grant.
ALTER TABLE payout_instrument_verification_max_age ENABLE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON payout_instrument_verification_max_age FOR SELECT USING (true);

-- =========================================================================
-- 2. Fingerprint owners (append-only). The first Person to register a
--    fingerprint in a tenant owns it; another Person conflicts on the PK.
-- =========================================================================

CREATE TABLE payout_instrument_fingerprint_owners (
    tenant_id        UUID NOT NULL REFERENCES tenants (id),
    fingerprint_kid  TEXT NOT NULL CHECK (fingerprint_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    fingerprint      TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    person_id        UUID NOT NULL REFERENCES persons (id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, fingerprint_kid, fingerprint),
    -- Target of payout_instruments' composite FK.
    UNIQUE (tenant_id, fingerprint_kid, fingerprint, person_id)
);

-- =========================================================================
-- 3. payout_instruments
-- =========================================================================


CREATE TABLE payout_instruments (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL,
    brand_id                UUID NOT NULL,
    player_account_id       UUID NOT NULL,
    person_id               UUID NOT NULL,
    kind                    TEXT NOT NULL REFERENCES payout_instrument_kinds (code),
    rail                    TEXT NOT NULL CHECK (rail ~ '^[a-z][a-z0-9_]{1,31}$'),
    asset_codes             TEXT[] NOT NULL CHECK (cardinality(asset_codes) BETWEEN 1 AND 16),
    -- AEAD (AES-256-GCM) ciphertext of the kind-specific detail; the plaintext
    -- is never stored. 12-byte nonce; the 16-byte tag is part of the ciphertext.
    detail_ciphertext       BYTEA NOT NULL CHECK (octet_length(detail_ciphertext) BETWEEN 17 AND 4112),
    detail_nonce            BYTEA NOT NULL CHECK (octet_length(detail_nonce) = 12),
    detail_key_kid          TEXT NOT NULL CHECK (detail_key_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    detail_schema_version   INT NOT NULL,
    display_mask            TEXT NOT NULL CHECK (octet_length(display_mask) BETWEEN 1 AND 64),
    fingerprint             TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    fingerprint_kid         TEXT NOT NULL CHECK (fingerprint_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    supersedes_instrument_id UUID NULL REFERENCES payout_instruments (id),
    instrument_seal         TEXT NOT NULL CHECK (instrument_seal ~ '^[0-9a-f]{64}$'),
    seal_kid                TEXT NOT NULL CHECK (seal_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    state                   TEXT NOT NULL DEFAULT 'pending_verification' CHECK (state IN (
        'pending_verification', 'verified', 'verification_expired', 'suspended', 'revoked', 'rejected', 'superseded')),
    current_verification_id UUID NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    state_changed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (state <> 'verified' OR current_verification_id IS NOT NULL),
    CHECK (supersedes_instrument_id IS NULL OR supersedes_instrument_id <> id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    -- brand = the player account's brand, structurally.
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id),
    -- A fingerprint is owned by exactly one Person per tenant; the instrument's
    -- Person must be that owner (H-2).
    FOREIGN KEY (tenant_id, fingerprint_kid, fingerprint, person_id)
        REFERENCES payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id),
    UNIQUE (id, tenant_id),
    UNIQUE (tenant_id, id, fingerprint)
);

CREATE UNIQUE INDEX payout_instruments_player_fingerprint_active
    ON payout_instruments (tenant_id, player_account_id, fingerprint_kid, fingerprint)
    WHERE state IN ('pending_verification', 'verified', 'verification_expired', 'suspended');
CREATE INDEX payout_instruments_player ON payout_instruments (tenant_id, player_account_id);
CREATE INDEX payout_instruments_supersedes ON payout_instruments (supersedes_instrument_id) WHERE supersedes_instrument_id IS NOT NULL;

-- =========================================================================
-- 4. Verifications (append-only)
-- =========================================================================

CREATE TABLE payout_instrument_verifications (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    instrument_id             UUID NOT NULL,
    source                    TEXT NOT NULL CHECK (source IN (
        'psp_account_verification', 'kyc_vendor_instrument_verification', 'custodian_address_verification', 'synthetic')),
    ownership_assertion       TEXT NOT NULL CHECK (ownership_assertion IN (
        'account_holder_matches_verified_identity', 'synthetic_asserted')),
    verifier_provider_id      TEXT NOT NULL CHECK (octet_length(verifier_provider_id) BETWEEN 1 AND 64),
    verifier_reference_hash   TEXT NULL CHECK (verifier_reference_hash IS NULL OR verifier_reference_hash ~ '^[0-9a-f]{64}$'),
    outcome                   TEXT NOT NULL CHECK (outcome IN ('verified', 'rejected')),
    verified_at               TIMESTAMPTZ NOT NULL,
    expires_at                TIMESTAMPTZ NOT NULL,
    verification_seal         TEXT NOT NULL CHECK (verification_seal ~ '^[0-9a-f]{64}$'),
    seal_kid                  TEXT NOT NULL CHECK (seal_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    created_txid              BIGINT NOT NULL,
    CHECK ((source = 'synthetic') = (ownership_assertion = 'synthetic_asserted')),
    CHECK (expires_at > verified_at),
    FOREIGN KEY (instrument_id, tenant_id) REFERENCES payout_instruments (id, tenant_id),
    UNIQUE (id, tenant_id),
    -- Target of payout_instruments.current_verification_id: the verification
    -- must belong to THIS instrument, structurally.
    UNIQUE (id, tenant_id, instrument_id)
);
CREATE INDEX payout_instrument_verifications_instrument ON payout_instrument_verifications (tenant_id, instrument_id, verified_at DESC);

ALTER TABLE payout_instruments
    ADD CONSTRAINT payout_instruments_current_verification_fk
    FOREIGN KEY (current_verification_id, tenant_id, id)
    REFERENCES payout_instrument_verifications (id, tenant_id, instrument_id);

-- =========================================================================
-- 5. Blocking events (append-only)
-- =========================================================================

CREATE TABLE payout_instrument_blocking_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    instrument_id  UUID NOT NULL,
    event          TEXT NOT NULL CHECK (event IN ('suspend', 'revoke')),
    actor_type     TEXT NOT NULL CHECK (actor_type IN ('player', 'staff', 'provider')),
    actor_id       TEXT NOT NULL CHECK (octet_length(actor_id) BETWEEN 1 AND 64),
    reason_code    TEXT NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_seal     TEXT NOT NULL CHECK (event_seal ~ '^[0-9a-f]{64}$'),
    seal_kid       TEXT NOT NULL CHECK (seal_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    created_txid   BIGINT NOT NULL,
    FOREIGN KEY (instrument_id, tenant_id) REFERENCES payout_instruments (id, tenant_id)
);
CREATE INDEX payout_instrument_blocking_events_instrument ON payout_instrument_blocking_events (tenant_id, instrument_id, occurred_at);

-- =========================================================================
-- 6. Triggers: structure the database CAN enforce
-- =========================================================================

-- 6.1 Fingerprint owners: append-only.
CREATE TRIGGER payout_instrument_fingerprint_owners_immutable
    BEFORE UPDATE OR DELETE ON payout_instrument_fingerprint_owners FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_instrument_fingerprint_owners_no_truncate
    BEFORE TRUNCATE ON payout_instrument_fingerprint_owners FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- 6.2 Instrument INSERT.
CREATE FUNCTION payout_instruments_before_insert() RETURNS TRIGGER AS $$
DECLARE
    v_kind payout_instrument_kinds%ROWTYPE;
    v_acct RECORD;
    v_sup RECORD;
    v_asset TEXT;
BEGIN
    SELECT * INTO v_kind FROM payout_instrument_kinds WHERE code = NEW.kind;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'payout_instruments: unknown kind' USING ERRCODE = 'PI001';
    END IF;
    IF NOT (NEW.rail = ANY (v_kind.allowed_rails)) THEN
        RAISE EXCEPTION 'payout_instruments: rail is not allowed for this kind' USING ERRCODE = 'PI002';
    END IF;
    SELECT pa.person_id, pa.brand_id INTO v_acct
      FROM player_accounts pa WHERE pa.id = NEW.player_account_id AND pa.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'payout_instruments: player account not found in tenant' USING ERRCODE = 'PI003';
    END IF;
    IF NEW.brand_id IS DISTINCT FROM v_acct.brand_id THEN
        RAISE EXCEPTION 'payout_instruments: brand must be the player account brand' USING ERRCODE = 'PI003';
    END IF;
    -- DB-forced from the account (the composite FK to the owners then binds it).
    NEW.person_id := v_acct.person_id;
    NEW.detail_schema_version := v_kind.detail_schema_version;
    IF NEW.state <> 'pending_verification' OR NEW.current_verification_id IS NOT NULL THEN
        RAISE EXCEPTION 'payout_instruments: a new instrument is pending_verification with no verification' USING ERRCODE = 'PI004';
    END IF;
    IF (SELECT count(DISTINCT a) FROM unnest(NEW.asset_codes) a) <> cardinality(NEW.asset_codes) THEN
        RAISE EXCEPTION 'payout_instruments: duplicate asset code' USING ERRCODE = 'PI005';
    END IF;
    FOREACH v_asset IN ARRAY NEW.asset_codes LOOP
        IF NOT EXISTS (SELECT 1 FROM assets WHERE code = v_asset AND active) THEN
            RAISE EXCEPTION 'payout_instruments: asset is not in the active asset registry' USING ERRCODE = 'PI005';
        END IF;
        IF v_kind.allowed_asset_type IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM assets WHERE code = v_asset AND asset_type = v_kind.allowed_asset_type) THEN
            RAISE EXCEPTION 'payout_instruments: the kind only admits % assets', v_kind.allowed_asset_type USING ERRCODE = 'PI007';
        END IF;
    END LOOP;
    IF NEW.supersedes_instrument_id IS NOT NULL THEN
        SELECT i.tenant_id, i.player_account_id, i.state INTO v_sup FROM payout_instruments i WHERE i.id = NEW.supersedes_instrument_id;
        IF NOT FOUND OR v_sup.tenant_id <> NEW.tenant_id OR v_sup.player_account_id <> NEW.player_account_id THEN
            RAISE EXCEPTION 'payout_instruments: supersedes must be a same-player instrument' USING ERRCODE = 'PI006';
        END IF;
        IF v_sup.state IN ('revoked', 'rejected', 'superseded') THEN
            RAISE EXCEPTION 'payout_instruments: cannot supersede a terminal instrument' USING ERRCODE = 'PI006';
        END IF;
    END IF;
    NEW.created_at := now();
    NEW.state_changed_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payout_instruments_before_insert
    BEFORE INSERT ON payout_instruments FOR EACH ROW EXECUTE FUNCTION payout_instruments_before_insert();

-- 6.3 Instrument UPDATE: the verification state machine (all sessions).
CREATE FUNCTION payout_instruments_before_update() RETURNS TRIGGER AS $$
DECLARE
    v_ok BOOLEAN;
BEGIN
    IF OLD.state IN ('revoked', 'rejected', 'superseded') THEN
        RAISE EXCEPTION 'payout_instruments: % is a terminal state', OLD.state USING ERRCODE = 'PI010';
    END IF;
    -- Column discipline: only state, state_changed_at (forced) and
    -- current_verification_id may change.
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.brand_id IS DISTINCT FROM OLD.brand_id OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
       OR NEW.person_id IS DISTINCT FROM OLD.person_id OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.rail IS DISTINCT FROM OLD.rail OR NEW.asset_codes IS DISTINCT FROM OLD.asset_codes
       OR NEW.detail_ciphertext IS DISTINCT FROM OLD.detail_ciphertext OR NEW.detail_nonce IS DISTINCT FROM OLD.detail_nonce
       OR NEW.detail_key_kid IS DISTINCT FROM OLD.detail_key_kid OR NEW.detail_schema_version IS DISTINCT FROM OLD.detail_schema_version
       OR NEW.display_mask IS DISTINCT FROM OLD.display_mask OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint
       OR NEW.fingerprint_kid IS DISTINCT FROM OLD.fingerprint_kid
       OR NEW.supersedes_instrument_id IS DISTINCT FROM OLD.supersedes_instrument_id
       OR NEW.instrument_seal IS DISTINCT FROM OLD.instrument_seal OR NEW.seal_kid IS DISTINCT FROM OLD.seal_kid
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'payout_instruments: identity columns are immutable (a destination change is a new instrument)' USING ERRCODE = 'PI011';
    END IF;

    IF NEW.state = OLD.state THEN
        IF NEW.current_verification_id IS NOT DISTINCT FROM OLD.current_verification_id THEN
            NEW.state_changed_at := OLD.state_changed_at;
            RETURN NEW;
        END IF;
        -- A fresh verification of an already-verified instrument (renewal) is
        -- the only same-state change, and it needs the same proof as any
        -- transition into verified, handled below.
        IF NEW.state <> 'verified' THEN
            RAISE EXCEPTION 'payout_instruments: current_verification_id changes only with a transition into verified' USING ERRCODE = 'PI012';
        END IF;
    END IF;

    -- Transition matrix.
    v_ok := CASE
        WHEN OLD.state = 'pending_verification' AND NEW.state IN ('verified', 'rejected', 'revoked', 'superseded') THEN true
        WHEN OLD.state = 'verified' AND NEW.state IN ('verified', 'verification_expired', 'suspended', 'revoked', 'superseded') THEN true
        WHEN OLD.state = 'verification_expired' AND NEW.state IN ('verified', 'suspended', 'revoked', 'superseded') THEN true
        WHEN OLD.state = 'suspended' AND NEW.state IN ('verified', 'revoked', 'superseded') THEN true
        ELSE false
    END;
    IF NOT v_ok THEN
        RAISE EXCEPTION 'payout_instruments: transition % -> % is not allowed', OLD.state, NEW.state USING ERRCODE = 'PI013';
    END IF;

    -- EVERY transition into verified (and a renewal) needs a verification row
    -- written in THIS transaction for THIS instrument with outcome verified:
    -- no un-blocking by a bare UPDATE, and an OLD verification can never be
    -- re-pointed (un-suspend via an old verification is refused).
    IF NEW.state = 'verified' THEN
        IF NEW.current_verification_id IS NULL OR NOT EXISTS (
            SELECT 1 FROM payout_instrument_verifications v
             WHERE v.id = NEW.current_verification_id AND v.tenant_id = NEW.tenant_id AND v.instrument_id = NEW.id
               AND v.outcome = 'verified' AND v.created_txid = txid_current() AND v.expires_at > now())
        THEN
            RAISE EXCEPTION 'payout_instruments: a transition into verified needs a same-transaction verified verification' USING ERRCODE = 'PI014';
        END IF;
    ELSIF NEW.current_verification_id IS DISTINCT FROM OLD.current_verification_id THEN
        RAISE EXCEPTION 'payout_instruments: current_verification_id changes only into verified' USING ERRCODE = 'PI012';
    END IF;

    -- Blocking transitions need their blocking event in the same transaction.
    IF NEW.state = 'suspended' AND NOT EXISTS (
        SELECT 1 FROM payout_instrument_blocking_events e
         WHERE e.tenant_id = NEW.tenant_id AND e.instrument_id = NEW.id AND e.event = 'suspend' AND e.created_txid = txid_current())
    THEN
        RAISE EXCEPTION 'payout_instruments: suspension needs a same-transaction blocking event' USING ERRCODE = 'PI015';
    END IF;
    IF NEW.state = 'revoked' AND NOT EXISTS (
        SELECT 1 FROM payout_instrument_blocking_events e
         WHERE e.tenant_id = NEW.tenant_id AND e.instrument_id = NEW.id AND e.event = 'revoke' AND e.created_txid = txid_current())
    THEN
        RAISE EXCEPTION 'payout_instruments: revocation needs a same-transaction blocking event' USING ERRCODE = 'PI015';
    END IF;

    -- Only an expiry that has really happened (L-4: written by the sweep only;
    -- gates never write it).
    IF NEW.state = 'verification_expired' AND NOT EXISTS (
        SELECT 1 FROM payout_instrument_verifications v
         WHERE v.id = OLD.current_verification_id AND v.tenant_id = OLD.tenant_id AND v.expires_at <= now())
    THEN
        RAISE EXCEPTION 'payout_instruments: the in-force verification has not expired' USING ERRCODE = 'PI016';
    END IF;

    -- Supersession: a replacing version must itself be verified, and the old
    -- instrument must not be in use (A-9 / L-3).
    IF NEW.state = 'superseded' THEN
        IF NOT EXISTS (
            SELECT 1 FROM payout_instruments r
             WHERE r.supersedes_instrument_id = OLD.id AND r.tenant_id = OLD.tenant_id AND r.state = 'verified')
        THEN
            RAISE EXCEPTION 'payout_instruments: no verified replacing version' USING ERRCODE = 'PI017';
        END IF;
        IF EXISTS (
            SELECT 1 FROM withdrawal_requests w
             WHERE w.tenant_id = OLD.tenant_id AND w.payout_instrument_id = OLD.id
               AND w.state IN ('requested', 'pending_review', 'approved', 'submitted'))
        THEN
            RAISE EXCEPTION 'payout_instruments: instrument is in use by a live withdrawal' USING ERRCODE = 'PI018';
        END IF;
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state OR NEW.current_verification_id IS DISTINCT FROM OLD.current_verification_id THEN
        IF NEW.state IS DISTINCT FROM OLD.state THEN
            NEW.state_changed_at := now();
        ELSE
            NEW.state_changed_at := OLD.state_changed_at;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payout_instruments_before_update
    BEFORE UPDATE ON payout_instruments FOR EACH ROW EXECUTE FUNCTION payout_instruments_before_update();
CREATE TRIGGER payout_instruments_no_delete
    BEFORE DELETE ON payout_instruments FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_instruments_no_truncate
    BEFORE TRUNCATE ON payout_instruments FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- 6.4 Verification INSERT (append-only otherwise).
CREATE FUNCTION payout_instrument_verifications_before_insert() RETURNS TRIGGER AS $$
DECLARE
    v_state TEXT;
BEGIN
    SELECT i.state INTO v_state FROM payout_instruments i
     WHERE i.id = NEW.instrument_id AND i.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'payout_instrument_verifications: instrument not found in tenant' USING ERRCODE = 'PI020';
    END IF;
    IF v_state IN ('revoked', 'rejected', 'superseded') THEN
        RAISE EXCEPTION 'payout_instrument_verifications: instrument is terminal' USING ERRCODE = 'PI021';
    END IF;
    -- DB-forced time and transaction id: the Go seal reads now() of this
    -- transaction first, and the database holds it to that value.
    IF NEW.verified_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'payout_instrument_verifications: verified_at must be the transaction time' USING ERRCODE = 'PI022';
    END IF;
    NEW.created_txid := txid_current();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payout_instrument_verifications_before_insert
    BEFORE INSERT ON payout_instrument_verifications FOR EACH ROW EXECUTE FUNCTION payout_instrument_verifications_before_insert();
CREATE TRIGGER payout_instrument_verifications_immutable
    BEFORE UPDATE OR DELETE ON payout_instrument_verifications FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_instrument_verifications_no_truncate
    BEFORE TRUNCATE ON payout_instrument_verifications FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- 6.5 Blocking events INSERT (append-only otherwise).
CREATE FUNCTION payout_instrument_blocking_events_before_insert() RETURNS TRIGGER AS $$
DECLARE
    v_state TEXT;
BEGIN
    SELECT i.state INTO v_state FROM payout_instruments i
     WHERE i.id = NEW.instrument_id AND i.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'payout_instrument_blocking_events: instrument not found in tenant' USING ERRCODE = 'PI030';
    END IF;
    IF v_state IN ('revoked', 'rejected', 'superseded') THEN
        RAISE EXCEPTION 'payout_instrument_blocking_events: instrument is terminal' USING ERRCODE = 'PI031';
    END IF;
    IF NEW.occurred_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'payout_instrument_blocking_events: occurred_at must be the transaction time' USING ERRCODE = 'PI032';
    END IF;
    NEW.created_txid := txid_current();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payout_instrument_blocking_events_before_insert
    BEFORE INSERT ON payout_instrument_blocking_events FOR EACH ROW EXECUTE FUNCTION payout_instrument_blocking_events_before_insert();
CREATE TRIGGER payout_instrument_blocking_events_immutable
    BEFORE UPDATE OR DELETE ON payout_instrument_blocking_events FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_instrument_blocking_events_no_truncate
    BEFORE TRUNCATE ON payout_instrument_blocking_events FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- =========================================================================
-- 7. withdrawal_requests: the binding columns (used by B13-B)
-- =========================================================================

ALTER TABLE withdrawal_requests
    ADD COLUMN payout_instrument_id UUID NULL,
    ADD COLUMN payout_instrument_fingerprint TEXT NULL,
    ADD CONSTRAINT withdrawal_requests_payout_binding_both_or_neither
        CHECK ((payout_instrument_id IS NULL) = (payout_instrument_fingerprint IS NULL)),
    ADD CONSTRAINT withdrawal_requests_payout_instrument_fk
        FOREIGN KEY (tenant_id, payout_instrument_id, payout_instrument_fingerprint)
        REFERENCES payout_instruments (tenant_id, id, fingerprint);
CREATE INDEX idx_withdrawal_requests_payout_instrument
    ON withdrawal_requests (tenant_id, payout_instrument_id) WHERE payout_instrument_id IS NOT NULL;

-- The binding is immutable after insert (decision: no destination change after
-- approval). Body = the 0026 body plus the two columns.
CREATE OR REPLACE FUNCTION withdrawal_requests_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
        OR NEW.payout_instrument_id IS DISTINCT FROM OLD.payout_instrument_id
        OR NEW.payout_instrument_fingerprint IS DISTINCT FROM OLD.payout_instrument_fingerprint
    THEN
        RAISE EXCEPTION 'withdrawal_requests: amount/asset/wallet/player/tenant/brand/idempotency_key/requested_at/payout binding are immutable after insert';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- BEFORE INSERT: when a binding is supplied it must be usable (TRANSITIONAL
-- NULL arm: see the header). Locks the instrument FOR SHARE so a revoke
-- (FOR UPDATE) serialises with the request.
CREATE FUNCTION withdrawal_requests_payout_binding_guard() RETURNS TRIGGER AS $$
DECLARE
    v_i payout_instruments%ROWTYPE;
    v_ver payout_instrument_verifications%ROWTYPE;
BEGIN
    IF NEW.payout_instrument_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT * INTO v_i FROM payout_instruments i
     WHERE i.id = NEW.payout_instrument_id AND i.tenant_id = NEW.tenant_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument not found' USING ERRCODE = 'PI040';
    END IF;
    IF v_i.player_account_id <> NEW.player_account_id OR v_i.brand_id <> NEW.brand_id
       OR v_i.fingerprint <> NEW.payout_instrument_fingerprint THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument does not belong to this player/brand' USING ERRCODE = 'PI041';
    END IF;
    IF v_i.state <> 'verified' THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument is not verified' USING ERRCODE = 'PI042';
    END IF;
    IF NOT (NEW.asset_code = ANY (v_i.asset_codes)) THEN
        RAISE EXCEPTION 'withdrawal_requests: asset is not listed on the payout instrument' USING ERRCODE = 'PI043';
    END IF;
    SELECT * INTO v_ver FROM payout_instrument_verifications v
     WHERE v.instrument_id = v_i.id AND v.tenant_id = v_i.tenant_id AND v.outcome = 'verified'
     ORDER BY v.verified_at DESC, v.id DESC LIMIT 1;
    IF NOT FOUND OR v_ver.id IS DISTINCT FROM v_i.current_verification_id OR v_ver.expires_at <= now() THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument has no in-force verification' USING ERRCODE = 'PI044';
    END IF;
    IF EXISTS (SELECT 1 FROM payout_instrument_blocking_events e
                WHERE e.instrument_id = v_i.id AND e.tenant_id = v_i.tenant_id
                  AND (e.event = 'revoke' OR (e.event = 'suspend' AND e.occurred_at > v_ver.verified_at)))
    THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument is blocked' USING ERRCODE = 'PI045';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER withdrawal_requests_payout_binding_guard
    BEFORE INSERT ON withdrawal_requests FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_payout_binding_guard();

-- =========================================================================
-- 8. Destination snapshots (write-once, 1:1 with a payout attempt)
-- =========================================================================

CREATE TABLE payout_attempt_destination_snapshots (
    attempt_id               UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    withdrawal_request_id    UUID NOT NULL,
    instrument_id            UUID NOT NULL,
    kind                     TEXT NOT NULL,
    rail                     TEXT NOT NULL,
    fingerprint              TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    fingerprint_kid          TEXT NOT NULL CHECK (fingerprint_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    verification_id          UUID NOT NULL,
    verification_source      TEXT NOT NULL,
    ownership_assertion      TEXT NOT NULL,
    verified_at              TIMESTAMPTZ NOT NULL,
    verification_expires_at  TIMESTAMPTZ NOT NULL,
    display_mask             TEXT NOT NULL CHECK (octet_length(display_mask) BETWEEN 1 AND 64),
    amount                   NUMERIC(38, 0) NOT NULL CHECK (amount > 0),
    asset_code               TEXT NOT NULL,
    snapshot_seal            TEXT NOT NULL CHECK (snapshot_seal ~ '^[0-9a-f]{64}$'),
    seal_kid                 TEXT NOT NULL CHECK (seal_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    created_txid             BIGINT NOT NULL,
    FOREIGN KEY (attempt_id, tenant_id) REFERENCES payment_attempts (id, tenant_id),
    FOREIGN KEY (withdrawal_request_id, tenant_id) REFERENCES withdrawal_requests (id, tenant_id),
    FOREIGN KEY (tenant_id, instrument_id, fingerprint) REFERENCES payout_instruments (tenant_id, id, fingerprint),
    FOREIGN KEY (verification_id, tenant_id, instrument_id)
        REFERENCES payout_instrument_verifications (id, tenant_id, instrument_id)
);
CREATE INDEX payout_attempt_destination_snapshots_withdrawal ON payout_attempt_destination_snapshots (tenant_id, withdrawal_request_id);

CREATE FUNCTION payout_attempt_destination_snapshots_before_insert() RETURNS TRIGGER AS $$
DECLARE
    w RECORD;
    a RECORD;
    i RECORD;
    v RECORD;
BEGIN
    SELECT wr.payout_instrument_id, wr.payout_instrument_fingerprint, wr.amount, wr.asset_code
      INTO w FROM withdrawal_requests wr WHERE wr.id = NEW.withdrawal_request_id AND wr.tenant_id = NEW.tenant_id;
    IF NOT FOUND OR w.payout_instrument_id IS DISTINCT FROM NEW.instrument_id
       OR w.payout_instrument_fingerprint IS DISTINCT FROM NEW.fingerprint
       OR w.amount IS DISTINCT FROM NEW.amount OR w.asset_code IS DISTINCT FROM NEW.asset_code
    THEN
        RAISE EXCEPTION 'payout snapshot: does not equal the withdrawal binding, amount and asset' USING ERRCODE = 'PI050';
    END IF;
    SELECT pa.operation, pa.withdrawal_request_id, pa.amount, pa.asset_code
      INTO a FROM payment_attempts pa WHERE pa.id = NEW.attempt_id AND pa.tenant_id = NEW.tenant_id;
    IF NOT FOUND OR a.operation <> 'payout' OR a.withdrawal_request_id IS DISTINCT FROM NEW.withdrawal_request_id
       OR a.amount IS DISTINCT FROM NEW.amount OR a.asset_code IS DISTINCT FROM NEW.asset_code
    THEN
        RAISE EXCEPTION 'payout snapshot: does not equal the payout attempt' USING ERRCODE = 'PI051';
    END IF;
    SELECT pi.kind, pi.rail, pi.fingerprint_kid, pi.display_mask INTO i
      FROM payout_instruments pi WHERE pi.id = NEW.instrument_id AND pi.tenant_id = NEW.tenant_id;
    IF NOT FOUND OR i.kind IS DISTINCT FROM NEW.kind OR i.rail IS DISTINCT FROM NEW.rail
       OR i.fingerprint_kid IS DISTINCT FROM NEW.fingerprint_kid OR i.display_mask IS DISTINCT FROM NEW.display_mask
    THEN
        RAISE EXCEPTION 'payout snapshot: does not equal the instrument' USING ERRCODE = 'PI052';
    END IF;
    SELECT ver.source, ver.ownership_assertion, ver.verified_at, ver.expires_at, ver.outcome INTO v
      FROM payout_instrument_verifications ver
     WHERE ver.id = NEW.verification_id AND ver.tenant_id = NEW.tenant_id AND ver.instrument_id = NEW.instrument_id;
    IF NOT FOUND OR v.outcome <> 'verified' OR v.source IS DISTINCT FROM NEW.verification_source
       OR v.ownership_assertion IS DISTINCT FROM NEW.ownership_assertion
       OR v.verified_at IS DISTINCT FROM NEW.verified_at OR v.expires_at IS DISTINCT FROM NEW.verification_expires_at
    THEN
        RAISE EXCEPTION 'payout snapshot: does not equal the verification' USING ERRCODE = 'PI053';
    END IF;
    -- Defence in depth (the Go gate is the primary control): the instrument is
    -- verified NOW, the snapshot's verification is its in-force one, it has not
    -- expired and no revoke / later suspend event exists.
    IF NOT EXISTS (
        SELECT 1 FROM payout_instruments pi
         WHERE pi.id = NEW.instrument_id AND pi.tenant_id = NEW.tenant_id
           AND pi.state = 'verified' AND pi.current_verification_id = NEW.verification_id)
       OR NEW.verification_expires_at <= now()
       OR EXISTS (SELECT 1 FROM payout_instrument_blocking_events e
                   WHERE e.instrument_id = NEW.instrument_id AND e.tenant_id = NEW.tenant_id
                     AND (e.event = 'revoke' OR (e.event = 'suspend' AND e.occurred_at > NEW.verified_at)))
    THEN
        RAISE EXCEPTION 'payout snapshot: the instrument is not verified, current, unexpired and unblocked' USING ERRCODE = 'PI054';
    END IF;
    NEW.created_txid := txid_current();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payout_attempt_destination_snapshots_before_insert
    BEFORE INSERT ON payout_attempt_destination_snapshots FOR EACH ROW EXECUTE FUNCTION payout_attempt_destination_snapshots_before_insert();
CREATE TRIGGER payout_attempt_destination_snapshots_immutable
    BEFORE UPDATE OR DELETE ON payout_attempt_destination_snapshots FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_attempt_destination_snapshots_no_truncate
    BEFORE TRUNCATE ON payout_attempt_destination_snapshots FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- A payout attempt for a bound withdrawal needs its snapshot, written in the
-- same transaction (deferred to commit). A side table keeps
-- payment_attempts_guard() unedited. Legacy NULL-binding withdrawals are
-- unaffected.
CREATE FUNCTION payment_attempts_require_destination_snapshot() RETURNS TRIGGER AS $$
DECLARE
    w RECORD;
BEGIN
    IF NEW.operation <> 'payout' THEN
        RETURN NULL;
    END IF;
    SELECT wr.payout_instrument_id, wr.payout_instrument_fingerprint, wr.amount, wr.asset_code
      INTO w FROM withdrawal_requests wr WHERE wr.id = NEW.withdrawal_request_id AND wr.tenant_id = NEW.tenant_id;
    -- A payout attempt always has its withdrawal (FK). A withdrawal that cannot be
    -- read (for example the tenant setting was cleared before commit) must FAIL
    -- CLOSED, never skip the check; only an explicit NULL binding returns.
    IF NOT FOUND OR NEW.withdrawal_request_id IS NULL THEN
        RAISE EXCEPTION 'payment_attempts: the withdrawal of a payout attempt could not be read' USING ERRCODE = 'PI055';
    END IF;
    IF w.payout_instrument_id IS NULL THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM payout_attempt_destination_snapshots s
         WHERE s.attempt_id = NEW.id AND s.tenant_id = NEW.tenant_id AND s.created_txid = txid_current()
           AND s.instrument_id = w.payout_instrument_id AND s.fingerprint = w.payout_instrument_fingerprint
           AND s.amount = w.amount AND s.amount = NEW.amount
           AND s.asset_code = w.asset_code AND s.asset_code = NEW.asset_code)
    THEN
        RAISE EXCEPTION 'payment_attempts: a payout attempt for a bound withdrawal needs its destination snapshot in the same transaction' USING ERRCODE = 'PI055';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE CONSTRAINT TRIGGER payment_attempts_require_destination_snapshot
    AFTER INSERT ON payment_attempts DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION payment_attempts_require_destination_snapshot();

-- =========================================================================
-- 9. RLS (ENABLE + FORCE; no FOR ALL; no SECURITY DEFINER)
-- =========================================================================

-- Tenant family (statically written: no dynamically generated policy, ADR 0099 6.2 / the A-18 scan).
ALTER TABLE payout_instruments ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_instruments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payout_instruments FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payout_instruments FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
ALTER TABLE payout_instrument_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_instrument_verifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payout_instrument_verifications FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payout_instrument_verifications FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
ALTER TABLE payout_instrument_blocking_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_instrument_blocking_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payout_instrument_blocking_events FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payout_instrument_blocking_events FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
ALTER TABLE payout_instrument_fingerprint_owners ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_instrument_fingerprint_owners FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payout_instrument_fingerprint_owners FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payout_instrument_fingerprint_owners FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
ALTER TABLE payout_attempt_destination_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_attempt_destination_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payout_attempt_destination_snapshots FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payout_attempt_destination_snapshots FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_update ON payout_instruments FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

-- Player family: SELECT own instruments only.
CREATE POLICY player_self_select ON payout_instruments FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
           AND NOT financial_acting_gucs_present());

-- Acting family: SELECT on snapshots only (the section 4/6 queues need nothing more).
CREATE POLICY acting_read ON payout_attempt_destination_snapshots FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- =========================================================================
-- 10. Runtime role grants (mirrors deploy/init-app-role.sql's 0123 block).
-- =========================================================================

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON payout_instrument_kinds FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON payout_instrument_kinds TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payout_instrument_verification_max_age FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON payout_instrument_verification_max_age TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payout_instruments FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON payout_instruments TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payout_instrument_verifications FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payout_instrument_verifications TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payout_instrument_fingerprint_owners FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payout_instrument_fingerprint_owners TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payout_instrument_blocking_events FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payout_instrument_blocking_events TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payout_attempt_destination_snapshots FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payout_attempt_destination_snapshots TO igaming_runtime';
    END IF;
END $$;
