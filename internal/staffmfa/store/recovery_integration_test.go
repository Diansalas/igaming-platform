//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// issueCodes issues a batch for ref in its own committed transaction.
func (w *world) issueCodes(ref staffmfa.StaffRef, at time.Time) []string {
	w.t.Helper()
	var out []string
	w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		b, err := w.st.IssueRecoveryCodes(ctx, tx, ref, at)
		if err != nil {
			return err
		}
		for _, c := range b.Codes() {
			out = append(out, c.RevealForDisplay())
		}
		return nil
	})
	return out
}

func (w *world) recover(ref staffmfa.StaffRef, code string, at time.Time) (VerifyResult, error) {
	var vr VerifyResult
	err := w.run(ref, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		vr, err = w.st.ConsumeRecoveryCode(ctx, tx, ref, staffmfa.NewRecoveryCodeAssertion(code), at)
		return err
	})
	return vr, err
}

func (w *world) remaining(ref staffmfa.StaffRef) int {
	w.t.Helper()
	var n int
	w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = w.st.RecoveryCodesRemaining(ctx, tx, ref)
		return err
	})
	return n
}

func TestRecovery_IssueStoresOnlyKeyedMACs(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	codes := w.issueCodes(w.a1, t0)
	if len(codes) != staffmfa.RecoveryCodesPerBatch || w.remaining(w.a1) != staffmfa.RecoveryCodesPerBatch {
		t.Fatalf("batch: %d codes, %d live", len(codes), w.remaining(w.a1))
	}
	var dump string
	w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(t::text, ' ') FROM staff_mfa_recovery_codes t WHERE staff_user_id = $1`, w.a1.StaffID).Scan(&dump)
	})
	up := strings.ToUpper(dump)
	for _, c := range codes {
		if strings.Contains(up, strings.ReplaceAll(c, "-", "")) || strings.Contains(up, c) {
			t.Fatal("a recovery code is stored in clear text")
		}
	}
	// Regenerating revokes the old batch: an old code no longer works, the new batch is live.
	fresh := w.issueCodes(w.a1, t0.Add(time.Minute))
	if w.remaining(w.a1) != staffmfa.RecoveryCodesPerBatch {
		t.Fatalf("after regeneration: %d live", w.remaining(w.a1))
	}
	vr, err := w.recover(w.a1, codes[0], t0.Add(2*time.Minute))
	if err != nil || vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("an old-batch code: %+v %v", vr, err)
	}
	vr, _ = w.recover(w.a1, fresh[0], t0.Add(2*time.Minute))
	if !vr.Accepted() {
		t.Fatalf("a new-batch code: %+v", vr)
	}
	// Issue needs an active factor.
	err = w.run(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueRecoveryCodes(ctx, tx, w.a2, t0)
		return e
	})
	if !errors.Is(err, ErrNoActiveFactor) {
		t.Fatalf("issue without a factor: %v", err)
	}
}

func TestRecovery_SingleUse_BindingAndNormalisation(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	w.enrol(w.a2, t0)
	c1 := w.issueCodes(w.a1, t0)
	c2 := w.issueCodes(w.a2, t0)
	at := t0.Add(time.Minute)

	// Normalisation: lower case, no hyphen.
	messy := strings.ToLower(strings.ReplaceAll(c1[0], "-", " "))
	vr, err := w.recover(w.a1, messy, at)
	if err != nil || !vr.Accepted() {
		t.Fatalf("normalised code: %+v %v", vr, err)
	}
	if w.remaining(w.a1) != staffmfa.RecoveryCodesPerBatch-1 {
		t.Fatalf("a consumed code must leave the live set: %d", w.remaining(w.a1))
	}
	// Single use.
	vr, _ = w.recover(w.a1, c1[0], at)
	if vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("reuse: %+v", vr)
	}
	// Bound to the staff member (the keyed MAC includes staff_user_id): a2's code at a1 fails.
	vr, _ = w.recover(w.a1, c2[0], at)
	if vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("another staff member's code: %+v", vr)
	}
	// Malformed input is a refusal that counts against the same factor budget (a fresh staff member: no lock yet).
	fresh := w.newStaff(&w.tenantA, "support")
	w.enrol(fresh, t0)
	w.issueCodes(fresh, t0)
	for i, bad := range []string{"", "!!!", "ABCDE-FGHJ", strings.Repeat("A", 100)} {
		vr, err = w.recover(fresh, bad, at)
		if err != nil || vr.Outcome != staffmfa.OutcomeRejected {
			t.Fatalf("malformed %q: %+v %v", bad, vr, err)
		}
		if got := w.active(fresh).Failed24h; got != i+1 {
			t.Fatalf("malformed recovery input %d must count against the factor budget (failed_24h_count %d)", i, got)
		}
	}
}

// 17.3: concurrent use of one recovery code gives exactly one success.
func TestRecovery_ConcurrentUse_ExactlyOneWins(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	codes := w.issueCodes(w.a1, t0)
	at := t0.Add(time.Minute)
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	acc := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			vr, err := w.recover(w.a1, codes[3], at)
			if err != nil {
				t.Error(err)
				return
			}
			if vr.Accepted() {
				mu.Lock()
				acc++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if acc != 1 {
		t.Fatalf("a recovery code must be consumable once: %d successes", acc)
	}
	if w.remaining(w.a1) != staffmfa.RecoveryCodesPerBatch-1 {
		t.Fatalf("live codes = %d", w.remaining(w.a1))
	}
}

// consumeCode's own condition: N transactions consume the same code WITHOUT the factor lock; exactly one wins
// (kills a mutant that drops "consumed_at IS NULL" from the UPDATE).
func TestConsumeCode_ConditionAloneAllowsExactlyOne(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	w.issueCodes(w.a1, t0)
	var cid uuid.UUID
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_mfa_recovery_codes WHERE staff_user_id = $1 LIMIT 1`, w.a1.StaffID).Scan(&cid)
	})
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
				ok, err := consumeCode(ctx, tx, cid, t0)
				if ok {
					mu.Lock()
					won++
					mu.Unlock()
				}
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("consumeCode reported success %d times, want exactly 1", won)
	}
}

