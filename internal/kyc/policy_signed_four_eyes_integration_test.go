//go:build integration

package kyc

// lic19 (migration 0185, S4): the KYC enforcement policy writes are signed, Person-distinct acts enforced by the database.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/testsupport/kycpolfix"
)

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestPolicyWrites_CreationNeedsAVerifiedProof(t *testing.T) {
	pool := testPool(t)
	author := polAdmin(t, pool)
	j := mustSeedJurisdiction(t, pool)
	insert := `INSERT INTO kyc_enforcement_policies (licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code,
		legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
		VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'L', 'test', 'platform_admin', $2)`
	// No proof at all.
	err := pool.WithPlatformAdmin(context.Background(), author, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insert, j, author)
		return err
	})
	if sqlState(err) != "AP001" {
		t.Fatalf("a creation without a proof: %v, want AP001", err)
	}
	// A proof for DIFFERENT content (threshold 101) does not cover this row.
	err = pool.WithPlatformAdmin(context.Background(), author, func(ctx context.Context, tx pgx.Tx) error {
		th, as := "101", "EUR"
		if err := polSigner.AttachCreate(ctx, tx, author, PolicyContent{LicensingJurisdictionID: j, TriggerType: "cumulative_deposit",
			ThresholdMinorUnits: &th, AssetCode: &as, LegalReviewReference: "L", ReasonCode: "test"}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, insert, j, author)
		return err
	})
	if sqlState(err) != "AP004" {
		t.Fatalf("a proof for other content: %v, want AP004", err)
	}
	// The matching proof works once; the same token cannot be replayed.
	err = pool.WithPlatformAdmin(context.Background(), author, func(ctx context.Context, tx pgx.Tx) error {
		th, as := "100", "EUR"
		if err := polSigner.AttachCreate(ctx, tx, author, PolicyContent{LicensingJurisdictionID: j, TriggerType: "cumulative_deposit",
			ThresholdMinorUnits: &th, AssetCode: &as, LegalReviewReference: "L", ReasonCode: "test"}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insert, j, author); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, insert, j, author) // same proof again, in the same transaction
		return err
	})
	if sqlState(err) != "AP005" {
		t.Fatalf("a replayed creation proof: %v, want AP005", err)
	}
}

func TestPolicyWrites_AccountWithoutAPersonLinkIsRefused_FailClosed(t *testing.T) {
	pool := testPool(t)
	kycpolfix.Install(t)
	j := mustSeedJurisdiction(t, pool)
	unlinked := kycpolfix.AdminNoPerson(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), unlinked, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rawDraftPolicy(ctx, tx, unlinked, j, "cumulative_deposit", "100", "EUR", nil, nil, "L", "test")
		return err
	})
	if sqlState(err) != "KP002" {
		t.Fatalf("an unlinked administrator creating: %v, want KP002", err)
	}
	// An unlinked ACTIVATOR is refused too, and a policy authored by an unlinked account can never be activated... (the author side
	// cannot be staged through the table: it needs a Person to create, so it is covered by the activator case below).
	author := polAdmin(t, pool)
	id := rawDraft(t, pool, author, j, "cumulative_deposit", "100", "EUR", nil, nil, "L")
	err = pool.WithPlatformAdmin(context.Background(), unlinked, func(ctx context.Context, tx pgx.Tx) error {
		return rawTransition(ctx, tx, unlinked, id, "draft", "active", nil)
	})
	if sqlState(err) != "KP002" {
		t.Fatalf("an unlinked administrator activating: %v, want KP002", err)
	}
}

func TestPolicyWrites_SamePersonDifferentAccountIsRefused(t *testing.T) {
	pool := testPool(t)
	author := polAdmin(t, pool)
	twin := kycpolfix.AdminSamePerson(t, pool, author)
	j := mustSeedJurisdiction(t, pool)
	id := rawDraft(t, pool, author, j, "cumulative_deposit", "100", "EUR", nil, nil, "L")
	err := pool.WithPlatformAdmin(context.Background(), twin, func(ctx context.Context, tx pgx.Tx) error {
		return rawTransition(ctx, tx, twin, id, "draft", "active", nil)
	})
	if sqlState(err) != "KP003" {
		t.Fatalf("a second account of the author's Person activating: %v, want KP003", err)
	}
	// Through the library too, and for a withdrawal.
	err = pool.WithPlatformAdmin(context.Background(), twin, func(ctx context.Context, tx pgx.Tx) error {
		return WithdrawEnforcementPolicy(ctx, tx, id, "draft", pol("fixture_withdraw"))
	})
	if ClassifyPolicyError(err) != PolicyFailureFourEyes {
		t.Fatalf("library withdrawal by the same Person: %v, want the four-eyes class", err)
	}
}

