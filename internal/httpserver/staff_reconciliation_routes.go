package httpserver

import (
	"context"
	"net/http"
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
	gate := auth.RequireAnyPermission(auth.PermPaymentForceResolveRead, auth.PermLedgerAdjustmentRead, auth.PermSportsbookReconciliationRead)
	mux.Handle("GET /v1/admin/tenants/{tenantID}/reconciliation/runs",
		staffRoute(deps, stepUpNone, gate(newStaffReconciliationRunsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/reconciliation/mismatches",
		staffRoute(deps, stepUpNone, gate(newStaffReconciliationMismatchesHandler(deps))))
	// A10c (ADR 0114 G5): single-run and single-mismatch reads, same gate and per-stream permission.
	mux.Handle("GET /v1/admin/tenants/{tenantID}/reconciliation/runs/{runID}",
		staffRoute(deps, stepUpNone, gate(newStaffReconciliationRunHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/reconciliation/mismatches/{mismatchID}",
		staffRoute(deps, stepUpNone, gate(newStaffReconciliationMismatchHandler(deps))))
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
	Exponent         *int16                    `json:"exponent"`
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

func toStaffReconciliationRunDTO(stream reconciliation.Stream, run reconciliation.Run) staffReconciliationRunDTO {
	status := string(run.Status)
	if status != string(reconciliation.StatusClean) && status != string(reconciliation.StatusMismatchesFound) {
		status = terminalReasonOther
	}
	return staffReconciliationRunDTO{
		ID: run.ID.String(), Stream: string(stream), PeriodStart: run.PeriodStart.UTC().Format(rfc3339),
		PeriodEnd: run.PeriodEnd.UTC().Format(rfc3339), RunAt: run.RunAt.UTC().Format(rfc3339), Status: status,
	}
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
				items = append(items, toStaffReconciliationRunDTO(stream, run))
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
		if !pok || !onlyParams(q, "stream", "status", "kind", "subject_id", "run_id", "limit", "offset") ||
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
		if q.Has("run_id") {
			v, one := singleParam(q, "run_id")
			id, err := uuid.Parse(v)
			if !one || err != nil || id == uuid.Nil || id.String() != v {
				bad()
				return
			}
			f.RunID = id
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
			res := &assetResolver{ctx: ctx, tx: tx, cache: map[string]*resolvedAsset{}}
			for _, m := range ms {
				items = append(items, toStaffReconciliationMismatchDTO(res, stream, vocab, m))
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

// --- mapping (the strict parsers live in internal/reconciliation/read_parse.go) ---

// assetResolver resolves an asset code candidate against the asset registry (case-insensitive
// match, canonical registry code returned), caching per request.
type assetResolver struct {
	ctx   context.Context
	tx    pgx.Tx
	cache map[string]*resolvedAsset
}

type resolvedAsset struct {
	code     string
	exponent int16
}

func (r *assetResolver) resolve(candidate *string) *resolvedAsset {
	if candidate == nil {
		return nil
	}
	key := strings.ToUpper(*candidate)
	if v, ok := r.cache[key]; ok {
		return v
	}
	var a resolvedAsset
	var out *resolvedAsset
	if err := r.tx.QueryRow(r.ctx, `SELECT code, decimal_exponent FROM assets WHERE upper(code) = $1`, key).Scan(&a.code, &a.exponent); err == nil {
		out = &a
	}
	r.cache[key] = out
	return out
}

func toStaffReconciliationMismatchDTO(res *assetResolver, stream reconciliation.Stream, vocab []reconciliation.MismatchKind, m reconciliation.StoredMismatch) staffReconciliationMismatchDTO {
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
	d := staffReconciliationMismatchDTO{
		ID: m.ID.String(), RunID: m.RunID.String(), Stream: string(stream), Kind: kind, Status: status,
		DetectedAt: m.CreatedAt.UTC().Format(rfc3339),
	}
	if s := reconciliation.MismatchSubject(stream, m.MismatchKind, m.ReconciliationKey, m.ExpectedValue); s != nil {
		d.SubjectRef = &reconciliationSubjectRef{Type: s.Type, ID: s.ID}
	}
	// The asset must exist in the registry; without one both amounts are null (ledger-finance F3).
	if a := res.resolve(reconciliation.MismatchAssetCandidate(m.ExpectedValue, m.ActualValue)); a != nil {
		code, exp := a.code, a.exponent
		d.AssetCode, d.Exponent = &code, &exp
		if reconciliation.AmountKindExposed(m.MismatchKind) {
			d.ExpectedMinorUOM = reconciliation.MismatchAmount(m.ExpectedValue)
			d.ActualMinorUOM = reconciliation.MismatchAmount(m.ActualValue)
		}
	}
	return d
}

// singleStaffReconciliationID parses the stream and the {pathName} id of a single-item read. Order:
// tenant/caller (beginStaffRead), stream (400), permission (403), id (400); no query has run.
func singleStaffReconciliationID(w http.ResponseWriter, r *http.Request, c staffReadCall, pathName string) (reconciliation.Stream, uuid.UUID, bool) {
	stream, ok := staffStreamRequest(w, r, c)
	if !ok {
		return "", uuid.Nil, false
	}
	if !onlyParams(r.URL.Query(), "stream") {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter")
		return "", uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue(pathName))
	if err != nil || id == uuid.Nil || id.String() != r.PathValue(pathName) {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid id")
		return "", uuid.Nil, false
	}
	return stream, id, true
}

func newStaffReconciliationRunHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		stream, id, ok := singleStaffReconciliationID(w, r, c, "runID")
		if !ok {
			return
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "reconciliation.platform_read", "reconciliation_run", id.String(),
			map[string]any{"op": "run", "stream": string(stream)})
		if err != nil {
			c.logger.Error("staff_reconciliation_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load reconciliation run")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		var dto staffReconciliationRunDTO
		found := false
		err = deps.DB.WithTenantReadOnly(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			run, ok, err := reconciliation.GetRunForStream(ctx, tx, c.target, stream, id)
			if err != nil || !ok {
				return err
			}
			found, dto = true, toStaffReconciliationRunDTO(stream, run)
			return nil
		})
		if err != nil {
			c.logger.Error("staff_reconciliation_run_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load reconciliation run")
			return
		}
		if !found {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	}
}

func newStaffReconciliationMismatchHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		stream, id, ok := singleStaffReconciliationID(w, r, c, "mismatchID")
		if !ok {
			return
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "reconciliation.platform_read", "reconciliation_mismatch", id.String(),
			map[string]any{"op": "mismatch", "stream": string(stream)})
		if err != nil {
			c.logger.Error("staff_reconciliation_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load reconciliation mismatch")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		vocab, _ := reconciliation.StaffViewMismatchKinds(stream)
		var dto staffReconciliationMismatchDTO
		found := false
		err = deps.DB.WithTenantReadOnly(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			m, ok, err := reconciliation.GetMismatchForStream(ctx, tx, c.target, stream, id)
			if err != nil || !ok {
				return err
			}
			res := &assetResolver{ctx: ctx, tx: tx, cache: map[string]*resolvedAsset{}}
			found, dto = true, toStaffReconciliationMismatchDTO(res, stream, vocab, m)
			return nil
		})
		if err != nil {
			c.logger.Error("staff_reconciliation_mismatch_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load reconciliation mismatch")
			return
		}
		if !found {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	}
}