// A locked factor refuses a recovery code BEFORE any lookup: the code stays unconsumed; failures share the budget.
func TestRecovery_LockedFactorRefusesBeforeLookup_SharedBudget(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	codes := w.issueCodes(w.a1, t0)
	at := t0.Add(time.Minute)
	// Five wrong RECOVERY codes soft-lock the factor for TOTP too (one budget).
	for i := 0; i < staffmfa.SoftLockThreshold; i++ {
		vr, err := w.recover(w.a1, "00000-00000", at)
		if err != nil || vr.Outcome != staffmfa.OutcomeRejected {
			t.Fatalf("wrong recovery %d: %+v %v", i, vr, err)
		}
	}
	vr, _ := w.verify(w.a1, codeAt(t, secret, at), at)
	if vr.Outcome != staffmfa.OutcomeLocked {
		t.Fatalf("recovery failures must lock TOTP too: %+v", vr)
	}
	// And the CORRECT recovery code is refused while locked, unconsumed.
	vr, _ = w.recover(w.a1, codes[0], at)
	if vr.Outcome != staffmfa.OutcomeLocked || w.remaining(w.a1) != staffmfa.RecoveryCodesPerBatch {
		t.Fatalf("locked recovery: %+v live=%d", vr, w.remaining(w.a1))
	}
	// Hard lock: a recovery code never bypasses it.
	w.enrol(w.a2, t0)
	c2 := w.issueCodes(w.a2, t0)
	var last VerifyResult
	for round := 0; round < 2; round++ {
		rt := at.Add(time.Duration(round) * (staffmfa.SoftLockDuration + time.Minute))
		for i := 0; i < staffmfa.SoftLockThreshold; i++ {
			last, _ = w.recover(w.a2, "00000-00000", rt)
		}
	}
	if !last.HardLockSet {
		t.Fatalf("10 recovery failures must hard-lock: %+v", last)
	}
	far := at.Add(72 * time.Hour)
	vr, _ = w.recover(w.a2, c2[0], far)
	if vr.Outcome != staffmfa.OutcomeHardLocked || w.remaining(w.a2) != staffmfa.RecoveryCodesPerBatch {
		t.Fatalf("hard-locked recovery: %+v", vr)
	}
}
