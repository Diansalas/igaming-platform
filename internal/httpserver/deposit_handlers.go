package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// maxWebhookBodyBytes bounds an inbound provider callback body - a
// webhook has no session/auth to rate-limit by, so a caller-controlled
// unbounded body is a resource-exhaustion vector in a way an
// authenticated JSON request already isn't (decodeJSON's
// maxRequestBodyBytes covers those).
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

type initiateDepositRequest struct {
	AssetCode      string `json:"asset_code"`
	Amount         int64  `json:"amount"`
	PaymentMethod  string `json:"payment_method"`
	IdempotencyKey string `json:"idempotency_key"`
}

type depositIntentResponse struct {
	ID                string `json:"id"`
	AssetCode         string `json:"asset_code"`
	Amount            int64  `json:"amount"`
	PaymentMethod     string `json:"payment_method"`
	Status            string `json:"status"`
	ProviderID        string `json:"provider_id,omitempty"`
	RedirectURL       string `json:"redirect_url,omitempty"`
	HostedFieldToken  string `json:"hosted_field_token,omitempty"`
	LedgerTransaction string `json:"ledger_transaction_id,omitempty"`
}

func toDepositIntentResponse(d payments.DepositIntent) depositIntentResponse {
	resp := depositIntentResponse{
		ID: d.ID.String(), AssetCode: d.AssetCode, Amount: d.Amount, PaymentMethod: d.PaymentMethod,
		Status: string(d.Status), RedirectURL: d.RedirectURL, HostedFieldToken: d.HostedFieldToken,
	}
	if d.ProviderID != nil {
		resp.ProviderID = *d.ProviderID
	}
	if d.LedgerTransactionID != nil {
		resp.LedgerTransaction = d.LedgerTransactionID.String()
	}
	return resp
}

// newInitiateDepositHandler resolves the player's own wallet (creating it
// on first use for this asset) and drives PaymentOrchestrator.InitiateDeposit.
// Per payment-orchestration.md §3, every identifying field (tenant, brand,
// player, wallet) is resolved server-side from the authenticated session -
// the request body supplies only asset_code, amount, payment_method, and
// the client's own idempotency key.
//
// Runs under db.Pool.WithTenant, NOT WithPlayerScope - see
// newGetWalletHandler's identical rationale in wallet_handlers.go: this
// handler writes (wallet.GetOrCreate, InitiateDeposit's deposit_intents
// insert and ledger posting), and deposit_intents' player_self_scope
// policy (migration 0025) is SELECT-only by design.
func newInitiateDepositHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.PaymentOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "deposits are not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}

		var req initiateDepositRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("asset_code", req.AssetCode)
		v.RequireNonEmpty("payment_method", req.PaymentMethod)
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		if req.Amount <= 0 {
			v.Add("amount", "must be a positive integer (minor units)")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var intent payments.DepositIntent
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			wl, err := wallet.GetOrCreate(ctx, tx, tc.TenantID, account.BrandID, playerAccountID, req.AssetCode)
			if err != nil {
				return err
			}
			intent, err = deps.PaymentOrchestrator.InitiateDeposit(ctx, tx, payments.InitiateDepositParams{
				Scope: payments.DepositScope{
					TenantID: tc.TenantID, BrandID: account.BrandID, PlayerAccountID: playerAccountID, WalletID: wl.ID,
				},
				AssetCode: req.AssetCode, Amount: req.Amount, PaymentMethod: req.PaymentMethod, IdempotencyKey: req.IdempotencyKey,
			})
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown asset code")
			return
		}
		if errors.Is(err, payments.ErrIdempotencyKeyReused) {
			apierror.Write(w, requestID, apierror.CodeConflict, "idempotency key already used with different parameters")
			return
		}
		if err != nil {
			logger.Error("initiate_deposit_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to initiate deposit")
			return
		}
		writeJSON(w, http.StatusCreated, toDepositIntentResponse(intent))
	}
}

// newGetDepositHandler returns the status of one of the player's own
// deposit intents - read under db.Pool.WithPlayerScope, per deposit_intents'
// player_self_scope RLS policy (migration 0025).
func newGetDepositHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}
		intentID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid deposit id")
			return
		}

		var intent payments.DepositIntent
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			intent, err = payments.GetDepositIntentByID(ctx, tx, intentID)
			return err
		})
		if errors.Is(err, payments.ErrDepositIntentNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "deposit not found")
			return
		}
		if err != nil {
			logger.Error("get_deposit_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load deposit")
			return
		}
		// player_self_scope's own RLS already filtered this SELECT to the
		// caller's own rows at the database level - this equality check is
		// belt-and-braces defense in depth, not the actual isolation
		// mechanism, matching the pattern used throughout this codebase's
		// financial RLS policies.
		if intent.PlayerAccountID != playerAccountID {
			apierror.Write(w, requestID, apierror.CodeNotFound, "deposit not found")
			return
		}
		writeJSON(w, http.StatusOK, toDepositIntentResponse(intent))
	}
}

