//go:build integration

package httpserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// Security S-1 regression: an ack/resolve holding the alert row lock must NOT
// block an in-transaction Raise on the same alert. The occurrence insert of
// every Raise takes FOR KEY SHARE on the alert row; the handler's lock
// (alertTransitionLockSQL) must therefore be FOR NO KEY UPDATE. The raiser
// runs with a lock_timeout so the outcome is deterministic: with FOR UPDATE the
// raise fails (55P03 propagates and the business transaction would roll back);
// with FOR NO KEY UPDATE it succeeds.
func TestAlertAdmin_AckLockDoesNotBlockInTxRaise(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	disc := "switch:" + uuid.NewString()
	iwRaiseAlert(t, a, tenant, disc)
	id, _, _, _, _ := iwAlertState(t, a, tenant, disc)
	admin := a.platformAdmin()

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
			var kind, severity, state string
			var subject *uuid.UUID
			if err := tx.QueryRow(ctx, alertTransitionLockSQL, id).Scan(&kind, &severity, &state, &subject); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-release // the ack transaction stays open, holding its row lock
			return nil
		})
	}()
	<-locked

	_, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(a.pool, tenant), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
			return err
		}
		return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
			Kind: alerting.KindPaymentKillSwitchEngaged, SubjectTenantID: tenant, Discriminator: disc,
			Attributes: map[string]alerting.AttrValue{"reason_code": "iw-admin"},
		})
	})
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatalf("ack-side lock transaction: %v", lockErr)
	}
	if err != nil {
		t.Fatalf("an in-transaction raise must not be blocked by an ack holding the alert row (S-1): %v", err)
	}
}
