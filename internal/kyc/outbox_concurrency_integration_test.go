//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.2): concurrency. Two workers never claim a row
// twice; N concurrent creates for one player yield ONE live create and the
// others return it, owned by the caller (security F3 / D2). Run with
// `-race -count=50` for the two-worker test (Q-R4).

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 14. Two workers drain the same outbox: each row is claimed exactly once and
// each vendor call happens exactly once.
func TestOutbox_14_TwoWorkers_EachRowClaimedOnce_EachCallOnce(t *testing.T) {
	r := newRig(t)
	type seeded struct {
		f   fixture
		vid uuid.UUID
	}
	var all []seeded
	for ti := 0; ti < 3; ti++ {
		base := r.f
		if ti > 0 {
			base = seedFixture(t, r.pool)
		}
		fixtures := []fixture{base}
		for i := 0; i < 3; i++ {
			fixtures = append(fixtures, seedSecondAccount(t, r.pool, base))
		}
		for _, f := range fixtures {
			v, _ := requestCreate(t, r.pool, f, "mock")
			all = append(all, seeded{f, v.ID})
		}
	}
	if len(all) != 12 {
		t.Fatalf("setup: %d rows", len(all))
	}

	w2 := workerFor(r.pool, NewMockOutboundResolver(), r.spy)
	var wg sync.WaitGroup
	var claimErrors atomic.Int32
	for _, w := range []*OutboxWorker{r.w, w2} {
		wg.Add(1)
		go func(w *OutboxWorker) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				st := w.RunPass(context.Background())
				if st.ClaimError {
					claimErrors.Add(1)
				}
				if st.Claimed == 0 {
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if n := claimErrors.Load(); n != 0 {
		t.Fatalf("concurrent claims must never collide (SKIP LOCKED gives disjoint rows): %d claim error(s)", n)
	}
	passUntilQuiet(t, r.w) // a straggler left for the second worker, if any

	calls, _ := r.calls()
	if calls != 12 {
		t.Fatalf("every create must reach the vendor exactly once: %d calls for 12 rows (keys %v)", calls, r.createKeys)
	}
	seen := map[string]int{}
	for _, k := range r.createKeys {
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("idempotency key %s was sent %d times", k, n)
		}
	}
	for _, s := range all {
		row := onlyRow(t, r.pool, s.f.tenantID, s.vid, OpCreate)
		if row.State != OutboxSent || row.Claims != 1 {
			t.Fatalf("row %+v: want sent with exactly one claim", row)
		}
	}
}

// 20. Per-player create bound under concurrency (security F3, IC C2/T-F): N
// concurrent creates for one player produce ONE live create row and ONE orphan
// verification; the others return it (existing); a create after the live row is
// sent starts a new verification.
func TestOutbox_20_ConcurrentCreates_OneLiveCreatePerPlayer_CallerOwned_D2(t *testing.T) {
	r := newRig(t)
	other := seedSecondAccount(t, r.pool, r.f) // a second player in the SAME tenant

	type res struct {
		v        Verification
		existing bool
		err      error
		owner    uuid.UUID
	}
	const perPlayer = 6
	results := make(chan res, 2*perPlayer)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, f := range []fixture{r.f, other} {
		for i := 0; i < perPlayer; i++ {
			wg.Add(1)
			go func(f fixture) {
				defer wg.Done()
				<-start
				v, existing, err := tryRequestCreate(r.pool, f, "mock")
				results <- res{v, existing, err, f.playerID}
			}(f)
		}
	}
	close(start)
	wg.Wait()
	close(results)

	byPlayer := map[uuid.UUID][]res{}
	for x := range results {
		if x.err != nil {
			t.Fatalf("a concurrent create failed: %v", x.err)
		}
		byPlayer[x.owner] = append(byPlayer[x.owner], x)
	}
	for owner, rs := range byPlayer {
		ids := map[uuid.UUID]bool{}
		fresh := 0
		for _, x := range rs {
			ids[x.v.ID] = true
			if !x.existing {
				fresh++
			}
			if x.v.PlayerAccountID != owner {
				t.Fatalf("D2: a caller received a verification owned by %s (caller is %s)", x.v.PlayerAccountID, owner)
			}
		}
		if len(ids) != 1 || fresh != 1 {
			t.Fatalf("player %s: %d distinct verifications and %d fresh creates, want 1 and 1", owner, len(ids), fresh)
		}
	}
	// Exactly one verification row and one live create row per player.
	for _, f := range []fixture{r.f, other} {
		var verifs, live int
		if err := r.pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM kyc_verifications WHERE player_account_id = $1`, f.playerID).Scan(&verifs); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_submission_outbox WHERE player_account_id = $1 AND operation = 'create' AND state IN ('pending','claimed')`, f.playerID).Scan(&live)
		}); err != nil {
			t.Fatal(err)
		}
		if verifs != 1 || live != 1 {
			t.Fatalf("player %s: %d verification rows and %d live creates, want 1 and 1 (no orphan amplification)", f.playerID, verifs, live)
		}
	}
	// After the live create is sent, a new create starts a NEW verification
	// (the existing re-verification path).
	first := byPlayer[r.f.playerID][0].v
	passUntilQuiet(t, r.w)
	again, existing := requestCreate(t, r.pool, r.f, "mock")
	if existing || again.ID == first.ID {
		t.Fatalf("a create after the live row is sent must start a new verification, got existing=%v same=%v", existing, again.ID == first.ID)
	}
}

