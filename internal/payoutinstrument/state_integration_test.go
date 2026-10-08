//go:build integration

package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errRollback = errors.New("rollback")

// try runs fn in a runtime tenant transaction that is ALWAYS rolled back and
// returns fn's error (nil if fn succeeded).
func (w *world) try(fn func(ctx context.Context, tx pgx.Tx) error) error {
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return errRollback
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	return err
}

func (w *world) tryOwner(fn func(ctx context.Context, tx pgx.Tx) error) error {
	err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return errRollback
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	return err
}

// ownerUnforced runs stmt as the table OWNER with row-level security un-forced
// (so the statement reaches the row triggers instead of being filtered to zero
// rows) in a transaction that is always rolled back. It returns the error.
func (w *world) ownerUnforced(table, stmt string, args ...any) error {
	return w.tryOwner(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE `+table+` NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, stmt, args...)
		return err
	})
}

func (w *world) block(inst Instrument, event string, actor Actor, p player) error {
	return w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		bp := BlockParams{TenantID: w.tenantID, InstrumentID: inst.ID, Actor: actor, ReasonCode: "test_reason"}
		var err error
		if event == EventRevoke {
			_, err = w.svc.Revoke(ctx, tx, bp)
		} else {
			_, err = w.svc.Suspend(ctx, tx, bp)
		}
		return err
	})
}

func staff() Actor { return Actor{Type: ActorStaff, ID: uuid.NewString()} }

