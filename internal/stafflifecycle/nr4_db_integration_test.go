//go:build integration

package stafflifecycle

// ADR 0119 S5 + S6 security re-review fix NR-4: one Person is bound to at most ONE live (non-offboarded) platform staff
// account, enforced by the database (partial unique index staff_users_platform_live_person), not only by the Go
// check-then-write. Two concurrent bindings of one Person to two platform accounts: the second waits for the first and
// fails 23505; through the service the loser answers ErrConflict / ErrPersonInUse and writes nothing.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/staffsv"
)

// legacyPlatform creates a legacy pre-0139 platform admin with no Person (owner, SF014 disabled for one statement).
func (w *lworld) legacyPlatform() uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return staffsv.InsertLegacyUnlinkedStaff(ctx, tx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active')`, id, "nr4-"+id.String()+"@example.com")
	}); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func TestNR4_OnePersonOneLivePlatformAccount_SQLRace(t *testing.T) {
	w := newLWorld(t)
	a, b := w.legacyPlatform(), w.legacyPlatform()
	p := w.person()
	ctx := context.Background()
	// tx1 binds a to p and stays open.
	tx1, err := w.rt.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	if _, err := tx1.Exec(ctx, `UPDATE staff_users SET person_id = $1 WHERE id = $2`, p, a); err != nil {
		t.Fatalf("tx1: %v", err)
	}
	// tx2 binds b to the same p: it must wait for tx1, then fail 23505 once tx1 commits.
	done := make(chan error, 1)
	go func() {
		done <- w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE staff_users SET person_id = $1 WHERE id = $2`, p, b)
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("the second binding must wait for the first (index entry in flight), got %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := code(<-done); got != "23505" {
		t.Fatalf("the second live platform binding of one Person: got %q, want 23505", got)
	}
	// Offboarded rows do not count: the index is partial.
	if err := w.exec(w.owner, uuid.Nil, `UPDATE staff_users SET status = 'offboarded', password_hash = NULL, offboarded_at = now() WHERE id = $1`, a); err != nil {
		t.Fatalf("offboard a: %v", err)
	}
	if err := w.exec(w.rt, uuid.Nil, `UPDATE staff_users SET person_id = $1 WHERE id = $2`, p, b); err != nil {
		t.Fatalf("binding p to b after a is offboarded: %v", err)
	}
}

// Through the service: two concurrent LinkPerson calls naming one Person for two legacy platform accounts; exactly one
// succeeds, the other is refused and writes nothing.
func TestNR4_ConcurrentLinkPerson_ExactlyOne(t *testing.T) {
	w := newLWorld(t)
	actor := w.staff(uuid.Nil, RolePlatformAdmin, true)
	a, b := w.legacyPlatform(), w.legacyPlatform()
	p := w.person()
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, target := range []uuid.UUID{a, b} {
		wg.Add(1)
		go func(i int, target uuid.UUID) {
			defer wg.Done()
			<-start
			_, errs[i] = w.svc.LinkPerson(context.Background(), w.actor(actor, uuid.Nil, RolePlatformAdmin), uuid.Nil, target, p)
		}(i, target)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrPersonInUse), errors.Is(err, ErrConflict):
		default:
			t.Fatalf("the losing link must be refused as in use / conflict, got %v", err)
		}
	}
	var bound int
	if err := w.scope(w.owner, uuid.Nil, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE person_id = $1 AND tenant_id IS NULL AND status <> 'offboarded'`, p).Scan(&bound)
	}); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || bound != 1 {
		t.Fatalf("concurrent links of one Person: %d succeeded, %d live platform accounts bound (want 1, 1)", ok, bound)
	}
}
