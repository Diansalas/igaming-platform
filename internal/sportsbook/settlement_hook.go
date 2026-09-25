//go:build !integration

package sportsbook

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// settlementAfterPostHook is a no-op in every non-test build. The
// integration-tagged twin (settlement_hook_integration.go) lets a test
// inject a failure between ledger.Post and the history insert (ADR 0088
// §14 Q3(b)); it is never compiled into an application binary.
func settlementAfterPostHook(context.Context, pgx.Tx) error { return nil }
