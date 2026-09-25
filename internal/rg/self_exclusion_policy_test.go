// Pure-Go unit tests for the OpenBetSelfExclusionPolicy resolution
// primitive - no database required. Integration-level tests (real
// tighten-only trigger enforcement, RLS, concurrency) live in
// self_exclusion_policy_integration_test.go.
package rg

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOpenBetSelfExclusionPolicyStrictness_Ordering(t *testing.T) {
	settle, err := openBetSelfExclusionPolicyStrictness(PolicySettleNormally)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	void, err := openBetSelfExclusionPolicyStrictness(PolicyVoidOnSelfExclusion)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if void <= settle {
		t.Fatalf("expected VOID_ON_SELF_EXCLUSION (%d) strictly greater than SETTLE_NORMALLY (%d)", void, settle)
	}
}

func TestOpenBetSelfExclusionPolicyStrictness_RejectsUnknownValue(t *testing.T) {
	if _, err := openBetSelfExclusionPolicyStrictness(OpenBetSelfExclusionPolicy("BOGUS")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func row(tenantID, brandID *uuid.UUID, value OpenBetSelfExclusionPolicy) OpenBetSelfExclusionPolicyRow {
	return OpenBetSelfExclusionPolicyRow{ID: uuid.New(), TenantID: tenantID, BrandID: brandID, PolicyValue: value}
}

func uPtr(u uuid.UUID) *uuid.UUID { return &u }

func TestAggregateStrictness_EmptyIsNotConfigured(t *testing.T) {
	agg, err := aggregateStrictness(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.HasJurisdictionFloor {
		t.Fatal("expected no jurisdiction floor with zero rows")
	}
}

func TestAggregateStrictness_JurisdictionOnly(t *testing.T) {
	rows := []OpenBetSelfExclusionPolicyRow{row(nil, nil, PolicySettleNormally)}
	agg, err := aggregateStrictness(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !agg.HasJurisdictionFloor {
		t.Fatal("expected jurisdiction floor to be detected")
	}
	if agg.MaxValue != PolicySettleNormally {
		t.Fatalf("expected SETTLE_NORMALLY, got %v", agg.MaxValue)
	}
}

func TestAggregateStrictness_TenantRowWithoutJurisdictionRowIsNotConfigured(t *testing.T) {
	tenantID := uuid.New()
	// A tenant-only row with NO jurisdiction floor present must never be
	// treated as "configured" - directive's fail-closed rule: only a
	// jurisdiction floor makes a scope configured at all.
	rows := []OpenBetSelfExclusionPolicyRow{row(uPtr(tenantID), nil, PolicyVoidOnSelfExclusion)}
	agg, err := aggregateStrictness(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.HasJurisdictionFloor {
		t.Fatal("expected HasJurisdictionFloor=false when only a tenant-scoped row is present")
	}
}

func TestAggregateStrictness_MaxAcrossScopesNeverLoosens(t *testing.T) {
	tenantID := uuid.New()
	// Jurisdiction floor = VOID (strict); a (hypothetically bypassed)
	// looser tenant row must never win the aggregation - this is the
	// pure-Go core of directive item 2's "read-time max() backstop."
	rows := []OpenBetSelfExclusionPolicyRow{
		row(nil, nil, PolicyVoidOnSelfExclusion),
		row(uPtr(tenantID), nil, PolicySettleNormally),
	}
	agg, err := aggregateStrictness(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !agg.HasJurisdictionFloor {
		t.Fatal("expected jurisdiction floor present")
	}
	if agg.MaxValue != PolicyVoidOnSelfExclusion {
		t.Fatalf("expected max() to resolve VOID_ON_SELF_EXCLUSION despite a looser tenant row, got %v", agg.MaxValue)
	}
}

func TestAggregateStrictness_TighterTenantRowWins(t *testing.T) {
	tenantID := uuid.New()
	rows := []OpenBetSelfExclusionPolicyRow{
		row(nil, nil, PolicySettleNormally),
		row(uPtr(tenantID), nil, PolicyVoidOnSelfExclusion),
	}
	agg, err := aggregateStrictness(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.MaxValue != PolicyVoidOnSelfExclusion {
		t.Fatalf("expected the tighter tenant override to win, got %v", agg.MaxValue)
	}
}

func TestSetOpenBetSelfExclusionPolicy_ValidatesInputWithoutTouchingDB(t *testing.T) {
	tests := []struct {
		name   string
		params SetOpenBetSelfExclusionPolicyParams
	}{
		{"missing jurisdiction", SetOpenBetSelfExclusionPolicyParams{PolicyValue: PolicySettleNormally, ReasonCode: "x", ActorType: "system"}},
		{"invalid policy value", SetOpenBetSelfExclusionPolicyParams{JurisdictionCode: "KM-ANJ", PolicyValue: "BOGUS", ReasonCode: "x", ActorType: "system"}},
		{"brand without tenant", SetOpenBetSelfExclusionPolicyParams{JurisdictionCode: "KM-ANJ", BrandID: uPtr(uuid.New()), PolicyValue: PolicySettleNormally, ReasonCode: "x", ActorType: "system"}},
		{"missing reason code", SetOpenBetSelfExclusionPolicyParams{JurisdictionCode: "KM-ANJ", PolicyValue: PolicySettleNormally, ActorType: "system"}},
		{"missing actor id for non-system actor", SetOpenBetSelfExclusionPolicyParams{JurisdictionCode: "KM-ANJ", PolicyValue: PolicySettleNormally, ReasonCode: "x", ActorType: "staff"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SetOpenBetSelfExclusionPolicy(nil, nil, tt.params) //nolint:staticcheck // validation runs before ctx/tx are ever used
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestResolveOpenBetSelfExclusionPolicy_ValidatesInputWithoutTouchingDB(t *testing.T) {
	tests := []struct {
		name   string
		params ResolveOpenBetSelfExclusionPolicyParams
	}{
		{"missing jurisdiction", ResolveOpenBetSelfExclusionPolicyParams{AsOf: time.Now(), ActorType: "system"}},
		{"zero as-of (S-8: never implicitly \"now\")", ResolveOpenBetSelfExclusionPolicyParams{JurisdictionCode: "KM-ANJ", ActorType: "system"}},
		{"missing actor id for non-system actor", ResolveOpenBetSelfExclusionPolicyParams{JurisdictionCode: "KM-ANJ", AsOf: time.Now(), ActorType: "staff"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveOpenBetSelfExclusionPolicy(nil, nil, tt.params) //nolint:staticcheck
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}
