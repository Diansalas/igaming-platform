-- Stage 10.3, KYC-REASON-BOUND-1 (docs/plans/stage-10.3-planning/
-- 03-kyc-reason-bound-analysis.md, security review C16, HD-10.3-3).
--
-- kyc_verifications.reason (migration 0040) is a plain TEXT column with a
-- comment saying it "must never contain raw KYC evidence" but nothing
-- enforcing that. This migration adds the length half of the bound
-- (internal/kyc.NormalizeReason, applied at ingestion, already enforces
-- length + charset in application code as of this same change) as a
-- database-level CHECK: defense in depth, since "staff-only, bounded" is
-- an API-shape property today, not a database guarantee, and a future
-- write path that forgets to call NormalizeReason must not be able to
-- write an unbounded value silently.
--
-- HD-10.3-3 (binding human ruling) superseded the original design's
-- player-facing `reason_code` column - players see STATUS ONLY, so this
-- migration does NOT add a reason_code column (unlike the provisional
-- §6 migration table in the planning proposal). It is bound-only.
--
-- kyc_documents.rejection_reason is explicitly OUT of scope here (W0
-- architect code-check addendum): it is staff-ENTERED, not provider
-- text, and is tracked separately as KYC-DOC-REJECTION-BOUND-1.
--
-- --- Pre-flight: normalize any pre-existing over-length/control-
-- character row before the CHECK is added, so the migration cannot fail
-- on legacy data (mirrors the intent of migration 0048's pre-flight, but
-- NOT its mechanism - see the note below on why).
--
-- kyc_verifications carries FORCE ROW LEVEL SECURITY (migration 0040).
-- Migration 0048's own documented lesson is that FORCE ROW LEVEL
-- SECURITY applies even to a migration connection running AS THE TABLE
-- OWNER once no `app.tenant_id` is set: a plain `UPDATE ... WHERE
-- <predicate>` from this connection would silently affect ZERO rows
-- (every row's tenant_isolation USING clause evaluates against a NULL
-- app.tenant_id and matches nothing), not fail loudly - exactly the trap
-- 0048 hit with a `SELECT count(*)`. 0048's own fix for ITS case was to
-- let `ADD CONSTRAINT`'s row-by-row validation (which RLS cannot filter,
-- since it scans the physical table) do the detection instead of a
-- blinded SELECT. That mechanism alone would suffice HERE too (a
-- constraint-validation failure would still correctly SURFACE an
-- offending row), but this migration goes one step further and actually
-- FIXES any such row first, because it can do so SAFELY without ever
-- disabling FORCE ROW LEVEL SECURITY (the exact trick 0048's own header
-- comment discloses was REMOVED at security review, because a script
-- erroring out mid-toggle would leave FORCE off permanently):
--
-- `tenants` (migration 0001) carries NO row-level security at all - it
-- is the platform-wide root table every tenant scope is defined
-- relative to, and is fully readable by the migration connection as-is.
-- This lets the pre-flight loop over every KNOWN tenant id, and for
-- each one, LEGITIMATELY set `app.tenant_id` to that value (the exact
-- GUC db.Pool.WithTenant sets for ordinary application code) before
-- touching that tenant's own rows - never bypassing the tenant_isolation
-- policy, just satisfying it correctly, once per tenant. No FORCE flag
-- is ever toggled, and every kyc_verifications row is reachable this way
-- (its tenant_id is transitively guaranteed to name a real tenant, via
-- its own FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id,
-- tenant_id), and brands.tenant_id itself REFERENCES tenants).
--
-- The values fixed here are stated, not assumed, to be synthetic-only:
-- MockKYCProvider is the only KYCProvider adapter this platform has ever
-- shipped (ADR 0028; no real vendor is contracted), so no real KYC
-- evidence or vendor status text has ever reached this column - only
-- test/dev fixture strings. Truncating/cleaning such a row is a safe,
-- reversible, non-destructive operation for what it is (a leftover test
-- fixture), never a real compliance record.
DO $$
DECLARE
    tenant_rec RECORD;
    row_rec RECORD;
    cleaned TEXT;
BEGIN
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);

        FOR row_rec IN
            SELECT id, reason
              FROM kyc_verifications
             WHERE tenant_id = tenant_rec.id
               AND reason IS NOT NULL
               AND (octet_length(reason) > 512 OR reason ~ '[\x00-\x1F\x7F]')
        LOOP
            -- Strip C0 controls and DEL (the byte-level subset a plain
            -- regexp_replace can reach; the fuller charset/bidi bound
            -- lives in internal/kyc.NormalizeReason, applied by every
            -- write path going forward - this pre-flight only needs to
            -- satisfy the length CHECK added below, and cleans obviously
            -- bad bytes as a bonus).
            cleaned := regexp_replace(row_rec.reason, '[\x00-\x1F\x7F]', '', 'g');
            -- Bound to 512 BYTES, trimming one character at a time so a
            -- multi-byte UTF-8 character is never split (mirrors
            -- NormalizeReason's own rune-boundary rule) - bounded by the
            -- string's own length, and this is a one-time migration over
            -- synthetic test data, not a hot path.
            WHILE octet_length(cleaned) > 512 LOOP
                cleaned := left(cleaned, length(cleaned) - 1);
            END LOOP;

            UPDATE kyc_verifications
               SET reason = cleaned
             WHERE id = row_rec.id
               AND tenant_id = tenant_rec.id;
        END LOOP;
    END LOOP;

    -- Leave no tenant context bleeding into whatever runs next in this
    -- migration file or connection.
    PERFORM set_config('app.tenant_id', '', true);
END $$;

ALTER TABLE kyc_verifications
    ADD CONSTRAINT kyc_verifications_reason_bound
    CHECK (reason IS NULL OR octet_length(reason) <= 512);