func TestStateMachine_TransitionsAndTerminalStates(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)

	// --- pending: no bare un-blocking, illegal edges, identity columns ---
	pend := w.mustRegister(p, ibanA)
	exec := func(q string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }
	}
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verified' WHERE id=$1`, pend.ID)), "PI014", "pending -> verified by a bare UPDATE")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='suspended' WHERE id=$1`, pend.ID)), "PI013", "pending -> suspended is not an edge")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verification_expired' WHERE id=$1`, pend.ID)), "PI013", "pending -> expired is not an edge")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET rail='pix' WHERE id=$1`, pend.ID)), "PI011", "identity column")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET display_mask='XX****0000' WHERE id=$1`, pend.ID)), "PI011", "mask column")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET detail_ciphertext=detail_ciphertext || '\x00'::bytea WHERE id=$1`, pend.ID)), "PI011", "ciphertext column")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET current_verification_id=gen_random_uuid() WHERE id=$1`, pend.ID)), "PI012", "pointer without a transition")

	// --- verified -> suspended: needs the same-tx event ---
	inst := w.verified(p, ibanB)
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='suspended' WHERE id=$1`, inst.ID)), "PI015", "suspend without a blocking event")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='revoked' WHERE id=$1`, inst.ID)), "PI015", "revoke without a blocking event")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verification_expired' WHERE id=$1`, inst.ID)), "PI016", "expiry before the verification expired")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='pending_verification' WHERE id=$1`, inst.ID)), "PI013", "verified -> pending")
	// An event of the wrong kind does not authorise the transition.
	requireCode(t, w.try(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO payout_instrument_blocking_events (tenant_id, instrument_id, event, actor_type, actor_id, reason_code, occurred_at, event_seal, seal_kid, created_txid)
			VALUES ($1,$2,'revoke','staff','x','r',now(),repeat('a',64),'m1',0)`, w.tenantID, inst.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payout_instruments SET state='suspended' WHERE id=$1`, inst.ID)
		return err
	}), "PI015", "a revoke event does not authorise a suspension")

	// --- state_changed_at is DB-forced ---
	before := w.load(inst.ID).StateChangedAt
	if err := w.try(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payout_instruments SET state_changed_at = '2001-01-01' WHERE id=$1`, inst.ID); err != nil {
			return err
		}
		var got time.Time
		if err := tx.QueryRow(ctx, `SELECT state_changed_at FROM payout_instruments WHERE id=$1`, inst.ID).Scan(&got); err != nil {
			return err
		}
		if !got.Equal(before) {
			return errors.New("state_changed_at moved without a state change")
		}
		return nil
	}); err != nil {
		t.Fatalf("forced state_changed_at (no state change): %v", err)
	}
	if err := w.block(inst, EventSuspend, staff(), p); err != nil {
		t.Fatal(err)
	}
	suspended := w.load(inst.ID)
	if suspended.State != StateSuspended || !suspended.StateChangedAt.After(before) || time.Since(suspended.StateChangedAt) > time.Minute {
		t.Fatalf("state_changed_at after suspend = %v (before %v)", suspended.StateChangedAt, before)
	}

	// --- un-suspend via the OLD verification is refused (SQL, runtime role) ---
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verified' WHERE id=$1`, inst.ID)), "PI014", "un-suspend by a bare UPDATE")
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verified', current_verification_id=$2 WHERE id=$1`, inst.ID, *inst.CurrentVerificationID)),
		"PI014", "un-suspend re-pointing at the OLD verification")
	// ... and from the owner session too (triggers fire in every session).
	requireCode(t, w.tryOwner(exec(`UPDATE payout_instruments SET state='verified' WHERE id=$1`, inst.ID)), "PI014", "un-suspend, owner session")
	// A same-transaction verification ROW with a junk seal moves the state (the
	// database cannot check a MAC) - and the Go gate then refuses it.
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		vid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verifications (id, tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
			VALUES ($1,$2,$3,'synthetic','synthetic_asserted','forged','verified',now(), now() + interval '1 day', repeat('b',64),'m1',0)`, vid, w.tenantID, inst.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payout_instruments SET state='verified', current_verification_id=$2 WHERE id=$1`, inst.ID, vid)
		return err
	}); err != nil {
		t.Fatalf("forged same-tx verification should satisfy the structural rule: %v", err)
	}
	_, gerr := w.gate(w.load(inst.ID), p, "EUR", nil)
	requireGateReason(t, gerr, ReasonSealInvalid, true)

	// A same-transaction verification whose outcome is REJECTED cannot lift a suspension.
	requireCode(t, w.try(func(ctx context.Context, tx pgx.Tx) error {
		vid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verifications (id, tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
			VALUES ($1,$2,$3,'synthetic','synthetic_asserted','forged','rejected',now(), now() + interval '1 day', repeat('b',64),'m1',0)`, vid, w.tenantID, inst.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payout_instruments SET state='verified', current_verification_id=$2 WHERE id=$1`, inst.ID, vid)
		return err
	}), "PI014", "a rejected same-tx verification must not lift a suspension")
	// A same-transaction verification of ANOTHER instrument cannot either.
	other := w.verified(p, "BE68539007547034")
	requireCode(t, w.try(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payout_instruments SET state='verified', current_verification_id=$2 WHERE id=$1`, inst.ID, *other.CurrentVerificationID)
		return err
	}), "PI014", "pointing at another instrument's verification")

	// --- terminal states are terminal in every session ---
	rev := w.verified(p, "FR1420041010050500013M02606")
	if err := w.block(rev, EventRevoke, Actor{Type: ActorPlayer, ID: p.ID.String()}, p); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"verified", "pending_verification", "suspended", "revoked", "rejected"} {
		for who, run := range map[string]func(func(context.Context, pgx.Tx) error) error{"runtime": w.try, "owner": w.tryOwner} {
			requireCode(t, run(exec(`UPDATE payout_instruments SET state=$2 WHERE id=$1`, rev.ID, target)), "PI010", "un-revoke to "+target+" ("+who+")")
		}
	}
	// A no-op UPDATE of a terminal row is refused too.
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='revoked' WHERE id=$1`, rev.ID)), "PI010", "terminal row, any UPDATE")

	// rejected is terminal
	pr := w.newPlayer(w.brandID)
	rejInst, err := w.registerWith(w.svc, RegisterParams{TenantID: w.tenantID, PlayerAccountID: pr.ID, Kind: KindSyntheticTest, Rail: "synthetic", AssetCodes: []string{"EUR"}, Detail: []byte(`{"label":"reject"}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.svc.Verify(context.Background(), w.rt, w.tenantID, rejInst.Instrument.ID)
	if err != nil || res.State != StateRejected || res.Verified {
		t.Fatalf("a rejecting verifier must reject: %+v %v", res, err)
	}
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verified' WHERE id=$1`, rejInst.Instrument.ID)), "PI010", "un-reject")
	// Revoking / suspending a rejected (terminal) instrument is a typed refusal, not a database error.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Revoke(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: rejInst.Instrument.ID, PlayerAccountID: pr.ID,
			Actor: Actor{Type: ActorPlayer, ID: pr.ID.String()}, ReasonCode: "player_revoked"})
		return err
	})
	if !errors.Is(err, ErrBadTransition) {
		t.Fatalf("revoking a rejected instrument = %v, want ErrBadTransition", err)
	}
	if _, err := w.svc.Verify(context.Background(), w.rt, w.tenantID, rejInst.Instrument.ID); !errors.Is(err, ErrNotVerifiable) {
		t.Fatalf("a rejected instrument is not verifiable again: %v", err)
	}

	// --- no DELETE / TRUNCATE anywhere ---
	for _, tbl := range []string{"payout_instruments", "payout_instrument_verifications", "payout_instrument_blocking_events", "payout_instrument_fingerprint_owners"} {
		if err := w.try(exec(`DELETE FROM `+tbl+` WHERE tenant_id=$1`, w.tenantID)); pgState(err) != "42501" {
			t.Errorf("runtime DELETE on %s: want 42501, got %v", tbl, err)
		}
		if err := w.ownerUnforced(tbl, `DELETE FROM `+tbl+` WHERE tenant_id=$1`, w.tenantID); err == nil || pgState(err) == "" {
			t.Errorf("owner DELETE on %s must be refused by the deny trigger, got %v", tbl, err)
		}
		if err := w.ownerUnforced(tbl, `TRUNCATE `+tbl+` CASCADE`); err == nil {
			t.Errorf("owner TRUNCATE on %s must be refused", tbl)
		}
	}
}

