package economicop

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Stage 4H-B1 Wave 3 Phase 10 (`architect`): the structural half of
// security-architecture.md §W3P6.5 item 1. Every case below is an
// authorization shape that, before this validator existed, minted
// successfully and then had ConsumeRootBudget silently skip the
// corresponding containment check at every later execution underneath it.
func TestValidateRootAuthorizationBounds(t *testing.T) {
	asset := "EUR"
	empty := ""
	subject := uuid.New()
	one := int32(1)
	five := int32(5)
	zero := int32(0)
	future := time.Now().UTC().Add(time.Hour)

	bounded := func(mutate func(*EconomicOperation)) EconomicOperation {
		op := EconomicOperation{
			OperationType:          OperationBonusBulkGrant,
			SubjectScope:           SubjectScopeEnumeratedSet,
			AssetCode:              &asset,
			IntendedAggregateValue: big.NewInt(1_000_000),
			RecipientCeiling:       &five,
			LineageKind:            LineageRoot,
			ExpiresAt:              future,
		}
		if mutate != nil {
			mutate(&op)
		}
		return op
	}

	cases := []struct {
		name    string
		op      EconomicOperation
		wantErr bool
	}{
		{name: "fully bounded bulk-grant root is accepted", op: bounded(nil)},
		{
			name: "fully bounded single-subject manual-grant root is accepted",
			op: bounded(func(o *EconomicOperation) {
				o.OperationType = OperationBonusManualGrant
				o.SubjectScope = SubjectScopeSingle
				o.SubjectRef = &subject
				o.RecipientCeiling = &one
			}),
		},
		{name: "nil asset_code is refused", op: bounded(func(o *EconomicOperation) { o.AssetCode = nil }), wantErr: true},
		{name: "empty asset_code is refused", op: bounded(func(o *EconomicOperation) { o.AssetCode = &empty }), wantErr: true},
		{name: "nil intended_aggregate_value is refused", op: bounded(func(o *EconomicOperation) { o.IntendedAggregateValue = nil }), wantErr: true},
		{name: "zero intended_aggregate_value is refused", op: bounded(func(o *EconomicOperation) { o.IntendedAggregateValue = big.NewInt(0) }), wantErr: true},
		{name: "negative intended_aggregate_value is refused", op: bounded(func(o *EconomicOperation) { o.IntendedAggregateValue = big.NewInt(-1) }), wantErr: true},
		{name: "nil recipient_ceiling is refused", op: bounded(func(o *EconomicOperation) { o.RecipientCeiling = nil }), wantErr: true},
		{name: "zero recipient_ceiling is refused", op: bounded(func(o *EconomicOperation) { o.RecipientCeiling = &zero }), wantErr: true},
		{name: "zero expires_at is refused", op: bounded(func(o *EconomicOperation) { o.ExpiresAt = time.Time{} }), wantErr: true},
		{
			name: "single_subject with no subject_ref is refused",
			op: bounded(func(o *EconomicOperation) {
				o.SubjectScope = SubjectScopeSingle
				o.RecipientCeiling = &one
			}),
			wantErr: true,
		},
		{
			name: "single_subject with a ceiling above one is refused",
			op: bounded(func(o *EconomicOperation) {
				o.SubjectScope = SubjectScopeSingle
				o.SubjectRef = &subject
			}),
			wantErr: true,
		},
		{name: "subject_scope none is refused for a value-creating type", op: bounded(func(o *EconomicOperation) { o.SubjectScope = SubjectScopeNone }), wantErr: true},
		{
			// Doc 34 §2.2: a null asset_code is legal and meaningful for a
			// non-monetary operation, and RK-W15P2-5 says such a root has no
			// enforceable value budget at all. The rule is permit-by-
			// enumeration precisely so this stays true.
			name: "an undeclared operation_type is left alone, unbounded and all",
			op: bounded(func(o *EconomicOperation) {
				o.OperationType = OperationCRMEngagementCampaignActivation
				o.AssetCode = nil
				o.IntendedAggregateValue = nil
				o.RecipientCeiling = nil
			}),
		},
		{
			// A child carries no independent budget - §3.4's budget is a
			// subtree aggregate keyed on root_operation_id.
			name: "a non-root lineage_kind is left alone",
			op: bounded(func(o *EconomicOperation) {
				o.LineageKind = LineageItem
				o.AssetCode = nil
				o.IntendedAggregateValue = nil
				o.RecipientCeiling = nil
			}),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRootAuthorizationBounds(c.op)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected a refusal, got nil")
				}
				if !errors.Is(err, ErrUnboundedRootAuthorization) {
					t.Fatalf("expected ErrUnboundedRootAuthorization, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
		})
	}
}
