//go:build integration

package kyc

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// This file exists ONLY in integration builds: no production binary contains it. A few migration tests build their
// data through the Go writers on a scratch database migrated to a version BEFORE the AML objects exist (migration 0141);
// the real hold read fails closed there (aml_hold_active() does not exist), so those tests install "no hold" for the
// duration of the test. Nothing else may use it.

var holdHookMu sync.Mutex

// SetHoldEvaluatorForTest replaces the AML hold read used by EvaluateEnforcement and returns the restore function.
func SetHoldEvaluatorForTest(f func(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op aml.Operation) (aml.HoldVerdict, error)) (restore func()) {
	holdHookMu.Lock()
	prev := evaluateHold
	evaluateHold = f
	return func() {
		evaluateHold = prev
		holdHookMu.Unlock()
	}
}

// NoHoldEvaluator is the "no hold ever" evaluator for schemas that predate the AML objects.
func NoHoldEvaluator(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (aml.HoldVerdict, error) {
	return aml.HoldVerdict{}, nil
}
