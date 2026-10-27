package alerting

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// RaiseLicenceSweep raises licence.expired, licence.expiring or licence.unmonitored for one licence (licensing slice 1, migration 0176).
// The sweep has no tenant and no platform-admin principal, so this is a platform, subjectless alert raised under the
// alert_dispatcher platform-service identity. The discriminator is "licence:<licence_id>", so the hourly sweep attaches
// occurrences to ONE open alert per (Kind, licence) instead of raising one alert per pass. Any other Kind, a nil
// licence id, or attributes outside the Kind's allowlist are refused in Go before any SQL (and counted). The caller
// treats a returned error as log-and-continue: a failed write never stops the sweep loop.
func RaiseLicenceSweep(ctx context.Context, pool *db.Pool, kind Kind, licenceID uuid.UUID, attrs map[string]AttrValue) error {
	if (kind != KindLicenceExpired && kind != KindLicenceExpiring && kind != KindLicenceUnmonitored) || licenceID == uuid.Nil {
		dctx, cancel := detachedCtx(ctx)
		defer cancel()
		recordRaiseFailure(dctx, kind, "detached")
		return &invalidAlertError{kind: kind, msg: "not a licence sweep Kind, or no licence id"}
	}
	if attrs == nil {
		attrs = map[string]AttrValue{}
	}
	attrs["licence_id"] = licenceID.String()
	return raiseLicenceSweepAs(ctx, pool, Alert{Kind: kind, Discriminator: fmt.Sprintf("licence:%s", licenceID), Attributes: attrs})
}

// Sweep failure phases (the closed vocabulary of the licence.sweep_failed "phase" attribute).
const (
	LicenceSweepPhaseListTenants = "list_tenants"
	LicenceSweepPhaseTenantRead  = "tenant_read"
	LicenceSweepPhaseRaise       = "raise"
)

// RaiseLicenceSweepFailed raises licence.sweep_failed (migration 0178, security review F-3) when the licence sweep could not
// complete: the tenant listing failed, at least one tenant read failed, or at least one alert write failed. Platform,
// subjectless, under the alert_dispatcher identity. One open alert per phase (discriminator "sweep:<phase>"), so a sweep that
// keeps failing adds occurrences instead of alerts. A phase outside the closed vocabulary is refused in Go before any SQL.
func RaiseLicenceSweepFailed(ctx context.Context, pool *db.Pool, phase string, errorCount int) error {
	switch phase {
	case LicenceSweepPhaseListTenants, LicenceSweepPhaseTenantRead, LicenceSweepPhaseRaise:
	default:
		dctx, cancel := detachedCtx(ctx)
		defer cancel()
		recordRaiseFailure(dctx, KindLicenceSweepFailed, "detached")
		return &invalidAlertError{kind: KindLicenceSweepFailed, msg: "unknown licence sweep failure phase"}
	}
	if errorCount < 1 {
		errorCount = 1
	}
	return raiseLicenceSweepAs(ctx, pool, Alert{Kind: KindLicenceSweepFailed, Discriminator: "sweep:" + phase,
		Attributes: map[string]AttrValue{"phase": phase, "error_count": errorCount}})
}
