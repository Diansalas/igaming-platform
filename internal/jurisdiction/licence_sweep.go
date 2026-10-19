package jurisdiction

// Licensing slice 1 (docs/plans/lic1-record.md; audit gap G-5): the licence expiry / review sweep. READ-ONLY and
// DETECTIVE, in the shape of the reconciliation scheduler: an hourly loop, one tenant-scoped read-only transaction per
// tenant (no cross-tenant read: the licences_read policy admits exactly the tenant's own bound licence), isolated
// per-tenant failures, a panic recovered, and a failed alert write logged and counted but never stopping the loop.
//
// It raises, per LICENCE (aggregated across the tenants bound to it):
//
//   - licence.expired  (p1): the licence's status is 'expired', or expires_at has been reached (UTC date; the same
//     half-open boundary as EvaluateLicenceValidity: the licence is NOT valid on its expires_at date).
//   - licence.expiring (p2): not expired, and expires_at or review_due_at falls on or before today + the warning
//     window (an already-overdue review also counts).
//
// The affected tenant and brand ids are attributes (ids only, bounded). Every tenant and brand bound to the licence is
// listed whatever its launch status: the sweep deliberately does not read tenants.status / brands.status (those readers
// are a reviewed, closed set, ADR 0112 LF7-LF9), so a closed tenant can appear in the list (documented residual). It NEVER suspends a brand, tenant or licence,
// and never changes any row: auto-suspension needs a human decision. The existing runtime market-gate refusal
// (EvaluateLicenceValidity in the operating-market resolver) remains the enforcement.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// LicenceExpiryWarningWindowDays is the pre-expiry / pre-review warning window. 30 days is an INTERIM ENGINEERING
// DEFAULT pending the owner decision (triage L-22 / audit G-5): it is a configuration constant, not a legal
// requirement, and invents no regulatory notice period.
const LicenceExpiryWarningWindowDays = 30

// LicenceSweepInterval is the sweep cadence ("hourly", like the reconciliation scheduler).
const LicenceSweepInterval = time.Hour

// maxAlertIDs bounds each id list in an alert payload (alert attributes are flat scalars of at most 2048 bytes in
// total); the exact counts are always carried in tenant_count / brand_count and "truncated" says when a list is cut.
const maxAlertIDs = 15

// Sweep triggers (closed vocabulary of the alert attribute "trigger").
const (
	TriggerStatusExpired    = "status_expired"
	TriggerExpiresAtReached = "expires_at_reached"
	TriggerExpiresAtSoon    = "expires_at_soon"
	TriggerReviewDue        = "review_due"
)

// ClassifyLicenceForSweep decides whether a licence needs an alert at asOf. It returns "" when none. Pure.
func ClassifyLicenceForSweep(status string, expiresAt, reviewDueAt *time.Time, asOf time.Time, windowDays int) (kind alerting.Kind, trigger string) {
	today := asOf.UTC().Truncate(24 * time.Hour)
	horizon := today.AddDate(0, 0, windowDays)
	if status == "expired" {
		return alerting.KindLicenceExpired, TriggerStatusExpired
	}
	if expiresAt != nil && !today.Before(*expiresAt) {
		return alerting.KindLicenceExpired, TriggerExpiresAtReached
	}
	if expiresAt != nil && !expiresAt.After(horizon) {
		return alerting.KindLicenceExpiring, TriggerExpiresAtSoon
	}
	if reviewDueAt != nil && !reviewDueAt.After(horizon) {
		return alerting.KindLicenceExpiring, TriggerReviewDue
	}
	return "", ""
}

// LicenceFinding is one alert-worthy licence with the tenants and brands that rely on it.
type LicenceFinding struct {
	LicenceID   uuid.UUID
	Kind        alerting.Kind
	Trigger     string
	ExpiresAt   *time.Time
	ReviewDueAt *time.Time
	TenantIDs   []uuid.UUID
	BrandIDs    []uuid.UUID
}

// LicenceSweepResult summarises one pass.
type LicenceSweepResult struct {
	TenantsRead  int
	TenantErrors int
	Findings     []LicenceFinding
	RaiseErrors  int
}

// raiseLicenceAlert is a test seam (production value: alerting.RaiseLicenceSweep).
var raiseLicenceAlert = alerting.RaiseLicenceSweep

type licenceAgg struct {
	status      string
	expiresAt   *time.Time
	reviewDueAt *time.Time
	tenants     []uuid.UUID
	brands      []uuid.UUID
}

