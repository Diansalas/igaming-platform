package httpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// ADR 0114 slice S2 - R1 (payment attempts list and detail) and R1b (the payout attempt summary on
// the admin withdrawal detail). READ-ONLY.
//
//   - Gate: payment_force_resolve:read ALONE (finance, compliance, platform_admin) on the route;
//     withdrawal:review is not accepted. R1b stays on the withdrawal detail's own gate
//     (withdrawal:review, finance only).
//   - Tenant: the {tenantID} path value through canActOnTenant (tenant caller: own tenant only,
//     404 otherwise); every query carries an explicit tenant_id predicate and runs in a
//     tenant-scoped RLS transaction. A platform caller's read is committed to the tenant-visible
//     platform-action audit log BEFORE the tenant data is read.
//   - Never returned: provider_reference text (only has_provider_reference), the merchant
//     reference, the idempotency key, the player or wallet id, any destination or instrument
//     detail, the raw terminal_reason text (a closed catalogue, unknown -> "other"), staff ids.
//   - Pagination: keyset on (created_at DESC, id DESC) with an opaque cursor; limit 1..200,
//     default 50. A cursor is a position, never a capability: tenant scope is applied on every
//     call, so a cursor minted for tenant A used against tenant B positions inside B's rows only.

func registerPaymentAttemptReadRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	// Literal patterns only (the webhook route guard fails closed on any non-literal pattern).
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payment-attempts",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveRead)(newListPaymentAttemptsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payment-attempts/{attemptID}",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveRead)(newGetPaymentAttemptHandler(deps))))
}

// --- the closed catalogue of terminal reasons ---

// paymentAttemptTerminalReasons is the closed output catalogue of attempt terminal reasons:
// every payout dispute reason, every deposit dispute reason (the invalid-reference prefix
// expanded over the closed providerref reasons) and the platform refusal codes of the rejected
// state. Any stored value outside it is reported as "other" (never passed through).
var paymentAttemptTerminalReasons = func() map[string]bool {
	m := map[string]bool{}
	for k := range payments.PayoutDisputeReasons() {
		m[k] = true
	}
	for _, r := range payments.DepositDisputeTerminalReasons() {
		if !strings.HasSuffix(r, ":") {
			m[r] = true
		}
	}
	for _, r := range []providerref.Reason{providerref.ReasonEmpty, providerref.ReasonTooLong, providerref.ReasonInvalidUTF8,
		providerref.ReasonControlChar, providerref.ReasonReservedNamespace} {
		m[payments.TerminalReasonInvalidProviderReference+":"+string(r)] = true
	}
	for _, r := range []string{"kill_switch", "no_routable_provider", "expired_before_submission", "intent_succeeded"} {
		m[r] = true
	}
	return m
}()

const terminalReasonOther = "other"

// terminalReasonOut maps a stored terminal_reason to the closed output vocabulary.
func terminalReasonOut(stored *string) *string {
	if stored == nil {
		return nil
	}
	out := terminalReasonOther
	if paymentAttemptTerminalReasons[*stored] {
		out = *stored
	}
	return &out
}

var paymentAttemptStates = map[string]bool{
	"created": true, "submitting": true, "pending": true, "ambiguous": true,
	"succeeded": true, "declined": true, "rejected": true, "disputed": true,
}

// --- DTOs ---

// paymentAttemptSubjectRef names what the attempt is for: exactly one field is set. It never
// carries a player or wallet identity.
type paymentAttemptSubjectRef struct {
	WithdrawalRequestID *string `json:"withdrawal_request_id,omitempty"`
	DepositIntentID     *string `json:"deposit_intent_id,omitempty"`
}

type paymentAttemptDTO struct {
	ID                   string                   `json:"id"`
	Operation            string                   `json:"operation"`
	AttemptNo            int                      `json:"attempt_no"`
	State                string                   `json:"state"`
	TerminalReason       *string                  `json:"terminal_reason"`
	SubjectRef           paymentAttemptSubjectRef `json:"subject_ref"`
	AmountMinorUnits     string                   `json:"amount_minor_units"`
	AssetCode            string                   `json:"asset_code"`
	Exponent             int16                    `json:"exponent"`
	ProviderID           *string                  `json:"provider_id"`
	HasProviderReference bool                     `json:"has_provider_reference"`
	CreatedAt            string                   `json:"created_at"`
	UpdatedAt            string                   `json:"updated_at"`
	ResolvedAt           *string                  `json:"resolved_at"`
	OpenResolutionID     *string                  `json:"open_resolution_id"`
	AdmissibleKinds      []string                 `json:"admissible_kinds"`
}

