//go:build integration

package payoutinstrument

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The binding columns, the write-once snapshot table and the payment_attempts
// snapshot constraint are in migration 0123 (ADR 0111 section 7: B13 is one
// migration); their Go integration is B13-B. These tests pin the SCHEMA, as the
// runtime role, so B13-B inherits enforced behaviour.

type bindWorld struct {
	*world
	p      player
	wallet uuid.UUID
	inst   Instrument
}

func newBindWorld(t *testing.T) *bindWorld {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	return &bindWorld{world: w, p: p, wallet: w.seedWallet(p, "EUR"), inst: w.verified(p, ibanA)}
}

func (b *bindWorld) insertWithdrawal(tx pgx.Tx, ctx context.Context, instID *uuid.UUID, fp *string, asset string, amount int64) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, b.tenantID, b.p.BrandID, b.p.ID, b.wallet, asset, amount, "k-"+uuid.NewString(), instID, fp).Scan(&id)
	return id, err
}

func (b *bindWorld) wd(instID *uuid.UUID, fp *string) (uuid.UUID, error) {
	var id uuid.UUID
	err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = b.insertWithdrawal(tx, ctx, instID, fp, "EUR", 100)
		return err
	})
	return id, err
}

func TestBinding_InsertGuard(t *testing.T) {
	b := newBindWorld(t)
	id, fp := b.inst.ID, b.inst.Fingerprint
	if _, err := b.wd(&id, &fp); err != nil {
		t.Fatalf("a verified instrument must bind: %v", err)
	}
	// Legacy NULL/NULL is tolerated by 0123 (B13-B closes it; see the header).
	if _, err := b.wd(nil, nil); err != nil {
		t.Fatalf("legacy NULL binding: %v", err)
	}
	// Half a binding is a CHECK violation.
	_, err := b.wd(&id, nil)
	requireCode(t, err, "23514", "instrument without fingerprint")
	_, err = b.wd(nil, &fp)
	requireCode(t, err, "23514", "fingerprint without instrument")
	// Wrong fingerprint for the instrument: composite FK.
	bad := "ff" + fp[2:]
	_, err = b.wd(&id, &bad)
	if pgState(err) != "PI041" && pgState(err) != "23503" {
		t.Fatalf("fingerprint mismatch: %v", err)
	}
	// Unknown instrument.
	ghost := uuid.New()
	_, err = b.wd(&ghost, &fp)
	if pgState(err) != "PI040" && pgState(err) != "23503" {
		t.Fatalf("unknown instrument: %v", err)
	}
	// Another player's instrument.
	q := b.newPlayer(b.brandID)
	qi := b.verified(q, ibanB)
	_, err = b.wd(&qi.ID, &qi.Fingerprint)
	requireCode(t, err, "PI041", "another player's instrument")
	// Not verified.
	pend := b.mustRegister(b.p, "FR1420041010050500013M02606")
	_, err = b.wd(&pend.ID, &pend.Fingerprint)
	requireCode(t, err, "PI042", "pending instrument")
	// Asset not listed.
	usd := b.seedWallet(b.p, "USD")
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
			VALUES ($1,$2,$3,$4,'USD',100,'k-usd',$5,$6)`, b.tenantID, b.p.BrandID, b.p.ID, usd, id, fp)
		return err
	})
	requireCode(t, err, "PI043", "asset not on the instrument")
	// Suspended / revoked instruments do not bind.
	susp := b.verified(b.p, "NL91ABNA0417164300")
	if err := b.block(susp, EventSuspend, staff(), b.p); err != nil {
		t.Fatal(err)
	}
	_, err = b.wd(&susp.ID, &susp.Fingerprint)
	requireCode(t, err, "PI042", "suspended instrument")
	rev := b.verified(b.p, "BE68539007547034")
	if err := b.block(rev, EventRevoke, Actor{Type: ActorPlayer, ID: b.p.ID.String()}, b.p); err != nil {
		t.Fatal(err)
	}
	_, err = b.wd(&rev.ID, &rev.Fingerprint)
	requireCode(t, err, "PI042", "revoked instrument")
	// State tampered back to verified: the events still block (PI045).
	b.tamper("payout_instruments", `UPDATE payout_instruments SET state='verified' WHERE id=$1`, rev.ID)
	_, err = b.wd(&rev.ID, &rev.Fingerprint)
	requireCode(t, err, "PI045", "revoke event with tampered state")
	// A later verification row that is not the in-force one: PI044.
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verifications (tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
			VALUES ($1,$2,'synthetic','synthetic_asserted','x','verified', now(), now() + interval '1 day', repeat('c',64),'m1',0)`, b.tenantID, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = b.wd(&id, &fp)
	requireCode(t, err, "PI044", "in-force verification is not the latest")
}

