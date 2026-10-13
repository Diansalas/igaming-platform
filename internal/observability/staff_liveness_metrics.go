package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ADR 0119 slice S4a (section 7.3, review N1): the per-request staff liveness
// check is tracked by one counter, staff_liveness_decisions_total{outcome,
// reason}. Both labels are closed enums (no tenant, staff or session id ever
// becomes a label). The pattern is webhook_admission_metrics.go's: a package
// meter from the global provider, the creation error discarded (a nil counter
// is a no-op), and recording can never affect the request (safelyRecord).
var staffLivenessMeter = otel.Meter("github.com/Diansalas/igaming-platform/internal/httpserver/staff_liveness")

// StaffLivenessOutcome is the `outcome` label.
type StaffLivenessOutcome string

const (
	StaffLivenessAllowed StaffLivenessOutcome = "allowed"
	StaffLivenessRefused StaffLivenessOutcome = "refused"
	StaffLivenessError   StaffLivenessOutcome = "error"
)

// StaffLivenessReason is the `reason` label: why a request was refused (or
// "ok" / "db_error").
type StaffLivenessReason string

const (
	StaffLivenessReasonOK              StaffLivenessReason = "ok"
	StaffLivenessReasonNoSession       StaffLivenessReason = "no_session"
	StaffLivenessReasonRevoked         StaffLivenessReason = "revoked"
	StaffLivenessReasonAbsoluteExpired StaffLivenessReason = "absolute_expired"
	StaffLivenessReasonStaffNotActive  StaffLivenessReason = "staff_not_active"
	StaffLivenessReasonRoleMismatch    StaffLivenessReason = "role_mismatch"
	StaffLivenessReasonVersionMismatch StaffLivenessReason = "version_mismatch"
	StaffLivenessReasonFactorUnusable  StaffLivenessReason = "factor_unusable"
	StaffLivenessReasonNoAssurance     StaffLivenessReason = "no_assurance"
	StaffLivenessReasonDBError         StaffLivenessReason = "db_error"
)

var staffLivenessDecisionsTotal, _ = staffLivenessMeter.Int64Counter(
	"staff_liveness_decisions_total",
	metric.WithDescription("Count of per-request staff liveness decisions by outcome/reason (ADR 0119 section 7.3)."),
)

// RecordStaffLivenessDecision records one liveness decision. It is called
// after the decision is made; it never influences it.
func RecordStaffLivenessDecision(ctx context.Context, outcome StaffLivenessOutcome, reason StaffLivenessReason) {
	if staffLivenessDecisionsTotal == nil {
		return
	}
	safelyRecord(func() {
		staffLivenessDecisionsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("outcome", string(outcome)),
			attribute.String("reason", string(reason)),
		))
	})
}