type paymentAttemptListResponse struct {
	Items      []paymentAttemptDTO `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

// payoutAttemptSummaryDTO is the R1b summary on the admin withdrawal detail: the latest payout
// attempt of the withdrawal, with the same exclusions as R1.
type payoutAttemptSummaryDTO struct {
	AttemptID        string  `json:"attempt_id"`
	State            string  `json:"state"`
	TerminalReason   *string `json:"terminal_reason"`
	OpenResolutionID *string `json:"open_resolution_id"`
	AttemptCount     int     `json:"attempt_count"`
}

// --- cursor ---

type paymentAttemptCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

const paymentAttemptCursorPrefix = "pa1"

func encodePaymentAttemptCursor(c paymentAttemptCursor) string {
	raw := paymentAttemptCursorPrefix + "." + strconv.FormatInt(c.CreatedAt.UTC().UnixMicro(), 10) + "." + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

var errBadPaymentAttemptCursor = errors.New("httpserver: malformed payment attempt cursor")

// decodePaymentAttemptCursor is the strict inverse of encodePaymentAttemptCursor.
func decodePaymentAttemptCursor(tok string) (paymentAttemptCursor, error) {
	if tok == "" || len(tok) > 128 {
		return paymentAttemptCursor{}, errBadPaymentAttemptCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return paymentAttemptCursor{}, errBadPaymentAttemptCursor
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 || parts[0] != paymentAttemptCursorPrefix {
		return paymentAttemptCursor{}, errBadPaymentAttemptCursor
	}
	us, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || us <= 0 || strconv.FormatInt(us, 10) != parts[1] {
		return paymentAttemptCursor{}, errBadPaymentAttemptCursor
	}
	id, err := uuid.Parse(parts[2])
	if err != nil || id == uuid.Nil || id.String() != parts[2] {
		return paymentAttemptCursor{}, errBadPaymentAttemptCursor
	}
	return paymentAttemptCursor{CreatedAt: time.UnixMicro(us).UTC(), ID: id}, nil
}

// --- reads ---

// attemptReadColumns selects one attempt for R1. open_resolution_id is the latest open (pending
// or executing) K3 resolution of the attempt; the two m4 booleans are the DATABASE predicate
// payment_m4_in_scope (migrations 0125/0127) evaluated for each M4 kind. The withdrawal state is
// read for the M2 precondition. provider_reference is only tested for presence.
const attemptReadSelect = `
SELECT a.id, a.operation, a.attempt_no, a.state, a.terminal_reason, a.deposit_intent_id, a.withdrawal_request_id,
       a.amount::text, a.asset_code, a.provider_id, (a.provider_reference IS NOT NULL),
       a.created_at, a.updated_at, a.resolved_at,
       (SELECT m.id FROM payment_manual_resolutions m
         WHERE m.tenant_id = a.tenant_id AND m.attempt_id = a.id AND m.state IN ('pending', 'executing')
         ORDER BY m.created_at DESC, m.id DESC LIMIT 1),
       w.state,
       payment_m4_in_scope('m4_evidence_paid', a.state, a.terminal_reason, a.provider_reference),
       payment_m4_in_scope('m4_evidence_not_paid', a.state, a.terminal_reason, a.provider_reference)
  FROM payment_attempts a
  LEFT JOIN withdrawal_requests w ON w.id = a.withdrawal_request_id AND w.tenant_id = a.tenant_id
 WHERE a.tenant_id = $1`

type attemptRow struct {
	id                      uuid.UUID
	operation               string
	attemptNo               int
	state                   string
	terminalReason          *string
	depositIntentID         *uuid.UUID
	withdrawalRequestID     *uuid.UUID
	amount                  string
	asset                   string
	providerID              *string
	hasRef                  bool
	createdAt, updatedAt    time.Time
	resolvedAt              *time.Time
	openResolutionID        *uuid.UUID
	withdrawalState         *string
	m4PaidScope, m4NotScope bool
}

func scanAttemptRow(row pgx.Row) (attemptRow, error) {
	var a attemptRow
	err := row.Scan(&a.id, &a.operation, &a.attemptNo, &a.state, &a.terminalReason, &a.depositIntentID, &a.withdrawalRequestID,
		&a.amount, &a.asset, &a.providerID, &a.hasRef, &a.createdAt, &a.updatedAt, &a.resolvedAt, &a.openResolutionID,
		&a.withdrawalState, &a.m4PaidScope, &a.m4NotScope)
	return a, err
}

// admissibleKinds is the HINT of which governed resolution kinds the request path would admit
// for the attempt (the request path re-checks in the database). M4 is the database predicate
// payment_m4_in_scope itself. M1 and M2 restate the closed preconditions of migration 0115's
// request guard (M1: a disputed deposit; M2: an ambiguous payout, or a disputed one with an
// allow-listed reason, with a provider, on a submitted withdrawal, "paid" also needing a held
// reference) through payments.M2ResolvableDispute, which a parity test pins to the database.
// payment_m2_admits cannot be called from a read: it answers only inside an executing
// resolution. An attempt with an open resolution admits nothing (one pending per attempt).
func admissibleKinds(a attemptRow) []string {
	out := []string{}
	if a.openResolutionID != nil {
		return out
	}
	if a.operation == "deposit" {
		if a.state == string(payments.AttemptDisputed) {
			out = append(out, string(payments.ResolutionM1DepositEvidence))
		}
		return out
	}
	if a.operation != "payout" {
		return out
	}
	m2 := a.providerID != nil && a.withdrawalState != nil && *a.withdrawalState == "submitted" &&
		payments.M2ResolvableDispute(payments.AttemptState(a.state), a.terminalReason)
	if m2 && a.hasRef {
		out = append(out, string(payments.ResolutionM2DeclarePaid))
	}
	if m2 {
		out = append(out, string(payments.ResolutionM2DeclareNotPaid))
	}
	if a.m4PaidScope {
		out = append(out, string(payments.ResolutionM4EvidencePaid))
	}
	if a.m4NotScope {
		out = append(out, string(payments.ResolutionM4EvidenceNotPaid))
	}
	return out
}

func (a attemptRow) dto(exponent int16) paymentAttemptDTO {
	d := paymentAttemptDTO{
		ID: a.id.String(), Operation: a.operation, AttemptNo: a.attemptNo, State: a.state,
		TerminalReason: terminalReasonOut(a.terminalReason), AmountMinorUnits: a.amount, AssetCode: a.asset, Exponent: exponent,
		ProviderID: a.providerID, HasProviderReference: a.hasRef,
		CreatedAt: a.createdAt.UTC().Format(rfc3339), UpdatedAt: a.updatedAt.UTC().Format(rfc3339),
		AdmissibleKinds: admissibleKinds(a),
	}
	if a.withdrawalRequestID != nil {
		s := a.withdrawalRequestID.String()
		d.SubjectRef.WithdrawalRequestID = &s
	}
	if a.depositIntentID != nil {
		s := a.depositIntentID.String()
		d.SubjectRef.DepositIntentID = &s
	}
	if a.resolvedAt != nil {
		s := a.resolvedAt.UTC().Format(rfc3339)
		d.ResolvedAt = &s
	}
	if a.openResolutionID != nil {
		s := a.openResolutionID.String()
		d.OpenResolutionID = &s
	}
	return d
}

// attemptExponents resolves the asset exponents of the rows from the asset registry.
func attemptExponents(ctx context.Context, tx pgx.Tx, rows []attemptRow) (map[string]int16, error) {
	out := map[string]int16{}
	for _, r := range rows {
		if _, ok := out[r.asset]; ok {
			continue
		}
		a, err := assetregistry.GetAsset(ctx, tx, r.asset)
		if err != nil {
			return nil, err
		}
		out[r.asset] = a.DecimalExponent
	}
	return out, nil
}

// readScope runs fn in the tenant-scoped read transaction of the call. K3 resolutions are
// visible only to a principal-scoped session (migration 0115 tenant_scope_select), so the
// transaction carries the caller's own subject; the tenant GUC is always the path tenant (the
// caller's own for a tenant caller). It opens no acting session and writes nothing.
func (c staffReadCall) readScope(ctx context.Context, deps Deps, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return deps.DB.WithPrincipalScope(ctx, c.target, c.subject, fn)
}

func newListPaymentAttemptsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		bad := func() { apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter") }
		if !onlyParams(q, "operation", "state", "terminal_reason", "withdrawal_request_id", "limit", "cursor") {
			bad()
			return
		}
		var operation, state, reason, wd, cur string
		for _, p := range []struct {
			name string
			dst  *string
		}{{"operation", &operation}, {"state", &state}, {"terminal_reason", &reason}, {"withdrawal_request_id", &wd}, {"cursor", &cur}} {
			v, one := singleParam(q, p.name)
			if !one {
				bad()
				return
			}
			*p.dst = v
		}
		if (q.Has("operation") && operation != "payout" && operation != "deposit") ||
			(q.Has("state") && !paymentAttemptStates[state]) ||
			(q.Has("terminal_reason") && !paymentAttemptTerminalReasons[reason]) {
			bad()
			return
		}
		var wdID uuid.UUID
		if q.Has("withdrawal_request_id") {
			id, err := uuid.Parse(wd)
			if err != nil || id == uuid.Nil || id.String() != wd {
				bad()
				return
			}
			wdID = id
		}
		limit := defaultPageLimit
		if q.Has("limit") {
			n, err := strconv.Atoi(q.Get("limit"))
			if err != nil || n < 1 || n > maxPageLimit || len(q["limit"]) != 1 {
				bad()
				return
			}
			limit = n
		}
		var after *paymentAttemptCursor
		if q.Has("cursor") {
			k, err := decodePaymentAttemptCursor(cur)
			if err != nil {
				bad()
				return
			}
			after = &k
		}

		exists, err := c.auditPlatformRead(r.Context(), deps, "payment_attempts.platform_read", "payment_attempt", "",
			map[string]any{"op": "list"})
		if err != nil {
			c.logger.Error("payment_attempts_read_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list payment attempts")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}

		sql := attemptReadSelect
		args := []any{c.target}
		add := func(clause string, v any) {
			args = append(args, v)
			sql += " AND " + strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args)))
		}
		if operation != "" {
			add("a.operation = ?", operation)
		}
		if state != "" {
			add("a.state = ?", state)
		}
		if reason != "" {
			add("a.terminal_reason = ?", reason)
		}
		if wdID != uuid.Nil {
			add("a.withdrawal_request_id = ?", wdID)
		}
		if after != nil {
			args = append(args, after.CreatedAt, after.ID)
			sql += " AND (a.created_at, a.id) < ($" + strconv.Itoa(len(args)-1) + ", $" + strconv.Itoa(len(args)) + ")"
		}
		args = append(args, limit+1)
		sql += " ORDER BY a.created_at DESC, a.id DESC LIMIT $" + strconv.Itoa(len(args))

		var resp paymentAttemptListResponse
		err = c.readScope(r.Context(), deps, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			var got []attemptRow
			for rows.Next() {
				a, err := scanAttemptRow(rows)
				if err != nil {
					rows.Close()
					return err
				}
				got = append(got, a)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			var next *paymentAttemptCursor
			if len(got) > limit {
				got = got[:limit]
				next = &paymentAttemptCursor{CreatedAt: got[limit-1].createdAt, ID: got[limit-1].id}
			}
			exps, err := attemptExponents(ctx, tx, got)
			if err != nil {
				return err
			}
			resp.Items = make([]paymentAttemptDTO, 0, len(got))
			for _, a := range got {
				resp.Items = append(resp.Items, a.dto(exps[a.asset]))
			}
			if next != nil {
				tok := encodePaymentAttemptCursor(*next)
				resp.NextCursor = &tok
			}
			return nil
		})
		if err != nil {
			c.logger.Error("list_payment_attempts_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list payment attempts")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func newGetPaymentAttemptHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		if len(r.URL.Query()) != 0 {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter")
			return
		}
		id, err := uuid.Parse(r.PathValue("attemptID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid attempt id")
			return
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "payment_attempts.platform_read", "payment_attempt", id.String(),
			map[string]any{"op": "get"})
		if err != nil {
			c.logger.Error("payment_attempts_read_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load payment attempt")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		var dto paymentAttemptDTO
		found := false
		err = c.readScope(r.Context(), deps, func(ctx context.Context, tx pgx.Tx) error {
			a, err := scanAttemptRow(tx.QueryRow(ctx, attemptReadSelect+" AND a.id = $2", c.target, id))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			asset, err := assetregistry.GetAsset(ctx, tx, a.asset)
			if err != nil {
				return err
			}
			found = true
			dto = a.dto(asset.DecimalExponent)
			return nil
		})
		if err != nil {
			c.logger.Error("get_payment_attempt_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load payment attempt")
			return
		}
		if !found {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	}
}

// loadPayoutAttemptSummary is R1b: the latest payout attempt of a withdrawal, or nil when none
// exists. It must run in a principal-scoped transaction (resolutions are invisible otherwise).
func loadPayoutAttemptSummary(ctx context.Context, tx pgx.Tx, tenantID, withdrawalID uuid.UUID) (*payoutAttemptSummaryDTO, error) {
	var (
		id     uuid.UUID
		state  string
		reason *string
		open   *uuid.UUID
		n      int
	)
	err := tx.QueryRow(ctx, `
		SELECT a.id, a.state, a.terminal_reason,
		       (SELECT m.id FROM payment_manual_resolutions m
		         WHERE m.tenant_id = a.tenant_id AND m.attempt_id = a.id AND m.state IN ('pending', 'executing')
		         ORDER BY m.created_at DESC, m.id DESC LIMIT 1),
		       (SELECT count(*) FROM payment_attempts c
		         WHERE c.tenant_id = a.tenant_id AND c.withdrawal_request_id = a.withdrawal_request_id AND c.operation = 'payout')
		  FROM payment_attempts a
		 WHERE a.tenant_id = $1 AND a.withdrawal_request_id = $2 AND a.operation = 'payout'
		 ORDER BY a.attempt_no DESC, a.id DESC LIMIT 1`, tenantID, withdrawalID).Scan(&id, &state, &reason, &open, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &payoutAttemptSummaryDTO{AttemptID: id.String(), State: state, TerminalReason: terminalReasonOut(reason), AttemptCount: n}
	if open != nil {
		s := open.String()
		d.OpenResolutionID = &s
	}
	return d, nil
}
