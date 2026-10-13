//go:build integration

package aml_test

// Failure injection for the AML hold probe's savepoint containment (ADR 0116 s5.1 fail closed; ISO-LATENCY-1 security
// review). Every test runs the REAL probe as the RUNTIME role and asserts, for each way the probe can fail, that:
//
//   - the verdict is a deny (fail closed);
//   - the caller's transaction is still usable afterwards for the money-neutral deny path (it can run statements, write the
//     in-tx alert and commit);
//   - the probe leaked nothing into the caller's transaction: its SET LOCAL-equivalent statement_timeout is undone, no
//     app.* / caller GUC changed, and the aml_hold_probe savepoint does not survive the probe;
//   - a caller's own savepoint (a pgx nested transaction, as casino / sportsbook use) is neither released nor rolled back
//     by the probe.
//
// Faults: a real evaluator error (AM026), a lock_timeout (55P03) and the probe's own statement_timeout (57014) raised while
// aml_hold_active waits for a lock held by another session, a Parse-time failure (function not resolvable on the search
// path) on a cold and a warm statement cache, a caller transaction that is already aborted (the first probe statement
// fails), and a context cancelled mid-batch.

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// probeSnapshot is every session setting the probe could leak into the caller's transaction.
const probeSnapshotSQL = `SELECT coalesce(string_agg(name || '=' || setting, ',' ORDER BY name), '') FROM pg_catalog.pg_settings
	WHERE name LIKE 'app.%' OR name LIKE 'igtest.%'
	   OR name IN ('statement_timeout', 'lock_timeout', 'search_path', 'idle_in_transaction_session_timeout')`

func probeSnapshot(t *testing.T, ctx context.Context, tx pgx.Tx) string {
	t.Helper()
	var s string
	if err := tx.QueryRow(ctx, probeSnapshotSQL).Scan(&s); err != nil {
		t.Fatalf("snapshot session settings: %v", err)
	}
	return s
}

// callerSettings gives the caller transaction its own non-default values, so a leak or a lost restore is visible.
func callerSettings(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', '37000', true), set_config('igtest.marker', 'caller', true)`); err != nil {
		t.Fatalf("caller settings: %v", err)
	}
}

// assertNoLeakedSavepoint proves aml_hold_probe does not exist any more, without disturbing the caller: inside a throwaway
// nested savepoint, ROLLBACK TO aml_hold_probe must fail with 3B001 (no such savepoint).
func assertNoLeakedSavepoint(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	nested, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("leak check: open nested savepoint: %v", err)
	}
	_, rbErr := nested.Exec(ctx, `ROLLBACK TO SAVEPOINT aml_hold_probe`)
	if err := nested.Rollback(ctx); err != nil {
		t.Fatalf("leak check: the checker's own savepoint is gone (aml_hold_probe leaked and sat above it?): %v", err)
	}
	if pgCode(rbErr) != "3B001" {
		t.Fatalf("the aml_hold_probe savepoint leaked into the caller's transaction (ROLLBACK TO it returned %v)", rbErr)
	}
}

// lockHolds makes aml_hold_active block: another (owner) session holds ACCESS EXCLUSIVE on aml_holds until release.
func lockHolds(t *testing.T, w *amlWorld) (release func()) {
	t.Helper()
	tx, err := w.Owner.Raw().Begin(bg)
	if err != nil {
		t.Fatalf("lock session: %v", err)
	}
	if _, err := tx.Exec(bg, `LOCK TABLE aml_holds IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = tx.Rollback(bg)
		t.Fatalf("lock aml_holds: %v", err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = tx.Rollback(bg)
		}
	}
	t.Cleanup(release)
	return release
}

// usableAfter runs the caller-side checks every failure case shares and then lets the transaction commit.
func usableAfter(t *testing.T, ctx context.Context, tx pgx.Tx, before string) {
	t.Helper()
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("the caller's transaction is not usable after the contained probe failure: %v", err)
	}
	if after := probeSnapshot(t, ctx, tx); after != before {
		t.Fatalf("the probe leaked session settings into the caller:\n before %s\n after  %s", before, after)
	}
	assertNoLeakedSavepoint(t, ctx, tx)
}

func alertRaised(t *testing.T, w *amlWorld, op aml.Operation) bool {
	t.Helper()
	var n int
	if err := w.Owner.WithPlatformAdmin(bg, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'aml.hold_eval_failed' AND subject_tenant_id = $1 AND discriminator = $2`,
			w.Tenant, "hold_eval:"+string(op)).Scan(&n)
	}); err != nil {
		t.Fatalf("read alerts: %v", err)
	}
	return n > 0
}

