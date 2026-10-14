//go:build integration

package stafflifecycle

// ADR 0119 S5 + S6 security re-review NR-4 (Go half): concurrent bindings of one Person through the service serialise
// on a per-Person advisory lock; the loser answers ErrPersonInUse and writes nothing. The database index is NOT built
// (see the residual pin below and ADR 0119 section 28.9).

import (
	"context"
	"errors"
	"sync"
	"testing"

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

// Residual S56-R14 (pinned visible, R1 class): the database does NOT enforce one live platform account per Person -
// the partial unique index the re-review recommended (NR-4) is not built (ADR 0119 section 28.9: it would make the
// same-Person second-platform-account defences of eight other governed flows untestable and needs an architect
// decision). Raw runtime SQL can still bind one Person to two live platform accounts. When the index lands, this test
// flips and must be replaced by the race test.
func TestResidual_NR4_RawSQLCanBindOnePersonToTwoPlatformAccounts(t *testing.T) {
	w := newLWorld(t)
	a, b := w.legacyPlatform(), w.legacyPlatform()
	p := w.person()
	for _, id := range []uuid.UUID{a, b} {
		if err := w.exec(w.rt, uuid.Nil, `UPDATE staff_users SET person_id = $1 WHERE id = $2`, p, id); err != nil {
			t.Fatalf("raw binding: %v (if this now fails with 23505 the NR-4 index exists: replace this residual pin)", err)
		}
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
