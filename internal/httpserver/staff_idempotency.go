package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// A10c (ADR 0114 G10): server-side Idempotency-Key for the K1 (capability grant), K3 (payment force
// resolution) and HSEC (withdrawal hold resolution) MUTATING staff routes (migration 0164).
//
// staffIdempotent wraps the route's own handler INSIDE its permission gate (and inside staffRoute, so
// authentication, liveness, aal2 and step-up have already passed: a replay is never served to a caller that
// would be refused by step-up). The header is OPTIONAL here: without it the route behaves exactly as before
// (idempotent by state: a replayed governed write is a 409 from the flow's own guards). With it:
//
//   - the key is 1-120 printable ASCII (400 otherwise) and scoped to (path tenant, acting staff id, key);
//   - the request is pinned by SHA-256 over method, path, query and raw body. The same key with a different
//     request is 409 IDEMPOTENCY_MISMATCH; the same key while the first request is still running is 409
//     IDEMPOTENCY_IN_PROGRESS;
//   - the first request's 2xx response is stored and replayed byte-identically (header Idempotent-Replayed:
//     true) without calling the handler again;
//   - a non-2xx outcome (or a panic) deletes the reservation, so the client can retry with the same key.
//
// What this is NOT: it grants no authority and decides nothing. The path tenant is checked with
// canActOnTenant before any database work (a refused caller reaches the handler's own refusal, nothing is
// reserved), the tenant GUC is the path tenant (RLS), and the stored response is only ever replayed to the
// same staff id in the same tenant. A crash between the handler's commit and the completion write leaves the
// key in_progress (retries see 409 IDEMPOTENCY_IN_PROGRESS); the governed flows' state guards still make a
// retry under a new key safe.

const (
	idempotencyHeader    = "Idempotency-Key"
	idempotencyMaxBody   = 1 << 20
	idempotencyMaxStored = 256 * 1024
)

func validIdempotencyKey(k string) bool {
	if len(k) < 1 || len(k) > 120 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// captureWriter buffers a handler's response so it can be stored before it is sent.
type captureWriter struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.h }
func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

func staffIdempotent(deps Deps, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values(idempotencyHeader)
		if len(keys) == 0 {
			inner.ServeHTTP(w, r)
			return
		}
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		if len(keys) != 1 || !validIdempotencyKey(keys[0]) {
			apierror.Write(w, requestID, apierror.CodeValidation, "the Idempotency-Key must be 1-120 printable ASCII characters")
			return
		}
		key := keys[0]
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			inner.ServeHTTP(w, r)
			return
		}
		subject, serr := uuid.Parse(tc.Subject)
		target, terr := uuid.Parse(r.PathValue("tenantID"))
		if serr != nil || terr != nil || subject == uuid.Nil || target == uuid.Nil || !canActOnTenant(tc, target) {
			// Not a caller this wrapper can scope: the handler gives its own refusal. Nothing is reserved.
			inner.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, idempotencyMaxBody))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.New()
		for _, part := range []string{r.Method, "\n", r.URL.Path, "\n", r.URL.RawQuery, "\n"} {
			sum.Write([]byte(part))
		}
		sum.Write(body)
		hash := sum.Sum(nil)
		route := r.Method + " " + r.URL.Path
		if len(route) > 255 {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request")
			return
		}

		var reserved bool
		var prior struct {
			hash   []byte
			state  string
			status *int
			body   []byte
		}
		err = deps.DB.WithTenant(r.Context(), target, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state)
				VALUES ($1, $2, $3, $4, $5, 'in_progress')
				ON CONFLICT (tenant_id, actor_id, idempotency_key) DO NOTHING`, target, subject, key, route, hash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				reserved = true
				return nil
			}
			return tx.QueryRow(ctx, `
				SELECT request_hash, state, response_status, response_body FROM staff_idempotency_keys
				WHERE tenant_id = $1 AND actor_id = $2 AND idempotency_key = $3`, target, subject, key).
				Scan(&prior.hash, &prior.state, &prior.status, &prior.body)
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				// The path tenant does not exist: the handler gives its own 404 and nothing is reserved.
				inner.ServeHTTP(w, r)
				return
			}
			logger.Error("staff_idempotency_reserve_failed", "err", err.Error())
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process the request")
			return
		}
		if !reserved {
			switch {
			case !bytes.Equal(prior.hash, hash):
				apierror.Write(w, requestID, apierror.CodeIdempotencyMismatch, "the Idempotency-Key was used with a different request")
			case prior.state != "completed" || prior.status == nil:
				apierror.Write(w, requestID, apierror.CodeIdempotencyInProgress, "a request with this Idempotency-Key is still being processed")
			default:
				w.Header().Set("Idempotent-Replayed", "true")
				if len(prior.body) > 0 {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
				}
				w.WriteHeader(*prior.status)
				_, _ = w.Write(prior.body)
			}
			return
		}

		release := func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			if err := deps.DB.WithTenant(rctx, target, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM staff_idempotency_keys
					WHERE tenant_id = $1 AND actor_id = $2 AND idempotency_key = $3 AND state = 'in_progress'`, target, subject, key)
				return err
			}); err != nil {
				logger.Error("staff_idempotency_release_failed", "err", err.Error())
			}
		}
		done := false
		defer func() {
			if !done { // the handler panicked: free the key, let the panic continue
				release()
			}
		}()
		cw := &captureWriter{h: http.Header{}}
		inner.ServeHTTP(cw, r)
		done = true
		if cw.status == 0 {
			cw.status = http.StatusOK
		}
		if cw.status >= 200 && cw.status < 300 && cw.body.Len() <= idempotencyMaxStored {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			err := deps.DB.WithTenant(rctx, target, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE staff_idempotency_keys SET state = 'completed', response_status = $4, response_body = $5, completed_at = now()
					WHERE tenant_id = $1 AND actor_id = $2 AND idempotency_key = $3 AND state = 'in_progress'`, target, subject, key, cw.status, cw.body.Bytes())
				return err
			})
			cancel()
			if err != nil {
				logger.Error("staff_idempotency_complete_failed", "err", err.Error())
			}
		} else {
			release()
		}
		for k, vs := range cw.h {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(cw.status)
		_, _ = w.Write(cw.body.Bytes())
	})
}
