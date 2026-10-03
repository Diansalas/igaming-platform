package httpserver

import (
	"context"
	"errors"
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
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// maxWebhookBodyBytes bounds an inbound provider callback body - a
// webhook has no session/auth to rate-limit by, so a caller-controlled
// unbounded body is a resource-exhaustion vector in a way an
// authenticated JSON request already isn't (decodeJSON's
// maxRequestBodyBytes covers those).
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// paymentWebhookRoute is the payments domain's parameter set for the shared
// webhook preamble (webhook_preamble.go). The event names and body limit
// are exactly Stage 10.1's; the scheme is the registered adapter's own
// (Stage 10.3 W1a) - for the mock, the unchanged payments MOCK scheme.
var paymentWebhookRoute = webhookRoute{
	schemeFor: func(deps Deps, providerID string) (webhookauth.VerificationScheme, bool) {
		if deps.PaymentOrchestrator == nil {
			return nil, false
		}
		return deps.PaymentOrchestrator.WebhookScheme(providerID)
	},
	maxBody:                 maxWebhookBodyBytes,
	domain:                  domainPayments,
	authFailedEvent:         "payment_webhook_auth_failed",
	tenantLookupFailedEvent: "payment_webhook_tenant_lookup_failed",
}

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

// toDepositAttemptResponse is toDepositIntentResponse's twin for the
// InitiateDepositAttempt (ADR 0095 two-phase) cutover path: the response
// shape is UNCHANGED (the handler contract this task preserves) but
// RedirectURL/HostedFieldToken now come from InitiateDepositAttemptResult
// itself (this synchronous call's own phase B DepositResult), never from
// the intent row, since the new path does not persist them onto
// deposit_intents (drive.go/deposit_v2.go's own doc comments).
func toDepositAttemptResponse(res payments.InitiateDepositAttemptResult) depositIntentResponse {
	resp := toDepositIntentResponse(res.Intent)
	resp.RedirectURL = res.RedirectURL
	resp.HostedFieldToken = res.HostedFieldToken
	return resp
}

// newInitiateDepositHandler resolves the player's own wallet (creating it
// on first use for this asset) and drives
// PaymentOrchestrator.InitiateDepositAttempt ([deleted by E2] this
// comment used to name InitiateDeposit, now deleted). Per payment-
// orchestration.md §3, every identifying field (tenant, brand, player,
// wallet) is resolved server-side from the authenticated session - the
// request body supplies only asset_code, amount, payment_method, and the
// client's own idempotency key.
//
// Runs under db.Pool.WithTenant, NOT WithPlayerScope - see
// newGetWalletHandler's identical rationale in wallet_handlers.go: this
// handler writes (wallet.GetOrCreate, InitiateDepositAttempt's
// deposit_intents insert and ledger posting), and deposit_intents'
// player_self_scope policy (migration 0025) is SELECT-only by design.
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

		if deps.PaymentsOutboundCredentials == nil {
			// Fail closed (mirrors LaunchGame's identical nil-resolver
			// convention) - InitiateDepositAttempt's own gate would also
			// refuse the call, but refusing here avoids opening the
			// wallet-resolution transaction for a request that can never
			// succeed.
			logger.Error("initiate_deposit_no_outbound_credential_resolver")
			apierror.Write(w, requestID, apierror.CodeUnavailable, "deposits are not enabled on this deployment")
			return
		}

		// ADR 0095 §15.1-style split (PRH-I1 deposit cutover): only the
		// identity/wallet resolution below still needs its own short
		// tenant-scoped transaction (a cheap, no-vendor-I/O read/get-or-
		// create) - InitiateDepositAttempt owns its own transaction
		// boundaries (phase A commits before any provider call; phase B
		// runs with no transaction held), so it is no longer called from
		// inside a WithTenant callback.
		var brandID, walletID uuid.UUID
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			wl, err := wallet.GetOrCreate(ctx, tx, tc.TenantID, account.BrandID, playerAccountID, req.AssetCode)
			if err != nil {
				return err
			}
			brandID, walletID = account.BrandID, wl.ID
			return nil
		})
		var result payments.InitiateDepositAttemptResult
		if err == nil {
			result, err = deps.PaymentOrchestrator.InitiateDepositAttempt(
				r.Context(), deps.DB, payments.KYCEnforcementDepositGate{}, deps.PaymentsOutboundCredentials,
				payments.InitiateDepositParams{
					Scope: payments.DepositScope{
						TenantID: tc.TenantID, BrandID: brandID, PlayerAccountID: playerAccountID, WalletID: walletID,
					},
					AssetCode: req.AssetCode, Amount: req.Amount, PaymentMethod: req.PaymentMethod, IdempotencyKey: req.IdempotencyKey,
				},
			)
		}
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
		writeJSON(w, http.StatusCreated, toDepositAttemptResponse(result))
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

		// RL-F4 (security review Low "leftover"): redact BEFORE the
		// orchestrator-nil check below, which is itself reachable with
		// arbitrary attacker-chosen path segments.
		markWebhookRouteForLogging(r)

		if deps.PaymentOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "payment webhooks are not enabled on this deployment")
			return
		}

		// retries429 is the adapter's declared retry semantics (ADR 0097
		// §6.3/§20 AC6), shared by A3 (security review C4: a registered
		// adapter's declaration is a static fact, known even before
		// verification) and B1 below.
		retries429 := func(providerID string) (allows, declared bool) {
			if deps.PaymentOrchestrator == nil {
				return true, false
			}
			sem, ok := deps.PaymentOrchestrator.WebhookRetrySemantics(providerID)
			if !ok {
				return true, false
			}
			return sem.Retries429, true
		}

		// ADR 0097 A2/A3/A4a (PAYWH-RL-1): the FIRST thing any webhook route
		// does - no DB, no body read (ORD-1/ORD-2). A nil admission runtime
		// (disabled) always admits.
		releaseInflight, admitted := deps.webhookAdmission.admitPreAuth(w, r, domainPayments, deps.TrustedProxyCount, func(id string) bool {
			if deps.PaymentOrchestrator == nil {
				return false
			}
			_, ok := deps.PaymentOrchestrator.WebhookScheme(id)
			return ok
		}, retries429)
		if !admitted {
			return
		}
		defer releaseInflight()

		// Steps 1-5 (provider_id charset, bounded body read, header format
		// - all before any tenant/DB work, ruling 5 / security review
		// P2-1 - then the platform-wide tenant lookup and active check) are
		// the shared webhook preamble every webhook domain uses (Stage
		// 10.2, ADR 0091, architect R2 / ruling J4). Every rejection there
		// is the IDENTICAL 401 "callback rejected" with one allow-listed
		// payment_webhook_auth_failed line - an oversized body or a bad
		// slug is never a distinguishable 400/404 (T9). ADR 0097 A4b gates
		// the platform-wide tenant lookup this now performs.
		t, providerID, body, ok := webhookPreamble(w, r, deps, paymentWebhookRoute)
		if !ok {
			return
		}

		// ADR 0094 §4.1: phase 1 (verification) holds NO transaction - its
		// reads run in short READ ONLY transactions that commit before any
		// secret-store fetch; the domain transaction opens only after it
		// succeeded, and re-checks the verified handle first. ADR 0097
		// §5.3: VerifyCallback's own WithTenantReadOnly calls are gated by
		// the SAME A4b bulkhead GetTenantBySlug used, via gatedReader in
		// place of deps.DB when admission is enabled.
		var reader webhookauth.TenantReader = deps.DB
		if deps.webhookAdmission != nil {
			tenantKey, providerKey := deps.webhookAdmission.preAuthKeys(t.Slug, providerID, func(id string) bool {
				_, ok := deps.PaymentOrchestrator.WebhookScheme(id)
				return ok
			})
			reader = deps.webhookAdmission.newGatedReader(deps.DB, domainPayments, tenantKey, providerKey)
		}
		var result payments.ReceiveCallbackResult
		verified, err := deps.PaymentOrchestrator.VerifyCallback(r.Context(), reader, t.ID, providerID, payments.InboundCallback{Header: r.Header, Body: body})
		if errors.Is(err, errDBGateUnavailable) {
			// Security review C2 of PRH-I4: every DB-gate 503 gets
			// Retry-After and a db_gate log line - this is payments'
			// FIRST read (ProviderAcceptsWebhook), which already propagated
			// the raw sentinel unwrapped even before the C1 fix; the
			// SECOND read (credential resolution) is handled below via the
			// wrapped AuthError branch.
			deps.webhookAdmission.writeAdmissionUnavailableAuthError(w, r, domainPayments, t.ID, providerID)
			return
		}
		// ADR 0097 B1/B2 (ORD-3/ORD-4): admitted ONLY off the just-verified
		// (tenant_id, provider_id) - never off preKey/URL values - and
		// STRICTLY before deps.DB.WithTenant opens the domain transaction.
		// The B2 release is held (ledger-finance C1) through the
		// reversal-rejection-audit transaction below, released exactly once
		// at the end of this handler.
		var releaseDomainTx func()
		if err == nil {
			var admittedVerified bool
			releaseDomainTx, admittedVerified = deps.webhookAdmission.admitVerified(w, r, domainPayments, t.ID, providerID, retries429)
			if !admittedVerified {
				return
			}
			defer func() {
				if releaseDomainTx != nil {
					releaseDomainTx()
				}
			}()
		}
		if err == nil {
			err = deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result, err = deps.PaymentOrchestrator.ReceiveVerifiedCallback(ctx, tx, t.ID, providerID, verified)
				return err
			})
		}

		var authErr *payments.CallbackAuthError
		if errors.As(err, &authErr) {
			// Security review C1 of PRH-I4 (HIGH): a DB-gate rejection
			// DURING credential resolution (the second read inside
			// VerifyCallback) surfaces as an AuthError with Reason ==
			// ReasonAdmissionUnavailable, not a real authentication
			// failure - it must answer 503+Retry-After (C2: with its own
			// db_gate log line), never the uniform 401.
			if authErr.Reason == webhookauth.ReasonAdmissionUnavailable {
				deps.webhookAdmission.writeAdmissionUnavailableAuthError(w, r, domainPayments, t.ID, providerID)
				return
			}
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
			// (see applyReversalReceiptEvidence/postDepositSuccess in
			// internal/payments' own comments) - never pass it to the logger.
			logger.Error("payment_webhook_provider_mismatch", "provider_id", providerID, "tenant_id", t.ID.String())
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
		if errors.Is(err, payments.ErrProviderReferenceInvalid) {
			// PROVIDER-REF-BOUND-1: verified callback, reference outside
			// the platform bound. Deterministic 400; nothing read or
			// written. Logged with length + hash prefix only.
			logProviderReferenceRejected(logger, "payment_webhook_provider_reference_rejected", err, providerID, t.ID.String(), requestID)
			code, msg := mapReceiveCallbackError(err, callbackRoutePublicWebhook)
			apierror.Write(w, requestID, code, msg)
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
		if errors.Is(err, payments.ErrDeferredReceiptCapExceeded) {
			// ADR 0095 §6.1 step 5 / S95-C2(i): the unapplied-receipt cap
			// for (tenant, provider) is already at or past the configured
			// limit. Nothing is stored - a retryable 503, never a 200
			// without storing and never a 404. A real PSP's own retry
			// (LF-C1, ADR 0097 §6.3) redelivers this event later; the P1
			// alert lets operations catch a backlog before the vendor's
			// redelivery window expires.
			logger.Error("payment_webhook_deferred_receipt_cap_exceeded", "provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID)
			w.Header().Set("Retry-After", "1")
			apierror.Write(w, requestID, apierror.CodeUnavailable, "service temporarily unavailable; retry later")
			return
		}
		if err != nil {
			logger.Error("payment_webhook_failed", "error", err, "provider_id", providerID)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}

		// ADR 0095 §6.2 / S95-C4: every 200 disposition (applied,
		// duplicate_effect, deferred_unresolved, anomaly) returns a
		// byte-identical body, apart from the request id - the disposition
		// itself is recorded only in the receipt, the audit record and
		// metrics, never echoed here, so a verified sender can never learn
		// whether a reference exists, is already applied, or is still
		// unresolved. This also closes today's 404 for `deferred_unresolved`
		// (old->new: `ErrDepositIntentNotFound` used to map to 404 for an
		// unresolved deposit callback; that sentinel is no longer reachable
		// from ReceiveVerifiedCallback's deposit/reversal branches at all,
		// which now defer instead of erroring - see receipt.go
		// ApplyReceiptEvidence). result.Disposition is logged at Info
		// (never returned to the caller) purely for operator observability.
		logger.Info("payment_webhook_applied", "provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID, "disposition", string(result.Disposition))
		writeWebhookReceivedResponse(w, requestID)
	}
}

// webhookReceivedResponse is the ADR 0095 §6.2/S95-C4 uniform payments
// webhook success body: identical for every disposition apart from
// request_id. Never carries deposit_intent_id, status, tombstoned or any
// other disposition-revealing field.
type webhookReceivedResponse struct {
	RequestID string `json:"request_id"`
	Received  bool   `json:"received"`
}

func writeWebhookReceivedResponse(w http.ResponseWriter, requestID string) {
	writeJSON(w, http.StatusOK, webhookReceivedResponse{RequestID: requestID, Received: true})
}
