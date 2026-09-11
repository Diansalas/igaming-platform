package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

type tenantConfigResponse struct {
	TenantID      string `json:"tenant_id"`
	DisplayName   string `json:"display_name"`
	Theme         string `json:"theme"`
	DefaultLocale string `json:"default_locale"`
}

// newTenantConfigHandler is the Stage 1 foundation's end-to-end
// demonstration of the tenant-isolation pattern: it takes the tenant id
// ONLY from the verified JWT (via tenant.FromContext - never a header,
// query param, or path segment), then reads through db.Pool.WithTenant so
// PostgreSQL's row-level security is what actually enforces isolation. A
// request carrying a valid token for tenant A structurally cannot see
// tenant B's config row, even if application code had a bug in the WHERE
// clause below - RLS is the backstop, not this handler.
func newTenantConfigHandler(pool *db.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
			return
		}

		var resp tenantConfigResponse
		err = pool.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			row := tx.QueryRow(ctx,
				`SELECT tenant_id, display_name, theme::text, default_locale
				 FROM tenant_config
				 WHERE tenant_id = $1`,
				tc.TenantID,
			)
			var tenantID string
			return row.Scan(&tenantID, &resp.DisplayName, &resp.Theme, &resp.DefaultLocale)
		})

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			apierror.Write(w, requestID, apierror.CodeNotFound, "no configuration found for this tenant")
			return
		case err != nil:
			observability.LoggerFromContext(r.Context(), logger).Error("tenant_config_query_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load tenant configuration")
			return
		}

		resp.TenantID = tc.TenantID.String()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
