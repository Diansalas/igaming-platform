//go:build integration

// Stage 4H-B1 Wave 3 Phase 10 (`architect`) — the structural EOI budget
// bound, proven at the MECHANISM'S OWN ENTRY POINT rather than only in
// the HTTP handler that happens to be its sole caller today.
//
// security-architecture.md §W3P6.5 item 1, verbatim: "the EOI budget
// bound lives only in the HTTP handler... bonus.MintRootOperation itself
// still accepts nil for all three... any future non-HTTP minting path
// therefore reopens the decomposition vector at the mechanism's own entry
// point." `security` routed the durable fix here rather than changing a
// shared contract unilaterally; this suite is its regression proof.
//
// Each case below minted SUCCESSFULLY before this fix, producing a real,
// approved, open `economic_operations` root against which
// ConsumeRootBudget would then skip the corresponding containment check
// for every execution underneath it, forever.
package bonus

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/economicop"
)

func TestMintRootOperation_RefusesUnboundedRoot(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)

	asset := f.assetCode
	ceilingOne := int32(1)
	ceilingFive := int32(5)

	bounded := func() MintRootOperationParams {
		return MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeSingle, SubjectRef: &f.playerID,
			AssetCode: &asset, IntendedAggregateValue: big.NewInt(1_000_000), RecipientCeiling: &ceilingOne,
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
			ApprovalState: economicop.ApprovalApproved,
		}
	}

	cases := []struct {
		name   string
		mutate func(*MintRootOperationParams)
	}{
		{name: "no recipient_ceiling", mutate: func(p *MintRootOperationParams) { p.RecipientCeiling = nil }},
		{name: "no intended_aggregate_value", mutate: func(p *MintRootOperationParams) { p.IntendedAggregateValue = nil }},
		{name: "no asset_code", mutate: func(p *MintRootOperationParams) { p.AssetCode = nil }},
		{name: "no expires_at", mutate: func(p *MintRootOperationParams) { p.ExpiresAt = time.Time{} }},
		{name: "single_subject with no subject_ref", mutate: func(p *MintRootOperationParams) { p.SubjectRef = nil }},
		{name: "single_subject with a ceiling above one", mutate: func(p *MintRootOperationParams) { p.RecipientCeiling = &ceilingFive }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := bounded()
			p.IdempotencyKey = "arch-unbounded-" + uuid.NewString()
			c.mutate(&p)

			var mintErr error
			var rowCount int
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, mintErr = MintRootOperation(ctx, tx, p)
				// Counted in the SAME transaction as the refused mint: the
				// falsifiable claim is "no row came into existence", not
				// merely "an error was returned".
				return tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM economic_operations WHERE tenant_id = $1 AND idempotency_key = $2`,
					f.tenantID, p.IdempotencyKey,
				).Scan(&rowCount)
			}); err != nil {
				t.Fatalf("count economic_operations: %v", err)
			}

			if mintErr == nil {
				t.Fatalf("expected the mint to be refused, got nil error")
			}
			if !errors.Is(mintErr, economicop.ErrUnboundedRootAuthorization) {
				t.Fatalf("expected ErrUnboundedRootAuthorization, got %v", mintErr)
			}
			if rowCount != 0 {
				t.Fatalf("expected no economic_operations row to be created, found %d", rowCount)
			}
		})
	}

	t.Run("a fully bounded root still mints, and is still idempotent", func(t *testing.T) {
		p := bounded()
		p.IdempotencyKey = "arch-bounded-" + uuid.NewString()

		var first, second economicop.EconomicOperation
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if first, err = MintRootOperation(ctx, tx, p); err != nil {
				return err
			}
			second, err = MintRootOperation(ctx, tx, p)
			return err
		}); err != nil {
			t.Fatalf("mint a fully bounded root: %v", err)
		}
		if first.OperationID == uuid.Nil {
			t.Fatalf("expected a real operation id")
		}
		if first.OperationID != second.OperationID {
			t.Fatalf("mint-once broken: %s != %s", first.OperationID, second.OperationID)
		}
	})
}
