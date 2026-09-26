//go:build integration

package casino

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// receiveCallbackInTx is a TEST-ONLY bridge (ADR 0094 §4.1) for the domain
// tests written against the pre-ADR-0094 single-transaction shape
// (ReceiveCallback(ctx, tx, ...)): it runs phase 1 (VerifyCallback) and
// phase 2 (ReceiveVerifiedCallback) back to back on the SAME caller
// transaction, so those tests keep asserting domain behaviour (ledger,
// idempotency, locks, audit) unchanged.
//
// It deliberately does what production must never do - run phase 1 inside
// a held transaction - so it may only be used where the resolver never
// reaches a secret store (MOCK resolvers and test doubles). The tests that
// pin the ADR 0094 properties (statement capture, INV-POOL, the re-check)
// drive the real two-phase shape instead. Phase 1 runs under a fresh
// context carrying only the request id, because the caller's ctx is
// txscope-marked and VerifyCallback refuses marked contexts.
func (o *Orchestrator) receiveCallbackInTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, in webhookauth.Inbound) (ReceiveCallbackResult, error) {
	var _ webhookauth.TenantReader = txReader{}
	v, err := o.VerifyCallback(phaseOneContext(ctx), txReader{tx: tx}, tenantID, providerID, in)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	return o.ReceiveVerifiedCallback(ctx, tx, tenantID, providerID, v)
}

// txReader runs a "read-only transaction" on the caller's existing
// transaction (TEST ONLY; see receiveCallbackInTx).
type txReader struct{ tx pgx.Tx }

func (r txReader) WithTenantReadOnly(ctx context.Context, _ uuid.UUID, fn db.TxFunc) error {
	return fn(ctx, r.tx)
}

// phaseOneContext is an unmarked context carrying ctx's request state.
func phaseOneContext(ctx context.Context) context.Context {
	out := context.Background()
	if rs := observability.RequestStateFromContext(ctx); rs != nil {
		out = observability.WithRequestState(out, rs)
	}
	return out
}
