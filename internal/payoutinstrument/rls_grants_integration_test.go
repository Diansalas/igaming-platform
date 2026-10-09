//go:build integration

package payoutinstrument

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var b13Tables = []string{
	"payout_instrument_kinds", "payout_instrument_verification_max_age", "payout_instruments", "payout_instrument_verifications",
	"payout_instrument_fingerprint_owners", "payout_instrument_blocking_events", "payout_attempt_destination_snapshots",
}

// Grant pin (security M-8): kinds and max-age SELECT only; instruments
// SELECT/INSERT/UPDATE; verifications, owners, blocking events and snapshots
// SELECT/INSERT; nothing has DELETE / TRUNCATE / REFERENCES / TRIGGER.
func TestGrantPin_RuntimeRole(t *testing.T) {
	w := newWorld(t)
	want := map[string]string{
		"payout_instrument_kinds":                "SELECT",
		"payout_instrument_verification_max_age": "SELECT",
		"payout_instruments":                     "INSERT,SELECT,UPDATE",
		"payout_instrument_verifications":        "INSERT,SELECT",
		"payout_instrument_fingerprint_owners":   "INSERT,SELECT",
		"payout_instrument_blocking_events":      "INSERT,SELECT",
		"payout_attempt_destination_snapshots":   "INSERT,SELECT",
	}
	all := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	for _, tbl := range b13Tables {
		var have []string
		for _, priv := range all {
			var ok bool
			if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, tbl, priv).Scan(&ok)
			}); err != nil {
				t.Fatal(err)
			}
			if ok {
				have = append(have, priv)
			}
		}
		sort.Strings(have)
		if got := strings.Join(have, ","); got != want[tbl] {
			t.Errorf("runtime grants on %s = %q, want %q", tbl, got, want[tbl])
		}
	}
	// The kinds / max-age tables are not writable at all.
	exec := func(q string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }
	}
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_kinds (code, detail_schema_version, allowed_rails, non_synthetic_enabled) VALUES ('evil_kind',1,'{x}',true)`)), "42501", "insert a kind")
	requireCode(t, w.try(exec(`UPDATE payout_instrument_kinds SET non_synthetic_enabled = true WHERE code = 'crypto_address'`)), "42501", "enable crypto")
	requireCode(t, w.try(exec(`DELETE FROM payout_instrument_kinds WHERE code = 'bank_account'`)), "42501", "delete a kind")
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_verification_max_age (jurisdiction_id, max_age) VALUES (gen_random_uuid(), '1 day')`)), "42501", "write max age")
	// The seeded kinds.
	var codes []string
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT code || ':' || non_synthetic_enabled::text FROM payout_instrument_kinds ORDER BY code`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			codes = append(codes, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codes, ","); got != "bank_account:true,card_token:true,crypto_address:false,ewallet_account:true,synthetic_test:false" {
		t.Fatalf("seeded kinds = %s", got)
	}
	// The Go registry covers exactly the seeded set.
	if got := strings.Join(DefaultKinds().Codes(), ","); got != "bank_account,card_token,crypto_address,ewallet_account,synthetic_test" {
		t.Fatalf("Go kinds = %s", got)
	}
}

// Policy shape pin: ENABLE + FORCE RLS, no FOR ALL policy, the acting family
// reads ONLY the snapshots, the player family reads ONLY its own instruments.
func TestPolicyPin(t *testing.T) {
	w := newWorld(t)
	type pol struct{ table, name, cmd, qual string }
	var pols []pol
	flags := map[string][2]bool{}
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tablename, policyname, cmd, coalesce(qual,'') || ' ' || coalesce(with_check,'') FROM pg_policies WHERE schemaname='public' AND tablename = ANY($1) ORDER BY 1,2`, b13Tables)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p pol
			if err := rows.Scan(&p.table, &p.name, &p.cmd, &p.qual); err != nil {
				rows.Close()
				return err
			}
			pols = append(pols, p)
		}
		rows.Close()
		frows, err := tx.Query(ctx, `SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class WHERE relnamespace='public'::regnamespace AND relname = ANY($1)`, b13Tables)
		if err != nil {
			return err
		}
		defer frows.Close()
		for frows.Next() {
			var n string
			var e, f bool
			if err := frows.Scan(&n, &e, &f); err != nil {
				return err
			}
			flags[n] = [2]bool{e, f}
		}
		return frows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, p := range pols {
		got[p.table] = append(got[p.table], p.name+":"+p.cmd)
		if p.cmd == "ALL" {
			t.Errorf("%s.%s is a FOR ALL policy", p.table, p.name)
		}
		acting := strings.Contains(p.qual, "acting_tenant_id")
		if acting && p.table != "payout_attempt_destination_snapshots" {
			t.Errorf("an acting-family policy exists on %s (only snapshots may have one)", p.table)
		}
		if acting && (p.cmd != "SELECT" || !strings.Contains(p.qual, "financial_acting_session_valid")) {
			t.Errorf("snapshot acting policy must be SELECT with a valid acting session: %+v", p)
		}
		if strings.Contains(p.qual, "player_account_id") && strings.Contains(p.name, "player") && p.table != "payout_instruments" {
			t.Errorf("player policy outside payout_instruments: %+v", p)
		}
	}
	wantPol := map[string]string{
		"payout_instruments":                     "player_self_select:SELECT,tenant_scope_insert:INSERT,tenant_scope_select:SELECT,tenant_scope_update:UPDATE",
		"payout_instrument_verifications":        "tenant_scope_insert:INSERT,tenant_scope_select:SELECT",
		"payout_instrument_blocking_events":      "tenant_scope_insert:INSERT,tenant_scope_select:SELECT",
		"payout_instrument_fingerprint_owners":   "tenant_scope_insert:INSERT,tenant_scope_select:SELECT",
		"payout_attempt_destination_snapshots":   "acting_read:SELECT,tenant_scope_insert:INSERT,tenant_scope_select:SELECT",
		"payout_instrument_kinds":                "reference_read:SELECT",
		"payout_instrument_verification_max_age": "reference_read:SELECT",
	}
	for tbl, want := range wantPol {
		s := got[tbl]
		sort.Strings(s)
		if strings.Join(s, ",") != want {
			t.Errorf("policies on %s = %v, want %s", tbl, s, want)
		}
	}
	for _, tbl := range b13Tables {
		f := flags[tbl]
		wantForce := tbl != "payout_instrument_verification_max_age"
		if !f[0] || f[1] != wantForce {
			t.Errorf("%s: rls=%v force=%v (want true/%v)", tbl, f[0], f[1], wantForce)
		}
	}
	// No SECURITY DEFINER function among the ones this migration created.
	var definers []string
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT proname FROM pg_proc WHERE pronamespace='public'::regnamespace AND prosecdef AND (proname LIKE 'payout%' OR proname LIKE '%payout%')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			definers = append(definers, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(definers) > 0 {
		t.Errorf("SECURITY DEFINER functions: %v", definers)
	}
}

