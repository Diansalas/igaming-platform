CREATE OR REPLACE FUNCTION withdrawal_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    requester_person_id UUID;
    approver_person_id  UUID;
BEGIN
    IF NEW.decision != 'approve' OR NEW.is_automated_approval THEN
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

ALTER TABLE withdrawal_policies DROP CONSTRAINT withdrawal_policies_jurisdiction_not_yet_resolvable;
ALTER TABLE withdrawal_policies DROP CONSTRAINT withdrawal_policies_approver_roles_not_yet_enforced;

ALTER TABLE withdrawal_policies DROP CONSTRAINT withdrawal_policies_brand_tenant_fkey;
ALTER TABLE withdrawal_policies ADD CONSTRAINT withdrawal_policies_brand_id_fkey
    FOREIGN KEY (brand_id) REFERENCES brands (id);

DROP POLICY tenant_isolation ON provider_capability_amount_limits;
CREATE POLICY tenant_isolation ON provider_capability_amount_limits
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY tenant_isolation ON withdrawal_policies;
CREATE POLICY tenant_isolation ON withdrawal_policies
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
