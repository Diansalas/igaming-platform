-- Asset (currency/crypto) registry per docs/decisions/0007-multi-wallet-
-- per-player-model.md. decimal_exponent is looked up here by every
-- money-handling code path - never assumed to be 2.

CREATE TABLE assets (
    code             TEXT PRIMARY KEY,
    asset_type       TEXT NOT NULL CHECK (asset_type IN ('fiat', 'crypto')),
    decimal_exponent SMALLINT NOT NULL CHECK (decimal_exponent BETWEEN 0 AND 18),
    display_name     TEXT NOT NULL,
    active           BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE assets IS 'Platform-wide reference registry of supported currencies/assets. Not tenant-scoped: which assets a tenant/jurisdiction actually offers is governed by tenant_jurisdiction_configs.allowed_currencies.';

INSERT INTO assets (code, asset_type, decimal_exponent, display_name) VALUES
    ('EUR',  'fiat',   2, 'Euro'),
    ('USD',  'fiat',   2, 'US Dollar'),
    ('GBP',  'fiat',   2, 'British Pound'),
    ('BRL',  'fiat',   2, 'Brazilian Real'),
    ('MXN',  'fiat',   2, 'Mexican Peso'),
    ('BTC',  'crypto', 8, 'Bitcoin'),
    ('USDT', 'crypto', 6, 'Tether USD');