// Cross-tenant, cross-brand and cross-player isolation as the runtime role.
func TestRLS_CrossTenantBrandPlayerIsolation(t *testing.T) {
	b := newBindWorld(t) // tenant 1: verified instrument + wallet
	g := b.gateRes()
	id, fp := b.inst.ID, b.inst.Fingerprint
	wid, err := b.wd(&id, &fp)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		a, err := b.insertAttempt(ctx, tx, wid, 100)
		if err != nil {
			return err
		}
		_, err = b.svc.WriteSnapshot(ctx, tx, g, SnapshotParams{AttemptID: a, WithdrawalRequestID: wid, Amount: "100", AssetCode: "EUR"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.block(b.verified(b.p, ibanB), EventSuspend, staff(), b.p); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range b13Tables[2:] {
		if n := b.count(tbl, "tenant_id = $1", b.tenantID); n < 1 {
			t.Fatalf("setup: tenant 1 has no rows in %s", tbl)
		}
	}

	// Tenant 2 (and no tenant at all) sees none of tenant 1's rows, in any table,
	// and cannot insert a row naming tenant 1.
	t2 := newWorld(t)
	for _, tbl := range b13Tables[2:] {
		var n int
		if err := t2.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id = $1`, b.tenantID).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("tenant 2 sees %d row(s) of tenant 1 in %s (%v)", n, tbl, err)
		}
		if err := b.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("a session with no tenant sees %d row(s) in %s (%v)", n, tbl, err)
		}
	}
	err = t2.try(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id) VALUES ($1,'f1',repeat('9',64),$2)`, b.tenantID, b.p.PersonID)
		return err
	})
	requireCode(t, err, "42501", "tenant 2 inserting a tenant 1 row")
	// Tenant 2 cannot register for tenant 1's player.
	_, err = t2.registerWith(t2.svc, RegisterParams{TenantID: t2.tenantID, PlayerAccountID: b.p.ID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"}, Detail: ibanDetail(ibanA)})
	if err == nil {
		t.Error("tenant 2 registering for another tenant's player must be refused")
	}
	// And a gate in tenant 2 cannot see tenant 1's instrument.
	err = t2.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := t2.svc.EvaluateGate(ctx, tx, GateParams{TenantID: t2.tenantID, BrandID: b.p.BrandID, PlayerAccountID: b.p.ID, PersonID: b.p.PersonID, InstrumentID: id, AssetCode: "EUR"})
		return err
	})
	requireGateReason(t, err, ReasonNotFound, false)

	// Player scope: a player sees only its own instruments, nothing else, and writes nothing.
	other := b.newPlayer(b.brandID)
	otherBrand := b.newPlayer(b.brand2ID)
	oi := b.verified(other, "FR1420041010050500013M02606")
	obi := b.verified(otherBrand, "NL91ABNA0417164300")
	playerCount := func(pl player, q string, args ...any) int {
		var n int
		if err := b.rt.WithPlayerScope(context.Background(), b.tenantID, pl.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, q, args...).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := playerCount(b.p, `SELECT count(*) FROM payout_instruments`); n != 2 {
		t.Errorf("player 1 sees %d instruments, want its own 2", n)
	}
	for _, id := range []uuid.UUID{oi.ID, obi.ID} {
		if n := playerCount(b.p, `SELECT count(*) FROM payout_instruments WHERE id = $1`, id); n != 0 {
			t.Errorf("player 1 sees another player's instrument %s", id)
		}
	}
	if n := playerCount(otherBrand, `SELECT count(*) FROM payout_instruments WHERE id = $1`, oi.ID); n != 0 {
		t.Error("a player of brand 2 sees a brand 1 player's instrument")
	}
	for _, tbl := range b13Tables[3:] {
		if n := playerCount(b.p, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("player scope sees %d row(s) of %s", n, tbl)
		}
	}
	if err := b.rt.WithPlayerScope(context.Background(), b.tenantID, b.p.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payout_instruments SET state = 'revoked' WHERE id = $1`, b.inst.ID)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s := b.load(b.inst.ID).State; s != StateVerified {
		t.Fatalf("a player-scope UPDATE changed state to %s (must affect no row)", s)
	}
	err = b.rt.WithPlayerScope(context.Background(), b.tenantID, b.p.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_blocking_events (tenant_id, instrument_id, event, actor_type, actor_id, reason_code, occurred_at, event_seal, seal_kid, created_txid)
			VALUES ($1,$2,'revoke','player','x','r',now(),repeat('a',64),'m1',0)`, b.tenantID, b.inst.ID)
		return err
	})
	requireCode(t, err, "42501", "player-scope insert")

	// Cross-brand structural: an instrument cannot name a brand other than the account's.
	err = b.try(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payout_instruments SET brand_id = $2 WHERE id = $1`, b.inst.ID, b.brand2ID)
		return err
	})
	requireCode(t, err, "PI011", "re-brand an instrument")

	// Acting family: without a VALID acting session an acting GUC shape reads nothing,
	// from the instruments (no acting policy) or the snapshots (needs a valid session).
	for _, tbl := range []string{"payout_instruments", "payout_attempt_destination_snapshots", "payout_instrument_verifications"} {
		var n int
		if err := b.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			for _, kv := range [][2]string{{"app.acting_tenant_id", b.tenantID.String()}, {"app.acting_principal_id", uuid.NewString()}, {"app.tenant_id", b.tenantID.String()}} {
				if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, kv[0], kv[1]); err != nil {
					return err
				}
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("an invalid acting session reads %d row(s) of %s (%v)", n, tbl, err)
		}
	}
	_ = fmt.Sprint
}

// Revoke racing a (re-)registration of the same destination: every interleaving
// ends with the old instrument revoked and AT MOST one live instrument.
func TestConcurrency_RevokeVsRegister(t *testing.T) {
	w := newWorld(t)
	for round := 0; round < 25; round++ {
		p := w.newPlayer(w.brandID)
		d := fmt.Sprintf(`{"country":"BR","account_number":"CONC%05d"}`, round)
		rp := RegisterParams{TenantID: w.tenantID, PlayerAccountID: p.ID, Kind: KindBankAccount, Rail: "pix", AssetCodes: []string{"EUR"}, Detail: []byte(d)}
		old, err := w.registerWith(w.svc, rp)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var rerr, gerr error
		var got RegisterResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			rerr = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.svc.Revoke(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: old.Instrument.ID, PlayerAccountID: p.ID, Actor: Actor{Type: ActorPlayer, ID: p.ID.String()}, ReasonCode: "player_revoked"})
				return err
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			got, gerr = w.registerWith(w.svc, rp)
		}()
		close(start)
		wg.Wait()
		if rerr != nil || gerr != nil {
			t.Fatalf("round %d: revoke=%v register=%v", round, rerr, gerr)
		}
		if s := w.load(old.Instrument.ID).State; s != StateRevoked {
			t.Fatalf("round %d: old instrument state %s", round, s)
		}
		live := w.count("payout_instruments", "player_account_id = $1 AND state IN ('pending_verification','verified','verification_expired','suspended')", p.ID)
		if live > 1 {
			t.Fatalf("round %d: %d live instruments for one destination", round, live)
		}
		if !got.Existing && live != 1 {
			t.Fatalf("round %d: register created a new instrument but %d are live", round, live)
		}
	}
}
