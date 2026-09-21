package db

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTableOwnershipChecker is a mock ConnectingRoleOwnsNoTables
// implementation so VerifyRuntimeRoleInProduction's branches are unit
// -testable without a real database connection.
type fakeTableOwnershipChecker struct {
	ownsNothing bool
	err         error
}

func (f fakeTableOwnershipChecker) ConnectingRoleOwnsNoTables(ctx context.Context) (bool, error) {
	return f.ownsNothing, f.err
}

func TestVerifyRuntimeRoleInProduction_NonProductionNeverGated(t *testing.T) {
	ctx := context.Background()
	// A checker that would fail the check if it were ever consulted -
	// proves the environment gate short-circuits before calling it at
	// all in every non-"production" environment.
	checker := fakeTableOwnershipChecker{ownsNothing: false}

	for _, env := range []string{"development", "staging", "test", "", "PRODUCTION", "Production"} {
		t.Run(env, func(t *testing.T) {
			if err := VerifyRuntimeRoleInProduction(ctx, env, checker); err != nil {
				t.Errorf("environment %q: expected no error (only the exact string \"production\" gates this check), got: %v", env, err)
			}
		})
	}
}

func TestVerifyRuntimeRoleInProduction_ProductionNonOwningRolePasses(t *testing.T) {
	ctx := context.Background()
	checker := fakeTableOwnershipChecker{ownsNothing: true}

	if err := VerifyRuntimeRoleInProduction(ctx, "production", checker); err != nil {
		t.Errorf("expected no error when the connecting role owns nothing, got: %v", err)
	}
}

func TestVerifyRuntimeRoleInProduction_ProductionOwningRoleFailsClosed(t *testing.T) {
	ctx := context.Background()
	checker := fakeTableOwnershipChecker{ownsNothing: false}

	err := VerifyRuntimeRoleInProduction(ctx, "production", checker)
	if err == nil {
		t.Fatal("expected an error when the connecting role owns tables in production, but got nil - this must fail closed")
	}
	if !strings.Contains(err.Error(), "owns tables") {
		t.Errorf("expected the error to clearly name the problem (\"owns tables\"), got: %v", err)
	}
}

func TestVerifyRuntimeRoleInProduction_ProductionCheckFailurePropagatesFailClosed(t *testing.T) {
	ctx := context.Background()
	checker := fakeTableOwnershipChecker{err: errors.New("simulated connectivity failure")}

	if err := VerifyRuntimeRoleInProduction(ctx, "production", checker); err == nil {
		t.Fatal("expected an error when the ownership check itself fails, but got nil - a broken check must fail closed, not silently pass")
	}
}
