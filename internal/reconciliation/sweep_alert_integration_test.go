//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// R3-LISTING-FAILURE-LOG-ONLY-1 end to end: a tenant LISTING failure inside a real sweep becomes a durable
// reconciliation.sweep_failed alert (real database, real alerting write under the dispatcher identity).
func TestSweepListingFailureBecomesADurableAlert(t *testing.T) {
	pool := sbScratchPool(t)
	f := seedFixture(t, pool)
	errForced := errors.New("forced non-active listing failure (contains secret-9f3a)")
	orig := listNonActiveTenants
	listNonActiveTenants = func(context.Context, *db.Pool, []uuid.UUID) ([]nonActiveTenant, error) { return nil, errForced }
	t.Cleanup(func() { listNonActiveTenants = orig })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()
	for i := 0; i < 2; i++ {
		_, err := RunSweepTenants(context.Background(), pool, logger, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
			sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
		if !errors.Is(err, ErrListNonActiveTenants) || !errors.Is(err, errForced) {
			t.Fatalf("sweep error must wrap ErrListNonActiveTenants and the cause, got %v", err)
		}
		handleSweepError(context.Background(), pool, logger, err)
	}

	type row struct {
		severity, disc, attrs string
		subject               *uuid.UUID
		occ                   int
	}
	var rows []row
	if err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		q, err := tx.Query(ctx, `SELECT a.severity, a.discriminator, a.attributes::text, a.subject_tenant_id,
			(SELECT count(*) FROM alert_occurrences o WHERE o.alert_id = a.id)
			FROM alerts a WHERE a.kind = 'reconciliation.sweep_failed'`)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var r row
			if err := q.Scan(&r.severity, &r.disc, &r.attrs, &r.subject, &r.occ); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return q.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("alerts = %d, want 1 (repeated failures of one phase dedupe)", len(rows))
	}
	r := rows[0]
	if r.severity != "p1" || r.disc != "phase:list_non_active" || r.occ != 2 || r.subject != nil {
		t.Fatalf("alert = %+v", r)
	}
	// Never the error text (the forced error carries a secret-looking string).
	if r.attrs != `{"phase": "list_non_active", "sqlstate_class": "58"}` {
		t.Fatalf("attributes = %s", r.attrs)
	}
}

// A sweep failure caused by shutdown writes nothing, even against a real database.
func TestSweepShutdownCancellationWritesNoAlert(t *testing.T) {
	pool := sbScratchPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handleSweepError(ctx, pool, nil, errors.Join(ErrListActiveTenants, context.Canceled))
	var n int
	if err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'reconciliation.sweep_failed'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("alerts after a shutdown cancellation: %d (%v)", n, err)
	}
}
