package httpserver

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// ADR 0114 slice S4 - R2: reconciliation runs and mismatches for the payment_statement,
// ledger_vs_projection and sportsbook_settlement streams. READ-ONLY (detection only: a
// correction goes through K2/K3, never from here). The casino streams keep their own route.
//
//   - stream is REQUIRED; an unknown stream, status, kind or subject_id is 400 (the `other`
//     bucket below applies to OUTPUT only).
//   - Permission per stream, checked BEFORE any query (and before the status/kind vocabularies
//     are consulted, so an unauthorised caller learns nothing about them):
//     payment_statement -> payment_force_resolve:read (finance, compliance, platform_admin),
//     ledger_vs_projection -> ledger_adjustment:read (same three),
//     sportsbook_settlement -> sportsbook_reconciliation:read (tenant_admin, compliance, finance).
//     The route-level gate admits any holder of one of the three; the handler then requires the
//     one for the requested stream.
//   - The stored reconciliation_key and the expected/actual texts are free-form diagnostic strings
//     that can embed provider references. They are NEVER returned. subject_ref, the minor-unit
//     amounts and the asset code are derived from them by strict parsers that accept only a
//     canonical UUID after a known token, a whole-value integer, and a short asset token; anything
//     else yields null. A subject_ref names an attempt, deposit intent, receipt, bet, wallet,
//     ledger account or ledger transaction, never a player identity.

func registerStaffReconciliationRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	gate := auth.RequireAnyPermission(auth.PermPaymentForceResolveRead, auth.PermLedgerAdjustmentRead, auth.PermSportsbookReconciliationRead)
	mux.Handle("GET /v1/admin/tenants/{tenantID}/reconciliation/runs",
		staff(gate(newStaffReconciliationRunsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/reconciliation/mismatches",
		staff(gate(newStaffReconciliationMismatchesHandler(deps))))
}

// staffStreamPermission is the per-stream read permission. A stream not in it is unknown (400).
var staffStreamPermission = map[reconciliation.Stream]auth.Permission{
	reconciliation.StreamPaymentStatement:     auth.PermPaymentForceResolveRead,
	reconciliation.StreamLedgerVsProjection:   auth.PermLedgerAdjustmentRead,
	reconciliation.StreamSportsbookSettlement: auth.PermSportsbookReconciliationRead,
}

const staffStreamMessage = "stream is required and must be one of payment_statement, ledger_vs_projection, sportsbook_settlement"

type staffReconciliationRunDTO struct {
	ID          string `json:"id"`
	Stream      string `json:"stream"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	RunAt       string `json:"run_at"`
	Status      string `json:"status"`
}

type reconciliationSubjectRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type staffReconciliationMismatchDTO struct {
	ID               string                    `json:"id"`
	RunID            string                    `json:"run_id"`
	Stream           string                    `json:"stream"`
	Kind             string                    `json:"kind"`
	Status           string                    `json:"status"`
	SubjectRef       *reconciliationSubjectRef `json:"subject_ref"`
	ExpectedMinorUOM *string                   `json:"expected_minor_units"`
	ActualMinorUOM   *string                   `json:"actual_minor_units"`
	AssetCode        *string                   `json:"asset_code"`
	DetectedAt       string                    `json:"detected_at"`
}

// staffStreamRequest parses and authorises the stream of an R2 request. It returns ok=false after
// writing the refusal. Order: tenant/caller (already done by beginStaffRead), stream (400),
// permission (403); no query has run.
func staffStreamRequest(w http.ResponseWriter, r *http.Request, c staffReadCall) (reconciliation.Stream, bool) {
	v, one := singleParam(r.URL.Query(), "stream")
	stream := reconciliation.Stream(v)
	perm, known := staffStreamPermission[stream]
	if !one || !known {
		apierror.Write(w, c.requestID, apierror.CodeValidation, staffStreamMessage)
		return "", false
	}
	if !auth.RoleHasPermission(auth.Role(c.tc.Role), perm) {
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return "", false
	}
	return stream, true
}

func newStaffReconciliationRunsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		stream, ok := staffStreamRequest(w, r, c)
		if !ok {
			return
		}
		q := r.URL.Query()
		limit, offset, pok := strictPage(q)
		if !pok || !onlyParams(q, "stream", "limit", "offset") || len(q["limit"]) > 1 || len(q["offset"]) > 1 {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter")
			return
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "reconciliation.platform_read", "reconciliation_run", "",
			map[string]any{"op": "runs", "stream": string(stream)})
		if err != nil {
			c.logger.Error("staff_reconciliation_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list reconciliation runs")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		var items []staffReconciliationRunDTO
		var total int
		err = deps.DB.WithTenantReadOnly(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			runs, count, err := reconciliation.ListRunsForStreams(ctx, tx, []reconciliation.Stream{stream}, limit, offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]staffReconciliationRunDTO, 0, len(runs))
			for _, run := range runs {
				status := string(run.Status)
				if status != string(reconciliation.StatusClean) && status != string(reconciliation.StatusMismatchesFound) {
					status = terminalReasonOther
				}
				items = append(items, staffReconciliationRunDTO{
					ID: run.ID.String(), Stream: string(stream), PeriodStart: run.PeriodStart.UTC().Format(rfc3339),
					PeriodEnd: run.PeriodEnd.UTC().Format(rfc3339), RunAt: run.RunAt.UTC().Format(rfc3339), Status: status,
				})
			}
			return nil
		})
		if err != nil {
			c.logger.Error("staff_reconciliation_runs_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list reconciliation runs")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, pageParams{Limit: limit, Offset: offset}, total))
	}
}

func newStaffReconciliationMismatchesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		stream, ok := staffStreamRequest(w, r, c)
		if !ok {
			return
		}
		q := r.URL.Query()
		bad := func() { apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter") }
		limit, offset, pok := strictPage(q)
		if !pok || !onlyParams(q, "stream", "status", "kind", "subject_id", "limit", "offset") ||
			len(q["limit"]) > 1 || len(q["offset"]) > 1 {
			bad()
			return
		}
		f := reconciliation.StaffMismatchFilter{Stream: stream}
		for _, p := range []struct {
			name string
			dst  *string
		}{{"status", &f.Status}, {"subject_id", &f.SubjectID}} {
			v, one := singleParam(q, p.name)
			if !one {
				bad()
				return
			}
			*p.dst = v
		}
		kind, one := singleParam(q, "kind")
		if !one {
			bad()
			return
		}
		if q.Has("status") && f.Status != "open" && f.Status != "investigating" && f.Status != "resolved" {
			bad()
			return
		}
		if q.Has("kind") {
			kinds, _ := reconciliation.StaffViewMismatchKinds(stream)
			found := false
			for _, k := range kinds {
				found = found || string(k) == kind
			}
			if !found {
				bad()
				return
			}
			f.Kind = reconciliation.MismatchKind(kind)
		}
		if q.Has("subject_id") {
			id, err := uuid.Parse(f.SubjectID)
			if err != nil || id == uuid.Nil || id.String() != f.SubjectID {
				bad()
				return
			}
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "reconciliation.platform_read", "reconciliation_mismatch", "",
			map[string]any{"op": "mismatches", "stream": string(stream)})
		if err != nil {
			c.logger.Error("staff_reconciliation_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list reconciliation mismatches")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		vocab, _ := reconciliation.StaffViewMismatchKinds(stream)
		var items []staffReconciliationMismatchDTO
		var total int
		err = deps.DB.WithTenantReadOnly(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			ms, count, err := reconciliation.ListMismatchesForStream(ctx, tx, f, limit, offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]staffReconciliationMismatchDTO, 0, len(ms))
			for _, m := range ms {
				items = append(items, toStaffReconciliationMismatchDTO(stream, vocab, m))
			}
			return nil
		})
		if err != nil {
			c.logger.Error("staff_reconciliation_mismatches_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list reconciliation mismatches")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, pageParams{Limit: limit, Offset: offset}, total))
	}
}

func toStaffReconciliationMismatchDTO(stream reconciliation.Stream, vocab []reconciliation.MismatchKind, m reconciliation.StoredMismatch) staffReconciliationMismatchDTO {
	kind := terminalReasonOther
	for _, k := range vocab {
		if k == m.MismatchKind {
			kind = string(k)
		}
	}
	status := m.InvestigationStatus
	if status != "open" && status != "investigating" && status != "resolved" {
		status = terminalReasonOther
	}
	return staffReconciliationMismatchDTO{
		ID: m.ID.String(), RunID: m.RunID.String(), Stream: string(stream), Kind: kind, Status: status,
		SubjectRef:       reconciliationSubject(stream, m.MismatchKind, m.ReconciliationKey),
		ExpectedMinorUOM: reconciliationMinorUnits(m.ExpectedValue),
		ActualMinorUOM:   reconciliationMinorUnits(m.ActualValue),
		AssetCode:        reconciliationAsset(m.ReconciliationKey, m.ExpectedValue, m.ActualValue),
		DetectedAt:       m.CreatedAt.UTC().Format(rfc3339),
	}
}

// --- strict parsers over the free-form reconciliation texts (DB-free, unit-tested) ---

const canonicalUUID = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var (
	wholeUUIDRe = regexp.MustCompile(`^` + canonicalUUID + `$`)
	// subjectTokens: the key tokens that name a subject, in preference order, with the closed
	// subject type each yields. "history=" and every other token are deliberately absent.
	subjectTokens = []struct {
		token, typ string
		re         *regexp.Regexp
	}{
		{"attempt", "payment_attempt", regexp.MustCompile(`(?:^| )attempt=(` + canonicalUUID + `)(?: |$)`)},
		{"deposit_intent", "deposit_intent", regexp.MustCompile(`(?:^| )deposit_intent=(` + canonicalUUID + `)(?: |$)`)},
		{"receipt", "payment_receipt", regexp.MustCompile(`(?:^| )receipt=(` + canonicalUUID + `)(?: |$)`)},
		{"bet", "bet", regexp.MustCompile(`(?:^| )bet=(` + canonicalUUID + `)(?: |$)`)},
		{"wallet", "wallet", regexp.MustCompile(`(?:^| )wallet=(` + canonicalUUID + `)(?: |$)`)},
		{"ledger_transaction", "ledger_transaction", regexp.MustCompile(`(?:^| )ledger_transaction=(` + canonicalUUID + `)(?: |$)`)},
	}
	wholeIntRe   = regexp.MustCompile(`^[a-z_]+=(-?[0-9]{1,40})$`)
	intValueRe   = regexp.MustCompile(`^-?[0-9]{1,40}$`)
	assetValueRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
)

// reconciliationSubject derives the subject_ref of a mismatch from its stored key, or nil.
func reconciliationSubject(stream reconciliation.Stream, kind reconciliation.MismatchKind, key string) *reconciliationSubjectRef {
	if stream == reconciliation.StreamLedgerVsProjection && wholeUUIDRe.MatchString(key) {
		switch kind {
		case reconciliation.MismatchKindMissingProjection, reconciliation.MismatchKindBalanceMismatch:
			return &reconciliationSubjectRef{Type: "ledger_account", ID: key}
		case reconciliation.MismatchKindLedgerUnlinkedManualAdjustment:
			return &reconciliationSubjectRef{Type: "ledger_transaction", ID: key}
		}
		return nil
	}
	for _, t := range subjectTokens {
		if m := t.re.FindStringSubmatch(key); m != nil {
			return &reconciliationSubjectRef{Type: t.typ, ID: m[1]}
		}
	}
	return nil
}

// reconciliationMinorUnits returns a minor-unit amount (a decimal integer string) when the
// diagnostic text carries exactly one unambiguous amount: the whole text is `name=<integer>`, or
// it holds exactly one space-separated `amount=<integer>` token. Anything else (multi-value
// texts, prose) is nil.
func reconciliationMinorUnits(text string) *string {
	if m := wholeIntRe.FindStringSubmatch(text); m != nil {
		return &m[1]
	}
	var found []string
	for _, f := range strings.Split(text, " ") {
		if v, ok := strings.CutPrefix(f, "amount="); ok {
			found = append(found, v)
		}
	}
	if len(found) == 1 && intValueRe.MatchString(found[0]) {
		return &found[0]
	}
	return nil
}

// reconciliationAsset returns the asset code when every space-separated `asset=` token in the
// key and both texts names the same short token; nil when there is none, a token is malformed,
// or they disagree.
func reconciliationAsset(texts ...string) *string {
	asset := ""
	for _, t := range texts {
		for _, f := range strings.Split(t, " ") {
			v, ok := strings.CutPrefix(f, "asset=")
			if !ok {
				continue
			}
			if !assetValueRe.MatchString(v) || (asset != "" && !strings.EqualFold(asset, v)) {
				return nil
			}
			asset = v
		}
	}
	if asset == "" {
		return nil
	}
	return &asset
}
