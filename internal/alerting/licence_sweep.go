package alerting

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// RaiseLicenceSweep raises licence.expired or licence.expiring for one licence (licensing slice 1, migration 0176).
// The sweep has no tenant and no platform-admin principal, so this is a platform, subjectless alert raised under the
// alert_dispatcher platform-service identity. The discriminator is "licence:<licence_id>", so the hourly sweep attaches
// occurrences to ONE open alert per (Kind, licence) instead of raising one alert per pass. Any other Kind, a nil
// licence id, or attributes outside the Kind's allowlist are refused in Go before any SQL (and counted). The caller
// treats a returned error as log-and-continue: a failed write never stops the sweep loop.
func RaiseLicenceSweep(ctx context.Context, pool *db.Pool, kind Kind, licenceID uuid.UUID, attrs map[string]AttrValue) error {
	if (kind != KindLicenceExpired && kind != KindLicenceExpiring) || licenceID == uuid.Nil {
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
