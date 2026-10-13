//go:build integration

package aml

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file exists ONLY in integration builds (the `integration` build tag): no production binary contains it. It lets a
// test inject an evaluator fault into the one database read of the enforcement path, to prove that an evaluator error or
// timeout denies exactly the evaluated operation and raises aml.hold_eval_failed (ADR 0116 s7.2 fail-closed).

// ProbeFunc replaces the hold read.
type ProbeFunc func(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (bool, error)

// NoHoldProbe is the "no hold ever" read for scratch schemas that predate the AML objects (migration 0141).
func NoHoldProbe(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, Operation) (bool, error) {
	return false, nil
}

// SetHoldProbeForTest installs f as the hold read and returns the restore function. Tests that use it must not run in
// parallel with each other; restores must be applied in reverse order of installation (t.Cleanup does).
func SetHoldProbeForTest(f ProbeFunc) (restore func()) {
	prev := holdProbe
	holdProbe = f
	return func() { holdProbe = prev }
}

// RealHoldProbeForTest is the production hold read itself (never the installed hook), so the failure-injection tests exercise
// the real savepoint / batch mechanism directly.
func RealHoldProbeForTest(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (bool, error) {
	return probeHoldActive(ctx, tx, tenantID, personID, accountID, op)
}
