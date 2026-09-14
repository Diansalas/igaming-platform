-- Reversible only if no row currently has status = 'identity_review_required'
-- (an append-only-adjacent property: this down migration does not attempt
-- to silently reassign such rows to a different status, since doing so
-- would be a data-altering decision this migration has no basis to make).

ALTER TABLE player_accounts DROP CONSTRAINT player_accounts_status_check;

ALTER TABLE player_accounts ADD CONSTRAINT player_accounts_status_check
    CHECK (status IN ('pending_verification', 'active', 'suspended', 'self_excluded', 'closed'));
