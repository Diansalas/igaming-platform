-- Reverses migration 0096 ONLY on a database where no provider credential
-- history exists (ADR 0093 §1 "Down migration"). A handle, request or
-- approval row is evidence, so its presence makes this migration
-- irreversible; roll forward instead.
--
-- Every check uses constraint validation, which FORCE ROW LEVEL SECURITY
-- cannot filter (a count(*) here would see zero tenant rows and wrongly
-- proceed - the migration 0078/0091 down-script technique). All checks run
-- before anything is dropped. The secrets themselves live in the external
-- store and are untouched by this migration in either direction.

DO $$
BEGIN
    ALTER TABLE provider_credential_handles ADD CONSTRAINT tmp_0096_handles_empty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0096 (down): provider_credential_handles is not empty. Provider credential history is evidence; this migration is irreversible once any handle exists; roll forward';
END $$;
ALTER TABLE provider_credential_handles DROP CONSTRAINT tmp_0096_handles_empty;

DO $$
BEGIN
    ALTER TABLE provider_credential_change_requests ADD CONSTRAINT tmp_0096_requests_empty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0096 (down): provider_credential_change_requests is not empty. Provider credential history is evidence; this migration is irreversible once any change request exists; roll forward';
END $$;
ALTER TABLE provider_credential_change_requests DROP CONSTRAINT tmp_0096_requests_empty;

DO $$
BEGIN
    ALTER TABLE provider_credential_change_approvals ADD CONSTRAINT tmp_0096_approvals_empty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0096 (down): provider_credential_change_approvals is not empty. Provider credential history is evidence; this migration is irreversible once any approval exists; roll forward';
END $$;
ALTER TABLE provider_credential_change_approvals DROP CONSTRAINT tmp_0096_approvals_empty;

-- Only now drop. The circular foreign keys go first.
ALTER TABLE provider_credential_change_requests DROP CONSTRAINT pccr_applied_handle_fk;
ALTER TABLE provider_credential_change_requests DROP CONSTRAINT pccr_predecessor_fk;
ALTER TABLE provider_credential_handles DROP CONSTRAINT pch_activation_request_fk;

DROP TABLE provider_credential_handles;
DROP TABLE provider_credential_change_approvals;
DROP TABLE provider_credential_change_requests;

DROP FUNCTION provider_credential_handles_transition();
DROP FUNCTION provider_credential_consume();
DROP FUNCTION provider_credential_handles_before_insert();
DROP FUNCTION provider_credential_change_approvals_before_insert();
DROP FUNCTION provider_credential_change_requests_guard();
DROP FUNCTION provider_credential_change_requests_before_insert();
DROP FUNCTION provider_credential_content_hash(UUID, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ, UUID, TEXT, TIMESTAMPTZ);
DROP FUNCTION provider_credential_hash_ts(TIMESTAMPTZ);
DROP FUNCTION provider_credential_hash_field(TEXT);
DROP FUNCTION provider_credential_ref_in_namespace(TEXT, UUID, TEXT, TEXT);
