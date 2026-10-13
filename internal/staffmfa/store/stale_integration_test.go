//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// The row lock makes a stale read unreachable in production; these tests reach it on purpose to prove that the
// CONDITIONAL updates are an independent second layer (ADR 0119 sections 6.5 and 17.3: a used step is burned once).

// verifyLocked on a row read BEFORE another transaction burned the step: the library accepts the stale row, the
// conditional burn refuses, and the result is a counted rejection, never an acceptance.
func TestVerifyLocked_StaleRowCannotBurnAStepTwice(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	now := t0.Add(31 * time.Second)
	code := codeAt(t, secret, now)

	var stale factorRow
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stale, err = scanFactor(tx.QueryRow(ctx, `SELECT `+factorColumns+` FROM staff_mfa_factors WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID))
		return err
	})
	if vr, err := w.verify(w.a1, code, now); err != nil || !vr.Accepted() {
		t.Fatalf("the first verification: %+v %v", vr, err)
	}
	var got VerifyResult
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = w.factor.verifyLocked(ctx, tx, stale, staffmfa.NewTOTPAssertion(code), now)
		return err
	})
	if got.Accepted() || got.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("a stale row must not burn the step a second time: %+v", got)
	}
	if f := w.active(w.a1); f.LastUsedStep != stepOf(t, now) || f.FailedCount != 1 {
		t.Fatalf("the replay must be a counted failure and the step stays burned once: %+v", f)
	}
}

// activate refuses a step the pending row already burned and leaves the active factor untouched (the savepoint
// rolls the replacement back).
func TestActivate_ConditionRefusesAnAlreadyBurnedStep(t *testing.T) {
	w := newWorld(t)
	activeID, _ := w.enrol(w.a1, t0)
	pid, _ := w.beginPending(w.a1, t0.Add(time.Minute))
	step := stepOf(t, t0) + 7
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET last_used_step = $2 WHERE id = $1`, pid, step)
		return e
	})
	var ok bool
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		row, err := lockPending(ctx, tx, w.a1, pid)
		if err != nil {
			return err
		}
		ok, err = w.factor.activate(ctx, tx, w.a1, row, step, t0.Add(2*time.Minute))
		return err
	})
	if ok {
		t.Fatal("activate must refuse a step at or below the pending row's last_used_step")
	}
	var act, pend int
	for _, f := range w.factors(w.a1) {
		switch {
		case f.ID == activeID && f.Status == "active":
			act++
		case f.ID == pid && f.Status == "pending":
			pend++
		}
	}
	if act != 1 || pend != 1 {
		t.Fatalf("a refused activation must change nothing (the old factor stays active): active=%d pending=%d", act, pend)
	}
}
