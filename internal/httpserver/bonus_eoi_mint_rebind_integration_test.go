//go:build integration

// Stage 4H-B1 Wave 3 Phase 6 (`security`): the payload-substitution cell
// of the SEC-W15-02 matrix for the EOI-MINTING surface.
//
// The other four surfaces pin their economic payload into a four-eyes
// approval, so payload substitution there means "apply an approval
// obtained for X to a different X'". The mint endpoint has no approval to
// substitute against - its analogous vector is the IDEMPOTENCY KEY:
// MintRootOperation resolves an already-used key to the EXISTING
// operation, so a caller must not be able to re-POST the same key with
// wider bounds (a larger value budget, a different subject) and have the
// returned, still-valid authorization silently carry the new ones. If it
// could, the mandatory-bounds fix would be trivially defeated: mint
// tightly, then re-mint the same key wide.
package httpserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestMintEconomicOperation_IdempotentReplayCannotWidenBounds(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-eoi-rebind")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-eoi-rebind")

	idemKey := "eoi-rebind-" + uuid.NewString()
	originalSubject := uuid.NewString()

	first := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, map[string]any{
		"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
		"subject_ref": originalSubject, "asset_code": "USD",
		"intended_aggregate_value": "1000", "recipient_ceiling": 1,
		"idempotency_key": idemKey,
	})
	defer first.Body.Close()
	if first.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, first, &apiErr)
		t.Fatalf("expected 201 for the initial, tightly-bounded mint, got %d: %+v", first.StatusCode, apiErr)
	}
	var firstOp economicOperationResponse
	decodeBody(t, first, &firstOp)

	// Same key, a 100,000x larger budget and a different beneficiary.
	second := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, map[string]any{
		"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
		"subject_ref": uuid.NewString(), "asset_code": "USD",
		"intended_aggregate_value": "100000000", "recipient_ceiling": 1,
		"idempotency_key": idemKey,
	})
	defer second.Body.Close()
	if second.StatusCode != 201 {
		t.Fatalf("expected 201 on the idempotent replay, got %d", second.StatusCode)
	}
	var secondOp economicOperationResponse
	decodeBody(t, second, &secondOp)
	if secondOp.OperationID != firstOp.OperationID {
		t.Fatalf("an idempotency-key replay minted a SECOND operation (%s vs %s) - the key is not binding", secondOp.OperationID, firstOp.OperationID)
	}

	var aggregate pgtype.Numeric
	var subjectRef *uuid.UUID
	var ceiling *int32
	opID := uuid.MustParse(firstOp.OperationID)
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT intended_aggregate_value, subject_ref, recipient_ceiling FROM economic_operations WHERE tenant_id = $1 AND operation_id = $2`,
			tenant.ID, opID,
		).Scan(&aggregate, &subjectRef, &ceiling)
	}); err != nil {
		t.Fatalf("read back the minted operation: %v", err)
	}

	if got := numericString(t, aggregate); got != "1000" {
		t.Fatalf("the replay WIDENED the authorized value budget: expected the original 1000, got %s", got)
	}
	if subjectRef == nil || subjectRef.String() != originalSubject {
		t.Fatalf("the replay REBOUND the authorized subject: expected %s, got %v", originalSubject, subjectRef)
	}
	if ceiling == nil || *ceiling != 1 {
		t.Fatalf("expected the original recipient_ceiling=1 to survive the replay, got %v", ceiling)
	}

	// The wider bounds must not have leaked into any second row either.
	var rows int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM economic_operations WHERE tenant_id = $1 AND idempotency_key = $2`, tenant.ID, idemKey).Scan(&rows)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly one economic_operations row for the idempotency key, got %d", rows)
	}
}