func TestBinding_ExpiredVerificationAndImmutability(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	wl := w.seedWallet(p, "EUR")
	short := NewMockVerifier()
	short.Now = func() time.Time { return time.Now().Add(-90*24*time.Hour + 1500*time.Millisecond) }
	svc, _ := NewService(w.keys, DefaultKinds(), short)
	inst := w.mustRegister(p, ibanA)
	if _, err := svc.Verify(context.Background(), w.rt, w.tenantID, inst.ID); err != nil {
		t.Fatal(err)
	}
	inst = w.load(inst.ID)
	ins := func(instID *uuid.UUID, fp *string) (uuid.UUID, error) {
		var id uuid.UUID
		err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
				VALUES ($1,$2,$3,$4,'EUR',100,$5,$6,$7) RETURNING id`, w.tenantID, p.BrandID, p.ID, wl, "k-"+uuid.NewString(), instID, fp).Scan(&id)
		})
		return id, err
	}
	wid, err := ins(&inst.ID, &inst.Fingerprint)
	if err != nil {
		t.Fatalf("bind before expiry: %v", err)
	}
	time.Sleep(1700 * time.Millisecond)
	_, err = ins(&inst.ID, &inst.Fingerprint)
	requireCode(t, err, "PI044", "expired verification at request creation")

	// Immutability: no destination change after the request exists.
	for name, stmt := range map[string]string{
		"clear the binding":      `UPDATE withdrawal_requests SET payout_instrument_id = NULL, payout_instrument_fingerprint = NULL WHERE id = $1`,
		"change the fingerprint": `UPDATE withdrawal_requests SET payout_instrument_fingerprint = repeat('a',64) WHERE id = $1`,
		"change the amount":      `UPDATE withdrawal_requests SET amount = 1 WHERE id = $1`,
	} {
		err := w.try(func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt, wid); return err })
		if err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	// A legacy NULL-binding row cannot gain a binding later.
	legacy, err := ins(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = w.try(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET payout_instrument_id = $2, payout_instrument_fingerprint = $3 WHERE id = $1`, legacy, inst.ID, inst.Fingerprint)
		return err
	})
	if err == nil {
		t.Error("a legacy row must not gain a binding after insert")
	}
}

