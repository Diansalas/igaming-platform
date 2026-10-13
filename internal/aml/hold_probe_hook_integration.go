//go:build integration

package aml

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file exists ONLY in integration builds (the `integration` build tag): no production binary contains it. It lets a
// test inject an evaluator fault into the one database read of the enforcement path, to prove that an evaluator error or
// timeout denies exactly the evaluated operation and raises aml.hold_eval_failed (ADR 0116 s7.2 fail-closed).

var probeHookMu sync.Mutex

// ProbeFunc replaces the hold read.
type ProbeFunc func(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op Operation) (bool, error)

// SetHoldProbeForTest installs f as the hold read and returns the restore function. Tests that use it must not run in
// parallel with each other.
func SetHoldProbeForTest(f ProbeFunc) (restore func()) {
	probeHookMu.Lock()
	prev := holdProbe
	holdProbe = f
	return func() {
		holdProbe = prev
		probeHookMu.Unlock()
	}
}
