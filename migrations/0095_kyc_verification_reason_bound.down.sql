-- Reverses migration 0095. Every reason value is already <=512 bytes and
-- control-character-free post-up (no reason_code column was ever added -
-- HD-10.3-3 superseded that part of the original design), so no data
-- transformation is needed going down: a clean, symmetric down migration.
ALTER TABLE kyc_verifications
    DROP CONSTRAINT IF EXISTS kyc_verifications_reason_bound;