// D2: the live create moves to `sent` in the window between the 23505 and the
// read: the caller gets that create's verification (owned by the caller),
// never an empty 200 and never a 500.
func TestOutbox_20b_CreateRace_WinnerSentBetweenConflictAndRead_ReturnsOwnVerification_D2(t *testing.T) {
	r := newRig(t)
	winner := r.create()
	t.Cleanup(func() { requestVerificationTestHook = nil })
	requestVerificationTestHook = func(stage string, attempt int) {
		if stage == "after_conflict" && attempt == 0 {
			requestVerificationTestHook = nil
			passUntilQuiet(t, r.w) // the worker sends the winner in exactly this window
		}
	}
	v, existing, err := tryRequestCreate(r.pool, r.f, "mock")
	if err != nil {
		t.Fatalf("expected the sent winner's verification, got error %v", err)
	}
	if !existing || v.ID != winner.ID || v.PlayerAccountID != r.f.playerID {
		t.Fatalf("expected the winner's verification (existing), got id=%s existing=%v owner=%s", v.ID, existing, v.PlayerAccountID)
	}
	if row := onlyRow(t, r.pool, r.f.tenantID, winner.ID, OpCreate); row.State != OutboxSent {
		t.Fatalf("setup: the winner must have moved to sent, got %s", row.State)
	}
}

// D2: the winner ends terminal in that window: the live index no longer blocks
// and the insert is retried once, producing a NEW verification.
func TestOutbox_20c_CreateRace_WinnerTerminalBetweenConflictAndRead_RetriesInsert_D2(t *testing.T) {
	r := newRig(t)
	winner := r.create()
	bad := workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy)
	t.Cleanup(func() { requestVerificationTestHook = nil })
	requestVerificationTestHook = func(stage string, attempt int) {
		if stage == "after_conflict" && attempt == 0 {
			requestVerificationTestHook = nil
			passUntilQuiet(t, bad) // binding mismatch: the winner ends failed_terminal at once
		}
	}
	v, existing, err := tryRequestCreate(r.pool, r.f, "mock")
	if err != nil {
		t.Fatalf("expected the insert to be retried, got %v", err)
	}
	if existing || v.ID == winner.ID {
		t.Fatalf("expected a NEW verification after the winner ended terminal, got existing=%v same=%v", existing, v.ID == winner.ID)
	}
	if row := onlyRow(t, r.pool, r.f.tenantID, winner.ID, OpCreate); row.State != OutboxFailedTerminal {
		t.Fatalf("setup: the winner must be failed_terminal, got %s", row.State)
	}
}

// D2: a second miss is the typed, retryable contention error - never an empty
// 200 and never a 500.
func TestOutbox_20d_CreateRace_PersistentContention_TypedError_D2(t *testing.T) {
	r := newRig(t)
	r.create() // W0
	bad := workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy)
	t.Cleanup(func() { requestVerificationTestHook = nil })
	requestVerificationTestHook = func(stage string, attempt int) {
		saved := requestVerificationTestHook
		requestVerificationTestHook = nil
		defer func() { requestVerificationTestHook = saved }()
		switch {
		case stage == "after_conflict":
			passUntilQuiet(t, bad) // the current winner ends terminal: the read finds nothing
		case stage == "before_retry":
			requestCreate(t, r.pool, r.f, "mock") // another request wins the live slot again
		}
	}
	_, _, err := tryRequestCreate(r.pool, r.f, "mock")
	if !errIs(err, ErrVerificationCreateContention) {
		t.Fatalf("expected ErrVerificationCreateContention, got %v", err)
	}
}
