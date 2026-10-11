//go:build integration

// ADR 0113 slice 1 (migration 0132) fixture support for this package: a withdrawal request is born
// `requested` with its hold leg linked (WD025, check A) and reaches a later state only along the
// legal edges E1-E10. The payout fixtures that used to INSERT a LEGACY (NULL-binding) row directly as
// `submitted` / `approved` with no hold now plant E0 + E0b (owner, inside the sanctioned binding-guard
// plant) and walk the request through the Go writers (internal/testsupport/wdpath): as igaming_runtime
// on the shared test database, as the scratch owner on a throwaway scratchdb database (no runtime
// grants there). The end state is the one the tests read, plus the hold the state implies; a
// `submitted` request now carries the provider_id the attempt is routed to (E7 requires it, WD022).
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// forceStateOwnerTamper relabels a withdrawal's state as the scratch OWNER with the 0132 state-transition
// guard disabled for that one statement (ADR 0110 9.4 owner tamper; re-enabled in the same
// transaction): "an inconsistent state a bug could produce", which since migration 0132 only a
// trigger-disabling owner can produce. Check A still runs (fired before the re-enable, which
// PostgreSQL refuses while the table has pending trigger events).
func (h *hsr) forceStateOwnerTamper(id uuid.UUID, state string) {
	h.t.Helper()
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`UPDATE withdrawal_requests SET state = '` + state + `' WHERE id = '` + id.String() + `'`,
			`SET CONSTRAINTS withdrawal_requests_ledger_link_at_commit IMMEDIATE`,
			`SET CONSTRAINTS withdrawal_requests_ledger_link_at_commit DEFERRED`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// legacyWithdrawalAt creates request id (legacy NULL binding, amount EUR, idempotency key key) in state
// target; providerID / providerRef are used by E7 / E8 when the target is submitted or beyond.
func legacyWithdrawalAt(t *testing.T, pool *db.Pool, f orchFixture, id uuid.UUID, amount int64, key string, target withdrawal.State, providerID, providerRef string) {
	t.Helper()
	spec := wdpath.Spec{ID: id, TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		AssetCode: "EUR", Amount: amount, IdempotencyKey: key, ProviderID: providerID, ProviderReference: providerRef}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := wdpath.Plant(ctx, tx, spec)
			return err
		})
	}); err != nil {
		t.Fatalf("plant legacy withdrawal %s: %v", id, err)
	}
	wdpath.AdvanceForFixture(context.Background(), t, pool, f.tenantID, id, target, spec)
}

// m4OwnerTamperHoldLegs runs fn (postings to a hold account that the database now refuses) with the two
// 0132 hold-entry triggers disabled for fn only, inside the caller's OWNER transaction on a throwaway
// scratch database (ADR 0110 9.4 owner tamper plant). The deferred balance events of fn's entries are
// fired first: ALTER TABLE refuses to run while the table has pending trigger events (55006).
func m4OwnerTamperHoldLegs(ctx context.Context, tx pgx.Tx, fn func() error) error {
	for _, q := range []string{
		`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
		`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	if err := fn(); err != nil {
		return err
	}
	for _, q := range []string{
		`SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`,
		`SET CONSTRAINTS ledger_entries_balanced DEFERRED`,
		`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
		`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
