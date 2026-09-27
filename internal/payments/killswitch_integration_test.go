//go:build integration

// PRH-I1 kill switch Go service layer (killswitch.go), exercised against
// the same migration-0105 scratch harness as migration_0105_integration_test.go.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestKillSwitch_EngageIsIdempotentAndReasonUpdates(t *testing.T) {
	pool := migration0105Scratch(t, "m0105ks_a")
	f := seedM0105Fixture(t, pool)

	var v1 int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := EngageKillSwitch(ctx, tx, f.tenantID, "*", KillSwitchOperationDeposit, "incident-1")
		if err != nil {
			return err
		}
		v1 = ks.Version
		if !ks.Engaged || ks.EngagedByScope == nil || *ks.EngagedByScope != KillSwitchScopeTenant {
			t.Fatalf("unexpected switch after engage: %+v", ks)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := EngageKillSwitch(ctx, tx, f.tenantID, "*", KillSwitchOperationDeposit, "incident-2")
		if err != nil {
			return err
		}
		if ks.ReasonCode != "incident-2" {
			t.Fatalf("reason_code = %q, want incident-2", ks.ReasonCode)
		}
		if ks.Version <= v1 {
			t.Fatalf("version did not advance on re-engage: %d -> %d", v1, ks.Version)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKillSwitch_ServiceLayerFourEyesRoundTrip(t *testing.T) {
	pool := migration0105Scratch(t, "m0105ks_b")
	f := seedM0105Fixture(t, pool)

	var ksID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := EngageKillSwitch(ctx, tx, f.tenantID, "mock", KillSwitchOperationPayout, "manual")
		ksID, version = ks.ID, ks.Version
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := RequestKillSwitchRelease(ctx, tx, f.tenantID, ksID, version, "resolved", 0)
		reqID = req.ID
		if req.Status != "open" {
			t.Fatalf("status = %q, want open", req.Status)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Requester cannot approve+release their own request.
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApproveAndReleaseKillSwitch(ctx, tx, ksID, reqID)
		return err
	})
	if err == nil {
		t.Fatal("expected self-approval to be refused")
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := ApproveAndReleaseKillSwitch(ctx, tx, ksID, reqID)
		if err != nil {
			return err
		}
		if ks.Engaged {
			t.Fatal("expected switch to be released")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		list, err := ListKillSwitches(ctx, tx, f.tenantID)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].Engaged {
			t.Fatalf("unexpected list after release: %+v", list)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKillSwitch_CancelOpenRequest(t *testing.T) {
	pool := migration0105Scratch(t, "m0105ks_c")
	f := seedM0105Fixture(t, pool)

	var ksID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := EngageKillSwitch(ctx, tx, f.tenantID, "*", KillSwitchOperationAny, "x")
		ksID, version = ks.ID, ks.Version
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := RequestKillSwitchRelease(ctx, tx, f.tenantID, ksID, version, "changed my mind", 0)
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return CancelKillSwitchRelease(ctx, tx, reqID)
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Cancelling again is refused (terminal, immutable).
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return CancelKillSwitchRelease(ctx, tx, reqID)
	})
	if !errors.Is(err, ErrAttemptStateConflict) {
		t.Fatalf("expected ErrAttemptStateConflict on double-cancel, got %v", err)
	}
}

func TestKillSwitch_ReadOnlyEngagementCheck(t *testing.T) {
	pool := migration0105Scratch(t, "m0105ks_d")
	f := seedM0105Fixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		engaged, err := KillSwitchEngaged(ctx, tx, f.tenantID, "mock", AttemptOperationDeposit)
		if err != nil {
			return err
		}
		if engaged {
			t.Fatal("expected not engaged before any switch exists")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "mock", KillSwitchOperationDeposit, "x")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		engaged, err := KillSwitchEngaged(ctx, tx, f.tenantID, "mock", AttemptOperationDeposit)
		if err != nil {
			return err
		}
		if !engaged {
			t.Fatal("expected engaged for the matching provider/operation")
		}
		engaged, err = KillSwitchEngaged(ctx, tx, f.tenantID, "other-provider", AttemptOperationDeposit)
		if err != nil {
			return err
		}
		if engaged {
			t.Fatal("expected not engaged for a non-matching provider (provider_scope was specific, not '*')")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
