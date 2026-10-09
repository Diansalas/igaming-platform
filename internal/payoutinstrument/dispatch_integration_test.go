//go:build integration

package payoutinstrument

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CheckSnapshot (B13-B): the attempt's write-once snapshot must exist, verify, and equal the withdrawal, the
// attempt, the instrument and the amount/asset. Every failure is an INTEGRITY refusal with a closed reason.
func TestCheckSnapshot(t *testing.T) {
	b := newBindWorld(t)
	g := b.gateRes()
	id, fp := b.inst.ID, b.inst.Fingerprint
	wid, err := b.wd(&id, &fp)
	if err != nil {
		t.Fatal(err)
	}
	var attempt uuid.UUID
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = b.insertAttempt(ctx, tx, wid, 100)
		if err != nil {
			return err
		}
		_, err = b.svc.WriteSnapshot(ctx, tx, g, SnapshotParams{AttemptID: attempt, WithdrawalRequestID: wid, Amount: "100", AssetCode: "EUR"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	good := SnapshotExpect{TenantID: b.tenantID, AttemptID: attempt, WithdrawalRequestID: wid, InstrumentID: id, Fingerprint: fp, Amount: 100, AssetCode: "EUR"}
	check := func(e SnapshotExpect) error {
		return b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := b.svc.CheckSnapshot(ctx, tx, e)
			return err
		})
	}
	if err := check(good); err != nil {
		t.Fatalf("a matching snapshot must pass: %v", err)
	}
	for name, mut := range map[string]func(*SnapshotExpect){
		"other withdrawal":  func(e *SnapshotExpect) { e.WithdrawalRequestID = uuid.New() },
		"other instrument":  func(e *SnapshotExpect) { e.InstrumentID = uuid.New() },
		"other fingerprint": func(e *SnapshotExpect) { e.Fingerprint = "ff" + fp[2:] },
		"other amount":      func(e *SnapshotExpect) { e.Amount = 101 },
		"other asset":       func(e *SnapshotExpect) { e.AssetCode = "USD" },
		"other tenant":      func(e *SnapshotExpect) { e.TenantID = uuid.New() },
	} {
		e := good
		mut(&e)
		err := check(e)
		g, ok := IsGateRefusal(err)
		wantReason := ReasonSnapshotMismatch
		if name == "other tenant" {
			wantReason = ReasonSnapshotMissing // not visible under RLS/tenant predicate
		}
		if !ok || !g.Integrity() || g.Reason != wantReason {
			t.Errorf("%s: %v, want an integrity refusal %q", name, err, wantReason)
		}
	}
	// No snapshot at all: snapshot_missing (decision 4).
	e := good
	e.AttemptID = uuid.New()
	if g, ok := IsGateRefusal(check(e)); !ok || !g.Integrity() || g.Reason != ReasonSnapshotMissing {
		t.Errorf("a missing snapshot must be snapshot_missing, got %+v", g)
	}
	// RailOf is a plain read (no usability decision) and unknown instruments are ErrNotFound.
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		rail, err := b.svc.RailOf(ctx, tx, b.tenantID, id)
		if err != nil || rail != "sepa" {
			t.Errorf("RailOf = %q %v", rail, err)
		}
		if _, err := b.svc.RailOf(ctx, tx, b.tenantID, uuid.New()); err != ErrNotFound {
			t.Errorf("unknown instrument: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A tampered seal is an integrity refusal (seal_invalid).
	b.tamper("payout_attempt_destination_snapshots", `UPDATE payout_attempt_destination_snapshots SET display_mask = 'tampered' WHERE attempt_id = $1`, attempt)
	if g, ok := IsGateRefusal(check(good)); !ok || !g.Integrity() || g.Reason != ReasonSealInvalid {
		t.Errorf("a tampered snapshot must be seal_invalid, got %+v", g)
	}
}
