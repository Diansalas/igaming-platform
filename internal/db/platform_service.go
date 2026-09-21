package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PlatformService is the closed vocabulary of non-human, non-request-scoped
// platform processes that are permitted to write platform-wide catalogue
// data. Adding a member is an ADR-level decision plus a migration that
// widens the corresponding policy predicate - never a bare string at a
// call site. See docs/decisions/0081-arch-db-2-catalogue-write-
// authorization.md §3.2.
type PlatformService string

// ServiceSportsbookCatalogueSync is the sportsbook catalogue sync's
// service identity - a startup-time background process with no HTTP
// request, no token, and no human principal (cmd/platform-api/main.go),
// used to satisfy migration 0084's sb_sports/sb_competitions/sb_events/
// sb_markets/sb_selections write policies.
const ServiceSportsbookCatalogueSync PlatformService = "sportsbook_catalogue_sync"

// platformServiceAllowlist is the closed vocabulary itself - the point is
// that the GUC value set below can never be attacker- or config-supplied,
// only one of these compiled-in constants.
var platformServiceAllowlist = map[PlatformService]struct{}{
	ServiceSportsbookCatalogueSync: {},
}

// ErrPlatformServiceScope is returned when a caller attempts to act as
// though it holds a platform-service-scoped transaction (see
// AssertPlatformServiceScope) but does not - a SERVER-side caller bug,
// not a client input error, mirroring internal/jurisdiction's
// ErrTransactionScope and internal/casino's ErrTransactionScope exactly.
var ErrPlatformServiceScope = errors.New("db: requires a platform-service-scoped transaction (use db.Pool.WithPlatformService)")

// WithPlatformService runs fn in a genuinely platform-scoped transaction
// (app.tenant_id and app.player_account_id deliberately never set) with
// the Postgres session variable "app.platform_service_id" set, for the
// lifetime of the transaction, to service. This is the ONLY sanctioned
// way to perform a migration-0084 sb_sports/sb_competitions/sb_events/
// sb_markets/sb_selections write.
//
// It exists because those five tables are platform-wide catalogue data
// written exclusively by a startup-time background process
// (sportsbook.SyncCatalogue, called from cmd/platform-api/main.go) that
// has no authenticated principal at all - WithPlatformAdmin's contract
// requires a real, request-scoped human platform-admin principal resolved
// from a verified token subject, and using it here would launder a
// machine process as a human principal; WithoutTenant is the SAME scope
// every ordinary platform read already uses (tenant lookup, brand lookup,
// the public catalogue browse itself), so a write policy keyed on it
// would grant nothing and would be security theatre. See ADR 0081 §3.2
// for the full rejection of both alternatives.
//
// service must be a member of the closed platformServiceAllowlist above -
// any other value (including "") is rejected BEFORE a transaction is
// opened, so an unknown or attacker-influenced string can never reach the
// database.
func (p *Pool) WithPlatformService(ctx context.Context, service PlatformService, fn TxFunc) error {
	if service == "" {
		return fmt.Errorf("db: WithPlatformService called with empty service identity")
	}
	if _, ok := platformServiceAllowlist[service]; !ok {
		return fmt.Errorf("db: WithPlatformService called with unknown service identity %q", service)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_service_id', $1, true)`, string(service)); err != nil {
		return fmt.Errorf("db: set platform service context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// AssertPlatformServiceScope verifies, from inside an already-open
// transaction, that app.platform_service_id equals service AND that
// app.tenant_id/app.player_account_id are both unset - the read-side
// analogue of WithPlatformService, for defence-in-depth Go-level
// assertions mirroring internal/jurisdiction's assertPlatformScope
// (evaluation_policy_admin.go). Migration 0084's RLS policies enforce the
// identical predicate independently at the database, so a caller that
// bypassed this check would still fail at the INSERT/UPDATE, but with a
// much less diagnosable error.
func AssertPlatformServiceScope(ctx context.Context, tx pgx.Tx, service PlatformService) error {
	var scopedService *string
	var scopedTenant *string
	var scopedPlayer *string
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_service_id', true), ''),
		        NULLIF(current_setting('app.tenant_id', true), ''),
		        NULLIF(current_setting('app.player_account_id', true), '')`,
	).Scan(&scopedService, &scopedTenant, &scopedPlayer); err != nil {
		return fmt.Errorf("db: read platform service scope: %w", err)
	}
	if scopedService == nil || *scopedService != string(service) || scopedTenant != nil || scopedPlayer != nil {
		return ErrPlatformServiceScope
	}
	return nil
}
