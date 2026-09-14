-- Stage 4E: adds exactly one new player_accounts.status value,
-- 'identity_review_required' - the safe state a registration lands in
-- when platform-side identity resolution (internal/identityresolution)
-- returns UNCERTAIN or is itself UNAVAILABLE, per ADR 0027 §6/§7. No new
-- identity model, no new status dimension - this is the SAME status
-- column RG enforcement (internal/rg.EvaluateEligibility) already reads
-- via "account.Status != PlayerStatusActive -> deny" (migration 0010,
-- unchanged since Stage 2), so a review-required account is automatically
-- blocked from real-money play by the EXISTING enforcement mechanism -
-- no new RG check is added or duplicated (directive §13).

ALTER TABLE player_accounts DROP CONSTRAINT player_accounts_status_check;

ALTER TABLE player_accounts ADD CONSTRAINT player_accounts_status_check
    CHECK (status IN ('pending_verification', 'active', 'suspended', 'self_excluded', 'closed', 'identity_review_required'));