func TestBlockingEvents_AppendOnlyAndSealedAtBirth(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	inst := w.verified(p, ibanA)
	if err := w.block(inst, EventSuspend, staff(), p); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }
	}
	// Deleting / editing a blocking event is refused (runtime: no grant; owner: deny trigger).
	if err := w.try(exec(`DELETE FROM payout_instrument_blocking_events WHERE instrument_id=$1`, inst.ID)); pgState(err) != "42501" {
		t.Errorf("runtime delete: %v", err)
	}
	if err := w.try(exec(`UPDATE payout_instrument_blocking_events SET reason_code='x' WHERE instrument_id=$1`, inst.ID)); pgState(err) != "42501" {
		t.Errorf("runtime update: %v", err)
	}
	if err := w.ownerUnforced("payout_instrument_blocking_events", `DELETE FROM payout_instrument_blocking_events WHERE instrument_id=$1`, inst.ID); err == nil {
		t.Error("owner delete must be refused by the deny trigger")
	}
	if err := w.ownerUnforced("payout_instrument_blocking_events", `UPDATE payout_instrument_blocking_events SET reason_code='x' WHERE instrument_id=$1`, inst.ID); err == nil {
		t.Error("owner update must be refused by the deny trigger")
	}
	if n := w.count("payout_instrument_blocking_events", "instrument_id = $1", inst.ID); n != 1 {
		t.Errorf("the blocking event must survive every attempt, rows = %d", n)
	}
	// occurred_at is the transaction time (the Go seal reads it first).
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_blocking_events (tenant_id, instrument_id, event, actor_type, actor_id, reason_code, occurred_at, event_seal, seal_kid, created_txid)
		VALUES ($1,$2,'suspend','staff','x','r', now() - interval '1 day', repeat('a',64),'m1',0)`, w.tenantID, inst.ID)), "PI032", "backdated blocking event")
	// Same for verifications.
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_verifications (tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
		VALUES ($1,$2,'synthetic','synthetic_asserted','x','verified', now() - interval '1 day', now() + interval '1 day', repeat('a',64),'m1',0)`, w.tenantID, inst.ID)), "PI022", "backdated verification")
	// source/ownership coupling CHECK.
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_verifications (tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
		VALUES ($1,$2,'synthetic','account_holder_matches_verified_identity','x','verified', now(), now() + interval '1 day', repeat('a',64),'m1',0)`, w.tenantID, inst.ID)), "23514", "synthetic source with a real ownership assertion")
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_verifications (tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
		VALUES ($1,$2,'psp_account_verification','synthetic_asserted','x','verified', now(), now() + interval '1 day', repeat('a',64),'m1',0)`, w.tenantID, inst.ID)), "23514", "real source with a synthetic assertion")
	// expires_at NOT NULL / > verified_at.
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_verifications (tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
		VALUES ($1,$2,'synthetic','synthetic_asserted','x','verified', now(), now(), repeat('a',64),'m1',0)`, w.tenantID, inst.ID)), "23514", "expires_at not after verified_at")
	// A blocking event for a terminal instrument is refused.
	rev := w.verified(p, ibanB)
	if err := w.block(rev, EventRevoke, Actor{Type: ActorPlayer, ID: p.ID.String()}, p); err != nil {
		t.Fatal(err)
	}
	requireCode(t, w.try(exec(`INSERT INTO payout_instrument_blocking_events (tenant_id, instrument_id, event, actor_type, actor_id, reason_code, occurred_at, event_seal, seal_kid, created_txid)
		VALUES ($1,$2,'suspend','staff','x','r', now(), repeat('a',64),'m1',0)`, w.tenantID, rev.ID)), "PI031", "event for a terminal instrument")
	// Idempotent revoke: a second revoke writes no second event.
	before := w.count("payout_instrument_blocking_events", "instrument_id = $1", rev.ID)
	if err := w.block(rev, EventRevoke, Actor{Type: ActorPlayer, ID: p.ID.String()}, p); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
	if after := w.count("payout_instrument_blocking_events", "instrument_id = $1", rev.ID); after != before {
		t.Fatalf("a repeat revoke wrote a second event (%d -> %d)", before, after)
	}
	// A blocking transition on a terminal instrument is a typed refusal (no event is written).
	eventsBefore := w.count("payout_instrument_blocking_events", "instrument_id = $1", rev.ID)
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Suspend(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: rev.ID, Actor: staff(), ReasonCode: "aml_review"})
		return err
	})
	if !errors.Is(err, ErrBadTransition) {
		t.Fatalf("suspending a revoked instrument = %v, want ErrBadTransition", err)
	}
	if n := w.count("payout_instrument_blocking_events", "instrument_id = $1", rev.ID); n != eventsBefore {
		t.Fatalf("a refused suspension wrote an event (%d -> %d)", eventsBefore, n)
	}
	// A suspension needs a verified or expired instrument: a pending one is refused.
	pendingInst := w.mustRegister(p, "NL91ABNA0417164300")
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Suspend(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: pendingInst.ID, Actor: staff(), ReasonCode: "aml_review"})
		return err
	})
	if !errors.Is(err, ErrBadTransition) {
		t.Fatalf("suspending a pending instrument = %v, want ErrBadTransition", err)
	}
	// Another player's instrument cannot be revoked by this player (404 shape).
	other := w.newPlayer(w.brandID)
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Revoke(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: inst.ID, PlayerAccountID: other.ID, Actor: Actor{Type: ActorPlayer, ID: other.ID.String()}, ReasonCode: "x_y"})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-player revoke = %v, want ErrNotFound", err)
	}
	// Bad reason codes are refused before any write.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Revoke(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: inst.ID, Actor: staff(), ReasonCode: "Bad Reason!"})
		return err
	})
	if !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("bad reason code = %v", err)
	}
}

func TestPlantedRows_NoOwnerRowAndUnsealedPending(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	good := w.mustRegister(p, ibanA)

	// A planted instrument whose fingerprint has no owner row is refused structurally.
	plant := func(q string, fp, seal string) error {
		return w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, uuid.New(), w.tenantID, w.brandID, p.ID, p.PersonID, good.DetailCiphertext, good.DetailNonce, fp, seal)
			return err
		})
	}
	const ins = `INSERT INTO payout_instruments (id, tenant_id, brand_id, player_account_id, person_id, kind, rail, asset_codes, detail_ciphertext, detail_nonce,
		detail_key_kid, detail_schema_version, display_mask, fingerprint, fingerprint_kid, instrument_seal, seal_kid)
		VALUES ($1,$2,$3,$4,$5,'bank_account','sepa','{EUR}',$6,$7,'m1',1,'GB****5432',$8,'f1',$9,'m1')`
	requireCode(t, plant(ins, "11"+good.Fingerprint[2:], "aa"+good.InstrumentSeal[2:]), "23503", "planted instrument without an owner row")

	// A planted PENDING row WITH an owner row but no valid seal: Verify refuses
	// before any vendor call (T11), writes an integrity audit row, and the row stays pending.
	fp := "22" + good.Fingerprint[2:]
	if err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id) VALUES ($1,'f1',$2,$3)`, w.tenantID, fp, p.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := plant(ins, fp, "bb"+good.InstrumentSeal[2:]); err != nil {
		t.Fatalf("plant with owner row: %v", err)
	}
	var plantedID uuid.UUID
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payout_instruments WHERE fingerprint = $1`, fp).Scan(&plantedID)
	}); err != nil {
		t.Fatal(err)
	}
	cv := &countingVerifier{}
	svc, _ := NewService(w.keys, DefaultKinds(), cv)
	_, err := svc.Verify(context.Background(), w.rt, w.tenantID, plantedID)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("an unsealed planted pending row must be refused: %v", err)
	}
	if cv.calls.Load() != 0 {
		t.Fatal("a vendor call was made for an unsealed row")
	}
	if got := w.load(plantedID).State; got != StatePendingVerification {
		t.Fatalf("planted row state = %s", got)
	}
	if n := w.count("audit_log", "action = 'payout_instrument.integrity_failure' AND target_id = $1", plantedID.String()); n != 1 {
		t.Fatalf("integrity audit rows = %d", n)
	}
	// A wrongly sealed REAL row (seal edited) is refused too.
	w.tamper("payout_instruments", `UPDATE payout_instruments SET instrument_seal = repeat('0', 64) WHERE id = $1`, good.ID)
	if _, err := svc.Verify(context.Background(), w.rt, w.tenantID, good.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrongly sealed row: %v", err)
	}
	if cv.calls.Load() != 0 {
		t.Fatal("a vendor call was made for a wrongly sealed row")
	}
}

// An instrument cannot be BORN in any state but pending_verification, and a new
// row cannot carry a verification pointer.
func TestInstrumentBornPending(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	good := w.mustRegister(p, ibanA)
	for i, state := range []string{"verified", "verification_expired", "suspended", "revoked", "rejected", "superseded"} {
		fp := fmt.Sprintf("%02x", 0x40+i) + good.Fingerprint[2:]
		if err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id) VALUES ($1,'f1',$2,$3)`, w.tenantID, fp, p.PersonID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payout_instruments (id, tenant_id, brand_id, player_account_id, person_id, kind, rail, asset_codes, detail_ciphertext, detail_nonce,
				detail_key_kid, detail_schema_version, display_mask, fingerprint, fingerprint_kid, instrument_seal, seal_kid, state)
				VALUES ($1,$2,$3,$4,$5,'bank_account','sepa','{EUR}',$6,$7,'m1',1,'GB****5432',$8,'f1',$9,'m1',$10)`,
				uuid.New(), w.tenantID, w.brandID, p.ID, p.PersonID, good.DetailCiphertext, good.DetailNonce, fp, good.InstrumentSeal, state)
			return err
		})
		requireCode(t, err, "PI004", "an instrument born "+state)
	}
}

func TestSweepExpiryAndReverification(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	short := NewMockVerifier()
	short.Now = func() time.Time { return time.Now().Add(-90*24*time.Hour + 2*time.Second) } // source expiry ~2s ahead
	svc, _ := NewService(w.keys, DefaultKinds(), short)
	inst := w.mustRegister(p, ibanA)
	if _, err := svc.Verify(context.Background(), w.rt, w.tenantID, inst.ID); err != nil {
		t.Fatal(err)
	}
	// Nothing to expire yet; a sweep does not touch an in-force verification.
	sweep := func() SweepResult {
		var r SweepResult
		if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = svc.Sweep(ctx, tx, w.tenantID, 100)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := sweep(); r.Expired != 0 {
		t.Fatalf("early sweep expired %d", r.Expired)
	}
	got := w.load(inst.ID)
	// The gate itself never writes expiry: once expired it only refuses.
	time.Sleep(2500 * time.Millisecond)
	_, gerr := w.gate(got, p, "EUR", nil)
	requireGateReason(t, gerr, ReasonVerificationExpired, false)
	if s := w.load(inst.ID).State; s != StateVerified {
		t.Fatalf("a gate refusal must not write expiry (state %s)", s)
	}
	if r := sweep(); r.Expired != 1 {
		t.Fatalf("sweep expired %d, want 1", r.Expired)
	}
	if s := w.load(inst.ID).State; s != StateVerificationExpired {
		t.Fatalf("state after sweep = %s", s)
	}
	_, gerr = w.gate(w.load(inst.ID), p, "EUR", nil)
	requireGateReason(t, gerr, ReasonNotVerified, false)
	// Re-verification from verification_expired makes it usable again.
	if _, err := w.svc.Verify(context.Background(), w.rt, w.tenantID, inst.ID); err != nil {
		t.Fatalf("re-verify: %v", err)
	}
	if _, err := w.gate(w.load(inst.ID), p, "EUR", nil); err != nil {
		t.Fatalf("gate after re-verification: %v", err)
	}
	if n := w.count("payout_instrument_verifications", "instrument_id = $1", inst.ID); n != 2 {
		t.Fatalf("verification rows = %d (history is append-only)", n)
	}
}

func TestSupersession(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	old := w.verified(p, ibanA)
	newRes, err := w.registerWith(w.svc, RegisterParams{TenantID: w.tenantID, PlayerAccountID: p.ID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"},
		Detail: ibanDetail(ibanB), Supersedes: &old.ID})
	if err != nil {
		t.Fatal(err)
	}
	// While the replacement is pending the old one cannot be superseded.
	exec := func(q string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }
	}
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='superseded' WHERE id=$1`, old.ID)), "PI017", "supersede before the replacement is verified")
	if _, err := w.svc.Verify(context.Background(), w.rt, w.tenantID, newRes.Instrument.ID); err != nil {
		t.Fatal(err)
	}
	// A live withdrawal binding the old instrument blocks supersession (A-9 / L-3).
	wl := w.seedWallet(p, "EUR")
	var wid uuid.UUID
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
			VALUES ($1,$2,$3,$4,'EUR',100,'k-supersede',$5,$6) RETURNING id`, w.tenantID, p.BrandID, p.ID, wl, old.ID, old.Fingerprint).Scan(&wid)
	}); err != nil {
		t.Fatalf("bound withdrawal: %v", err)
	}
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='superseded' WHERE id=$1`, old.ID)), "PI018", "supersede an in-use instrument")
	var sr SweepResult
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sr, err = w.svc.Sweep(ctx, tx, w.tenantID, 100)
		return err
	}); err != nil || sr.Superseded != 0 {
		t.Fatalf("sweep must not supersede an in-use instrument: %+v %v", sr, err)
	}
	// A player revoke is refused while in use (409 IN_USE); a provider revoke is never refused.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Revoke(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: old.ID, PlayerAccountID: p.ID, Actor: Actor{Type: ActorPlayer, ID: p.ID.String()}, ReasonCode: "player_revoked"})
		return err
	})
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("player revoke of an in-use instrument = %v, want ErrInUse", err)
	}
	// Release the withdrawal (terminal), then the sweep supersedes.
	w.tamper("withdrawal_requests", `UPDATE withdrawal_requests SET state='cancelled' WHERE id = $1`, wid)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sr, err = w.svc.Sweep(ctx, tx, w.tenantID, 100)
		return err
	}); err != nil || sr.Superseded != 1 {
		t.Fatalf("sweep after release: %+v %v", sr, err)
	}
	if s := w.load(old.ID).State; s != StateSuperseded {
		t.Fatalf("old state = %s", s)
	}
	requireCode(t, w.try(exec(`UPDATE payout_instruments SET state='verified' WHERE id=$1`, old.ID)), "PI010", "superseded is terminal")
	// A superseding registration of a terminal instrument is refused.
	_, err = w.registerWith(w.svc, RegisterParams{TenantID: w.tenantID, PlayerAccountID: p.ID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"},
		Detail: ibanDetail("FR1420041010050500013M02606"), Supersedes: &old.ID})
	if err == nil {
		t.Fatal("superseding a terminal instrument must be refused")
	}
	// Superseding another player's instrument is refused.
	q := w.newPlayer(w.brandID)
	qi := w.verified(q, "NL91ABNA0417164300")
	_, err = w.registerWith(w.svc, RegisterParams{TenantID: w.tenantID, PlayerAccountID: p.ID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"},
		Detail: ibanDetail("BE68539007547034"), Supersedes: &qi.ID})
	if err == nil {
		t.Fatal("superseding another player's instrument must be refused")
	}
}
