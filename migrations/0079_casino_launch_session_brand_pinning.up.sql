-- Stage 7 pre-stage security fix (directive §2): casino_launch_sessions
-- has the same brand-pinning weakness Stage 6.1 found and fixed in
-- sportsbook_bets. The original three composite FKs
-- ((wallet_id,tenant_id)->wallets, (wallet_id,player_account_id)->wallets,
-- (brand_id,tenant_id)->brands) pin wallet->tenant and wallet->player, but
-- leave brand_id pinned only to "some brand in this tenant," not
-- specifically the player's own brand. Not reachable via the application
-- today (newLaunchCasinoGameHandler always derives BrandID from
-- identity.GetPlayerAccountByID's own account.BrandID, never a
-- client-supplied value), but a real DB-level integrity gap per
-- CLAUDE.md's RLS-not-application-discipline rule and Stage 6.1 ADR
-- 0047's own precedent.
--
-- Fixed exactly like sportsbook_bets: replace the separate
-- (brand_id,tenant_id)->brands FK with a single composite
-- (player_account_id, tenant_id, brand_id) FK against player_accounts,
-- reusing the UNIQUE (id, tenant_id, brand_id) key migration 0019 already
-- added to player_accounts for wallets' own identical pinning (the same
-- established platform pattern migration 0057's bonus_grants and
-- migration 0078's sportsbook_bets also use).
ALTER TABLE casino_launch_sessions DROP CONSTRAINT casino_launch_sessions_brand_id_tenant_id_fkey;
ALTER TABLE casino_launch_sessions
    ADD CONSTRAINT casino_launch_sessions_player_tenant_brand_fkey
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id);
