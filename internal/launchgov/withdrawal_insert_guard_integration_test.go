//go:build integration

// Migration 0131 (ADR 0112 slice 4; MONEY-PATH TRIGGER, ledger-finance review): the database refuses
// the CREATION of a withdrawal request for a tenant or brand that is not active, for every writer,
// including a RAW INSERT by the runtime credential that bypasses the Go gate (slice-3 residual
// 14(b)). Every assertion runs as igaming_runtime unless it says otherwise; the owner seeds
// fixtures. A skip is NOT evidence.
package launchgov_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
)

type wdFx struct {
	tid, brand, player, wallet, instrument uuid.UUID
	fingerprint                            string
}

// wdFixture seeds an ACTIVE tenant and brand, a player with an EUR wallet and a verified payout
// instrument (so the 0126 binding guard is satisfied and a refusal can only be the status guard).
func (w *world) wdFixture() wdFx {
	w.t.Helper()
	tid := w.activeTenant()
	b := w.activeBrand(tid)
	return w.wdFixtureIn(tid, b)
}

func (w *world) wdFixtureIn(tid, b uuid.UUID) wdFx {
	w.t.Helper()
	player, wl := w.playerWithWallet(tid, b)
	inst := pitest.Bind(w.t, w.owner, tid, player, "EUR")
	f := wdFx{tid: tid, brand: b, player: player, wallet: wl, instrument: inst}
	if err := w.owner.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT fingerprint FROM payout_instruments WHERE id = $1`, inst).Scan(&f.fingerprint)
	}); err != nil {
		w.t.Fatalf("instrument fingerprint: %v", err)
	}
	return f
}

const rawWithdrawalInsert = `INSERT INTO withdrawal_requests
	(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
	VALUES ($1, $2, $3, $4, $5, 'EUR', 1000, $6, $7, $8)`

// rawInsertRT is a RAW insert as the runtime role in the tenant's own scope (no Go gate). Since ADR
// 0113 (migration 0132) a request commits only with its hold leg linked (check A, WD031 otherwise), so
// the raw insert is the legal E0 + E0b path of the shared helper (wdpath.Plant: the same INSERT, then
// the withdrawal_requested hold leg and its link), still with no Go gate. A refused INSERT (WD010 /
// WD011) is refused at its first statement exactly as before.
func (w *world) rawInsertRT(f wdFx, key string) error {
	return w.rt.WithTenant(context.Background(), f.tid, func(ctx context.Context, tx pgx.Tx) error {
		return w.rawPlant(ctx, tx, f, key)
	})
}

func (w *world) rawPlant(ctx context.Context, tx pgx.Tx, f wdFx, key string) error {
	inst, fp := f.instrument, f.fingerprint
	_, err := wdpath.Plant(ctx, tx, wdpath.Spec{TenantID: f.tid, BrandID: f.brand, PlayerAccountID: f.player, WalletID: f.wallet,
		AssetCode: "EUR", Amount: 1000, IdempotencyKey: key, InstrumentID: &inst, Fingerprint: &fp})
	return err
}

// A raw runtime INSERT is refused for every non-active tenant / brand status (WD010 tenant, WD011
// brand), creates nothing, and is accepted for an active subject (the control that makes the
// refusals non-vacuous). A sibling brand of a suspended brand is unaffected.
func TestWithdrawalInsertGuard_RawRuntimeInsert_RefusedOnNonActiveSubject(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Control: active / active.
	ctl := w.wdFixture()
	if err := w.rawInsertRT(ctl, "ctl-"+uuid.NewString()); err != nil {
		t.Fatalf("control: a raw insert on an active tenant and brand must succeed: %v", err)
	}
	if n := w.withdrawalCount(ctl.tid); n != 1 {
		t.Fatalf("control row: %d", n)
	}

	cases := []struct {
		name string
		want string
		set  func(f wdFx) error
	}{
		{"brand pending_launch", "WD011", func(f wdFx) error { return launchfix.ForcePending(ctx, t, w.owner, f.tid, &f.brand) }},
		{"brand suspended", "WD011", func(f wdFx) error { return launchfix.TrySetBrandStatus(ctx, f.tid, f.brand, "suspended") }},
		{"brand closed", "WD011", func(f wdFx) error { return launchfix.TrySetBrandStatus(ctx, f.tid, f.brand, "closed") }},
		{"tenant pending_launch", "WD010", func(f wdFx) error { return launchfix.ForcePending(ctx, t, w.owner, f.tid, nil) }},
		{"tenant suspended", "WD010", func(f wdFx) error { return launchfix.TrySetTenantStatus(ctx, f.tid, "suspended") }},
		{"tenant closed", "WD010", func(f wdFx) error { return launchfix.TrySetTenantStatus(ctx, f.tid, "closed") }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := w.wdFixture()
			if err := c.set(f); err != nil {
				t.Fatalf("set status: %v", err)
			}
			err := w.rawInsertRT(f, "raw-"+uuid.NewString())
			if got := pgCode(err); got != c.want {
				t.Fatalf("a raw runtime INSERT on a %s subject must be refused %s by the database, got %v", c.name, c.want, err)
			}
			if n := w.withdrawalCount(f.tid); n != 0 {
				t.Fatalf("a refused insert creates nothing, got %d rows", n)
			}
		})
	}

	// Both non-active: the tenant is reported first (lock and check order tenant, then brand).
	both := w.wdFixture()
	if err := launchfix.TrySetBrandStatus(ctx, both.tid, both.brand, "suspended"); err != nil {
		t.Fatal(err)
	}
	if err := launchfix.TrySetTenantStatus(ctx, both.tid, "suspended"); err != nil {
		t.Fatal(err)
	}
	if got := pgCode(w.rawInsertRT(both, "both-"+uuid.NewString())); got != "WD010" {
		t.Fatalf("tenant and brand non-active: want WD010, got %q", got)
	}

	// A sibling brand of the same tenant stays insertable while this brand is suspended.
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	f1, f2 := w.wdFixtureIn(tid, b1), w.wdFixtureIn(tid, b2)
	if err := launchfix.TrySetBrandStatus(ctx, tid, b1, "suspended"); err != nil {
		t.Fatal(err)
	}
	if got := pgCode(w.rawInsertRT(f1, "sib1-"+uuid.NewString())); got != "WD011" {
		t.Fatalf("suspended brand: %q", got)
	}
	if err := w.rawInsertRT(f2, "sib2-"+uuid.NewString()); err != nil {
		t.Fatalf("the sibling (active) brand must accept the insert: %v", err)
	}
}

// Fail closed: a session that cannot see the subject rows (a player scope) is refused by the guard
// itself (WD010), not waved through; without the guard RLS would have answered 42501, so the code
// proves the guard ran first. The owner role has no exemption either.
func TestWithdrawalInsertGuard_InvisibleSubject_FailsClosed_OwnerNotExempt(t *testing.T) {
	w := newWorld(t)
	f := w.wdFixture()
	insert := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, rawWithdrawalInsert, uuid.New(), f.tid, f.brand, f.player, f.wallet, "inv-"+uuid.NewString(), f.instrument, f.fingerprint)
		return err
	}
	// A PLAYER-scoped session cannot read the tenants row (tenants_read hides it from player scopes):
	// the guard reads no status and refuses (WD010) instead of waving the row through. Without the
	// guard the answer would be the binding guard (PI040) or RLS, so the code proves the guard ran.
	if got := pgCode(w.rt.WithPlayerScope(context.Background(), f.tid, f.player, insert)); got != "WD010" {
		t.Fatalf("a session that cannot see the tenant row: want WD010 (fail closed), got %q", got)
	}
	// The owner is bound by the same refusal on a non-active subject.
	if err := launchfix.TrySetBrandStatus(context.Background(), f.tid, f.brand, "suspended"); err != nil {
		t.Fatal(err)
	}
	err := w.owner.WithTenant(context.Background(), f.tid, insert)
	if got := pgCode(err); got != "WD011" {
		t.Fatalf("the owner role has no exemption: want WD011, got %v", err)
	}
}

// NO replay short-circuit (security C1): an INSERT whose key already exists on a non-active subject
// is refused by the status guard like any other (the Go path answers replays before it inserts).
func TestWithdrawalInsertGuard_ReplayOfExistingKey_IsStillGuarded(t *testing.T) {
	w := newWorld(t)
	f := w.wdFixture()
	key := "replay-" + uuid.NewString()
	if err := w.rawInsertRT(f, key); err != nil {
		t.Fatal(err)
	}
	if err := launchfix.TrySetTenantStatus(context.Background(), f.tid, "suspended"); err != nil {
		t.Fatal(err)
	}
	if got := pgCode(w.rawInsertRT(f, key)); got != "WD010" {
		t.Fatalf("an INSERT of an existing key on a suspended tenant must be refused WD010, got %q", got)
	}
	if got := pgCode(w.rawInsertRT(f, "new-"+uuid.NewString())); got != "WD010" {
		t.Fatalf("a NEW key on the suspended tenant must be refused WD010, got %q", got)
	}
	if n := w.withdrawalCount(f.tid); n != 1 {
		t.Fatalf("exactly the original row, got %d", n)
	}
}

// RACE, order 1: a raw insert holds the status keys SHARED (uncommitted) when a closure arrives. The
// closure waits; once the insert commits the closure is refused LA023 (the committed request is
// hold-bearing). The raw insert is the case the Go gate does not cover. Brand and tenant.
func TestWithdrawalInsertGuard_Race_InsertHoldsKeys_ClosureWaits_ThenLA023(t *testing.T) {
	for _, kind := range []string{"brand", "tenant"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			f := w.wdFixture()
			sub := launchgov.Subject{TenantID: f.tid}
			if kind == "brand" {
				sub.BrandID = &f.brand
			}
			started, release := make(chan struct{}), make(chan struct{})
			insDone := make(chan error, 1)
			go func() {
				insDone <- w.rt.WithTenant(ctx, f.tid, func(ctx context.Context, tx pgx.Tx) error {
					if err := w.rawPlant(ctx, tx, f, "race1-"+uuid.NewString()); err != nil {
						return err
					}
					close(started)
					<-release
					return nil
				})
			}()
			select {
			case <-started:
			case err := <-insDone:
				t.Fatalf("the raw insert failed: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("the raw insert did not start")
			}
			closeDone := make(chan error, 1)
			go func() {
				if kind == "brand" {
					closeDone <- launchfix.TrySetBrandStatus(ctx, f.tid, f.brand, "closed")
				} else {
					closeDone <- launchfix.TrySetTenantStatus(ctx, f.tid, "closed")
				}
			}()
			select {
			case err := <-closeDone:
				close(release)
				t.Fatalf("the closure must WAIT for the uncommitted raw insert (the guard holds the status key shared), returned: %v", err)
			case <-time.After(800 * time.Millisecond):
			}
			close(release)
			if err := <-insDone; err != nil {
				t.Fatalf("the raw insert must commit: %v", err)
			}
			select {
			case err := <-closeDone:
				if got := pgCode(err); got != "LA023" {
					t.Fatalf("the closure must then be refused LA023, got %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the closure never returned")
			}
			if st := w.status(sub); st != "active" {
				t.Fatalf("the subject must stay active, got %s", st)
			}
		})
	}
}

// RACE, order 2: the closure holds the status key EXCLUSIVE (uncommitted) when a raw insert arrives.
// The insert waits, the closure commits, the insert is then refused (WD011 brand / WD010 tenant) and
// creates nothing: no request can slip in behind a committed closure.
func TestWithdrawalInsertGuard_Race_ClosureHoldsKeys_InsertWaits_ThenRefused(t *testing.T) {
	for _, kind := range []string{"brand", "tenant"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			f := w.wdFixture()
			insDone := make(chan error, 1)
			blocked := false
			hold := func() {
				go func() { insDone <- w.rawInsertRT(f, "race2-"+uuid.NewString()) }()
				select {
				case err := <-insDone:
					insDone <- err
				case <-time.After(800 * time.Millisecond):
					blocked = true
				}
			}
			var err error
			want := "WD011"
			if kind == "brand" {
				err = launchfix.TrySetBrandStatusHoldingOn(ctx, t, w.owner, f.tid, f.brand, "closed", hold)
			} else {
				want = "WD010"
				err = launchfix.TrySetTenantStatusHoldingOn(ctx, t, w.owner, f.tid, "closed", hold)
			}
			if err != nil {
				t.Fatalf("the closure (no request existed) must commit: %v", err)
			}
			if !blocked {
				t.Fatal("the raw insert must WAIT while the closure holds the status key")
			}
			select {
			case ierr := <-insDone:
				if got := pgCode(ierr); got != want {
					t.Fatalf("after the closure committed the insert must be refused %s, got %v", want, ierr)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the raw insert never returned")
			}
			if n := w.withdrawalCount(f.tid); n != 0 {
				t.Fatalf("no request may exist behind a committed closure, got %d", n)
			}
		})
	}
}

// Catalog: the trigger is a row-level BEFORE INSERT trigger, enabled, firing BEFORE the payout
// binding guard (name order), its function pins search_path and is not SECURITY DEFINER, and it
// shares the lock keys with the status writers.
func TestWithdrawalInsertGuard_Catalog(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tgname FROM pg_trigger
			WHERE tgrelid = 'withdrawal_requests'::regclass AND NOT tgisinternal
			  AND (tgtype & 1) = 1 AND (tgtype & 2) = 2 AND (tgtype & 4) = 4 AND tgenabled = 'O'
			ORDER BY tgname`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			names = append(names, n)
		}
		// ADR 0113 (migration 0132): the state-transition guard (WD025, a pure shape check that takes no
		// lock) sorts FIRST, then this status guard, then the binding guard; the status guard is still
		// before every trigger that takes a lock or a row (stricter pin than before: the full prefix).
		if len(names) < 2 || names[0] != "withdrawal_requests_aa_state_transition_guard" || names[1] != "withdrawal_requests_aa_subject_active_guard" {
			t.Fatalf("BEFORE INSERT order must start [aa_state_transition_guard, aa_subject_active_guard], got %v", names)
		}
		foundBinding := false
		for _, n := range names {
			foundBinding = foundBinding || n == "withdrawal_requests_payout_binding_guard"
		}
		if !foundBinding {
			t.Fatalf("the binding guard must still exist: %v", names)
		}
		var secdef bool
		var cfg []string
		var src string
		if err := tx.QueryRow(ctx, `SELECT prosecdef, coalesce(proconfig, '{}'), prosrc FROM pg_proc WHERE proname = 'withdrawal_requests_subject_active_guard'`).Scan(&secdef, &cfg, &src); err != nil {
			return err
		}
		if secdef {
			t.Fatal("the guard must not be SECURITY DEFINER")
		}
		pinned := false
		for _, c := range cfg {
			pinned = pinned || c == "search_path=pg_catalog, public, pg_temp"
		}
		if !pinned {
			t.Fatalf("search_path must be pinned: %v", cfg)
		}
		for _, want := range []string{"tenant_status_gate_key", "brand_status_gate_key", "pg_advisory_xact_lock_shared", "WD010", "WD011"} {
			if !containsSub(src, want) {
				t.Fatalf("the guard body must contain %q", want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
