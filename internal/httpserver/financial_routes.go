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

	// Staff four-eyes withdrawal review queue - tenant-scoped. Stage 3D
	// splits what was one PermWithdrawalApprove into four distinct
	// permissions (business decision #4/#5: staff-management and
	// withdrawal authority must be separable, and a broad admin role
	// must never hold withdrawal authority implicitly) - only RoleFinance
	// holds any of them (internal/auth/permission.go). RequireTenantScope
	// excludes platform_admin's nil-tenant token, matching every other
	// tenant-scoped admin route.
	mux.Handle("GET /v1/admin/withdrawals",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalReview)(newListPendingWithdrawalsHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/approve",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalApprove)(newApproveWithdrawalHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/reject",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalReject)(newRejectWithdrawalHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/submit",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalSubmit)(newSubmitWithdrawalHandler(deps)))))

	// Stage 3C stranded-hold recovery - same permission as submit/review
	// respectively, since resolving a submitted withdrawal is the same
	// class of action (deciding what happens to money already in
	// flight) as submitting it, and listing submitted requests is the
	// same class of action as reviewing the pending queue.
	mux.Handle("GET /v1/admin/withdrawals/submitted",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalReview)(newListSubmittedWithdrawalsHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawals/{id}/resolve",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalSubmit)(newResolveWithdrawalHandler(deps)))))

	// Stage 5 Back Office read-only history/detail views - reuse
	// PermWithdrawalReview (same "may see withdrawal data" authority the
	// pending queue above already gates). NOT registered at
	// "GET /v1/admin/withdrawals" - that exact path is already bound to
	// the frozen pending-only queue above (newListPendingWithdrawalsHandler,
	// which additionally promotes requested -> pending_review as a side
	// effect and must not be touched this stage), and net/http's ServeMux
	// rejects a second literal registration of the same method+pattern.
	// "/history" is this file's equivalent of the existing "/submitted"
	// naming convention for a second, differently-scoped GET list under
	// the same resource. See this task's own completion report for the
	// exact rationale - flagged there for the orchestrator's OpenAPI
	// consolidation pass.
	mux.Handle("GET /v1/admin/withdrawals/history",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalReview)(newListAdminWithdrawalsHandler(deps)))))
	mux.Handle("GET /v1/admin/withdrawals/{id}",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalReview)(newGetAdminWithdrawalHandler(deps)))))

	// Provider capability configuration - tenant-scoped administrative
	// action, gated by PermProviderConfigWrite.
	mux.Handle("PUT /v1/admin/providers/{providerID}/capability",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermProviderConfigWrite)(newWriteProviderCapabilityHandler(deps)))))

	// Stage 3D withdrawal-policy configuration - the minimal admin API
	// boundary directive item 4 requires so a real policy can be
	// configured without direct database editing. Gated by
	// PermWithdrawalPolicyWrite - deliberately its OWN permission, held
	// only by RoleTenantAdmin (not RoleFinance: the role that approves
	// withdrawals should not also be the role that can loosen the
	// policy gating its own approvals - see docs/decisions/0024 §5).
	mux.Handle("GET /v1/admin/withdrawal-policies",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalPolicyWrite)(newListWithdrawalPoliciesHandler(deps)))))
	mux.Handle("POST /v1/admin/withdrawal-policies",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalPolicyWrite)(newWriteWithdrawalPolicyHandler(deps)))))
	mux.Handle("DELETE /v1/admin/withdrawal-policies/{id}",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermWithdrawalPolicyWrite)(newDeleteWithdrawalPolicyHandler(deps)))))
}