// RunLicenceSweepOnce performs one pass at now. It returns an error only when the tenant LISTING fails (nothing could
// be assessed); per-tenant read failures and alert-write failures are counted in the result and logged.
func RunLicenceSweepOnce(ctx context.Context, pool *db.Pool, logger *slog.Logger, now time.Time) (LicenceSweepResult, error) {
	var res LicenceSweepResult
	type tenantRow struct{ tenant, licence uuid.UUID }
	var tenants []tenantRow
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, licence_id FROM tenants WHERE licence_id IS NOT NULL ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r tenantRow
			if err := rows.Scan(&r.tenant, &r.licence); err != nil {
				return err
			}
			tenants = append(tenants, r)
		}
		return rows.Err()
	})
	if err != nil {
		return res, fmt.Errorf("jurisdiction: licence sweep: list tenants: %w", err)
	}

	aggs := map[uuid.UUID]*licenceAgg{}
	for _, t := range tenants {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var status string
		var expires, review *time.Time
		var brands []uuid.UUID
		err := pool.WithTenantReadOnly(ctx, t.tenant, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT status, expires_at, review_due_at FROM licences WHERE id = $1`, t.licence).Scan(&status, &expires, &review); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT id FROM brands WHERE tenant_id = $1 ORDER BY id`, t.tenant)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					return err
				}
				brands = append(brands, id)
			}
			return rows.Err()
		})
		if err != nil {
			res.TenantErrors++
			if logger != nil {
				logger.Error("licence_sweep_tenant_read_failed", "tenant_id", t.tenant, "licence_id", t.licence, "error_type", fmt.Sprintf("%T", err))
			}
			continue
		}
		res.TenantsRead++
		a := aggs[t.licence]
		if a == nil {
			a = &licenceAgg{status: status, expiresAt: expires, reviewDueAt: review}
			aggs[t.licence] = a
		}
		a.tenants = append(a.tenants, t.tenant)
		a.brands = append(a.brands, brands...)
	}

	ids := make([]uuid.UUID, 0, len(aggs))
	for id := range aggs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		a := aggs[id]
		kind, trigger := ClassifyLicenceForSweep(a.status, a.expiresAt, a.reviewDueAt, now, LicenceExpiryWarningWindowDays)
		if kind == "" {
			continue
		}
		f := LicenceFinding{LicenceID: id, Kind: kind, Trigger: trigger, ExpiresAt: a.expiresAt, ReviewDueAt: a.reviewDueAt,
			TenantIDs: a.tenants, BrandIDs: a.brands}
		res.Findings = append(res.Findings, f)
		if err := raiseLicenceAlert(ctx, pool, kind, id, findingAttributes(f)); err != nil {
			res.RaiseErrors++
			if logger != nil {
				logger.Error("licence_sweep_alert_not_written", "licence_id", id, "kind", string(kind))
			}
		}
	}
	return res, nil
}

func idList(ids []uuid.UUID) (string, bool) {
	cut := len(ids) > maxAlertIDs
	if cut {
		ids = ids[:maxAlertIDs]
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return strings.Join(parts, ","), cut
}

func findingAttributes(f LicenceFinding) map[string]alerting.AttrValue {
	t, tcut := idList(f.TenantIDs)
	b, bcut := idList(f.BrandIDs)
	attrs := map[string]alerting.AttrValue{
		"trigger": f.Trigger, "tenant_count": len(f.TenantIDs), "brand_count": len(f.BrandIDs),
		"tenant_ids": t, "brand_ids": b, "truncated": tcut || bcut,
	}
	if f.ExpiresAt != nil {
		attrs["expires_at"] = dateString(f.ExpiresAt)
	}
	if f.ReviewDueAt != nil {
		attrs["review_due_at"] = dateString(f.ReviewDueAt)
	}
	return attrs
}

// RunLicenceSweepLoop runs RunLicenceSweepOnce immediately and then every interval until ctx is cancelled. A pass's
// error or panic is logged and never stops the loop or the process.
func RunLicenceSweepLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval time.Duration) {
	once := func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("licence_sweep_panic_recovered", "panic_type", fmt.Sprintf("%T", r))
			}
		}()
		if _, err := RunLicenceSweepOnce(ctx, pool, logger, time.Now().UTC()); err != nil && ctx.Err() == nil && logger != nil {
			logger.Error("licence_sweep_failed", "error_type", fmt.Sprintf("%T", err))
		}
	}
	once()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			once()
		}
	}
}