// (8) The successful probe restores the caller's statement_timeout (its set_config(..., true) is undone by ROLLBACK TO
// SAVEPOINT), touches no app.* setting and leaves no savepoint.
func TestHoldProbe_Success_RestoresSettings_NoLeakedSavepoint(t *testing.T) {
	w := newWorld(t, "own_licence")
	p := w.player()
	if err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		callerSettings(t, ctx, tx)
		before := probeSnapshot(t, ctx, tx)
		for i := 0; i < 3; i++ { // cold, then warm statement cache
			held, err := aml.RealHoldProbeForTest(ctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpDeposit)
			if err != nil || held {
				t.Fatalf("run %d: probe = %v, %v; want no hold, no error", i, held, err)
			}
			usableAfter(t, ctx, tx, before)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// (1) A real evaluator error raised mid-batch by aml_hold_active (AM026: the session tenant is not the requested tenant).
func TestHoldProbe_EvaluatorErrorMidBatch_Contained(t *testing.T) {
	w := newWorld(t, "own_licence")
	p := w.player()
	if err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		callerSettings(t, ctx, tx)
		before := probeSnapshot(t, ctx, tx)
		for i := 0; i < 2; i++ { // cold, then warm statement cache
			_, err := aml.RealHoldProbeForTest(ctx, tx, uuid.New(), p.PersonID, p.AccountID, aml.OpDeposit)
			if err == nil || !strings.Contains(err.Error(), "AM026") {
				t.Fatalf("run %d: want the AM026 evaluator error, got %v", i, err)
			}
			usableAfter(t, ctx, tx, before)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// (1) lock_timeout (55P03) and the probe's own statement_timeout (57014) while aml_hold_active waits for a lock, through
// EvaluateHold: deny, the in-tx alert is written, the caller's transaction commits.
func TestHoldProbe_LockAndStatementTimeout_DenyAndCallerCommits(t *testing.T) {
	for _, tc := range []struct {
		name, lockTimeout string
		op                aml.Operation
		minWait           time.Duration
	}{
		{"lock_timeout_55P03", "150", aml.OpDeposit, 100 * time.Millisecond},
		{"statement_timeout_57014", "0", aml.OpWithdrawal, aml.HoldEvalTimeout - 200*time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, "own_licence")
			p := w.player()
			release := lockHolds(t, w)
			var v aml.HoldVerdict
			if err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
				callerSettings(t, ctx, tx)
				if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, tc.lockTimeout); err != nil {
					return err
				}
				before := probeSnapshot(t, ctx, tx)
				start := time.Now()
				var err error
				v, err = aml.EvaluateHold(ctx, tx, w.Tenant, p.PersonID, p.AccountID, tc.op)
				if err != nil {
					t.Fatalf("the in-tx alert after a contained probe failure must succeed: %v", err)
				}
				if el := time.Since(start); el < tc.minWait {
					t.Fatalf("the probe returned after %s: the injected wait did not happen", el)
				}
				release()
				usableAfter(t, ctx, tx, before)
				return nil
			}); err != nil {
				t.Fatalf("the caller's transaction must commit after the contained failure: %v", err)
			}
			if !v.EvalFailed || !v.Denied() || v.Held {
				t.Fatalf("fail closed: want EvalFailed deny, got %+v", v)
			}
			if !alertRaised(t, w, tc.op) {
				t.Fatal("the committed caller transaction must carry the aml.hold_eval_failed alert")
			}
		})
	}
}

// (2) Parse-time failure: aml_hold_active is not resolvable on the caller's search_path, so the batch fails while pgx
// PREPARES its statements (cold statement cache: Parse/Describe of every new statement, Sync, BEFORE any statement of the
// batch executes) or at Bind (warm cache: the cached plan is invalidated by the search_path change and re-analysed).
func TestHoldProbe_ParseFailure_ColdAndWarmCache_Contained(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold_cache"
		if warm {
			name = "warm_cache"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, "own_licence")
			p := w.player()
			if err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
				callerSettings(t, ctx, tx)
				if err := tx.Conn().DeallocateAll(ctx); err != nil { // a connection that never ran the probe
					return err
				}
				if warm {
					if _, err := aml.RealHoldProbeForTest(ctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpDeposit); err != nil {
						t.Fatalf("warm-up probe: %v", err)
					}
				}
				if _, err := tx.Exec(ctx, `SELECT set_config('search_path', 'pg_catalog', true)`); err != nil {
					return err
				}
				before := probeSnapshot(t, ctx, tx)
				_, err := aml.RealHoldProbeForTest(ctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpDeposit)
				if err == nil {
					t.Fatal("the probe must fail when aml_hold_active cannot be resolved")
				}
				t.Logf("probe error: %v", err)
				usableAfter(t, ctx, tx, before)
				return nil
			}); err != nil {
				t.Fatalf("the caller's transaction must commit after the contained failure: %v", err)
			}
		})
	}
}

