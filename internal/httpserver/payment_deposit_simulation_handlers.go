package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Stage 9.3: there is no way to complete a mock deposit against a running
// platform-api process through the real HTTP API alone. A deposit only
// reaches 'succeeded' via POST /v1/webhooks/payments/{tenantSlug}/
// {providerID} (deposit_handlers.go), which requires a validly-signed
// callback body - and *payments.MockProvider's HMAC signing secret is
// generated in-process at construction and never exposed via any API or
// persisted (mock.go's own doc comment: this is deliberate, unrecoverable-
// from-outside-the-process behavior standing in for a genuinely
// independent, credentialed provider, and must not change). The existing
// Go integration test suite completes a mock deposit by holding an
// in-process reference to the same *MockProvider instance and calling its
// CallbackPayload helper directly - a seam that does not exist for a real,
// standalone HTTP-only staging process.
//
// This file is the payments-domain twin of casino_play_handlers.go, built
// to the identical precedent: newSimulateDepositCallbackHandler asks the
// tenant's own registered *payments.MockProvider to mint a correctly-
// signed callback payload (provider.CallbackPayload - the exact helper the
// existing payments test suite already uses) for the AUTHENTICATED
// PLAYER's own, already-created deposit intent, then feeds it through
// Orchestrator.ReceiveCallback - byte-for-byte the SAME parse-verify-post
// pipeline the public webhook uses. Nothing here bypasses HandleCallback's
// signature verification or constructs a CallbackEvent directly; nothing
// in internal/payments' postDepositSuccess/receipt.go evidence-application
// path changes.
//
// CRITICAL TRUST-BOUNDARY WARNING (mirrors casino_play_handlers.go's own,
// independently-reviewed finding exactly): the mock adapter's signature is
// minted by THIS PROCESS on the AUTHENTICATED PLAYER's own behalf, so
// unlike a real provider webhook it authenticates nothing about which
// deposit/amount/outcome is being named. receipt.go's evidence-application
// path/postDepositSuccess correctly trust a signed payload's identifying fields
// when the signer is an independent, credentialed provider - they were
// never designed to be handed a payload whose signer is the platform
// acting on an untrusted caller's instructions. Every field this handler
// feeds into CallbackPayload is therefore sourced from the deposit
// intent's OWN row (loaded and ownership-checked BEFORE any payload is
// built), never from this request's body:
//
//   - provider_reference comes from the intent's own ProviderReference
//     (assigned by attemptDeposit when the mock's synchronous Deposit call
//     returned OutcomePending - orchestrator.go), never a caller-supplied
//     string. A forged or foreign reference could otherwise resolve to a
//     DIFFERENT deposit intent entirely (the same class of finding
//     requireRollbackTargetOwnedByRound closes for casino rollback).
//   - amount/asset_code come from the intent's own Amount/AssetCode,
//     never request-body fields - postDepositSuccess's own
//     ErrCallbackProviderMismatch check is a second, independent backstop
//     against this, but this handler does not rely on that backstop alone.
//   - outcome is hardcoded to OutcomeSucceeded - this endpoint simulates
//     only "the provider eventually settled the deposit", never a decline
//     or ambiguous outcome a caller could otherwise choose to force.
//   - the intent must belong to the AUTHENTICATED caller (resolveOwnDepositIntent
//     below) and must still be DepositIntentPending (requireDepositAwaitingCallback
//     below) - a deposit already succeeded/declined/failed/ambiguous is
//     rejected. This second check matters beyond mere workflow hygiene:
//     ADR 0095 §4.4/T13 (receipt.go's applyResolvedReceiptEvidence) treats a
//     matched-amount Succeeded outcome landing on an already-DECLINED
//     attempt as a genuine second capture and DOES post it (the money is
//     real - a cascaded sibling may have already been charged at another
//     provider); postDepositSuccess's own idempotency short-circuit only
//     fires when the SAME provider_reference already posted. Neither is a
//     guard against the SIMULATE endpoint itself manufacturing a fake
//     second capture for an intent it forced to Declined earlier - so this
//     handler's own pending-only gate is the operative defense that keeps
//     a simulated callback from ever reaching that branch at all, not a
//     redundant one.
//
// Deliberately restricted to *payments.MockProvider (never any other
// PaymentProvider implementation) - see requireMockPaymentProvider's own
// doc comment - AND gated behind Deps.PaymentsMockSettlementEnabled (see
// financial_routes.go), which cmd/platform-api/main.go sets only outside
// production: a mock provider is, by definition, one that says yes to
// everything, so "the provider type is restricted" is not by itself a
// deployment safety gate.
//
// Idempotency: calling this endpoint twice concurrently against the SAME
// still-pending intent is safe by construction, without this handler
// doing any locking of its own - both would build the IDENTICAL signed
// payload (same provider_reference/amount/asset_code/outcome, so the same
// HMAC) and both would reach ledger.Post with the SAME idempotency key
// (the provider reference - postDepositSuccess's own doc comment), whose
// database-level uniqueness is what actually prevents a double posting,
// exactly as payment-orchestration.md §8 requires of every adapter.
// Calling it again SEQUENTIALLY, after the first call already completed,
// is rejected by requireDepositAwaitingCallback (409) rather than silently
// replayed - the deposit's final state is unchanged either way (still
// succeeded exactly once), which is what CLAUDE.md's idempotency
// requirement is actually protecting.
//
// SECURITY-SENSITIVE: this is a new authenticated-player-triggerable
// financial-state-mutating endpoint. Per CLAUDE.md, it requires explicit
// `security` specialist review before being marked complete.

