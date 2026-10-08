package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
	"github.com/Diansalas/igaming-platform/internal/wallet"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type requestWithdrawalRequest struct {
	AssetCode      string `json:"asset_code"`
	Amount         int64  `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

type withdrawalRequestResponse struct {
	ID                string `json:"id"`
	AssetCode         string `json:"asset_code"`
	Amount            int64  `json:"amount"`
	State             string `json:"state"`
	RequestedAt       string `json:"requested_at"`
	LedgerTransaction string `json:"hold_ledger_transaction_id,omitempty"`
}

func toWithdrawalRequestResponse(wr withdrawal.WithdrawalRequest) withdrawalRequestResponse {
	resp := withdrawalRequestResponse{
		ID: wr.ID.String(), AssetCode: wr.AssetCode, Amount: wr.Amount,
		State: string(wr.State), RequestedAt: wr.RequestedAt.UTC().Format(rfc3339),
	}
	if wr.HoldLedgerTransactionID != nil {
		resp.LedgerTransaction = wr.HoldLedgerTransactionID.String()
	}
	return resp
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

// newRequestWithdrawalHandler resolves the player's own wallet (never
// accepting a client-supplied wallet id, per withdrawal-state-machine.md
// §7) and calls withdrawal.RequestWithdrawal, which posts the Flow 3 Step
// A hold atomically with creating the request row.
//
// Runs under db.Pool.WithTenant, NOT WithPlayerScope - see
// wallet_handlers.go's newGetWalletHandler for the identical rationale:
// this handler writes (RequestWithdrawal's insert plus ledger.Post), and
// withdrawal_requests' player_self_scope policy (migration 0026) is
// SELECT-only by design.
//
// Deliberately does NOT also call withdrawal.MoveToPendingReview here.
// withdrawal-state-machine.md §1 documents `requested` -> `pending_review`
// as firing "once automated KYC/velocity/risk checks are queued" (owned by
// identity-compliance, not Stage 3B scope), and §7's ARCHITECTURAL
// DECISION is explicit that a request stays player-cancellable for a real
// window "before pending_review resolves" specifically to avoid a
// cancel-vs-approve race. Promoting synchronously on every request would
// collapse that window to zero and silently remove the cancellation
// feature. See newListPendingWithdrawalsHandler for where the promotion
// actually happens.
func newRequestWithdrawalHandler(deps Deps) http.HandlerFunc {
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

		var req requestWithdrawalRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("asset_code", req.AssetCode)
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		if req.Amount <= 0 {
			v.Add("amount", "must be a positive integer (minor units)")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var wr withdrawal.WithdrawalRequest
		// LF-I3-3 (ledger-finance's preferred fix, 2026-09-27): a KYC deny
		// must commit its decision + audit rows in THIS SAME transaction,
		// never roll back and rely on a second, separately-committed
		// transaction (the earlier design, which left a real window where
		// a process crash between the rollback and the fresh commit could
		// lose the denial record - see ADR 0096 §16.2's own disclosure of
		// that gap, now closed). RequestWithdrawal itself already wrote
		// the decision/audit rows before returning *KYCDeniedError - this
		// closure detects that specific error, captures it, and returns
		// nil so WithTenant COMMITS rather than rolling back. Every OTHER
		// error still propagates and rolls back normally.
		var kycDenied *withdrawal.KYCDeniedError
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			wl, err := wallet.GetByPlayerAndAsset(ctx, tx, playerAccountID, req.AssetCode)
			if err != nil {
				return err
			}
			wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
				TenantID: tc.TenantID, BrandID: account.BrandID, PlayerAccountID: playerAccountID, PersonID: account.PersonID, WalletID: wl.ID,
				AssetCode: req.AssetCode, Amount: req.Amount, IdempotencyKey: req.IdempotencyKey,
			})
			if errors.As(err, &kycDenied) {
				return nil
			}
			return err
		})
		if kycDenied != nil {
			// B1 (code review rv-prh-i3-code-review.md): OutcomeUnavailable
			// means EvaluateEnforcement itself could not determine an
			// outcome (a DB failure, a malformed policy row) - a transient
			// server condition, never a real compliance decision. It MUST
			// map to a retryable 503, never the same 409 a genuine
			// pending/failed denial gets, so a brief KYC-store outage is
			// never presented to the player as "you need to verify your
			// identity".
			if kycDenied.Decision.Outcome == kyc.OutcomeUnavailable {
				apierror.Write(w, requestID, apierror.CodeUnavailable, "verification check is temporarily unavailable, please retry")
				return
			}
			// Player-facing surface: status only, never matched_trigger/
			// policy_version/an amount (security condition 8) - a closed
			// enum drawn from the decision's own Outcome.
			apierror.Write(w, requestID, apierror.CodeConflict, "withdrawal requires a passed identity verification: "+string(kycDenied.Decision.Outcome))
			return
		}
		if errors.Is(err, withdrawal.ErrKYCUnavailable) {
			// ErrKYCUnavailable wraps a STRUCTURALLY INVALID call to
			// EvaluateEnforcement (a caller bug - a missing tenant/brand/
			// player/person id), never a business or transient-outage
			// condition - CodeInternal (500), non-retryable, is correct
			// here (distinct from the OutcomeUnavailable 503 branch above).
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to evaluate identity verification requirements")
			return
		}
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
			// H-SEC-11: tenant or brand not active; nothing was created.
			logger.Warn("request_withdrawal_refused_not_active", "request_id", requestID)
			apierror.Write(w, requestID, apierror.CodeTenantOrBrandNotActive, "withdrawals are not available right now")
			return
		}
		if errors.Is(err, wallet.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeValidation, "no wallet for this asset - deposit first")
			return
		}
		if errors.Is(err, withdrawal.ErrInsufficientFunds) {
			apierror.Write(w, requestID, apierror.CodeConflict, "insufficient available balance")
			return
		}
		if errors.Is(err, withdrawal.ErrIdempotencyKeyReused) {
			apierror.Write(w, requestID, apierror.CodeConflict, "idempotency key already used with different parameters")
			return
		}
		if err != nil {
			logger.Error("request_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to request withdrawal")
			return
		}
		writeJSON(w, http.StatusCreated, toWithdrawalRequestResponse(wr))
	}
}

// newListWithdrawalsHandler lists the player's own withdrawal requests.
func newListWithdrawalsHandler(deps Deps) http.HandlerFunc {
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

		var resp []withdrawalRequestResponse
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			requests, err := withdrawal.ListForPlayer(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			resp = make([]withdrawalRequestResponse, 0, len(requests))
			for _, wr := range requests {
				resp = append(resp, toWithdrawalRequestResponse(wr))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_withdrawals_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list withdrawals")
			return
		}
		if resp == nil {
			resp = []withdrawalRequestResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// newGetWithdrawalHandler returns one of the player's own withdrawal
// requests.
func newGetWithdrawalHandler(deps Deps) http.HandlerFunc {
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
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		var wr withdrawal.WithdrawalRequest
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			wr, err = withdrawal.GetByID(ctx, tx, id)
			return err
		})
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if err != nil {
			logger.Error("get_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load withdrawal")
			return
		}
		if wr.PlayerAccountID != playerAccountID {
			// Belt-and-braces on top of player_self_scope's own RLS filter
			// (migration 0026) - see deposit_handlers.go's identical
			// comment for why this is defense in depth, not the mechanism.
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		writeJSON(w, http.StatusOK, toWithdrawalRequestResponse(wr))
	}
}

// --- Staff four-eyes approval queue ---

type staffWithdrawalResponse struct {
	withdrawalRequestResponse
	PlayerAccountID string `json:"player_account_id"`
	// DecimalExponent is populated ONLY by the Stage 5 admin history/detail
	// handlers below (newListAdminWithdrawalsHandler/
	// newGetAdminWithdrawalHandler) - omitempty so the pre-existing
	// pending-queue/submitted-queue handlers, which do not populate it,
	// never emit a misleading `0` (which would tell a client to render
	// every amount as whole units regardless of the asset's real
	// exponent). A ledger-finance review flagged the Back Office
	// withdrawal-approval page rendering raw minor units with no exponent
	// (e.g. "10000 EUR" for what is actually EUR 100.00) as a real
	// financial-clarity defect at an irreversible decision point - this
	// field, plus the frontend formatter that consumes it, closes that.
	DecimalExponent int16 `json:"decimal_exponent,omitempty"`
}

// newListPendingWithdrawalsHandler is the staff review queue - tenant-
// scoped, no player restriction (staff legitimately see every player's
// pending withdrawals in their own tenant, same tenant-wide-staff-access
// pattern already used for admin/players).
//
// Before querying, promotes every `requested` row in this tenant to
// `pending_review` via withdrawal.MoveToPendingReview. This is the actual
// Stage 3B wiring of the `requested` -> `pending_review` transition
// (withdrawal-state-machine.md §1's "automated KYC/velocity/risk checks
// queued", owned by identity-compliance and not implemented this stage -
// see newRequestWithdrawalHandler's comment for why the promotion is
// deliberately NOT done at request time instead): opening the review
// queue is the first point in Stage 3B's implementation at which a
// reviewer could otherwise race a player's cancellation, so it is also
// the natural point at which the documented cancellable window closes.
// Each promotion is independently best-effort (a row already moved by a
// concurrent list call is simply skipped, never an error).
func newListPendingWithdrawalsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var resp []staffWithdrawalResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			requestedIDs, err := tx.Query(ctx, `SELECT id FROM withdrawal_requests WHERE state = 'requested'`)
			if err != nil {
				return err
			}
			var ids []uuid.UUID
			for requestedIDs.Next() {
				var id uuid.UUID
				if err := requestedIDs.Scan(&id); err != nil {
					requestedIDs.Close()
					return err
				}
				ids = append(ids, id)
			}
			if err := requestedIDs.Err(); err != nil {
				return err
			}
			requestedIDs.Close()
			for _, id := range ids {
				if err := withdrawal.MoveToPendingReview(ctx, tx, id); err != nil && !errors.Is(err, withdrawal.ErrStateConflict) {
					return err
				}
			}

			rows, err := tx.Query(ctx,
				`SELECT id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state,
					idempotency_key, provider_id, provider_reference, hold_ledger_transaction_id, release_ledger_transaction_id,
					requested_at, updated_at
				 FROM withdrawal_requests WHERE state = 'pending_review' ORDER BY requested_at ASC`,
			)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var wr withdrawal.WithdrawalRequest
				if err := rows.Scan(
					&wr.ID, &wr.TenantID, &wr.BrandID, &wr.PlayerAccountID, &wr.WalletID, &wr.AssetCode, &wr.Amount, &wr.State,
					&wr.IdempotencyKey, &wr.ProviderID, &wr.ProviderReference, &wr.HoldLedgerTransactionID, &wr.ReleaseLedgerTransactionID,
					&wr.RequestedAt, &wr.UpdatedAt,
				); err != nil {
					return err
				}
				resp = append(resp, staffWithdrawalResponse{
					withdrawalRequestResponse: toWithdrawalRequestResponse(wr),
					PlayerAccountID:           wr.PlayerAccountID.String(),
				})
			}
			return rows.Err()
		})
		if err != nil {
			logger.Error("list_pending_withdrawals_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list pending withdrawals")
			return
		}
		if resp == nil {
			resp = []staffWithdrawalResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- Stage 5 Back Office withdrawal history (read-only) ---

// newListAdminWithdrawalsHandler is the Stage 5 Back Office withdrawal
// history view: tenant-wide (not player-restricted) and across the FULL
// state range the state machine defines (withdrawal.ValidStates), not just
// the pending-review subset newListPendingWithdrawalsHandler serves -
// this is the operator-facing "show me everything" queue that view needs
// to display before drilling into one withdrawal via
// newGetAdminWithdrawalHandler. Paginated per the shared Stage 5
// convention (pagination.go).
//
// Deliberately a pure read: unlike newListPendingWithdrawalsHandler, it
// never calls withdrawal.MoveToPendingReview - a mere history view must
// not itself advance a player's still-cancellable `requested` window
// (withdrawal-state-machine.md §7's documented cancel-vs-approve race
// rationale), and doing so here would let a Back Office table simply
// being open in a browser tab silently close that window for every
// `requested` row in the tenant.
func newListAdminWithdrawalsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var statusFilter *withdrawal.State
		if raw := r.URL.Query().Get("status"); raw != "" {
			s := withdrawal.State(raw)
			if !withdrawal.IsValidState(s) {
				apierror.Write(w, requestID, apierror.CodeValidation, "invalid status filter")
				return
			}
			statusFilter = &s
		}
		p := parsePageParams(r)

		var items []staffWithdrawalResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			requests, count, err := withdrawal.ListForTenant(ctx, tx, statusFilter, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]staffWithdrawalResponse, 0, len(requests))
			// One asset-registry lookup per distinct asset code in this page,
			// not per row - a Back Office withdrawal page realistically spans
			// a small, repeating set of assets.
			exponents := make(map[string]int16)
			for _, wr := range requests {
				exp, ok := exponents[wr.AssetCode]
				if !ok {
					a, err := assetregistry.GetAsset(ctx, tx, wr.AssetCode)
					if err != nil {
						return fmt.Errorf("withdrawal admin list: look up asset %q: %w", wr.AssetCode, err)
					}
					exp = a.DecimalExponent
					exponents[wr.AssetCode] = exp
				}
				items = append(items, staffWithdrawalResponse{
					withdrawalRequestResponse: toWithdrawalRequestResponse(wr),
					PlayerAccountID:           wr.PlayerAccountID.String(),
					DecimalExponent:           exp,
				})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_admin_withdrawals_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list withdrawals")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

// newGetAdminWithdrawalHandler is the Stage 5 Back Office withdrawal
// detail view: tenant-wide (any withdrawal belonging to the caller's own
// tenant, not just the caller's own - staff legitimately inspect any
// player's withdrawal in their tenant before acting on it via the
// existing approve/reject/submit/resolve endpoints). A pure read: takes
// no row lock and calls no state-transition function.
func newGetAdminWithdrawalHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		var wr withdrawal.WithdrawalRequest
		var decimalExponent int16
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			wr, err = withdrawal.GetByID(ctx, tx, id)
			if err != nil {
				return err
			}
			a, err := assetregistry.GetAsset(ctx, tx, wr.AssetCode)
			if err != nil {
				return fmt.Errorf("withdrawal admin detail: look up asset %q: %w", wr.AssetCode, err)
			}
			decimalExponent = a.DecimalExponent
			return nil
		})
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if err != nil {
			logger.Error("get_admin_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load withdrawal")
			return
		}
		if wr.TenantID != tc.TenantID {
			// Belt-and-braces on top of tenant_staff_scope's own RLS filter
			// (migration 0026), which already makes this branch practically
			// unreachable - mirrors newGetWithdrawalHandler's identical
			// player-ownership check, applied here at the tenant boundary:
			// never let a caller distinguish "exists in another tenant" from
			// "does not exist at all".
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		writeJSON(w, http.StatusOK, staffWithdrawalResponse{
			withdrawalRequestResponse: toWithdrawalRequestResponse(wr),
			PlayerAccountID:           wr.PlayerAccountID.String(),
			DecimalExponent:           decimalExponent,
		})
	}
}

// approverEligibilityCheck adapts a direct staff_users lookup into
// withdrawal.ApproverEligibility - Stage 3D's mandatory-Person-linkage/
// active-status check (docs/decisions/0024 §1). Shared by every handler
// that records or acts on a human withdrawal decision (approve, reject,
// submit, resolve - the business decision's own "approve, reject, or
// submit" list), so there is exactly one Go-level query implementing this
// check, not one per handler.
//
// A staff row that cannot be resolved at all (deleted, or the id simply
// doesn't exist) reports linked=false, active=false rather than an
// error - "unresolvable identity" is exactly as ineligible as "resolved
// but unlinked/inactive" per docs/decisions/0024 §1, and treating it as a
// hard error here would let a caller mistake a 500 for something other
// than "this principal is not eligible."
func approverEligibilityCheck(ctx context.Context, tx pgx.Tx) withdrawal.ApproverEligibility {
	return func(approverPrincipalID uuid.UUID) (linked, active bool, err error) {
		var personID *uuid.UUID
		var status string
		err = tx.QueryRow(ctx, `SELECT person_id, status FROM staff_users WHERE id = $1`, approverPrincipalID).Scan(&personID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		if err != nil {
			return false, false, fmt.Errorf("httpserver: approver eligibility lookup: %w", err)
		}
		return personID != nil, status == "active", nil
	}
}

type approveWithdrawalResponse struct {
	Approved bool `json:"approved"`
}

// newApproveWithdrawalHandler records a four-eyes 'approve' decision.
// approverPrincipalID is ALWAYS the calling staff member's own
// authenticated subject id - never anything from the request body - per
// withdrawal-state-machine.md §5 bypass #1; RequirePermission(
// PermWithdrawalApprove) has already confirmed the caller's ROLE may
// approve at all before this handler runs, and internal/withdrawal.Approve
// itself enforces the distinct-approver/threshold/duplicate invariants.
//
// beneficiaryCheck (Stage 3C): closes withdrawal-state-machine.md §5
// bypass #2 - "a staff member who is also a player self-approves their
// own payout". migration 0029 added staff_users.person_id, so this
// handler can now resolve both sides of the comparison and pass a real
// check to withdrawal.Approve, which was already designed to accept one
// (see BeneficiaryCheck's own doc comment in internal/withdrawal). This
// is the SERVICE-LAYER half of a defense-in-depth pair: the same
// migration also added an authoritative BEFORE INSERT trigger on
// withdrawal_approvals that rejects the exact same condition at the
// database itself, regardless of whether this check is ever bypassed,
// disabled, or has a bug - see migration 0029's own doc comment for why
// that trigger, not this closure, is what actually makes the rule
// "authoritative, not merely audit-detectable" (the Stage 3C directive's
// own phrase). This closure exists so a genuine self-approval attempt
// fails with a clean ErrSelfApproval/409 instead of a raw Postgres
// exception surfacing as a 500.
func newApproveWithdrawalHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		approverID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		var approved bool
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			beneficiaryCheck := func(approverPrincipalID uuid.UUID) (bool, error) {
				var isBeneficiary bool
				err := tx.QueryRow(ctx, `
					SELECT pa.person_id IS NOT NULL
						AND su.person_id IS NOT NULL
						AND pa.person_id = su.person_id
					FROM withdrawal_requests wr
					JOIN player_accounts pa ON pa.id = wr.player_account_id
					LEFT JOIN staff_users su ON su.id = $2
					WHERE wr.id = $1`,
					id, approverPrincipalID,
				).Scan(&isBeneficiary)
				if errors.Is(err, pgx.ErrNoRows) {
					// The withdrawal itself doesn't exist (or isn't
					// visible in this tenant scope) - Approve's own
					// lockRequestForUpdate, called before this check runs,
					// already returns ErrNotFound for that case; reaching
					// here with no row is unexpected, so fail closed
					// rather than silently reporting "not a beneficiary".
					return false, fmt.Errorf("withdrawal: beneficiary check: request not found")
				}
				return isBeneficiary, err
			}
			var err error
			approved, err = withdrawal.Approve(ctx, tx, id, approverID, false, beneficiaryCheck, approverEligibilityCheck(ctx, tx))
			if err != nil {
				return err
			}
			// Supplementary to withdrawal.Approve's own internal audit
			// record (which has no access to the *http.Request and so
			// cannot carry IP/user-agent/request-id - CLAUDE.md requires
			// them on every mutating admin/financial action). This record
			// is what actually answers "which staff member, from where"
			// for a four-eyes decision; the package-level one only
			// answers "what changed".
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: approverID,
				Action: "withdrawal.approve.http", TargetType: "withdrawal_request", TargetID: id.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"approved": approved},
			})
		})
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if errors.Is(err, withdrawal.ErrStateConflict) {
			apierror.Write(w, requestID, apierror.CodeConflict, "withdrawal is not awaiting review")
			return
		}
		if errors.Is(err, withdrawal.ErrDuplicateApproval) {
			apierror.Write(w, requestID, apierror.CodeConflict, "you have already recorded a decision for this withdrawal")
			return
		}
		if errors.Is(err, withdrawal.ErrSelfApproval) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "cannot approve your own withdrawal")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverNotLinked) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account has no confirmed Person linkage and is not eligible to approve withdrawals")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverInactive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account is not active")
			return
		}
		if errors.Is(err, withdrawal.ErrStepUpRequired) {
			// Stage 3C directive item 6: the resolved policy requires a
			// step-up/MFA challenge this platform cannot yet perform
			// (ADR 0017) - fail closed rather than silently approve.
			apierror.Write(w, requestID, apierror.CodeForbidden, "this withdrawal requires a step-up/MFA challenge that is not yet available")
			return
		}
		if err != nil {
			logger.Error("approve_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to approve withdrawal")
			return
		}
		writeJSON(w, http.StatusOK, approveWithdrawalResponse{Approved: approved})
	}
}

type rejectWithdrawalRequest struct {
	ReasonCode string `json:"reason_code"`
}

// newRejectWithdrawalHandler records a 'reject' decision and reverses the
// hold. Same approverPrincipalID sourcing rule as approve.
func newRejectWithdrawalHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		approverID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		var req rejectWithdrawalRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := withdrawal.Reject(ctx, tx, id, approverID, req.ReasonCode, approverEligibilityCheck(ctx, tx)); err != nil {
				return err
			}
			// See newApproveWithdrawalHandler's identical rationale.
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: approverID,
				Action: "withdrawal.reject.http", TargetType: "withdrawal_request", TargetID: id.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"reason_code": req.ReasonCode},
			})
		})
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if errors.Is(err, withdrawal.ErrStateConflict) {
			apierror.Write(w, requestID, apierror.CodeConflict, "withdrawal is not awaiting review")
			return
		}
		if errors.Is(err, withdrawal.ErrDuplicateApproval) {
			apierror.Write(w, requestID, apierror.CodeConflict, "you have already recorded a decision for this withdrawal")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverNotLinked) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account has no confirmed Person linkage and is not eligible to reject withdrawals")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverInactive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account is not active")
			return
		}
		if err != nil {
			logger.Error("reject_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to reject withdrawal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type submitWithdrawalRequest struct {
	PaymentMethod string `json:"payment_method"`
}

type submitWithdrawalResponse struct {
	withdrawalRequestResponse
	ProviderID        string `json:"provider_id,omitempty"`
	ProviderReference string `json:"provider_reference,omitempty"`
}

// newSubmitWithdrawalHandler is staff's explicit trigger to dispatch an
// `approved` withdrawal to a PaymentProvider for payout.
//
// PAYOUT DISPATCH CUTOVER (ADR 0095, PRH-I1): this handler no longer calls
// provider.Withdraw inside the domain transaction. That old path (still
// visible in version control) is the exact double-payout hazard the
// security review named (prh-ref-provider-reference-bound.md §9.1 item 1 /
// §10 C1, "F-POOL-2"): if the provider call executed the payout and then
// ANYTHING after it failed (a NOT NULL/CHECK violation, an oversize
// provider reference, a crash), the transaction rolled back, the request
// stayed `approved`, and a staff retry called Withdraw a second time.
//
// The replacement is the three-phase pattern payments.ClaimForDispatch/
// DispatchWithdraw/ApplyPayoutResult implement (internal/payments/
// payout.go, mirroring drive.go's identical pattern for deposits):
//
//  1. ClaimForDispatch (T1p): one short transaction that locks the
//     withdrawal row, re-runs the ADR 0096 payout KYC gate, and on ALLOW
//     commits approved -> submitted PLUS the payout's payment_attempts
//     row (∅ -> submitting) BEFORE this handler ever calls a provider. A
//     DENY commits withdrawal.DenyForCompliance instead - no attempt row,
//     hold already released.
//  2. DispatchWithdraw (phase B): the actual provider.Withdraw call, with
//     NO transaction held.
//  3. ApplyPayoutResult (phase C): applies the outcome under its own
//     short transaction (context.WithoutCancel-bound, so it commits even
//     if this request's own context is later cancelled).
//
// PaymentMethod is supplied here rather than stored on the request, since
// routing needs a concrete rail and Stage 3B's withdrawal-state-machine.md
// never fixed one at request time (unlike deposits, whose payment_method
// is chosen by the player up front).
//
// Deliberately narrower than the deposit orchestrator: no cascade-on-
// decline across a chain of providers (payout.go's own doc comment), and
// an OutcomePending/OutcomeAmbiguous result leaves the request at
// `submitted` with no further automated ledger effect - resolving those
// is newResolveWithdrawalHandler's job (QueryStatus, never a resend/
// reroute). A withdrawal stuck at `submitted` is a safe state (no
// incorrect ledger effect), not an incorrect one.
func newSubmitWithdrawalHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.PaymentOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "withdrawal submission is not enabled on this deployment")
			return
		}
		if deps.PaymentsOutboundCredentials == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "withdrawal submission is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		submitterID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		var req submitWithdrawalRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("payment_method", req.PaymentMethod)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		// Stage 3D business decision #1's "approve, reject, or submit" list
		// includes submit - unlike approve/reject, no INSERT into
		// withdrawal_approvals happens here for the database's own
		// withdrawal_approvals_enforce_governance trigger to catch, so this
		// Go-level check is the ONLY enforcement point for this transition.
		// Checked before ClaimForDispatch, so an ineligible submitter never
		// reaches the payout dispatch path at all.
		var eligible bool
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			linked, active, err := approverEligibilityCheck(ctx, tx)(submitterID)
			if err != nil {
				return fmt.Errorf("withdrawal: submitter eligibility check: %w", err)
			}
			if !linked {
				return withdrawal.ErrApproverNotLinked
			}
			if !active {
				return withdrawal.ErrApproverInactive
			}
			eligible = true
			return nil
		})
		if errors.Is(err, withdrawal.ErrApproverNotLinked) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account has no confirmed Person linkage and is not eligible to submit withdrawals")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverInactive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account is not active")
			return
		}
		if err != nil || !eligible {
			logger.Error("submit_withdrawal_eligibility_check_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to submit withdrawal")
			return
		}

		// B6 (RV-PRH-I1 code review): the staff-attribution audit
		// ("withdrawal.submit.http") is now written BY ClaimForDispatch
		// itself, inside the SAME transaction as the deny/allow outcome -
		// never a best-effort, discardable call made after the fact.
		actor := payments.SubmitActor{StaffID: submitterID, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID}
		claim, err := deps.PaymentOrchestrator.ClaimForDispatch(
			r.Context(), deps.DB, payments.KYCEnforcementPayoutGate{}, tc.TenantID, id, req.PaymentMethod, actor,
		)
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
			// H-SEC-11: tenant or brand not active; the request is left
			// `approved`, no attempt, no provider call.
			logger.Warn("submit_withdrawal_refused_not_active", "request_id", requestID, "tenant_id", tc.TenantID)
			apierror.Write(w, requestID, apierror.CodeTenantOrBrandNotActive, "withdrawals are not available right now")
			return
		}
		if errors.Is(err, withdrawal.ErrStateConflict) {
			apierror.Write(w, requestID, apierror.CodeConflict, "withdrawal is not approved and ready for submission")
			return
		}
		if errors.Is(err, payments.ErrNoRoutableProvider) {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "no payment provider available for this withdrawal")
			return
		}
		if errors.Is(err, payments.ErrPayoutKYCUnavailable) {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "verification check is temporarily unavailable, please retry")
			return
		}
		if errors.Is(err, payments.ErrPayoutKillSwitchEngaged) {
			// Fail-closed, transient (migration 0105): the request is left
			// exactly `approved` (the whole T1p transaction rolled back) -
			// safe and expected to retry once the switch is released.
			logger.Error("submit_withdrawal_kill_switch_engaged", "error", err)
			apierror.Write(w, requestID, apierror.CodeUnavailable, "payouts are temporarily paused for this provider - please retry shortly")
			return
		}
		if err != nil {
			logger.Error("submit_withdrawal_claim_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to submit withdrawal")
			return
		}

		wr := claim.Request
		if claim.Denied {
			resp := submitWithdrawalResponse{withdrawalRequestResponse: toWithdrawalRequestResponse(wr)}
			writeJSON(w, http.StatusOK, resp)
			return
		}

		provider, ok := deps.PaymentOrchestrator.Provider(claim.Capability.ProviderID)
		if !ok {
			logger.Error("submit_withdrawal_provider_not_registered", "provider_id", claim.Capability.ProviderID)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to submit withdrawal")
			return
		}

		// Phase B: no transaction held.
		gr := payments.DispatchWithdraw(r.Context(), deps.DB, deps.PaymentsOutboundCredentials, provider, claim.Attempt)

		// Phase C: applies under its own context.WithoutCancel-bound
		// transaction, so it commits even if r.Context() is cancelled by
		// now (payout.go's own doc comment).
		if err := payments.ApplyPayoutResult(r.Context(), deps.DB, tc.TenantID, id, claim.Attempt, gr, payments.EvidenceSync); err != nil {
			// B2 (RV-PRH-I1 code review): this is no longer a dead end.
			// ClaimForDispatch (T1p) already committed next_action_at =
			// lease_until on the attempt row, so - whatever failed here -
			// the sweeper will pick this payout back up on its own and
			// converge it (T2/T12/QueryStatus), with no further staff
			// action required. The OLD "check /resolve" message was false:
			// /resolve refused any `submitted` row with no provider
			// reference yet, which is exactly this shape (P95-C2).
			logger.Error("submit_withdrawal_apply_result_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "withdrawal dispatch outcome could not be recorded immediately - it will be retried automatically")
			return
		}

		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			wr, err = withdrawal.GetByID(ctx, tx, id)
			return err
		})
		if err != nil {
			logger.Error("submit_withdrawal_reread_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to submit withdrawal")
			return
		}
		resp := submitWithdrawalResponse{withdrawalRequestResponse: toWithdrawalRequestResponse(wr)}
		if wr.ProviderID != nil {
			resp.ProviderID = *wr.ProviderID
		}
		if wr.ProviderReference != nil {
			resp.ProviderReference = *wr.ProviderReference
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// newListSubmittedWithdrawalsHandler is the staff-facing stranded-hold
// recovery queue (Stage 3C): every withdrawal currently at `submitted`
// with no automated path forward on its own (see LockSubmittedForResolution's
// doc comment) - what a real operations team would need to see to know
// which payouts need a manual status check.
func newListSubmittedWithdrawalsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var resp []staffWithdrawalResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			requests, err := withdrawal.ListSubmittedForTenant(ctx, tx)
			if err != nil {
				return err
			}
			resp = make([]staffWithdrawalResponse, 0, len(requests))
			for _, wr := range requests {
				resp = append(resp, staffWithdrawalResponse{
					withdrawalRequestResponse: toWithdrawalRequestResponse(wr),
					PlayerAccountID:           wr.PlayerAccountID.String(),
				})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_submitted_withdrawals_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list submitted withdrawals")
			return
		}
		if resp == nil {
			resp = []staffWithdrawalResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// newResolveWithdrawalHandler is the Stage 3C stranded-hold recovery
// mechanism: for a `submitted` withdrawal, queries the SAME provider and
// reference already recorded - never a second Withdraw call (no
// resubmission) and never a different provider (no cascade) - and
// transitions to `completed`/`failed` accordingly, or leaves the request
// at `submitted` if the provider itself is still unresolved.
//
// H4/B7 (RV-PRH-I1 ledger-finance + code review): this handler no longer
// calls provider.QueryStatus itself, and no longer calls
// withdrawal.Complete/Fail directly. Both broke INV-IO-1 (QueryStatus ran
// INSIDE deps.DB.WithTenant, holding the withdrawal_requests L1 lock
// across outbound I/O) and left the payment_attempts row permanently out
// of sync with the withdrawal (attempt stuck pending/ambiguous forever
// while the withdrawal itself moved to completed/failed, which then broke
// every later sweeper tick on the same attempt). This handler now:
//  1. reads (no lock) the withdrawal and its live attempt;
//  2. delegates to payments.PollPayoutStatus - the SAME phase-B (QueryStatus,
//     no transaction held, on a bounded/detached context)/phase-C (the
//     state-aware evidence function, amount/asset cross-check included)
//     the sweeper itself uses, so staff-triggered and automatic resolution
//     can never diverge;
//  3. re-reads and audits the outcome.
//
// Safe to call any number of times, by any number of concurrent callers:
// PollPayoutStatus's own phase-C CAS guards make a resolution that already
// happened (by a concurrent caller, the sweeper, or a prior call) a no-op
// rather than a double-post.
// errResolveNoLiveAttempt is a local sentinel: a `submitted` withdrawal
// with no live payment_attempts row is a data shape this handler cannot
// resolve (there is nothing to poll QueryStatus against) - distinct from
// every other error path so it maps to its own, honest response.
var errResolveNoLiveAttempt = errors.New("httpserver: no live payment attempt for this withdrawal")

func newResolveWithdrawalHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.PaymentOrchestrator == nil || deps.PaymentsOutboundCredentials == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "withdrawal resolution is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		resolverID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		var wr withdrawal.WithdrawalRequest
		var attempt payments.PaymentAttempt
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			// Same rationale as newSubmitWithdrawalHandler's identical
			// check: resolving a stranded `submitted` withdrawal is the
			// same class of withdrawal-governance decision as submitting
			// it, and no withdrawal_approvals row is inserted here for the
			// database trigger to catch - this Go-level check is the only
			// enforcement point.
			linked, active, err := approverEligibilityCheck(ctx, tx)(resolverID)
			if err != nil {
				return fmt.Errorf("withdrawal: resolver eligibility check: %w", err)
			}
			if !linked {
				return withdrawal.ErrApproverNotLinked
			}
			if !active {
				return withdrawal.ErrApproverInactive
			}

			// Read-only (no lock, no I/O held): H4 requires no transaction
			// held across the QueryStatus call, which now happens entirely
			// outside this closure.
			wr, err = withdrawal.GetByID(ctx, tx, id)
			if err != nil {
				return err
			}
			if wr.State != withdrawal.StateSubmitted {
				return withdrawal.ErrStateConflict
			}
			var found bool
			attempt, found, err = payments.GetLiveAttemptForWithdrawalRequest(ctx, tx, id)
			if err != nil {
				return err
			}
			if !found {
				return errResolveNoLiveAttempt
			}
			return nil
		})
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if errors.Is(err, withdrawal.ErrStateConflict) {
			apierror.Write(w, requestID, apierror.CodeConflict, "withdrawal is not submitted and awaiting resolution")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverNotLinked) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account has no confirmed Person linkage and is not eligible to resolve withdrawals")
			return
		}
		if errors.Is(err, withdrawal.ErrApproverInactive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account is not active")
			return
		}
		if errors.Is(err, errResolveNoLiveAttempt) {
			apierror.Write(w, requestID, apierror.CodeConflict, "no live payment attempt found for this withdrawal - it will be picked up automatically once one exists")
			return
		}
		if err != nil {
			logger.Error("resolve_withdrawal_read_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to resolve withdrawal")
			return
		}
		if attempt.State == payments.AttemptCreated {
			// A `created` payout attempt (a NotSent re-claim candidate)
			// has nothing for QueryStatus to look up yet - the sweeper's
			// own T2 re-claim is the correct path forward, not this
			// handler (never a resend from here).
			apierror.Write(w, requestID, apierror.CodeConflict, "this withdrawal is awaiting automatic re-claim, not yet resolvable via QueryStatus")
			return
		}

		// N3 (RV-PRH-I1 re-review 2): the staff-attribution audit is now
		// written BY PollPayoutStatus itself, inside the SAME transaction
		// as whatever state change it performs - never a separate,
		// best-effort call after the fact (the same B6 rule, now here).
		actor := &payments.SubmitActor{StaffID: resolverID, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID}

		// Phase B+C: no transaction held (H4/INV-IO-1); PollPayoutStatus
		// applies the state-aware evidence matrix including the amount/
		// asset cross-check (B3/H2), and transitions the attempt row
		// alongside the withdrawal (H4's "bypasses payment_attempts" fix).
		if err := payments.PollPayoutStatus(r.Context(), deps.DB, deps.PaymentOrchestrator, deps.PaymentsOutboundCredentials, tc.TenantID, attempt, time.Now().Add(30*time.Second), actor); err != nil {
			if errors.Is(err, payments.ErrPayoutDispatchInFlight) {
				// R2 (RV-PRH-I1 ledger re-review): the attempt's own lease
				// has not expired - its phase B may be running right now.
				// Refuse rather than forcing it to ambiguous and racing the
				// original dispatch's own phase C.
				apierror.Write(w, requestID, apierror.CodeConflict, "dispatch is still in progress for this withdrawal - try again shortly")
				return
			}
			if errors.Is(err, payments.ErrSweeperProviderNotRegistered) {
				logger.Error("resolve_withdrawal_unknown_provider", "error", err)
				apierror.Write(w, requestID, apierror.CodeUnavailable, "the provider recorded on this withdrawal is not available on this deployment")
				return
			}
			logger.Error("resolve_withdrawal_poll_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to resolve withdrawal")
			return
		}

		// B3/H2: PollPayoutStatus handles an amount/asset mismatch itself
		// (parks the attempt via T10, never an error return) - re-read the
		// attempt so this handler can still surface the SAME distinct 409
		// staff already expect for that condition, rather than reporting a
		// generic 200 while silently leaving the payout disputed.
		var resolvedAttempt payments.PaymentAttempt
		if err := deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			resolvedAttempt, err = payments.GetAttemptByID(ctx, tx, attempt.ID)
			return err
		}); err != nil {
			logger.Error("resolve_withdrawal_reread_attempt_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to resolve withdrawal")
			return
		}
		if resolvedAttempt.State == payments.AttemptDisputed && resolvedAttempt.TerminalReason != nil && *resolvedAttempt.TerminalReason == "amount_asset_mismatch" {
			logger.Error("resolve_withdrawal_provider_amount_mismatch", "attempt_id", attempt.ID.String())
			apierror.Write(w, requestID, apierror.CodeConflict, "provider-confirmed amount/asset does not match the withdrawal request")
			return
		}

		// Re-read only - the staff audit itself already committed inside
		// PollPayoutStatus's own transaction (N3, above), so this closure
		// no longer needs to write anything.
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			wr, err = withdrawal.GetByID(ctx, tx, id)
			return err
		})
		if errors.Is(err, payments.ErrUnknownProvider) {
			// Realistic ops scenario (a provider deregistered/renamed
			// between submission and resolution) rather than the
			// structurally-unreachable nil-reference case above - mapped
			// the same way the analogous submit-path routing failure is
			// (newSubmitWithdrawalHandler's ErrNoRoutableProvider ->
			// CodeUnavailable), not a generic 500.
			logger.Error("resolve_withdrawal_unknown_provider", "error", err)
			apierror.Write(w, requestID, apierror.CodeUnavailable, "the provider recorded on this withdrawal is not available on this deployment")
			return
		}
		if err != nil {
			logger.Error("resolve_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to resolve withdrawal")
			return
		}
		resp := submitWithdrawalResponse{withdrawalRequestResponse: toWithdrawalRequestResponse(wr)}
		if wr.ProviderID != nil {
			resp.ProviderID = *wr.ProviderID
		}
		if wr.ProviderReference != nil {
			resp.ProviderReference = *wr.ProviderReference
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// newCancelWithdrawalHandler lets a player cancel their own withdrawal
// while it is still in `requested` state (withdrawal-state-machine.md
// §1). Uses WithPlayerScope for the lookup-and-ownership guarantee, then
// re-opens a tenant-only scoped transaction to perform the actual
// transition, since Cancel's own ledger postings/audit writes are system-
// scoped operations, exactly like every other withdrawal state
// transition in this codebase - a player's session never directly holds
// the tenant-only scope withdrawal.Cancel's internal ledger.Post/audit.Record
// calls run under.
func newCancelWithdrawalHandler(deps Deps) http.HandlerFunc {
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
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}

		// Ownership check first, under the player's own restricted scope -
		// a player must never be able to cancel another player's request
		// merely by guessing its id.
		var owned bool
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			wr, err := withdrawal.GetByID(ctx, tx, id)
			if err != nil {
				return err
			}
			owned = wr.PlayerAccountID == playerAccountID
			return nil
		})
		if errors.Is(err, withdrawal.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}
		if err != nil {
			logger.Error("cancel_withdrawal_ownership_check_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to cancel withdrawal")
			return
		}
		if !owned {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		}

		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return withdrawal.Cancel(ctx, tx, id)
		})
		if errors.Is(err, withdrawal.ErrStateConflict) {
			apierror.Write(w, requestID, apierror.CodeConflict, "withdrawal can no longer be cancelled")
			return
		}
		if err != nil {
			logger.Error("cancel_withdrawal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to cancel withdrawal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
