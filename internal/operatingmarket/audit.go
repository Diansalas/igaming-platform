package operatingmarket

// This file implements the mandatory audit record for every mutation
// (ADR 0045 §8), mirroring jurisdiction.recordRegistryAudit exactly.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// Binding auditor rule (ADR 0045 §3.5-A AMENDMENT-2 / §10): an auditor
// searching for events that widened the operating footprint filters on
// widening_capable = true. Filtering on state = 'enabled' is wrong and will
// miss every inherit-rung withdrawal, which is the shape that re-permits
// without ever writing the word "enabled".

// recordOperatingMarketAudit writes the mandatory audit record for a
// mutating operating-market operation, in the SAME transaction as the
// mutation itself. tenantID selects the audit row's scope (audit_log's
// dual-scope RLS, ADR 0013): uuid.Nil for a genuinely PLATFORM-scoped
// mutation (the licence ceiling; tx is WithPlatformAdmin), or the
// specific tenant id for a tenant-scoped mutation (tx is WithTenant,
// required by audit_log's WITH CHECK policy).
//
// Metadata NEVER contains a player account id, a player residence value,
// a location/geo signal, an IP-derived country, or any EvidenceSet/
// PlayerJurisdictionResult field - structurally guaranteed by INV-M-1
// (this package cannot reach those types) and asserted by
// TestOperatingMarketAudit_RecordsBeforeAfterAndNeverPlayerEvidence.
// country_code here is an ADMINISTRATIVE POLICY SUBJECT, not player
// evidence - canonical-model §5.3's "never the evidence VALUES" rule
// governs player-jurisdiction determinations, not administrative
// configuration, which is why before/after are REQUIRED here by
// CLAUDE.md's audit rule rather than excluded.
func recordOperatingMarketAudit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor ActorContext, action, targetType, targetID string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["reason_code"] = actor.ReasonCode
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.ActorID,
		Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: audit.OutcomeSuccess, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent,
		RequestID: actor.RequestID, Metadata: metadata,
	}); err != nil {
		return fmt.Errorf("operatingmarket: audit %s: %w", action, err)
	}
	return nil
}

// ceilingRecordState renders a CeilingRecord as audit-log-friendly
// before/after state.
func ceilingRecordState(r CeilingRecord) map[string]any {
	var effectiveTo any
	if r.EffectiveTo != nil {
		effectiveTo = r.EffectiveTo.Format(time.RFC3339Nano)
	}
	return map[string]any{
		"id":                      r.ID.String(),
		"licence_id":              r.LicenceID.String(),
		"country_code":            r.CountryCode,
		"state":                   string(r.State),
		"status":                  string(r.Status),
		"authorization_reference": r.AuthorizationReference,
		"reason_code":             r.ReasonCode,
		"policy_version":          r.PolicyVersion,
		"effective_from":          r.EffectiveFrom.Format(time.RFC3339Nano),
		"effective_to":            effectiveTo,
		"created_by_actor_type":   r.CreatedByActorType,
		"created_by_actor_id":     r.CreatedByActorID.String(),
	}
}

// policyRecordState renders a PolicyRecord as audit-log-friendly
// before/after state.
func policyRecordState(r PolicyRecord) map[string]any {
	var effectiveTo any
	if r.EffectiveTo != nil {
		effectiveTo = r.EffectiveTo.Format(time.RFC3339Nano)
	}
	var brandID any
	if r.BrandID != nil {
		brandID = r.BrandID.String()
	}
	var operationCode any
	if r.OperationCode != nil {
		operationCode = *r.OperationCode
	}
	var productCode any
	if r.ProductCode != nil {
		productCode = *r.ProductCode
	}
	return map[string]any{
		"id":                      r.ID.String(),
		"tenant_id":               r.TenantID.String(),
		"scope":                   string(r.Scope),
		"brand_id":                brandID,
		"operation_code":          operationCode,
		"product_code":            productCode,
		"country_code":            r.CountryCode,
		"state":                   string(r.State),
		"status":                  string(r.Status),
		"authorization_reference": r.AuthorizationReference,
		"reason_code":             r.ReasonCode,
		"policy_version":          r.PolicyVersion,
		"effective_from":          r.EffectiveFrom.Format(time.RFC3339Nano),
		"effective_to":            effectiveTo,
		"created_by_actor_type":   r.CreatedByActorType,
		"created_by_actor_id":     r.CreatedByActorID.String(),
	}
}
