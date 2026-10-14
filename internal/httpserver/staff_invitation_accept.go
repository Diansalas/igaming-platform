package httpserver

// ADR 0119 S5 + S6 (sections 6.3, 10.1): POST /v1/staff/invitations/accept. The invitee presents the emailed invite
// token and chooses a password; the account moves invited -> active, the token is consumed, and an `enrol`
// challenge is returned (the account cannot obtain a session until a factor is confirmed through the S3
// /enrol/start and /enrol/confirm steps). The staff member and tenant come from the TOKEN ROW (the S3 two-phase
// lookup), never from the body. Every refusal is the single 401 invalid_enrolment_token.

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/stafflifecycle"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// minStaffPasswordLen is the staff password floor at invitation acceptance (review fix SR-9: the cmd/seed-admin
// platform_admin floor, for every staff role).
const minStaffPasswordLen = 12

type invitationAcceptRequest struct {
	Token    redacted[string] `json:"token"`
	Password redacted[string] `json:"password"`
}

var invitePurposes = []store.EnrolmentPurpose{store.EnrolInvite}

func newInvitationAcceptHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil || deps.DB == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		var req invitationAcceptRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.Token.reveal() == "" || len(req.Password.reveal()) < minStaffPasswordLen {
			apierror.Write(w, requestID, apierror.CodeValidation, "token and a password of at least 12 characters are required")
			return
		}
		ip, ua := clientIP(r), r.UserAgent()
		// The Argon2 cost is paid on every path (success or refusal) before any lookup, so the answer's timing does not
		// depend on whether the token exists.
		hash, err := auth.HashPassword(req.Password.reveal())
		if err != nil {
			logger.Error("staff_invitation_accept_hash_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		ref, err := m.store.ResolveEnrolmentToken(r.Context(), deps.DB, invitePurposes, req.Token.reveal())
		if errors.Is(err, store.ErrInvalidEnrolmentToken) {
			if aerr := deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{ActorType: audit.ActorSystem, Action: "staff.enrolment_token_rejected",
					Outcome: audit.OutcomeFailure, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "unknown_or_unusable", "purpose": string(store.EnrolInvite)}})
			}); aerr != nil {
				logger.Error("staff_invitation_accept_audit_failed", "error", aerr)
				writeStaffAuthUnavailable(w, requestID)
				return
			}
			writeMFAOutcome(w, requestID, mfaInvalidEnrolmentToken)
			return
		}
		if err != nil {
			logger.Error("staff_invitation_accept_lookup_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		tok, err := staffmfa.ParseToken(req.Token.reveal())
		if err != nil {
			writeMFAOutcome(w, requestID, mfaInvalidEnrolmentToken)
			return
		}
		var (
			out  mfaOutcome
			resp staffChallenge
		)
		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			// Review fix SR-2: the inviter (and the token's issuer) must still be active; taken FOR SHARE before the
			// token and the invitee are locked (the order a concurrent suspension of the inviter also takes).
			live, err := stafflifecycle.EnrolmentIssuersLiveInTx(ctx, tx, ref.ID)
			if err != nil {
				return err
			}
			if !live {
				out = mfaInvalidEnrolmentToken
				return staffAudit(ctx, tx, r, requestID, ref.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": "issuer_not_live", "purpose": string(store.EnrolInvite)})
			}
			row, err := m.store.LockEnrolmentToken(ctx, tx, store.EnrolInvite, tok, now)
			if errors.Is(err, store.ErrInvalidEnrolmentToken) {
				out = mfaInvalidEnrolmentToken
				return staffAudit(ctx, tx, r, requestID, ref.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": "unusable", "purpose": string(store.EnrolInvite)})
			}
			if err != nil {
				return err
			}
			sv, err := stafflifecycle.AcceptInvitationInTx(ctx, tx, row.Staff.StaffID, row.Staff.TenantID, hash,
				stafflifecycle.Meta{RequestID: requestID, IP: ip, UserAgent: ua})
			if errors.Is(err, stafflifecycle.ErrTransition) || errors.Is(err, stafflifecycle.ErrNotFound) {
				out = mfaInvalidEnrolmentToken
				return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": "account_status", "purpose": string(store.EnrolInvite)})
			}
			if err != nil {
				return err
			}
			if err := m.store.ConsumeEnrolmentToken(ctx, tx, row.ID, now); err != nil {
				return err
			}
			next, err := m.store.IssueChallenge(ctx, tx, row.Staff, store.ChallengeEnrol, store.OriginInvite, sv, now)
			if err != nil {
				return err
			}
			resp = newStaffChallenge(string(store.ChallengeEnrol), next, challengeExpiresIn, enrolChallengeMethods)
			out = mfaSuccess
			return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_redeemed", audit.OutcomeSuccess,
				map[string]any{"purpose": string(store.EnrolInvite)})
		})
		if err != nil {
			logger.Error("staff_invitation_accept_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		if out == mfaSuccess {
			writeStaffSecretJSON(w, http.StatusOK, resp)
			return
		}
		writeMFAOutcome(w, requestID, out)
	}
}
