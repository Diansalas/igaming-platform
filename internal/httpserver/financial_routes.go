package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerFinancialRoutes wires every Stage 3B wallet/deposit/withdrawal
// endpoint. Mirrors registerIdentityRoutes' pattern (routes.go) - grouped
// here rather than scattered across server.go so the full route table,
// and which middleware chain guards each route, is visible in one place.
//
// Explicitly NOT exposed this stage (CLAUDE.md's Stage 3B scope gate):
// anything bonus/casino/sportsbook/crypto-related, and any endpoint that
// would let a client name its own tenant_id/player_account_id/wallet_id -
// every identifying field below is resolved server-side from the
// authenticated session (see each handler's own doc comment).
func registerFinancialRoutes(mux *http.ServeMux, deps Deps) {
	// Player self-service wallet/balance reads.
	mux.Handle("GET /v1/me/wallets", auth.Middleware(deps.AuthIssuer)(newListWalletsHandler(deps)))
	mux.Handle("GET /v1/me/wallets/{assetCode}", auth.Middleware(deps.AuthIssuer)(newGetWalletHandler(deps)))

	// Player self-service deposits.
	mux.Handle("POST /v1/me/deposits", auth.Middleware(deps.AuthIssuer)(newInitiateDepositHandler(deps)))
	mux.Handle("GET /v1/me/deposits", auth.Middleware(deps.AuthIssuer)(newListDepositsHandler(deps)))
	mux.Handle("GET /v1/me/deposits/{id}", auth.Middleware(deps.AuthIssuer)(newGetDepositHandler(deps)))

	// Provider callback - no bearer-token middleware (a provider webhook
	// is not an authenticated platform principal); the handler itself
	// resolves tenant scope from the URL's tenant slug and verifies the
	// payload's signature via the named adapter, per
	// payment-orchestration.md §10 and deposit_handlers.go's own comment.
	mux.HandleFunc("POST /v1/webhooks/payments/{tenantSlug}/{providerID}", newPaymentWebhookHandler(deps))

	// Player self-service withdrawals.
	mux.Handle("POST /v1/me/withdrawals", auth.Middleware(deps.AuthIssuer)(newRequestWithdrawalHandler(deps)))
	mux.Handle("GET /v1/me/withdrawals", auth.Middleware(deps.AuthIssuer)(newListWithdrawalsHandler(deps)))
	mux.Handle("GET /v1/me/withdrawals/{id}", auth.Middleware(deps.AuthIssuer)(newGetWithdrawalHandler(deps)))
	mux.Handle("POST /v1/me/withdrawals/{id}/cancel", auth.Middleware(deps.AuthIssuer)(newCancelWithdrawalHandler(deps)))

	// Staff four-eyes withdrawal review queue - tenant-scoped, gated by
	// PermWithdrawalApprove (finance/tenant_admin roles - internal/auth/
	// permission.go). RequireTenantScope excludes platform_admin's
	// nil-tenant token, matching every other tenant-scoped admin route.
	mux.Handle("GET /v1/admin/withdrawals",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalApprove)(newListPendingWithdrawalsHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/approve",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalApprove)(newApproveWithdrawalHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/reject",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalApprove)(newRejectWithdrawalHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/submit",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalApprove)(newSubmitWithdrawalHandler(deps)))))

	// Provider capability configuration - tenant-scoped administrative
	// action, gated by PermProviderConfigWrite.
	mux.Handle("PUT /v1/admin/providers/{providerID}/capability",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermProviderConfigWrite)(newWriteProviderCapabilityHandler(deps)))))
}
