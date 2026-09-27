//go:build integration

// RV-PRH-I1 kill-switch security review, finding H1 (blocks completion):
// permanent regression coverage for the ADR 0095 §10.3 kill-switch
// predicate on EVERY claim form, not just T2 (which
// migration_0105_integration_test.go's TestMigration0105_
// ClaimPredicateBlocksSubmissionWhenEngaged already covers). The review's
// own probe P2 showed a mutation deleting the predicate from
// InsertSubmittingAttempt (T1+T2/T1p), ResubmitAmbiguous (T12) or the
// cascade InsertCreatedAttempt (T1) passed the entire existing suite - this
// file is the adapted, permanent version of that probe, one test per form,
// each with a positive control (a non-matching provider/operation must
// still succeed) so the test cannot be satisfied by simply never claiming
// anything. Every test here is re-run under a deliberate mutation (removing
// the predicate from the corresponding SQL string in attempt.go) as part of
// this fix round's mutation-kill evidence.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestKillSwitchPredicate_T1T2_Deposit_InsertSubmittingAttempt is H1's T1+T2
// coverage: the combined deposit insert-and-claim path
// (InsertSubmittingAttempt) must refuse when a matching switch is engaged,
// and must still succeed for a non-matching provider.
func TestKillSwitchPredicate_T1T2_Deposit_InsertSubmittingAttempt(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_h1a")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "mock", "deposit", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}

	blockedIntent := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &blockedIntent, ProviderID: "mock", PaymentMethod: "card", AssetCode: "EUR",
			Amount: 1000, Interactive: true, ClaimToken: uuid.New(), LeaseOwner: "w", LeaseUntil: time.Now().Add(time.Minute),
		})
		return err
	})
	if err == nil {
		t.Fatal("expected InsertSubmittingAttempt (T1+T2, deposit) to be refused for a matching engaged switch")
	}

	// Positive control: a different provider is not blocked by this switch.
	allowedIntent := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &allowedIntent, ProviderID: "other-provider", PaymentMethod: "card", AssetCode: "EUR",
			Amount: 1000, Interactive: true, ClaimToken: uuid.New(), LeaseOwner: "w", LeaseUntil: time.Now().Add(time.Minute),
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected InsertSubmittingAttempt for a non-matching provider to succeed, got %v", err)
	}
}

// TestKillSwitchPredicate_T1p_Payout_InsertSubmittingAttempt is H1's T1p
// coverage: the payout claim insert path.
func TestKillSwitchPredicate_T1p_Payout_InsertSubmittingAttempt(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_h1b")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "mock", "payout", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}

	blockedWithdrawal := insertWithdrawalRequest(t, pool, f, "approved", nil, nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &blockedWithdrawal, ProviderID: "mock", PaymentMethod: "bank_transfer", AssetCode: "EUR",
			Amount: 500, Interactive: false, ClaimToken: uuid.New(), LeaseOwner: "w", LeaseUntil: time.Now().Add(time.Minute),
		})
		return err
	})
	if err == nil {
		t.Fatal("expected InsertSubmittingAttempt (T1p, payout) to be refused for a matching engaged switch")
	}

	allowedWithdrawal := insertWithdrawalRequest(t, pool, f, "approved", nil, nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &allowedWithdrawal, ProviderID: "other-provider", PaymentMethod: "bank_transfer", AssetCode: "EUR",
			Amount: 500, Interactive: false, ClaimToken: uuid.New(), LeaseOwner: "w", LeaseUntil: time.Now().Add(time.Minute),
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected InsertSubmittingAttempt (T1p) for a non-matching provider to succeed, got %v", err)
	}
}

// TestKillSwitchPredicate_T12_ResubmitAmbiguous is H1's T12 coverage.
func TestKillSwitchPredicate_T12_ResubmitAmbiguous(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_h1c")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	mkAmbiguous := func(providerID string) uuid.UUID {
		intentID := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
		var attemptID uuid.UUID
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			a, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
				ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
				DepositIntentID: &intentID, ProviderID: providerID, PaymentMethod: "card", AssetCode: "EUR",
				Amount: 1000, Interactive: true, ClaimToken: uuid.New(), LeaseOwner: "w", LeaseUntil: time.Now().Add(time.Minute),
			})
			if err != nil {
				return err
			}
			attemptID = a.ID
			return MarkAmbiguousFromSubmitting(ctx, tx, attemptID, EvidenceSweeper, time.Now())
		}); err != nil {
			t.Fatalf("build ambiguous attempt: %v", err)
		}
		return attemptID
	}

	blocked := mkAmbiguous("mock")
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "mock", "deposit", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ResubmitAmbiguous(ctx, tx, blocked, uuid.New(), "w", time.Now().Add(time.Minute), 0)
	})
	if err == nil {
		t.Fatal("expected ResubmitAmbiguous (T12) to be refused for a matching engaged switch")
	}

	allowed := mkAmbiguous("other-provider")
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ResubmitAmbiguous(ctx, tx, allowed, uuid.New(), "w", time.Now().Add(time.Minute), 0)
	})
	if err != nil {
		t.Fatalf("expected ResubmitAmbiguous for a non-matching provider to succeed, got %v", err)
	}
}

