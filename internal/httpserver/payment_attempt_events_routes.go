package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
)

// A10c (ADR 0114 G2): the attempt event history, READ-ONLY, same gate and tenant scope as R1
// (payment_force_resolve:read, canActOnTenant, RLS transaction, platform reads audited first).
//
// paymentAttemptEventDTO is one verified provider callback receipt (payment_provider_events) linked to the
// attempt. Closed vocabularies only (the table's CHECK constraints). Never returned: provider_reference,
// original_provider_reference, merchant_reference, settlement_reference, decline_reason (provider free text),
// the event fingerprint. Events not yet linked to an attempt (attempt_id NULL) are not shown here.
type paymentAttemptEventDTO struct {
	ID                   string  `json:"id"`
	EventType            string  `json:"event_type"`
	Outcome              string  `json:"outcome"`
	DispositionAtReceipt string  `json:"disposition_at_receipt"`
	Resolution           *string `json:"resolution"`
	DeclineStage         *string `json:"decline_stage"`
	AmountMinorUnits     *string `json:"amount_minor_units"`
	AssetCode            *string `json:"asset_code"`
	ReceivedAt           string  `json:"received_at"`
	ResolvedAt           *string `json:"resolved_at"`
}

func newListPaymentAttemptEventsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		limit, offset, pok := strictPage(q)
		if !pok || !onlyParams(q, "limit", "offset") || len(q["limit"]) > 1 || len(q["offset"]) > 1 {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter")
			return
		}
		id, err := uuid.Parse(r.PathValue("attemptID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid attempt id")
			return
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "payment_attempts.platform_read", "payment_attempt", id.String(),
			map[string]any{"op": "events"})
		if err != nil {
			c.logger.Error("payment_attempts_read_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load payment attempt events")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		var items []paymentAttemptEventDTO
		var total int
		found := false
		err = c.readScope(r.Context(), deps, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_attempts WHERE tenant_id = $1 AND id = $2)`, c.target, id).Scan(&found); err != nil || !found {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND attempt_id = $2`, c.target, id).Scan(&total); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `
				SELECT id, event_type, outcome, disposition_at_receipt, resolution, decline_stage, amount::text, asset_code, received_at, resolved_at
				  FROM payment_provider_events
				 WHERE tenant_id = $1 AND attempt_id = $2
				 ORDER BY received_at ASC, id ASC
				 LIMIT $3 OFFSET $4`, c.target, id, limit, offset)
			if err != nil {
				return err
			}
			defer rows.Close()
			items = []paymentAttemptEventDTO{}
			for rows.Next() {
				var d paymentAttemptEventDTO
				var evID uuid.UUID
				var received time.Time
				var resolved *time.Time
				if err := rows.Scan(&evID, &d.EventType, &d.Outcome, &d.DispositionAtReceipt, &d.Resolution, &d.DeclineStage,
					&d.AmountMinorUnits, &d.AssetCode, &received, &resolved); err != nil {
					return err
				}
				d.ID, d.ReceivedAt = evID.String(), received.UTC().Format(rfc3339)
				if resolved != nil {
					s := resolved.UTC().Format(rfc3339)
					d.ResolvedAt = &s
				}
				items = append(items, d)
			}
			return rows.Err()
		})
		if err != nil {
			c.logger.Error("list_payment_attempt_events_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to load payment attempt events")
			return
		}
		if !found {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, pageParams{Limit: limit, Offset: offset}, total))
	}
}