// requireMockPaymentProvider resolves providerID's own registered adapter
// and asserts it is the mock adapter - mirrors requireMockCasinoProvider's
// identical rationale in casino_play_handlers.go: simulated settlement
// only exists because no real PSP integration exists to deliver a genuine
// webhook this stage, and it must never be reachable for a real,
// credentialed PaymentProvider adapter, whose signature would actually
// authenticate an independent third party rather than this same process
// acting on the player's behalf. A tenant somehow routed to a non-mock
// adapter gets CodeUnavailable, not a panic or a silently-wrong
// simulation.
func requireMockPaymentProvider(deps Deps, providerID string) (*payments.MockProvider, bool) {
	provider, ok := deps.PaymentOrchestrator.Provider(providerID)
	if !ok {
		return nil, false
	}
	mock, ok := provider.(*payments.MockProvider)
	return mock, ok
}

// resolveOwnDepositIntent loads depositID and verifies it genuinely
// belongs to playerAccountID under the caller's own tenant scope.
// deposit_intents' player_self_scope RLS policy (migration 0025) is
// SELECT-only by design, so - exactly like newInitiateDepositHandler - this
// handler runs under db.Pool.WithTenant, not WithPlayerScope, which means
// this explicit ownership check is what actually authorizes anything here,
// not row-level security on this path. A mismatch returns
// payments.ErrDepositIntentNotFound (never a distinguishable "wrong
// player" error) so a caller cannot enumerate another player's deposit ids
// by status code alone - mirrors resolvePlayerOwnedSession's identical
// rationale in casino_play_handlers.go.
func resolveOwnDepositIntent(ctx context.Context, tx pgx.Tx, depositID, playerAccountID uuid.UUID) (payments.DepositIntent, error) {
	intent, err := payments.GetDepositIntentByID(ctx, tx, depositID)
	if err != nil {
		return payments.DepositIntent{}, err
	}
	if intent.PlayerAccountID != playerAccountID {
		return payments.DepositIntent{}, payments.ErrDepositIntentNotFound
	}
	return intent, nil
}

// requireDepositAwaitingCallback rejects any intent not currently
// DepositIntentPending - see this file's own doc comment for why this is
// the operative safety check (not merely workflow hygiene) against
// resurrecting an already-declined/failed/succeeded/ambiguous intent into
// a second, spurious ledger credit.
func requireDepositAwaitingCallback(intent payments.DepositIntent) error {
	if intent.Status != payments.DepositIntentPending {
		return &apierror.Error{Code: apierror.CodeConflict, Message: "deposit is not awaiting a provider callback"}
	}
	if intent.ProviderID == nil || intent.ProviderReference == nil {
		// Defensive only: every DepositIntentPending row is written with
		// both fields set in the same statement (attemptDeposit,
		// orchestrator.go) - this branch should be unreachable.
		return &apierror.Error{Code: apierror.CodeConflict, Message: "deposit is not awaiting a provider callback"}
	}
	return nil
}