// newListDepositsHandler lists the player's own deposit intents.
func newListDepositsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}

		var resp []depositIntentResponse
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			intents, err := payments.ListDepositIntentsForPlayer(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			resp = make([]depositIntentResponse, 0, len(intents))
			for _, in := range intents {
				resp = append(resp, toDepositIntentResponse(in))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_deposits_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list deposits")
			return
		}
		if resp == nil {
			resp = []depositIntentResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// newPaymentWebhookHandler receives a provider callback and dispatches it
// via PaymentOrchestrator.ReceiveCallback. There is no bearer-token
// middleware on this route - a provider webhook is not an authenticated
// platform principal. Tenant binding is per docs/decisions/0022 §3 as
// amended 2026-09-26 (PAY-WH-TENANT-1, ADR 0090 item 3): the tenant slug in
// the URL is only a LOOKUP HINT, selecting one candidate credential, which
// must then verify a signature whose input includes the route-resolved
// tenant_id/provider_id - never any field inside the body. Every
// pre-verification failure (unknown tenant, inactive tenant, bad
// provider_id, unregistered/unconfigured provider, no resolver, no
// credential, bad signature, key material) gets the IDENTICAL 401
// "callback rejected" response, so an unauthenticated caller can never
// enumerate which of those is true (design §3.2).
func newPaymentWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.PaymentOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "payment webhooks are not enabled on this deployment")
			return
		}

		tenantSlug := r.PathValue("tenantSlug")
		providerID := r.PathValue("providerID")
		if tenantSlug == "" || providerID == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "tenant slug and provider id are required")
			return
		}

		// Step 1: provider_id charset, BEFORE any tenant/DB work (ruling 5).
		if !payments.ValidProviderIDFormat(providerID) {
			logCallbackAuthFailure(logger, r, requestID, payments.ReasonProviderInvalid, nil, providerID, false, "", "", 0)
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}

		// Step 2: body size limit, STILL before any tenant/DB work
		// (security review P2-1/code review F1/architect PW-1, ruling 5).
		// A too-large or unreadable body used to be checked AFTER the
		// tenant lookup below, which made it a distinguishable 400 for an
		// active, resolvable tenant slug versus the uniform 401 an
		// unknown/suspended slug got for the identical oversized body -
		// exactly the tenant-enumeration oracle this contract exists to
		// remove. It is now folded into the SAME uniform 401 family, with
		// no tenant lookup performed first: the response is now byte-
		// identical whether the tenant slug is known or not (T9).
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
		if err != nil {
			logCallbackAuthFailure(logger, r, requestID, payments.ReasonBodyTooLarge, nil, providerID, true, "", "", 0)
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}
		if len(body) > maxWebhookBodyBytes {
			logCallbackAuthFailure(logger, r, requestID, payments.ReasonBodyTooLarge, nil, providerID, true, "", "", len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}

		// Step 3: header format validation, STILL before any tenant/DB
		// work (ruling 5) - ReceiveCallback re-validates this itself too
		// (it must be self-sufficient for tests that call it directly),
		// but failing here first avoids an unnecessary tenant lookup and
		// WithTenant round trip for the common "no headers at all" case.
		_, _, reason, ok := payments.ParseWebhookAuthHeaders(r.Header)
		if !ok {
			logCallbackAuthFailure(logger, r, requestID, reason, nil, providerID, true, "", "", len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}

		// Step 4/5: tenant lookup and active check. A bad slug and a
		// suspended tenant get the SAME uniform 401 as every other auth
		// failure (folded into ErrCallbackAuthFailed's reason set), not a
		// distinguishable 404 - closing the residual enumeration gap the
		// earlier NotFound-shaped response still had relative to the rest
		// of this contract.
		t, err := identity.GetTenantBySlug(r.Context(), deps.DB, tenantSlug)
		if errors.Is(err, identity.ErrNotFound) {
			logCallbackAuthFailure(logger, r, requestID, payments.ReasonTenantUnknown, nil, providerID, true, "", "", len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}
		if err != nil {
			logger.Error("payment_webhook_tenant_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}
		if t.Status != "active" {
			logCallbackAuthFailure(logger, r, requestID, payments.ReasonTenantInactive, &t.ID, providerID, true, "", "", len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}

		var result payments.ReceiveCallbackResult
		err = deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = deps.PaymentOrchestrator.ReceiveCallback(ctx, tx, t.ID, providerID, payments.InboundCallback{Header: r.Header, Body: body})
			return err
		})

		var authErr *payments.CallbackAuthError
		if errors.As(err, &authErr) {
			logCallbackAuthFailure(logger, r, requestID, authErr.Reason, &t.ID, providerID, true, authErr.KeyID, authErr.CredentialFingerprint, len(body))
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
			return
		}
		if errors.Is(err, payments.ErrCallbackPayloadMismatch) {
			// Stage 10 F-7 remediation: a provider reference already posted,
			// redelivered with a different payload (e.g. a reversal
			// reference naming a different deposit). Nothing was posted.
			// Integrity alert + 409; the body never echoes references or
			// amounts. This is a VERIFIED-caller outcome (S-3), so it keeps
			// its own audit trail and is not part of the pre-verification
			// auth-failure contract above.
			logger.Error("payment_webhook_integrity_alert_payload_mismatch", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
			return
		}
		if errors.Is(err, payments.ErrDepositAlreadyReversed) {
			// Stage 10.1 PAY-REV-1 (ADR 0090): a distinct-reference
			// reversal callback naming an already-reversed deposit. Nothing
			// was posted. The alert's fields are DELIBERATELY restricted to
			// provider_id/tenant_id/request_id (security review) - never
			// amounts, references or account ids: a second PSP reversal can
			// be a genuine real-world event (e.g. a refund plus a later
			// chargeback) that operations and PSP reconciliation must see,
			// but this log line itself carries no financial detail.
			logger.Error("payment_webhook_integrity_alert_deposit_already_reversed",
				"provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID)
			// The denial audit is written in a SEPARATE, freshly-opened
			// tenant-scoped transaction: WithTenant above already rolled the
			// failed one back because ReceiveCallback returned a non-nil
			// error, so nothing recorded on that transaction would ever
			// commit (ADR 0088 §4.7's pattern, mirrored from the sportsbook
			// settlement handler's identical ErrSettlementIntegrity
			// treatment). A failure to write this audit record never blocks
			// the 409 - the alert log above already fired.
			//
			// ledger-finance P2-B / security P2-2 / code review F2: err is
			// always a *payments.DepositAlreadyReversedError on this path
			// (both the S4 and the ledger-backstop path construct one) -
			// the detail it carries, not the allow-listed alert above,
			// gives operations/PSP reconciliation something to act on.
			var detail *payments.DepositAlreadyReversedError
			if errors.As(err, &detail) {
				if auditErr := deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
					return payments.RecordDepositReversalRejection(ctx, tx, t.ID, providerID, requestID, trustedProxyClientIP(r, deps.TrustedProxyCount), detail)
				}); auditErr != nil {
					logger.Error("payment_webhook_reversal_rejection_audit_failed", "error", auditErr, "provider_id", providerID)
				}
			} else {
				logger.Error("payment_webhook_reversal_rejection_audit_failed",
					"error", "ErrDepositAlreadyReversed without a DepositAlreadyReversedError detail", "provider_id", providerID)
			}
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
			return
		}
		if errors.Is(err, payments.ErrCallbackProviderMismatch) {
			// Security review P3 (S-5, optional): err's own text embeds the
			// callback's claimed amount/asset and the deposit's real ones
			// (see receiveDepositReversalCallback/postDepositSuccess's own
			// comments) - never pass it to the logger.
			logger.Error("payment_webhook_provider_mismatch", "provider_id", providerID, "tenant_id", t.ID.String())
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
			return
		}
		if errors.Is(err, payments.ErrDepositIntentNotFound) {
			// Only reachable by a caller who already passed verification -
			// see this handler's own doc comment: NOT another enumeration
			// oracle.
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
			return
		}
		if errors.Is(err, payments.ErrDepositReversalIntegrity) {
			// ledger-finance P3-5 (Stage 10.1 review): a data-corruption
			// case - the deposit_intents row a reversal callback resolved
			// names a ledger_transactions id that either does not exist for
			// this tenant, or is not itself a 'deposit'. This is distinct
			// from every other denial above (never a legitimate late/
			// duplicate reversal, never routed to the tombstone branch) and
			// deserves its OWN named alert rather than folding silently
			// into the generic "payment_webhook_failed" 500 line below -
			// err's own text names the deposit_intent and ledger
			// transaction ids, which are already-verified, tenant-scoped
			// identifiers (not payload content), so it is safe to log here.
			logger.Error("payment_webhook_integrity_alert_reversal_link", "error", err, "provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}
		if errors.Is(err, payments.ErrCallbackMalformedBody) {
			// Security review PW-2/P3-5 (Stage 10.1): a VERIFIED callback
			// (the sender proved knowledge of the shared credential) whose
			// body is structurally malformed. Reachable only after
			// signature verification succeeded, so this is NOT part of the
			// pre-verification auth-failure contract and is not an
			// enumeration oracle - logged without the error text/body
			// content regardless, since a verified sender's malformed
			// payload is still never guaranteed free of accidental
			// sensitive content.
			logger.Warn("payment_webhook_malformed_body_after_verification", "provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID)
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
			return
		}
		if err != nil {
			logger.Error("payment_webhook_failed", "error", err, "provider_id", providerID)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"deposit_intent_id": result.DepositIntentID.String(),
			"status":            string(result.Status),
			"tombstoned":        result.Tombstoned,
		})
	}
}