// TestKillSwitchPredicate_CascadeT1_InsertCreatedAttempt is H1's cascade T1
// coverage. The cascade insert has not chosen a provider yet, so it can
// only be blocked by a wildcard provider_scope='*' switch (attempt.go's own
// documented limitation) - the positive control here is a SPECIFIC-provider
// switch, which must NOT block the cascade insert.
func TestKillSwitchPredicate_CascadeT1_InsertCreatedAttempt(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_h1d")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}

	blockedIntent := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &blockedIntent, AttemptNo: 2, ExcludedProviderIDs: []string{"mock"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 1000, Interactive: true,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected cascade InsertCreatedAttempt to be refused under a wildcard-engaged switch")
	}

	// Positive control 1: a SPECIFIC-provider switch does not block the
	// cascade insert (attempt.go's own documented limitation - the cascade
	// row has not chosen a provider yet).
	pool2 := migration0105Scratch(t, "m0105_h1d2")
	f2 := seedM0101Fixture(t, pool2)
	staff2 := seedKillSwitchStaff(t, pool2, f2.tenantID)
	if err := pool2.WithPrincipalScope(context.Background(), f2.tenantID, staff2, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f2.tenantID, "mock", "deposit", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}
	allowedIntent := insertDepositIntent(t, pool2, f2, "pending", nil, nil, nil)
	err = pool2.WithTenant(context.Background(), f2.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f2.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &allowedIntent, AttemptNo: 2, ExcludedProviderIDs: []string{"mock"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 1000, Interactive: true,
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected cascade InsertCreatedAttempt to succeed under a specific-provider switch (no provider chosen yet), got %v", err)
	}

	// Positive control 2: a wildcard switch for a DIFFERENT operation
	// (payout) does not block a deposit cascade insert.
	pool3 := migration0105Scratch(t, "m0105_h1d3")
	f3 := seedM0101Fixture(t, pool3)
	staff3 := seedKillSwitchStaff(t, pool3, f3.tenantID)
	if err := pool3.WithPrincipalScope(context.Background(), f3.tenantID, staff3, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f3.tenantID, "*", "payout", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}
	allowedIntent3 := insertDepositIntent(t, pool3, f3, "pending", nil, nil, nil)
	err = pool3.WithTenant(context.Background(), f3.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f3.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &allowedIntent3, AttemptNo: 2, ExcludedProviderIDs: []string{"mock"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 1000, Interactive: true,
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected cascade InsertCreatedAttempt (deposit) to succeed under a payout-only wildcard switch, got %v", err)
	}
}

// TestKillSwitchPredicate_PlayerScopedT1T2_ClaimsNothing is the §16.2 item
// 20 pin the security review named as also missing: a player-scoped
// session (app.player_account_id set) must claim NOTHING on the T1+T2
// combined path - RLS on payment_attempts itself (tenant_staff_scope
// requires app.player_account_id unset) refuses the INSERT outright, before
// the kill-switch predicate is even relevant.
func TestKillSwitchPredicate_PlayerScopedT1T2_ClaimsNothing(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_h1e")
	f := seedM0101Fixture(t, pool)

	intentID := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &intentID, ProviderID: "mock", PaymentMethod: "card", AssetCode: "EUR",
			Amount: 1000, Interactive: true, ClaimToken: uuid.New(), LeaseOwner: "w", LeaseUntil: time.Now().Add(time.Minute),
		})
		return err
	})
	if err == nil {
		t.Fatal("expected a player-scoped T1+T2 insert to claim nothing (RLS refusal), per §16.2 item 20")
	}

	// Confirm nothing was actually written under any context.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if scanErr := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id=$1`, intentID).Scan(&n); scanErr != nil {
			return scanErr
		}
		if n != 0 {
			t.Fatalf("expected zero payment_attempts rows for this intent, found %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seedKillSwitchStaff inserts one tenant-scoped staff_users row a test can
// use as the acting principal for WithPrincipalScope engage calls.
func seedKillSwitchStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,$2,$3,'x','tenant_admin','active')`,
			id, tenantID, "h1-"+id.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	return id
}