// (2) + (7) Parse-time failure through EvaluateHold on a cold cache: the fault is scoped to the probe (search_path narrowed
// just for it, then put back), so what follows is exactly the production deny path: the in-tx aml.hold_eval_failed alert is
// written and the caller's transaction commits.
func TestHoldProbe_ParseFailure_EvaluateHold_AlertAndCommit(t *testing.T) {
	w := newWorld(t, "own_licence")
	p := w.player()
	t.Cleanup(aml.SetHoldProbeForTest(func(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op aml.Operation) (bool, error) {
		if _, err := tx.Exec(ctx, `SELECT set_config('search_path', 'pg_catalog', true)`); err != nil {
			return false, err
		}
		held, perr := aml.RealHoldProbeForTest(ctx, tx, tenantID, personID, accountID, op)
		if _, err := tx.Exec(ctx, `SELECT set_config('search_path', '"$user", public', true)`); err != nil {
			t.Errorf("the caller cannot even restore its search_path after the probe failure: %v", err)
		}
		return held, perr
	}))
	var v aml.HoldVerdict
	if err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		callerSettings(t, ctx, tx)
		before := probeSnapshot(t, ctx, tx)
		if err := tx.Conn().DeallocateAll(ctx); err != nil {
			return err
		}
		var err error
		if v, err = aml.EvaluateHold(ctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpPayout); err != nil {
			t.Fatalf("the in-tx alert after a contained Parse failure must succeed: %v", err)
		}
		usableAfter(t, ctx, tx, before)
		return nil
	}); err != nil {
		t.Fatalf("the deny path must commit: %v", err)
	}
	if !v.EvalFailed || !v.Denied() {
		t.Fatalf("fail closed: %+v", v)
	}
	if !alertRaised(t, w, aml.OpPayout) {
		t.Fatal("the committed caller transaction must carry the aml.hold_eval_failed alert")
	}
}

// (3) + (5) The caller's transaction is already aborted, so the FIRST probe statement fails. The verdict is a deny and the
// hard error is surfaced (the caller cannot write anything in that transaction anyway).
func TestHoldProbe_CallerAlreadyAborted_FirstStatementFails_Denies(t *testing.T) {
	w := newWorld(t, "own_licence")
	p := w.player()
	sentinel := errors.New("rollback")
	err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
			t.Fatal("want the caller's division by zero")
		}
		v, herr := aml.EvaluateHold(ctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpDeposit)
		if !v.EvalFailed || !v.Denied() {
			t.Fatalf("fail closed: %+v", v)
		}
		if herr == nil {
			t.Fatal("the in-tx alert cannot be written in an aborted transaction: the error must be surfaced")
		}
		if _, err := aml.RealHoldProbeForTest(ctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpDeposit); pgCode(err) != "25P02" {
			t.Fatalf("the first probe statement must fail with 25P02, got %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("unexpected: %v", err)
	}
}

// (6) A caller savepoint (pgx nested transaction, as the casino / sportsbook bet paths use) around the probe: the probe's
// failure neither releases nor rolls back the caller's savepoint, and the caller can still commit or roll back its own
// savepoint afterwards. Covered for an execution-time fault (lock timeout) and a Parse-time fault (cold cache).
func TestHoldProbe_InsideCallerSavepoint_CallerSavepointIntact(t *testing.T) {
	for _, fault := range []string{"lock_timeout", "parse_cold_cache"} {
		for _, commitNested := range []bool{true, false} {
			name := fault + map[bool]string{true: "_release_nested", false: "_rollback_nested"}[commitNested]
			t.Run(name, func(t *testing.T) {
				w := newWorld(t, "own_licence")
				p := w.player()
				if fault == "lock_timeout" {
					lockHolds(t, w)
				}
				if err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
					callerSettings(t, ctx, tx)
					outer := probeSnapshot(t, ctx, tx)
					if err := tx.Conn().DeallocateAll(ctx); err != nil {
						return err
					}
					nested, err := tx.Begin(ctx)
					if err != nil {
						return err
					}
					if _, err := nested.Exec(ctx, `SELECT set_config('igtest.marker', 'nested', true)`); err != nil {
						return err
					}
					switch fault {
					case "lock_timeout":
						if _, err := nested.Exec(ctx, `SELECT set_config('lock_timeout', '150', true)`); err != nil {
							return err
						}
					default:
						if _, err := nested.Exec(ctx, `SELECT set_config('search_path', 'pg_catalog', true)`); err != nil {
							return err
						}
					}
					inNested := probeSnapshot(t, ctx, nested)
					if _, err := aml.RealHoldProbeForTest(ctx, nested, w.Tenant, p.PersonID, p.AccountID, aml.OpCasinoStake); err == nil {
						t.Fatal("want a probe failure")
					}
					var one int
					if err := nested.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
						t.Fatalf("the caller's nested transaction is not usable after the contained probe failure: %v", err)
					}
					if got := probeSnapshot(t, ctx, nested); got != inNested {
						t.Fatalf("leak inside the caller savepoint:\n before %s\n after  %s", inNested, got)
					}
					if commitNested {
						if err := nested.Commit(ctx); err != nil {
							t.Fatalf("the caller's savepoint must still be releasable: %v", err)
						}
					} else {
						if err := nested.Rollback(ctx); err != nil {
							t.Fatalf("the caller's savepoint must still be rollback-able: %v", err)
						}
						if got := probeSnapshot(t, ctx, tx); got != outer {
							t.Fatalf("rolling back the caller savepoint must restore the outer state:\n want %s\n got  %s", outer, got)
						}
					}
					assertNoLeakedSavepoint(t, ctx, tx)
					return nil
				}); err != nil {
					t.Fatalf("the outer caller transaction must commit: %v", err)
				}
			})
		}
	}
}

