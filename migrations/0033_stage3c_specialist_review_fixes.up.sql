-- Stage 3C specialist review pass (7 parallel specialists: ledger-
-- finance, payments, security, architect, backend, qa, code-reviewer)
-- found several concrete database-level defects in migrations 0029-0032.
-- Those migrations are historical and are not edited in place (CLAUDE.md:
-- "do not modify historical migrations") - this migration corrects them
-- forward.

-- security F5 / ledger-finance F2: withdrawal_policies' tenant_isolation
-- policy (migration 0032) was a bare tenant_id match, missing the
-- player-scope exclusion guard every other staff/system-only financial
-- table carries (migration 0028's own fix for exactly this shape). A
-- db.Pool.WithPlayerScope-scoped transaction (used by player self-
-- service handlers) would satisfy this policy's USING/WITH CHECK and
-- could read or write the tenant's own four-eyes approval policy - the
-- exact "latent trap" 0028 already fixed for five sibling tables.
DROP POLICY tenant_isolation ON withdrawal_policies;
CREATE POLICY tenant_isolation ON withdrawal_policies
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Same defect, same fix, on provider_capability_amount_limits: migration
-- 0030 gave it a direct tenant_id policy (correctly resolving the
-- ownership-ambiguity/subquery issue it set out to fix) but did not
-- carry forward 0028's player-scope guard onto the table's NEW direct
-- policy shape.
DROP POLICY tenant_isolation ON provider_capability_amount_limits;
CREATE POLICY tenant_isolation ON provider_capability_amount_limits
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- architect finding 9 / security F7: withdrawal_policies.brand_id
-- (migration 0032) was a single-column FK into brands(id), unlike every
-- sibling table (provider_capabilities, withdrawal_requests), which
-- correctly uses the composite (brand_id, tenant_id) -> brands(id,
-- tenant_id) form brands' own UNIQUE (id, tenant_id) constraint (0008)
-- exists to support. A single-column FK lets tenant A write a policy
-- row naming tenant B's brand_id - not a cross-tenant read/write (RLS
-- still requires tenant_id = A on the row itself), but a silently dead
-- row that can never match ResolveApprovalPolicy's brand_id = $4
-- comparison for the tenant that (mistakenly or maliciously) wrote it,
-- and an FK that doesn't actually assert what it appears to assert.
ALTER TABLE withdrawal_policies DROP CONSTRAINT withdrawal_policies_brand_id_fkey;
ALTER TABLE withdrawal_policies ADD CONSTRAINT withdrawal_policies_brand_tenant_fkey
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id);

-- security F7: required_approver_roles is accepted by the schema and
-- read by nothing (internal/withdrawal.Approve does not enforce it -
-- see docs/architecture/withdrawal-policy-configuration.md §6). Writing
-- a non-NULL value today creates a false sense of enforcement CLAUDE.md's
-- "no fake completion" rule forbids - fail closed until the enforcement
-- code exists, rather than silently accepting and ignoring it.
ALTER TABLE withdrawal_policies ADD CONSTRAINT withdrawal_policies_approver_roles_not_yet_enforced
    CHECK (required_approver_roles IS NULL);

-- architect finding 11: jurisdiction_code is free TEXT with no relation
-- to the platform's actual jurisdiction model (jurisdictions.id, a real
-- FK target with a UNIQUE code - migration 0002), and
-- ResolveApprovalPolicy never receives a non-nil jurisdictionCode today
-- (no per-withdrawal jurisdiction assignment exists yet - see
-- docs/architecture/withdrawal-policy-configuration.md §5.2). A written
-- jurisdiction-scoped row would silently never be selected - the same
-- "false sense of enforcement" as required_approver_roles above. Fail
-- closed identically, until real jurisdiction resolution exists.
ALTER TABLE withdrawal_policies ADD CONSTRAINT withdrawal_policies_jurisdiction_not_yet_resolvable
    CHECK (jurisdiction_code IS NULL);

-- security F4: the withdrawal_approvals_deny_self_approval trigger
-- (migration 0029) skipped its check entirely when
-- NEW.is_automated_approval is true, trusting a caller-supplied boolean
-- rather than the data itself. The exemption was already redundant (a
-- genuine service identity has no staff_users row, so the join's
-- approver_person_id is NULL and the equality check never matches
-- anyway) and, per the security review, actively weakens the guarantee:
-- a future caller that mislabels a staff principal as automated would
-- have disabled the DB-level backstop by that label alone. Redefining
-- the function to remove the shortcut makes the guard purely
-- data-driven - it no longer trusts anything the inserting transaction
-- claims about itself.
CREATE OR REPLACE FUNCTION withdrawal_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    requester_person_id UUID;
    approver_person_id  UUID;
BEGIN
    IF NEW.decision != 'approve' THEN
        RETURN NEW;
    END IF;

    SELECT pa.person_id INTO requester_person_id
    FROM withdrawal_requests wr
    JOIN player_accounts pa ON pa.id = wr.player_account_id
    WHERE wr.id = NEW.withdrawal_request_id;

    SELECT su.person_id INTO approver_person_id
    FROM staff_users su
    WHERE su.id = NEW.approver_principal_id;

    IF requester_person_id IS NOT NULL
        AND approver_person_id IS NOT NULL
        AND requester_person_id = approver_person_id
    THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver resolves to the same person as the withdrawing player (self-approval)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
