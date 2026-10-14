package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ADR 0119 S4b section 9.5 (ledger-finance review): a governed final approval
// refused because the ACTING staff member's own row was suspended or changed
// role between the request's liveness check and the in-transaction FOR SHARE
// re-assertion (db.ErrActorStaffNotLive). Rare by construction; a burst means a
// suspension raced live approvals (or a compromised session kept acting). One
// counter, staff_reassertion_refusals_total{flow}; the label is a closed enum
// (no tenant, staff or request id). Same pattern as staff_liveness_metrics.go.
var staffReassertionMeter = otel.Meter("github.com/Diansalas/igaming-platform/internal/httpserver/staff_reassertion")

// StaffReassertionFlow is the `flow` label.
type StaffReassertionFlow string

const (
	StaffReassertionFlowWithdrawalApprove StaffReassertionFlow = "withdrawal_approve"
	StaffReassertionFlowManualAdjustment  StaffReassertionFlow = "manual_adjustment"
	StaffReassertionFlowForceResolution   StaffReassertionFlow = "payment_force_resolution"
	StaffReassertionFlowHoldResolution    StaffReassertionFlow = "withdrawal_hold_resolution"
)

var staffReassertionRefusalsTotal, _ = staffReassertionMeter.Int64Counter(
	"staff_reassertion_refusals_total",
	metric.WithDescription("Governed approvals refused by the in-transaction staff re-assertion (ADR 0119 section 9.5)."),
)

// RecordStaffReassertionRefusal records one refusal. It never influences the
// request.
func RecordStaffReassertionRefusal(ctx context.Context, flow StaffReassertionFlow) {
	if staffReassertionRefusalsTotal == nil {
		return
	}
	safelyRecord(func() {
		staffReassertionRefusalsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("flow", string(flow))))
	})
}