// (4) A context cancelled while the batch is in flight (aml_hold_active waiting for a lock): the verdict is a deny. pgx
// tears the connection down on cancellation, so the caller's transaction is gone and cannot commit: still fail closed.
func TestHoldProbe_ContextCancelledMidBatch_Denies(t *testing.T) {
	w := newWorld(t, "own_licence")
	p := w.player()
	release := lockHolds(t, w)
	var v aml.HoldVerdict
	err := w.RT.WithTenant(bg, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		var herr error
		v, herr = aml.EvaluateHold(cctx, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpDeposit)
		release()
		t.Logf("EvaluateHold error after cancellation: %v", herr)
		var one int
		uerr := tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
		t.Logf("caller statement after cancellation: %v", uerr)
		return nil
	})
	t.Logf("caller commit after cancellation: %v", err)
	if !v.EvalFailed || !v.Denied() {
		t.Fatalf("fail closed: %+v", v)
	}
}

// countingConn counts client-to-server writes: one pipelined flush per round trip.
type countingConn struct {
	net.Conn
	writes *atomic.Int64
}

func (c countingConn) Write(b []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(b)
}

// ISO-LATENCY-1: the probe costs exactly ONE round trip (one client flush), on a cold connection (nothing prepared) and on a
// warm one alike.
func TestHoldProbe_OneRoundTrip_ColdAndWarm(t *testing.T) {
	w := newWorld(t, "own_licence")
	p := w.player()
	cfg, err := pgx.ParseConfig(os.Getenv("TEST_RUNTIME_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countingConn{Conn: c, writes: &writes}, nil
	}
	conn, err := pgx.ConnectConfig(bg, cfg)
	if err != nil {
		t.Fatalf("connect runtime: %v", err)
	}
	defer func() { _ = conn.Close(bg) }()
	tx, err := conn.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(bg) }()
	if _, err := tx.Exec(bg, `SELECT set_config('app.tenant_id', $1, true)`, w.Tenant.String()); err != nil {
		t.Fatal(err)
	}
	for i, label := range []string{"cold", "warm", "warm again"} {
		before := writes.Load()
		held, err := aml.RealHoldProbeForTest(bg, tx, w.Tenant, p.PersonID, p.AccountID, aml.OpCasinoStake)
		if err != nil || held {
			t.Fatalf("run %d: probe = %v, %v", i, held, err)
		}
		if n := writes.Load() - before; n != 1 {
			t.Errorf("%s probe: %d round trips, want exactly 1", label, n)
		}
	}
}

// The real probe decodes an active hold (the text-format boolean) and scopes it by operation.
func TestHoldProbe_ActiveHoldDecodes(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	if _, err := c.Svc.PlaceHold(bg, c.A(), aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeWithdrawalAndPayout, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); err != nil {
		t.Fatalf("place hold: %v", err)
	}
	if err := c.RT.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		for op, want := range map[aml.Operation]bool{aml.OpWithdrawal: true, aml.OpPayout: true, aml.OpDeposit: false, aml.OpCasinoStake: false} {
			held, err := aml.RealHoldProbeForTest(ctx, tx, c.Tenant, c.P.PersonID, c.P.AccountID, op)
			if err != nil || held != want {
				t.Errorf("%s: probe = %v, %v; want %v", op, held, err, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
