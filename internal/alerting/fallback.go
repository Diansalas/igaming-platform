// This file is the ONLY place in internal/alerting (besides dispatcher.go,
// for the meta-Kind delivery bookkeeping) that constructs an
// alerting.raise_failed Alert or uses the db.ServiceAlertDispatcher
// identity to write one. A static test (static_dispatcher_identity_test.go)
// enforces this.
package alerting

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// raiseFailed is the ADR §6.3 terminal fallback (LF F3; security Part 1
// ACCEPTED). It is called only when every other attempt to raise the
// ORIGINAL Kind has been exhausted (in-tx swallow followed by a Go
// validation failure, or a detached retry that exhausted its attempts).
// It persists exactly {kind, sqlstate_class} under the alert_dispatcher
// platform-service identity - migration 0108's attribute trigger forces
// the discriminator to "kind:<kind>" independently, so there is no
// free-text channel into this P1 even if this function's own discriminator
// argument were wrong.
//
// This is an unexported function precisely so nothing outside this
// package - and nothing outside this one file within the package - can
// ever construct a raise_failed Alert (AL-10/AL-14). If the fallback
// itself fails, it is logged at Error and counted with phase="fallback";
// it NEVER recurses (it never calls itself or RaiseDetached/RaiseGuarded
// about its own failure - C-102-6/SR-6, security Part 1).
func raiseFailed(ctx context.Context, pool *db.Pool, kind Kind, sqlstateClass string) {
	// The Error log line may carry tenant_id for operators (the
	// originating scope's tenant); the metric itself never does
	// (§6.3: "the metric has no tenant label" - recordRaiseFailure below
	// never takes a tenant argument at all, so this is structural, not a
	// discipline).
	slog.Default().Error("alert_raise_fallback", "kind", kind, "sqlstate_class", sqlstateClass)

	if pool == nil {
		recordRaiseFailure(ctx, kind, "fallback")
		slog.Default().Error("alert_raise_fallback_failed", "kind", kind, "sqlstate_class", sqlstateClass, "detail", "no pool available for the terminal fallback")
		return
	}

	attrs := map[string]AttrValue{"kind": string(kind), "sqlstate_class": sqlstateClass}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		recordRaiseFailure(ctx, kind, "fallback")
		slog.Default().Error("alert_raise_fallback_failed", "kind", kind, "sqlstate_class", sqlstateClass, "error", err)
		return
	}

	fallbackAlert := Alert{
		Kind: KindAlertingRaiseFailed,
		// Overwritten by migration 0108's raise_failed attribute trigger
		// to "kind:<attributes.kind>" regardless of what is sent here -
		// this value is never honoured, only provided for readability.
		Discriminator: "kind:" + string(kind),
		Attributes:    attrs,
	}

	err = pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return insertOrAttachOccurrence(ctx, tx, fallbackAlert, attrsJSON)
	})
	if err != nil {
		// No recursion: this failure is logged and counted, and this
		// function never raises about itself.
		recordRaiseFailure(ctx, kind, "fallback")
		slog.Default().Error("alert_raise_fallback_failed", "kind", kind, "sqlstate_class", sqlstateClass, "error", err)
	}
}
