package httpserver

// F6 / SA-3 (docs/plans/f5-f6-record.md; docs/plans/staging-acceptance-suite-record.md): the platform operations
// status read.
//
//	GET /v1/admin/platform/status   platform_status:read   build commit, migration head, database_environment anchor
//
// Platform scope only, in three layers each pinned by its own test: the permission (granted solely to
// RolePlatformAdmin), an explicit tc.TenantID == uuid.Nil check (platformAlertCaller), and the route registration
// (staffRoute, class none: a read never steps up). The response carries three facts and nothing else:
//
//   - build.commit: the commit injected at link time (internal/buildinfo), "unknown" when none is valid;
//   - migrations: the highest applied version and the number of recorded versions, read by the runtime role from
//     schema_migrations (SELECT is granted to it; no owner connection, no SECURITY DEFINER function);
//   - database_environment: the owner-written anchor (migration 0135) as a closed token.
//
// It never returns a connection string, host, role name, secret reference, checksum or any tenant data. Like the other
// platform reads, it writes a platform-scope audit row (tenant_id NULL, no subject tenant) BEFORE reading anything, so
// a read that cannot be audited returns nothing (fail closed).

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/buildinfo"
	"github.com/Diansalas/igaming-platform/internal/observability"
)

// AuditActionPlatformStatusRead is the platform-scope audit action of the status read.
const AuditActionPlatformStatusRead = "platform.status_read"

func registerPlatformStatusRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/platform/status",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStatusRead)(newPlatformStatusHandler(deps))))
}

type platformStatusBuild struct {
	Commit string `json:"commit"`
}

type platformStatusMigrations struct {
	HeadVersion  int64 `json:"head_version"`
	AppliedCount int64 `json:"applied_count"`
}

type platformStatusEnvironment struct {
	// Anchor is one of production, staging, development, test, unrecorded (no row: the database reads it as production but
	// the production launch gate refuses to start on it) or unrecognised (a value outside the closed set).
	Anchor   string `json:"anchor"`
	Recorded bool   `json:"recorded"`
}

type platformStatusResponse struct {
	Build               platformStatusBuild       `json:"build"`
	Migrations          platformStatusMigrations  `json:"migrations"`
	DatabaseEnvironment platformStatusEnvironment `json:"database_environment"`
}

// platformStatusAnchor maps the stored anchor to the closed response token.
func platformStatusAnchor(stored string) platformStatusEnvironment {
	switch stored {
	case "":
		return platformStatusEnvironment{Anchor: "unrecorded", Recorded: false}
	case "production", "staging", "development", "test":
		return platformStatusEnvironment{Anchor: stored, Recorded: true}
	default:
		return platformStatusEnvironment{Anchor: "unrecognised", Recorded: true}
	}
}

func newPlatformStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, actor, requestID, ok := platformAlertCaller(w, r)
		if !ok {
			return
		}
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		entry := audit.Entry{
			ActorType: audit.ActorStaff, ActorID: actor,
			Action: AuditActionPlatformStatusRead, TargetType: "platform_status", Outcome: audit.OutcomeSuccess,
			IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
		}
		if err := deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			return audit.Record(ctx, tx, entry)
		}); err != nil {
			logger.Error("platform_status_audit_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}

		head, err := deps.DB.ReadMigrationHead(r.Context())
		if err != nil {
			logger.Error("platform_status_migration_head_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}
		anchor, err := deps.DB.ReadEnvironmentAnchor(r.Context())
		if err != nil {
			logger.Error("platform_status_anchor_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, platformStatusResponse{
			Build:               platformStatusBuild{Commit: buildinfo.CommitID()},
			Migrations:          platformStatusMigrations{HeadVersion: head.HeadVersion, AppliedCount: head.AppliedCount},
			DatabaseEnvironment: platformStatusAnchor(anchor),
		})
	}
}