func TestPolicyWrites_TransitionProofIsBoundToTheTransitionAndTheReason(t *testing.T) {
	pool := testPool(t)
	author, activator := polAdmin(t, pool), polAdmin(t, pool)
	j := mustSeedJurisdiction(t, pool)
	id := rawDraft(t, pool, author, j, "cumulative_deposit", "100", "EUR", nil, nil, "L")
	other := rawDraft(t, pool, author, j, "cumulative_deposit", "200", "EUR", nil, nil, "L2")
	update := `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`
	run := func(prep func(ctx context.Context, tx pgx.Tx) error) error {
		return pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
			if err := prep(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, update, id)
			return err
		})
	}
	attach := func(ctx context.Context, tx pgx.Tx, pid uuid.UUID, from, to, reason string) error {
		return polSigner.AttachTransition(ctx, tx, activator, PolicyTransition{ID: pid, From: from, To: to, ReasonCode: reason})
	}
	setReason := func(ctx context.Context, tx pgx.Tx, r string) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.kyc_policy_reason_code', $1, true)`, r)
		return err
	}
	// No reason code.
	if err := run(func(ctx context.Context, tx pgx.Tx) error { return attach(ctx, tx, id, "draft", "active", "r1") }); sqlState(err) != "KP004" {
		t.Fatalf("no reason code: %v, want KP004", err)
	}
	// A proof signed for reason r1 while the transaction states r2.
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		if err := setReason(ctx, tx, "r2"); err != nil {
			return err
		}
		return attach(ctx, tx, id, "draft", "active", "r1")
	}); sqlState(err) != "AP004" {
		t.Fatalf("a proof for another reason: %v, want AP004", err)
	}
	// A proof for ANOTHER policy.
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		if err := setReason(ctx, tx, "r1"); err != nil {
			return err
		}
		return attach(ctx, tx, other, "draft", "active", "r1")
	}); sqlState(err) != "AP004" {
		t.Fatalf("a proof for another policy: %v, want AP004", err)
	}
	// A withdraw-class proof cannot activate (operation is part of the verified binding).
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		if err := setReason(ctx, tx, "r1"); err != nil {
			return err
		}
		return attach(ctx, tx, id, "draft", "withdrawn", "r1")
	}); sqlState(err) != "AP004" {
		t.Fatalf("a withdrawal proof used to activate: %v, want AP004", err)
	}
	// The matching proof and reason work.
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		if err := setReason(ctx, tx, "r1"); err != nil {
			return err
		}
		return attach(ctx, tx, id, "draft", "active", "r1")
	}); err != nil {
		t.Fatalf("matching proof: %v", err)
	}
}

func TestPolicyWrites_LibraryRefusesWithoutAProofSigner(t *testing.T) {
	pool := testPool(t)
	author := polAdmin(t, pool)
	j := mustSeedJurisdiction(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), author, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateEnforcementPolicy(ctx, tx, CreateEnforcementPolicyParams{LicensingJurisdictionID: j, TriggerType: "play",
			PlayOperation: strPtr("casino_play"), LegalReviewReference: "L", ReasonCode: "test"})
		return err
	})
	if !errors.Is(err, ErrPolicyProofRequired) {
		t.Fatalf("creation without a signer: %v, want ErrPolicyProofRequired", err)
	}
	id := rawDraft(t, pool, author, j, "cumulative_deposit", "100", "EUR", nil, nil, "L")
	activator := polAdmin(t, pool)
	err = pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, id, PolicyWrite{ReasonCode: "r"})
	})
	if !errors.Is(err, ErrPolicyProofRequired) {
		t.Fatalf("activation without a signer: %v, want ErrPolicyProofRequired", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, id, PolicyWrite{Proof: polSigner, ReasonCode: "not a code!"})
	})
	if !errors.Is(err, ErrPolicyInvalidInput) {
		t.Fatalf("a malformed reason code: %v, want ErrPolicyInvalidInput", err)
	}
}

// A tenant-scoped session is still stopped by row-level security before the proof trigger is reached (unchanged behaviour).
func TestPolicyWrites_TenantSessionStillRefusedByRLS(t *testing.T) {
	pool := testPool(t)
	kycpolfix.Install(t)
	j := mustSeedJurisdiction(t, pool)
	admin := polAdmin(t, pool)
	f := seedFixture(t, pool)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_enforcement_policies (licensing_jurisdiction_id, trigger_type, threshold_minor_units, asset_code,
			legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id) VALUES ($1, 'cumulative_deposit', 100, 'EUR', 'L', 'test', 'platform_admin', $2)`, j, admin)
		return err
	})
	if err == nil {
		t.Fatal("a tenant session must not write a policy")
	}
}
