//go:build integration

package withdrawal

// This file exists ONLY in integration builds: no production binary contains it (the internal/kyc hold_hook_integration.go
// precedent). A few migration tests of OTHER packages (internal/payments' r32 0127 backfill test) build their fixture data
// through the production request / payout-claim path on a scratch database migrated to a version BEFORE migration 0182; the
// restriction evaluation fails closed there (withdrawal_restriction_set_version() does not exist), so those tests switch it to
// the pre-0182 behaviour (no evaluation, allow, no provenance) while the fixture is built, and restore it before migrating
// up. Nothing else may use it (TestLic24_Static_Pre0182HookIsIntegrationOnly).

// SetPre0182SchemaForTest makes every restriction evaluation answer the pre-0182 behaviour until the returned restore runs.
// Process-global: callers must not run in parallel with other tests of their package.
func SetPre0182SchemaForTest() (restore func()) {
	prev := pre0182SchemaHook.Swap(true)
	return func() { pre0182SchemaHook.Store(prev) }
}
