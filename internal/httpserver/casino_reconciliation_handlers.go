package httpserver

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Stage 10.3 W2b (CAS-RECON-1): read-only Back Office views of the
// casino_consistency reconciliation evidence and the casino callback
// rejection record. Stage 10.3 W3a (CAS-RECON-STMT-1) extends the runs and
// mismatches views to the casino_statement stream (an optional ?stream=
// filter; absent = both casino streams). A casino_statement result against
// today's MOCK source is tautological; the MOCK label is carried in every
// such mismatch's actual_value. Tenant-wide, staff-scoped, paginated with the shared
// pageParams/pagedResponse convention - the newListAdminBetsHandler /
// newListAdminCasinoRoundsHandler pattern exactly. Pure reads: nothing here
// resolves, annotates or compensates a mismatch. Resolution and the
// four-eyes compensating adjustment are LEDGER-MANUAL-ADJ-4EYES-1, which is
// NOT IMPLEMENTED; no route exists for them.

type casinoReconciliationRunResponse struct {
	ID          string `json:"id"`
	Stream      string `json:"stream"`
	Status      string `json:"status"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	RunAt       string `json:"run_at"`
}

type casinoReconciliationMismatchResponse struct {
	ID                  string  `json:"id"`
	RunID               string  `json:"run_id"`
	MismatchKind        string  `json:"mismatch_kind"`
	ReconciliationKey   string  `json:"reconciliation_key"`
	ExpectedValue       string  `json:"expected_value"`
	ActualValue         string  `json:"actual_value"`
	InvestigationStatus string  `json:"investigation_status"`
	CreatedAt           string  `json:"created_at"`
	ResolvedAt          *string `json:"resolved_at"`
}

type casinoCallbackRejectionResponse struct {
	ID                   string  `json:"id"`
	ProviderID           string  `json:"provider_id"`
	EventType            string  `json:"event_type"`
	ProviderTxID         string  `json:"provider_tx_id"`
	OriginalProviderTxID *string `json:"original_provider_tx_id"`
	RoundID              *string `json:"round_id"`
	AssetCode            *string `json:"asset_code"`
	// Amount is minor units as a decimal string (NUMERIC(38,0)), never a
	// float.
	Amount      *string `json:"amount"`
	ReasonClass string  `json:"reason_class"`
	FirstSeenAt string  `json:"first_seen_at"`
}

var validMismatchStatuses = map[string]bool{"": true, "open": true, "investigating": true, "resolved": true}

// casinoReconciliationStreams maps the optional ?stream= filter (Stage 10.3
// W3a, CAS-RECON-STMT-1) to the casino streams it selects: absent means
// both casino_consistency and casino_statement.
func casinoReconciliationStreams(v string) ([]reconciliation.Stream, bool) {
	switch reconciliation.Stream(v) {
	case "":
		return reconciliation.CasinoStreams, true
	case reconciliation.StreamCasinoConsistency, reconciliation.StreamCasinoStatement:
		return []reconciliation.Stream{reconciliation.Stream(v)}, true
	}
	return nil, false
}

const casinoStreamFilterMessage = "stream must be one of casino_consistency, casino_statement"

func newListCasinoReconciliationRunsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		streams, ok := casinoReconciliationStreams(r.URL.Query().Get("stream"))
		if !ok {
			apierror.Write(w, requestID, apierror.CodeValidation, casinoStreamFilterMessage)
			return
		}
		p := parsePageParams(r)
		var items []casinoReconciliationRunResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			runs, count, err := reconciliation.ListRunsForStreams(ctx, tx, streams, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]casinoReconciliationRunResponse, 0, len(runs))
			for _, run := range runs {
				items = append(items, casinoReconciliationRunResponse{
					ID: run.ID.String(), Stream: string(run.Stream), Status: string(run.Status),
					PeriodStart: run.PeriodStart.UTC().Format(rfc3339), PeriodEnd: run.PeriodEnd.UTC().Format(rfc3339),
					RunAt: run.RunAt.UTC().Format(rfc3339),
				})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_casino_reconciliation_runs_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list reconciliation runs")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

func newListCasinoReconciliationMismatchesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		status := r.URL.Query().Get("status")
		if !validMismatchStatuses[status] {
			apierror.Write(w, requestID, apierror.CodeValidation, "status must be one of open, investigating, resolved")
			return
		}
		kinds, ok := reconciliation.CasinoMismatchKindsForStream(reconciliation.Stream(r.URL.Query().Get("stream")))
		if !ok {
			apierror.Write(w, requestID, apierror.CodeValidation, casinoStreamFilterMessage)
			return
		}
		p := parsePageParams(r)
		var items []casinoReconciliationMismatchResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			ms, count, err := reconciliation.ListMismatchesOfKinds(ctx, tx, kinds, status, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]casinoReconciliationMismatchResponse, 0, len(ms))
			for _, m := range ms {
				item := casinoReconciliationMismatchResponse{
					ID: m.ID.String(), RunID: m.RunID.String(), MismatchKind: string(m.MismatchKind),
					ReconciliationKey: m.ReconciliationKey, ExpectedValue: m.ExpectedValue, ActualValue: m.ActualValue,
					InvestigationStatus: m.InvestigationStatus, CreatedAt: m.CreatedAt.UTC().Format(rfc3339),
				}
				if m.ResolvedAt != nil {
					s := m.ResolvedAt.UTC().Format(rfc3339)
					item.ResolvedAt = &s
				}
				items = append(items, item)
			}
			return nil
		})
		if err != nil {
			logger.Error("list_casino_reconciliation_mismatches_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list reconciliation mismatches")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

func newListCasinoCallbackRejectionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		p := parsePageParams(r)
		var items []casinoCallbackRejectionResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, count, err := casino.ListCallbackRejections(ctx, tx, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]casinoCallbackRejectionResponse, 0, len(rows))
			for _, row := range rows {
				items = append(items, casinoCallbackRejectionResponse{
					ID: row.ID.String(), ProviderID: row.ProviderID, EventType: row.EventType, ProviderTxID: row.ProviderTxID,
					OriginalProviderTxID: row.OriginalProviderTxID, RoundID: row.RoundID, AssetCode: row.AssetCode,
					Amount: row.Amount, ReasonClass: row.ReasonClass, FirstSeenAt: row.FirstSeenAt.UTC().Format(rfc3339),
				})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_casino_callback_rejections_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list callback rejections")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}
