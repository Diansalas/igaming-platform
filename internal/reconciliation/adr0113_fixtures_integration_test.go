//go:build integration

// ADR 0113 slice 1 (migration 0132) fixture support for this package. Since 0132 a withdrawal request
// is born `requested` with its hold leg linked (WD025, check A) and reaches `submitted` only along the
// legal edges, and every player_withdrawal_hold entry is the pinned leg of a request (B-i / B-ii). The
// payout fixtures here used to post a free-standing hold leg (key "<prefix>wreq-<id>") and INSERT the
// request directly as `submitted`; they now plant E0 + E0b (legacy NULL binding, owner, inside the
// sanctioned binding-guard plant) and walk the request to `submitted` through the Go writers as
// igaming_runtime (internal/testsupport/wdpath). The end state the tests read is the same: a
// `submitted` request with provider_id / provider_reference and a funded hold of the amount.
package reconciliation

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// legacySubmittedWithdrawal creates request wrID (legacy NULL binding, amount EUR, idempotency key
// idemKey) `submitted` to provider with provider_reference instruction ("" leaves it NULL).
func legacySubmittedWithdrawal(t *testing.T, pool *db.Pool, f fixture, wrID uuid.UUID, amount int64, idemKey, provider, instruction string) {
	t.Helper()
	spec := wdpath.Spec{ID: wrID, TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		AssetCode: "EUR", Amount: amount, IdempotencyKey: idemKey, ProviderID: provider, ProviderReference: instruction}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := wdpath.Plant(ctx, tx, spec)
			return err
		})
	}); err != nil {
		t.Fatalf("plant legacy withdrawal %s: %v", wrID, err)
	}
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, wrID, withdrawal.StateSubmitted, spec)
}

// ownerTamperHoldLegs runs fn (which posts hold-account legs that NO withdrawal request names) with the
// two 0132 hold-entry triggers disabled for fn only, inside the caller's OWNER transaction - an ADR
// 0110 9.4 owner tamper plant reproducing history that the database now refuses to every session
// (WD040). Used only by the reconciliation tests whose subject IS such an unattributed posting. The
// deferred balance check of fn's entries is fired first: ALTER TABLE refuses to run while the table has
// pending trigger events (55006).
//
// Callers first call wdpath.SkipOnPersistentDatabase: the planted legs are permanent (append-only).
func ownerTamperHoldLegs(ctx context.Context, tx pgx.Tx, fn func() error) error {
	var tenant string
	if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&tenant); err != nil {
		return err
	}
	if id, err := uuid.Parse(tenant); err == nil {
		plantedTenants.Store(id, true)
	}
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

// plantedTenants are the tenants that received an ownerTamperHoldLegs plant in this test binary.
var plantedTenants sync.Map

// withoutPlantedUnlinkedHoldLegs drops check D's WD042 findings (an unattributed hold leg, ADR 0113
// slices 2-3 review LF C1) for tenants whose unattributed legs were planted on purpose by
// ownerTamperHoldLegs: the payout reconciliation suites using it assert "no OTHER finding", and WD042
// for their own plant is expected (it is asserted by TestADR0113D_UnlinkedZeroNetPair_IsNotClean). A
// WD042 for any other tenant, and every other kind, is kept.
func withoutPlantedUnlinkedHoldLegs(ms []Mismatch) []Mismatch {
	var out []Mismatch
	for _, m := range ms {
		_, planted := plantedTenants.Load(m.TenantID)
		if planted && m.MismatchKind == MismatchKindWithdrawalRequestLedgerLinkInvalid &&
			strings.HasPrefix(m.ReconciliationKey, "ledger_transaction:") && strings.HasPrefix(m.ActualValue, "violation=WD042 ") {
			continue
		}
		out = append(out, m)
	}
	return out
}
