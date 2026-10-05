package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// PlatformService is the closed vocabulary of non-human, non-request-scoped
// platform processes that run under a platform-service identity: the
// sportsbook catalogue sync (platform-wide catalogue writes), the alert
// dispatcher (ADR 0102) and the KYC submission worker (ADR 0106; discovery
// and claim on one table only). Adding a member is an ADR-level decision
// plus a migration that widens the corresponding policy predicate - never a
// bare string at a call site. See docs/decisions/0081-arch-db-2-catalogue-
// write-authorization.md §3.2.
type PlatformService string

// ServiceSportsbookCatalogueSync is the sportsbook catalogue sync's
// service identity - a startup-time background process with no HTTP
// request, no token, and no human principal (cmd/platform-api/main.go),
// used to satisfy migration 0084's sb_sports/sb_competitions/sb_events/
// sb_markets/sb_selections write policies.
const ServiceSportsbookCatalogueSync PlatformService = "sportsbook_catalogue_sync"

// ServiceAlertDispatcher is the ADR 0102 ("Durable Alerting and
// Provider-Neutral Delivery", PRH-2 I-core) platform-service identity for
// the alert dispatcher (internal/alerting): a background loop with no
// HTTP request, no token and no human principal, that reads due alert
// work and records delivery/escalation outcomes. Migration 0110 grants
// this identity SELECT on alert_kinds/alerts/alert_occurrences/alert_routes,
// INSERT on alert_deliveries, and - as a narrow, ADR-reviewed exception
// (ADR 0102 §6.3, security's Q1 ruling) - INSERT of exactly the three
// accepted meta-Kinds ('alerting.unrouted', 'alerting.delivery_dead',
// 'alerting.raise_failed') on alerts/alert_occurrences. It never has
// UPDATE or DELETE on any alert table. Only internal/alerting's dispatcher
// and fallback code may use this identity (ADR 0102 AL-10; a static test
// in internal/alerting enforces this).
const ServiceAlertDispatcher PlatformService = "alert_dispatcher"

// ServiceKYCSubmissionWorker is the PRH-2 E1 (ADR 0106, HD-PRH2-11)
// dedicated, least-privilege, non-human identity of the KYC submission
// outbox worker (internal/kyc). It exists for exactly ONE statement: the
// discovery-and-claim UPDATE ... RETURNING on kyc_submission_outbox
// (internal/kyc/outbox_worker.go:claimNext). Migration 0114 gives it two
// policies on that one table (kso_kyc_worker_select, kso_kyc_worker_claim),
// restricts it with the 36 kyc_worker_fence_* policies on the nine tables
// where a session with no tenant GUC would otherwise reach more than public
// reference data, and it holds no grant or policy anywhere else. Every
// tenant-data step of the worker runs in a plain WithTenant transaction for
// the tenant id the claim returned - never under this identity. Only
// internal/kyc/outbox_worker.go:claimNext may use it (a static test
// enforces this).
const ServiceKYCSubmissionWorker PlatformService = "kyc_submission_worker"

// platformServiceAllowlist is the closed vocabulary itself - the point is
// that the GUC value set below can never be attacker- or config-supplied,
// only one of these compiled-in constants.
var platformServiceAllowlist = map[PlatformService]struct{}{
	ServiceSportsbookCatalogueSync: {},
	ServiceAlertDispatcher:         {},
	ServiceKYCSubmissionWorker:     {},
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
// sb_markets/sb_selections write, a migration-0110 dispatcher statement
// or the ADR 0106 KYC outbox claim; each identity's own policies bound
// what it can do (ADR 0106 section 3.3 for the KYC worker's fence).
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

	if err := fn(txscope.Mark(ctx), tx); err != nil {
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
