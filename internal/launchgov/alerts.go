package launchgov

import (
	"context"
	"log/slog"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// raiseSuspensionExhausted raises the P1 alert launch.suspension_exhausted (security C4): every
// bounded attempt to execute a suspension ended in lock contention (or the C-3(b) commit check),
// so a subject that should have been suspended was not. It is raised AFTER the business
// transactions rolled back, through the detached path (alerting.RaiseDetached, ADR 0102 7.4):
// it opens its own transaction in exactly the originating scope of the actor, retries
// internally and falls back to alerting.raise_failed. The business outcome never depends on
// the alert row: a failure is already logged and counted by the alerting package and is not
// returned. The alert carries ids and counts only (no free text, no actor identity beyond the
// request id the route supplied). Notification routing for this Kind is NOT IMPLEMENTED
// (ALERT-DELIVERY-1 OPEN): the row is the P1 record, nobody is paged.
func (s *Service) raiseSuspensionExhausted(ctx context.Context, a Actor, sub Subject, attempts int) {
	pool, ok := s.cfg.Pool.(*db.Pool)
	if !ok {
		slog.Default().Error("launch_suspension_exhausted_alert_not_raised", "reason", "pool is not a *db.Pool")
		return
	}
	var runner alerting.ScopedRunner
	if a.Scope == ScopeTenant {
		runner = alerting.NewTenantRunner(pool, a.TenantID)
	} else {
		runner = alerting.NewPlatformAdminRunner(pool, a.StaffID)
	}
	id := sub.TenantID.String()
	if sub.BrandID != nil {
		id = sub.BrandID.String()
	}
	attrs := map[string]alerting.AttrValue{"subject_kind": sub.Kind(), "attempts": attempts}
	if a.RequestID != "" {
		attrs["request_id"] = a.RequestID
	}
	_ = alerting.RaiseDetached(ctx, runner, alerting.Alert{
		Kind:            alerting.KindLaunchSuspensionExhausted,
		SubjectTenantID: sub.TenantID,
		Discriminator:   "suspend:" + sub.Kind() + ":" + id,
		Attributes:      attrs,
	})
}
