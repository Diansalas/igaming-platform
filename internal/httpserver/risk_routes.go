package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerRiskRoutes wires Stage 4G's Risk & Limits admin API - tenant-
// scoped only this stage (see risk_handlers.go's own top-of-file doc
// comment for the platform-wide-rule limitation). Mirrors every other
// tenant-scoped admin surface in this codebase: RequireTenantScope +
// RequirePermission, never inferred from the UI.
func registerRiskRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/risk/rules",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermRiskConfigRead)(newListRiskRulesHandler(deps)))))
	mux.Handle("POST /v1/admin/risk/rules",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermRiskConfigManage)(newCreateRiskRuleHandler(deps)))))
	mux.Handle("POST /v1/admin/risk/rules/{id}/disable",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermRiskConfigManage)(newDisableRiskRuleHandler(deps)))))
}