func (b *bindWorld) insertAttempt(ctx context.Context, tx pgx.Tx, wid uuid.UUID, amount int64) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, withdrawal_request_id, attempt_no, provider_id, payment_method, asset_code, amount,
		interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
		VALUES ($1,$2,'payout',$3,1,'mock-payments','sepa','EUR',$4,false,$5,$5,'created','platform')`, id, b.tenantID, wid, amount, "m-"+id.String()[:20])
	return id, err
}

func (b *bindWorld) gateRes() GateResult {
	var g GateResult
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = b.svc.EvaluateGate(ctx, tx, GateParams{TenantID: b.tenantID, BrandID: b.p.BrandID, PlayerAccountID: b.p.ID, PersonID: b.p.PersonID,
			InstrumentID: b.inst.ID, AssetCode: "EUR", Adapter: syntheticAdapter{}, Lock: true})
		return err
	}); err != nil {
		b.t.Fatal(err)
	}
	return g
}

func TestSnapshot_WriteOnceAndAttemptConstraint(t *testing.T) {
	b := newBindWorld(t)
	g := b.gateRes()
	id, fp := b.inst.ID, b.inst.Fingerprint

	// A payout attempt for a BOUND withdrawal without a snapshot cannot commit.
	wid, err := b.wd(&id, &fp)
	if err != nil {
		t.Fatal(err)
	}
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := b.insertAttempt(ctx, tx, wid, 100)
		return err
	})
	requireCode(t, err, "PI055", "attempt without a snapshot")

	// With the snapshot in the same transaction it commits.
	var attempt uuid.UUID
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = b.insertAttempt(ctx, tx, wid, 100)
		if err != nil {
			return err
		}
		_, err = b.svc.WriteSnapshot(ctx, tx, g, SnapshotParams{AttemptID: attempt, WithdrawalRequestID: wid, Amount: "100", AssetCode: "EUR"})
		return err
	})
	if err != nil {
		t.Fatalf("attempt + snapshot: %v", err)
	}
	var snap Snapshot
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snap, err = b.svc.LoadSnapshot(ctx, tx, b.tenantID, attempt)
		return err
	}); err != nil {
		t.Fatalf("load snapshot (seal verifies): %v", err)
	}
	if snap.Fingerprint != fp || snap.Amount != "100" || snap.VerificationSource != SourceSynthetic || snap.Kind != KindBankAccount {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Write-once: no UPDATE/DELETE/TRUNCATE in the tenant session, nor as the owner.
	exec := func(q string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }
	}
	requireCode(t, b.try(exec(`UPDATE payout_attempt_destination_snapshots SET display_mask='x' WHERE attempt_id=$1`, attempt)), "42501", "snapshot UPDATE (runtime)")
	requireCode(t, b.try(exec(`DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id=$1`, attempt)), "42501", "snapshot DELETE (runtime)")
	if err := b.ownerUnforced("payout_attempt_destination_snapshots", `UPDATE payout_attempt_destination_snapshots SET display_mask='x' WHERE attempt_id=$1`, attempt); err == nil {
		t.Error("snapshot UPDATE must be refused for the owner session")
	}
	if err := b.ownerUnforced("payout_attempt_destination_snapshots", `DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id=$1`, attempt); err == nil {
		t.Error("snapshot DELETE must be refused for the owner session")
	}
	// A second snapshot for the same attempt (PK).
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := b.svc.WriteSnapshot(ctx, tx, g, SnapshotParams{AttemptID: attempt, WithdrawalRequestID: wid, Amount: "100", AssetCode: "EUR"})
		return err
	})
	requireCode(t, err, "23505", "second snapshot for one attempt")

	// Snapshot seal tamper is detected on load.
	b.tamper("payout_attempt_destination_snapshots", `UPDATE payout_attempt_destination_snapshots SET display_mask = 'tampered' WHERE attempt_id = $1`, attempt)
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := b.svc.LoadSnapshot(ctx, tx, b.tenantID, attempt)
		return err
	})
	if err != ErrSealInvalid {
		t.Fatalf("tampered snapshot: %v", err)
	}

	// Snapshot = withdrawal = attempt for amount and asset.
	wid2, _ := b.wd(&id, &fp)
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		a, err := b.insertAttempt(ctx, tx, wid2, 100)
		if err != nil {
			return err
		}
		_, err = b.svc.WriteSnapshot(ctx, tx, g, SnapshotParams{AttemptID: a, WithdrawalRequestID: wid2, Amount: "99", AssetCode: "EUR"})
		return err
	})
	requireCode(t, err, "PI050", "snapshot amount differs from the withdrawal")
	wid3, _ := b.wd(&id, &fp)
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		a, err := b.insertAttempt(ctx, tx, wid3, 100)
		if err != nil {
			return err
		}
		other := g
		other.Instrument.DisplayMask = "XX****0000"
		_, err = b.svc.WriteSnapshot(ctx, tx, other, SnapshotParams{AttemptID: a, WithdrawalRequestID: wid3, Amount: "100", AssetCode: "EUR"})
		return err
	})
	requireCode(t, err, "PI052", "snapshot differs from the instrument")
	// Attempt amount differs from the withdrawal/snapshot.
	wid4, _ := b.wd(&id, &fp)
	err = b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		a, err := b.insertAttempt(ctx, tx, wid4, 77)
		if err != nil {
			return err
		}
		_, err = b.svc.WriteSnapshot(ctx, tx, g, SnapshotParams{AttemptID: a, WithdrawalRequestID: wid4, Amount: "100", AssetCode: "EUR"})
		return err
	})
	requireCode(t, err, "PI051", "snapshot differs from the attempt")

	// A legacy NULL-binding withdrawal needs no snapshot.
	legacy, err := b.wd(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := b.insertAttempt(ctx, tx, legacy, 100)
		return err
	}); err != nil {
		t.Fatalf("legacy attempt without a snapshot: %v", err)
	}
	_ = fmt.Sprint
}

// The bound instrument cannot be swapped for ANOTHER instrument that carries the
// same fingerprint (the composite FK alone would allow it): the instrument id is
// immutable on the request as well as the fingerprint.
func TestBinding_InstrumentIDImmutableEvenForSameFingerprint(t *testing.T) {
	b := newBindWorld(t)
	id, fp := b.inst.ID, b.inst.Fingerprint
	wid, err := b.wd(&id, &fp)
	if err != nil {
		t.Fatal(err)
	}
	// The request ends (terminal), the player revokes A and registers the same destination again.
	b.tamper("withdrawal_requests", `UPDATE withdrawal_requests SET state='cancelled' WHERE id = $1`, wid)
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := b.svc.Revoke(ctx, tx, BlockParams{TenantID: b.tenantID, InstrumentID: id, Actor: Actor{Type: ActorPlayer, ID: b.p.ID.String()}, ReasonCode: "player_revoked"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	again, err := b.register(b.p, ibanA)
	if err != nil || again.Existing {
		t.Fatalf("re-register after revoke: %+v %v", again, err)
	}
	if again.Instrument.Fingerprint != fp || again.Instrument.ID == id {
		t.Fatal("setup: the second instrument must share the fingerprint")
	}
	err = b.try(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET payout_instrument_id = $2 WHERE id = $1`, wid, again.Instrument.ID)
		return err
	})
	if err == nil {
		t.Fatal("re-pointing a request at another instrument with the same fingerprint must be refused")
	}
}