// recordDepositSimulationAudit adds a PLAYER-attributed audit record
// alongside whatever ActorSystem record the receipt path itself already
// wrote (ApplyReceiptEvidence/receiveCallbackViaReceiptPath, orchestrator.go/
// receipt.go - correct and unchanged for their real intended caller, a
// provider webhook, and not touched here) - mirrors
// recordCasinoPlaySimulationAudit's identical
// rationale: without this, a player-triggered financial mutation is
// indistinguishable in audit_log from a genuine provider callback, which
// would make an abuse investigation of this simulation seam impossible
// (CLAUDE.md's "every mutating administrative/financial action writes an
// audit record (actor, tenant, entity, ..., IP, ...)").
//
// Security-review addition (Stage 9.3): this record also carries the
// caller's IP/User-Agent/request id, which recordCasinoPlaySimulationAudit
// does NOT. That is deliberate rather than gratuitous divergence - the
// entire justification for writing this row at all is "so an abuse
// investigation of this simulation seam is possible later", and the two
// questions such an investigation actually asks (WHICH client drove it,
// and which request log line corresponds to it) are unanswerable from
// actor_id alone when one player account is shared across a staging test
// harness, a browser and an automation script. Errors from audit.Record
// are deliberately not surfaced to the caller, but this is NOT
// "best-effort" in the sense of being droppable: it runs inside the same
// transaction as the financial effect, so a failed insert aborts that
// transaction and the deposit does not settle either. An unaudited
// simulated settlement is therefore structurally impossible.
func recordDepositSimulationAudit(ctx context.Context, tx pgx.Tx, r *http.Request, tenantID, playerAccountID, depositIntentID uuid.UUID, providerID, providerReference string, result payments.ReceiveCallbackResult) {
	outcome := audit.OutcomeSuccess
	if result.Status != payments.DepositIntentSucceeded {
		outcome = audit.OutcomeDenied
	}
	metadata := map[string]any{"provider_id": providerID, "provider_reference": providerReference, "status": string(result.Status)}
	if result.LedgerTransactionID != nil {
		metadata["ledger_transaction_id"] = result.LedgerTransactionID.String()
	}
	_ = audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorPlayer, ActorID: playerAccountID,
		Action: "payments_simulation.deposit_callback", TargetType: "deposit_intent", TargetID: depositIntentID.String(),
		Outcome: outcome, IPAddress: clientIP(r), UserAgent: r.UserAgent(),
		RequestID: observability.RequestIDFromContext(ctx), Metadata: metadata,
	})
}

// writeDepositCallbackError maps ReceiveCallback's sentinel errors to
// their player-facing HTTP response via the SAME shared mapper
// newPaymentWebhookHandler uses (deposit_handlers.go/
// payment_callback_errors.go, backend review finding 1) - this is the SAME
// pipeline, so the SAME error set can occur, even though every field this
// handler feeds it is drawn from the intent's own row and should not
// normally trigger any of them. A *payments.CallbackAuthError here means
// the mock/resolver wiring is itself broken (502/503, never 401 - the
// caller is an already-authenticated player, not an unauthenticated third
// party).
func writeDepositCallbackError(w http.ResponseWriter, requestID string, logger interface {
	Error(string, ...any)
}, err error) {
	var authErr *payments.CallbackAuthError
	if errors.As(err, &authErr) {
		logger.Error("payment_simulation_auth_failed", "reason", string(authErr.Reason))
		code, msg := mapReceiveCallbackError(err, callbackRouteSimulate)
		apierror.Write(w, requestID, code, msg)
		return
	}
	if errors.Is(err, payments.ErrDepositIntentNotFound) {
		code, msg := mapReceiveCallbackError(err, callbackRouteSimulate)
		apierror.Write(w, requestID, code, msg)
		return
	}
	if errors.Is(err, payments.ErrCallbackPayloadMismatch) {
		logger.Error("payment_simulation_integrity_alert_payload_mismatch", "error", err)
		code, msg := mapReceiveCallbackError(err, callbackRouteSimulate)
		apierror.Write(w, requestID, code, msg)
		return
	}
	if errors.Is(err, payments.ErrDepositAlreadyReversed) {
		code, msg := mapReceiveCallbackError(err, callbackRouteSimulate)
		apierror.Write(w, requestID, code, msg)
		return
	}
	if errors.Is(err, payments.ErrCallbackProviderMismatch) {
		logger.Error("payment_simulation_provider_mismatch", "error", err)
		code, msg := mapReceiveCallbackError(err, callbackRouteSimulate)
		apierror.Write(w, requestID, code, msg)
		return
	}
	logger.Error("payment_simulation_failed", "error", err)
	code, msg := mapReceiveCallbackError(err, callbackRouteSimulate)
	apierror.Write(w, requestID, code, msg)
}

