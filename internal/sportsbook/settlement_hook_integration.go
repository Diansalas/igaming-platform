//go:build integration

package sportsbook

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Test seam (ADR 0088 §14 Q3(b)), compiled only under the integration
// build tag: a hook run after every ledger.Post of a settlement operation
// and before its history rows are inserted, for partial-failure and §4.7
// fault-injection tests.
var (
	settlementHookMu sync.Mutex
	settlementHook   func(context.Context, pgx.Tx) error
)

// SetSettlementAfterPostHookForTest installs fn (nil clears it) and
// returns a restore function.
func SetSettlementAfterPostHookForTest(fn func(context.Context, pgx.Tx) error) (restore func()) {
	settlementHookMu.Lock()
	prev := settlementHook
	settlementHook = fn
	settlementHookMu.Unlock()
	return func() {
		settlementHookMu.Lock()
		settlementHook = prev
		settlementHookMu.Unlock()
	}
}

func settlementAfterPostHook(ctx context.Context, tx pgx.Tx) error {
	settlementHookMu.Lock()
	fn := settlementHook
	settlementHookMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, tx)
}
