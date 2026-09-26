-- Reverses migration 0094. Always safe at any time: this only DROPS the
-- backstop CHECK constraint, never any data, and only ever RELAXES what
-- casino_provider_capabilities allows - it never re-introduces a row that
-- was valid before 0094 and would now be invalid. The primary control
-- (WriteCapability's own identical application-level check,
-- internal/casino/capability.go) is unaffected by whether this
-- constraint exists.
ALTER TABLE casino_provider_capabilities
    DROP CONSTRAINT IF EXISTS casino_provider_capabilities_bet_requires_settlement;