// depositSimulationBetweenPhasesHook is a TEST SEAM (nil in production;
// set only by this package's own tests): it runs between step 2
// (VerifyCallback) and step 3 (the domain transaction) of the deposit
// simulation, so a test can change the intent in the gap step 3 must
// re-validate (TestSimulateDepositCallback_IntentRevalidatedInDomainTx).
var depositSimulationBetweenPhasesHook func(depositID uuid.UUID)

// newSimulateDepositCallbackHandler simulates the provider-side webhook
// delivery for the caller's own, still-pending deposit intent - see this
// file's own doc comment for the full trust-boundary rationale.
func newSimulateDepositCallbackHandler(deps Deps) http.HandlerFunc {
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
		depositID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid deposit id")
			return
		}

		// ADR 0094 §4.1, three steps: (1) a READ ONLY transaction reads the
		// caller's own intent and builds the payload; (2) VerifyCallback
		// runs with NO transaction held; (3) the domain transaction
		// RE-VALIDATES the intent (a TOCTOU gap otherwise: it could have
		// settled between (1) and (3)), then redeems the verified callback
		// and writes the simulation audit record.
		var (
			result            payments.ReceiveCallbackResult
			providerID        string
			providerReference string
			inbound           payments.InboundCallback
		)
		err = deps.DB.WithTenantReadOnly(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			intent, err := resolveOwnDepositIntent(ctx, tx, depositID, playerAccountID)
			if err != nil {
				return err
			}
			if err := requireDepositAwaitingCallback(intent); err != nil {
				return err
			}
			providerID = *intent.ProviderID
			providerReference = *intent.ProviderReference

			mock, ok := requireMockPaymentProvider(deps, providerID)
			if !ok {
				return &apierror.Error{Code: apierror.CodeUnavailable, Message: "simulated settlement is only available for the mock provider"}
			}

			// The tenant signed for is ALWAYS tc.TenantID - the authenticated
			// JWT's own tenant, resolved before this point, and never a
			// request-body field (design §6: "no request field can name
			// one"). The signed bytes/headers never leave this process; the
			// response below carries status only, so this route cannot mint
			// a callback replayable at another tenant's webhook URL.
			inbound = mock.CallbackPayload(tc.TenantID, payments.CallbackEventDeposit, providerReference, "",
				payments.OutcomeSucceeded, intent.Amount, intent.AssetCode, "", false)
			return nil
		})
		var verified *payments.VerifiedCallback
		if err == nil {
			verified, err = deps.PaymentOrchestrator.VerifyCallback(r.Context(), deps.DB, tc.TenantID, providerID, inbound)
		}
		if err == nil && depositSimulationBetweenPhasesHook != nil {
			depositSimulationBetweenPhasesHook(depositID)
		}
		if err == nil {
			err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				intent, err := resolveOwnDepositIntent(ctx, tx, depositID, playerAccountID)
				if err != nil {
					return err
				}
				if err := requireDepositAwaitingCallback(intent); err != nil {
					return err
				}
				if intent.ProviderID == nil || *intent.ProviderID != providerID ||
					intent.ProviderReference == nil || *intent.ProviderReference != providerReference {
					return &apierror.Error{Code: apierror.CodeConflict, Message: "deposit changed during simulation"}
				}
				result, err = deps.PaymentOrchestrator.ReceiveVerifiedCallback(ctx, tx, tc.TenantID, providerID, verified)
				if err != nil {
					return err
				}
				recordDepositSimulationAudit(ctx, tx, r, tc.TenantID, playerAccountID, intent.ID, providerID, providerReference, result)
				return nil
			})
		}
		if errors.Is(err, payments.ErrDepositIntentNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "deposit not found")
			return
		}
		var apiErr *apierror.Error
		if errors.As(err, &apiErr) {
			apierror.Write(w, requestID, apiErr.Code, apiErr.Message)
			return
		}
		if err != nil {
			writeDepositCallbackError(w, requestID, logger, err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"deposit_intent_id": result.DepositIntentID.String(),
			"status":            string(result.Status),
			"tombstoned":        result.Tombstoned,
		})
	}
}
