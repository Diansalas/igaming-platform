//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// Security C1 (live exploit of the first 0131 draft) and residual 14(b2): a withdrawal request can
// be neither DELETEd nor TRUNCATEd by ANY role (WD012), so "delete the existing row in an open
// transaction, then raw-INSERT the same key on a suspended brand" is impossible. Both roles.
func TestWithdrawalDeleteGuard_NoRoleCanDeleteOrTruncate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.wdFixture()
	if err := w.rawInsertRT(f, "del-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	del := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM withdrawal_requests WHERE tenant_id = $1`, f.tid)
		if err == nil {
			return errors.New("DELETE unexpectedly succeeded; rolling back")
		}
		return err
	}
	trunc := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE withdrawal_requests CASCADE`)
		if err == nil {
			return errors.New("TRUNCATE unexpectedly succeeded; rolling back")
		}
		return err
	}
	for name, run := range map[string]func() error{
		"runtime DELETE":   func() error { return w.rt.WithTenant(ctx, f.tid, del) },
		"owner DELETE":     func() error { return w.owner.WithTenant(ctx, f.tid, del) },
		"owner TRUNCATE":   func() error { return w.owner.WithoutTenant(ctx, trunc) },
		"runtime TRUNCATE": func() error { return w.rt.WithoutTenant(ctx, trunc) },
	} {
		err := run()
		code := pgCode(err)
		// The runtime role has no TRUNCATE privilege on a default grant set; either refusal is a refusal.
		if code != "WD012" && (name != "runtime TRUNCATE" || code != "42501") {
			t.Errorf("%s must be refused WD012 (or lack the privilege), got %v", name, err)
		}
	}
	if n := w.withdrawalCount(f.tid); n != 1 {
		t.Fatalf("the row must survive every attempt, got %d", n)
	}
}

// The exact exploit sequence of the review: B deletes the row for key K and stays open; A raw-INSERTs key K
// on a SUSPENDED brand. B's delete is refused, so A is refused by the status guard (WD011) and nothing is
// accepted.
func TestWithdrawalDeleteGuard_ReviewExploitSequence_IsRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.wdFixture()
	key := "exploit-" + uuid.NewString()
	if err := w.rawInsertRT(f, key); err != nil {
		t.Fatal(err)
	}
	if err := launchfix.TrySetBrandStatus(ctx, f.tid, f.brand, "suspended"); err != nil {
		t.Fatal(err)
	}
	bErr := make(chan error, 1)
	bStarted, bRelease := make(chan struct{}), make(chan struct{})
	go func() {
		bErr <- w.rt.WithTenant(ctx, f.tid, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `DELETE FROM withdrawal_requests WHERE tenant_id = $1 AND idempotency_key = $2`, f.tid, key); err != nil {
				close(bStarted)
				return err
			}
			close(bStarted)
			<-bRelease
			return nil
		})
	}()
	<-bStarted
	aCode := pgCode(w.rawInsertRT(f, key))
	close(bRelease)
	if err := <-bErr; pgCode(err) != "WD012" {
		t.Fatalf("B's delete must be refused WD012, got %v", err)
	}
	if aCode != "WD011" {
		t.Fatalf("A's raw insert on the suspended brand must be refused WD011, got %q", aCode)
	}
	if n := w.withdrawalCount(f.tid); n != 1 {
		t.Fatalf("exactly the original row must exist, got %d", n)
	}
}
