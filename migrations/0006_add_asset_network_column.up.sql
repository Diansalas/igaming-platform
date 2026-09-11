-- Fix: docs/architecture/06-wallet-ledger-architecture.md uses
-- "USDT-TRC20" to illustrate that the same symbol can exist on different
-- chains, but the registry had no way to represent that - two USDT
-- deployments on different networks are different deposit/withdrawal
-- destinations and must never be conflated. Caught in Stage 1 specialist
-- review.
--
-- network is NULL for fiat assets (meaningless there) and required for
-- crypto assets. The registry's primary key stays the human-readable
-- `code`; where a symbol exists on more than one chain, each network gets
-- its own row with a distinguishing code (e.g. 'USDT-TRC20',
-- 'USDT-ERC20') rather than relying on this column alone to disambiguate
-- - the code itself is what ledger entries and wallets reference.

ALTER TABLE assets ADD COLUMN network TEXT;

-- Backfill existing crypto rows BEFORE the CHECK constraint below is
-- added - a CHECK constraint is validated against all existing rows at
-- creation time, so adding it first would fail on the seed data from
-- migration 0003.
UPDATE assets SET network = 'mainnet' WHERE code = 'BTC';
UPDATE assets SET network = 'TRC20' WHERE code = 'USDT';

ALTER TABLE assets ADD CONSTRAINT assets_network_required_for_crypto
    CHECK (asset_type = 'fiat' OR network IS NOT NULL);
