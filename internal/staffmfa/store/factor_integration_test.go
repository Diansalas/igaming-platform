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
	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
)

func stepOf(t testing.TB, at time.Time) int64 {
	t.Helper()
	s, err := totp.StepAt(at)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Begin / Confirm: the pending factor, the stored ciphertext, activation, replacement, expiry.
func TestFactor_BeginConfirm_Lifecycle(t *testing.T) {
	w := newWorld(t)
	for _, ref := range []staffmfa.StaffRef{w.a1, w.platform} {
		var id uuid.UUID
		var secret []byte
		w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
			var m staffmfa.EnrolmentMaterial
			var err error
			id, m, err = w.factor.Begin(ctx, tx, ref, t0)
			secret = secretFrom(t, m)
			uri, b32s := m.RevealForEnrolment()
			if !strings.HasPrefix(uri, "otpauth://totp/") || !strings.Contains(uri, "issuer=iGaming%20Test%20Back%20Office") || b32s == "" {
				t.Fatalf("enrolment material: %q", uri)
			}
			return err
		})
		fs := w.factors(ref)
		if len(fs) != 1 || fs[0].Status != "pending" || fs[0].ID != id {
			t.Fatalf("after Begin: %+v", fs)
		}
		// The database never holds the plaintext secret: not in any column of the row.
		var rowText string
		w.mustOwner(ref, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT t::text FROM staff_mfa_factors t WHERE id = $1`, id).Scan(&rowText)
		})
		if strings.Contains(strings.ToLower(rowText), strings.ToLower(b32.EncodeToString(secret))) {
			t.Fatal("the base32 secret is in the stored row")
		}
		var exp, created time.Time
		w.mustOwner(ref, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT pending_expires_at, created_at FROM staff_mfa_factors WHERE id = $1`, id).Scan(&exp, &created)
		})
		if !created.Equal(t0) || !exp.Equal(t0.Add(staffmfa.PendingFactorTTL)) {
			t.Fatalf("pending window: created=%s exp=%s", created, exp)
		}

		// A wrong code: refused, counted on the pending row, still pending (committed through the nil-error contract).
		err := w.run(ref, func(ctx context.Context, tx pgx.Tx) error {
			e := w.factor.Confirm(ctx, tx, ref, id, staffmfa.NewTOTPAssertion(wrongCode(t, secret, t0)), t0)
			if !errors.Is(e, ErrRejected) || !Refused(e) {
				t.Fatalf("wrong code: %v", e)
			}
			return nil // the caller commits a refusal so its counters persist
		})
		wantOK(t, "commit refusal", err)
		if fs := w.factors(ref); fs[0].Status != "pending" || fs[0].FailedCount != 1 {
			t.Fatalf("after a wrong confirm: %+v", fs[0])
		}

		// The right code activates it and burns the step.
		w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
			return w.factor.Confirm(ctx, tx, ref, id, staffmfa.NewTOTPAssertion(codeAt(t, secret, t0)), t0)
		})
		act := w.active(ref)
		if act.ID != id || act.LastUsedStep != stepOf(t, t0) || act.FailedCount != 0 || act.ActivatedAt == nil {
			t.Fatalf("after Confirm: %+v", act)
		}
		// Confirming again: it is no longer pending.
		err = w.run(ref, func(ctx context.Context, tx pgx.Tx) error {
			return w.factor.Confirm(ctx, tx, ref, id, staffmfa.NewTOTPAssertion(codeAt(t, secret, t0)), t0)
		})
		if !errors.Is(err, ErrFactorNotFound) {
			t.Fatalf("re-confirm: %v", err)
		}

		// Re-enrolment: the new factor replaces the old one only when it is CONFIRMED.
		var id2 uuid.UUID
		var secret2 []byte
		at := t0.Add(2 * time.Minute)
		w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
			var m staffmfa.EnrolmentMaterial
			var err error
			id2, m, err = w.factor.Begin(ctx, tx, ref, at)
			secret2 = secretFrom(t, m)
			return err
		})
		if got := w.active(ref); got.ID != id {
			t.Fatalf("the old factor must stay active until the new one is confirmed: %+v", got)
		}
		w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
			return w.factor.Confirm(ctx, tx, ref, id2, staffmfa.NewTOTPAssertion(codeAt(t, secret2, at)), at)
		})
		if got := w.active(ref); got.ID != id2 {
			t.Fatalf("the new factor must be active: %+v", got)
		}
		for _, f := range w.factors(ref) {
			if f.ID == id && (f.Status != "revoked" || f.RevokedReason == nil || *f.RevokedReason != "replaced") {
				t.Fatalf("the old factor must be revoked 'replaced': %+v", f)
			}
		}
	}
}

func TestFactor_Begin_SupersedesAndExpiresPending(t *testing.T) {
	w := newWorld(t)
	var id1, id2, id3 uuid.UUID
	begin := func(at time.Time) uuid.UUID {
		var id uuid.UUID
		w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			id, _, err = w.factor.Begin(ctx, tx, w.a1, at)
			return err
		})
		return id
	}
	id1 = begin(t0)
	id2 = begin(t0.Add(time.Minute)) // within the window: superseded
	id3 = begin(t0.Add(time.Minute + staffmfa.PendingFactorTTL + time.Second))
	reasons := map[uuid.UUID]string{}
	for _, f := range w.factors(w.a1) {
		r := ""
		if f.RevokedReason != nil {
			r = *f.RevokedReason
		}
		reasons[f.ID] = f.Status + "/" + r
	}
	// id2 was begun at t0+1m and expired at t0+11m; id3 was begun after that => pending_expired.
	if reasons[id1] != "revoked/pending_superseded" || reasons[id2] != "revoked/pending_expired" || reasons[id3] != "pending/" {
		t.Fatalf("pending lifecycle: %v", reasons)
	}

	// Confirming an expired pending factor is refused without a compare or a write.
	var secret []byte
	var id4 uuid.UUID
	w.mustRun(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		var m staffmfa.EnrolmentMaterial
		var err error
		id4, m, err = w.factor.Begin(ctx, tx, w.a2, t0)
		secret = secretFrom(t, m)
		return err
	})
	late := t0.Add(staffmfa.PendingFactorTTL)
	err := w.run(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		return w.factor.Confirm(ctx, tx, w.a2, id4, staffmfa.NewTOTPAssertion(codeAt(t, secret, late)), late)
	})
	if !errors.Is(err, ErrPendingExpired) {
		t.Fatalf("expired confirm: %v", err)
	}
	if fs := w.factors(w.a2); fs[0].Status != "pending" || fs[0].FailedCount != 0 {
		t.Fatalf("an expired confirm must not write: %+v", fs[0])
	}
}

func TestFactor_Begin_RefusesIneligibleStaff(t *testing.T) {
	w := newWorld(t)
	// A staff member of another tenant, a platform ref for a tenant staff member, a tenant ref for a platform
	// staff member, an unknown staff member: all ErrStaffNotEligible.
	for name, c := range map[string]struct{ scope, target staffmfa.StaffRef }{
		"other tenant":             {w.b1, w.a1},
		"tenant row, platform ref": {w.platform, staffmfa.StaffRef{StaffID: w.a1.StaffID}},
		"platform row, tenant ref": {w.a1, staffmfa.StaffRef{StaffID: w.platform.StaffID, TenantID: w.tenantA}},
		"unknown":                  {w.a1, staffmfa.StaffRef{StaffID: uuid.New(), TenantID: w.tenantA}},
	} {
		err := w.run(c.scope, func(ctx context.Context, tx pgx.Tx) error {
			_, _, e := w.factor.Begin(ctx, tx, c.target, t0)
			return e
		})
		if !errors.Is(err, ErrStaffNotEligible) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A suspended staff member cannot begin enrolment.
	susp := w.newStaff(&w.tenantA, "support")
	w.mustOwner(susp, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, susp.StaffID)
		return err
	})
	err := w.run(susp, func(ctx context.Context, tx pgx.Tx) error {
		_, _, e := w.factor.Begin(ctx, tx, susp, t0)
		return e
	})
	if !errors.Is(err, ErrStaffNotEligible) {
		t.Fatalf("suspended: %v", err)
	}
	// Nothing was written for any of them.
	for _, r := range []staffmfa.StaffRef{w.a1, w.b1, w.platform, susp} {
		if len(w.factors(r)) != 0 {
			t.Fatalf("a refused Begin left a row for %v", r.StaffID)
		}
	}
}

func TestFactor_Verify_ReplayAndMonotonic(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	s0 := stepOf(t, t0)

	// The step burned at enrolment cannot be used again (same code, same step).
	vr, err := w.verify(w.a1, codeAt(t, secret, t0), t0)
	if err != nil || vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("replay of the enrolment code: %+v %v", vr, err)
	}
	now := t0.Add(31 * time.Second) // next step
	code := codeAt(t, secret, now)
	vr, err = w.verify(w.a1, code, now)
	if err != nil || !vr.Accepted() || vr.AcceptedStep != s0+1 || vr.AMR != staffmfa.AMROTP {
		t.Fatalf("fresh code: %+v %v", vr, err)
	}
	if got := w.active(w.a1).LastUsedStep; got != s0+1 {
		t.Fatalf("last_used_step = %d, want %d", got, s0+1)
	}
	// The same code again inside its step: refused (17.3: one success, one refusal).
	vr, _ = w.verify(w.a1, code, now)
	if vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("same code twice: %+v", vr)
	}
	// An OLDER step's code after a newer one was accepted: refused (monotonic), even inside the window.
	vr, _ = w.verify(w.a1, codeAt(t, secret, t0), now)
	if vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("older step after newer: %+v", vr)
	}
	// One step AHEAD (T+1) is inside the window and accepted; two ahead is not.
	vr, _ = w.verify(w.a1, codeAt(t, secret, now.Add(3*30*time.Second)), now)
	if vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("T+3 steps must be refused: %+v", vr)
	}
	vr, _ = w.verify(w.a1, codeAt(t, secret, now.Add(30*time.Second)), now)
	if !vr.Accepted() || vr.AcceptedStep != s0+2 {
		t.Fatalf("T+1 must be accepted: %+v", vr)
	}
	// A malformed value is a refusal that counts.
	before := w.active(w.a1).FailedCount
	vr, _ = w.verify(w.a1, "12ab56", now)
	if vr.Outcome != staffmfa.OutcomeRejected || w.active(w.a1).FailedCount != before+1 {
		t.Fatalf("malformed: %+v", vr)
	}
	// No factor: ErrNoActiveFactor (absent), not an outcome.
	_, err = w.verify(w.a2, "123456", now)
	if !errors.Is(err, ErrNoActiveFactor) {
		t.Fatalf("no factor: %v", err)
	}
}

// 17.3: N parallel verifiers of the SAME valid code give exactly one success and last_used_step is set once.
func TestFactor_BurnStep_RaceExactlyOneWins(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	now := t0.Add(31 * time.Second)
	code := codeAt(t, secret, now)
	const n = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]VerifyResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.verify(w.a1, code, now)
		}(i)
	}
	close(start)
	wg.Wait()
	acc, rej, lock := 0, 0, 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("verifier %d: %v", i, errs[i])
		}
		switch results[i].Outcome {
		case staffmfa.OutcomeAccepted:
			acc++
		case staffmfa.OutcomeRejected:
			rej++
		case staffmfa.OutcomeLocked:
			lock++
		default:
			t.Fatalf("unexpected outcome %v", results[i].Outcome)
		}
	}
	if acc != 1 || acc+rej+lock != n {
		t.Fatalf("exactly one verifier may win: accepted=%d rejected=%d locked=%d", acc, rej, lock)
	}
	if got := w.active(w.a1).LastUsedStep; got != stepOf(t, now) {
		t.Fatalf("last_used_step = %d, want %d", got, stepOf(t, now))
	}
}

// The conditional UPDATE is itself a replay guard: N transactions burn the same step WITHOUT the factor row
// lock; exactly one burn reports success (this kills a mutant that drops the "last_used_step < $step" condition).
func TestBurnStep_ConditionAloneAllowsExactlyOne(t *testing.T) {
	w := newWorld(t)
	id, _ := w.enrol(w.a1, t0)
	step := stepOf(t, t0) + 5
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
				ok, err := burnStep(ctx, tx, id, step, staffmfa.Counters{})
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
		t.Fatalf("burnStep succeeded %d times for one step, want exactly 1", won)
	}
	// And a LOWER step after a higher one never burns.
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		ok, err := burnStep(ctx, tx, id, step-1, staffmfa.Counters{})
		if ok {
			t.Fatal("a lower step must not burn")
		}
		return err
	})
}

func TestFactor_Lockout_SoftLockRefusesBeforeCompareThenExpires(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	now := t0.Add(31 * time.Second)
	bad := wrongCode(t, secret, now)
	for i := 1; i <= 4; i++ {
		vr, err := w.verify(w.a1, bad, now)
		if err != nil || vr.Outcome != staffmfa.OutcomeRejected || vr.HardLockSet {
			t.Fatalf("failure %d: %+v %v", i, vr, err)
		}
		if got := w.active(w.a1).FailedCount; got != i {
			t.Fatalf("failed_count after %d failures = %d", i, got)
		}
	}
	vr, _ := w.verify(w.a1, bad, now) // the 5th failure sets the soft lock
	f := w.active(w.a1)
	if vr.Outcome != staffmfa.OutcomeRejected || f.LockedUntil == nil || !f.LockedUntil.Equal(now.Add(staffmfa.SoftLockDuration)) ||
		f.FailedCount != 0 || f.Failed24h != 5 {
		t.Fatalf("5th failure: %+v / %+v", vr, f)
	}
	// 17.4: the 6th attempt with the CORRECT code answers locked, compares nothing, writes nothing, burns nothing.
	right := codeAt(t, secret, now)
	before := w.active(w.a1)
	vr, err := w.verify(w.a1, right, now.Add(time.Minute))
	after := w.active(w.a1)
	if err != nil || vr.Outcome != staffmfa.OutcomeLocked || before.LastUsedStep != after.LastUsedStep ||
		before.Failed24h != after.Failed24h || before.FailedCount != after.FailedCount {
		t.Fatalf("locked verify: %+v %v / %+v -> %+v", vr, err, before, after)
	}
	// The same answer for a wrong code while locked (the response does not depend on the code).
	vr2, _ := w.verify(w.a1, bad, now.Add(time.Minute))
	if vr2.Outcome != vr.Outcome {
		t.Fatalf("locked answers must not depend on the code: %v vs %v", vr.Outcome, vr2.Outcome)
	}
	// After locked_until passes the correct code is accepted again.
	later := now.Add(staffmfa.SoftLockDuration + time.Second)
	vr, _ = w.verify(w.a1, codeAt(t, secret, later), later)
	if !vr.Accepted() {
		t.Fatalf("after the soft lock: %+v", vr)
	}
}

func TestFactor_Lockout_HardLockNeedsReset(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	at := t0.Add(31 * time.Second)
	bad := wrongCode(t, secret, at)
	// Failures 1-5 at at; the soft lock expires; failures 6-10 inside 24 h.
	var last VerifyResult
	for i := 0; i < 5; i++ {
		last, _ = w.verify(w.a1, bad, at)
	}
	at2 := at.Add(staffmfa.SoftLockDuration + time.Minute)
	for i := 1; i <= 5; i++ {
		last, _ = w.verify(w.a1, bad, at2)
		if i < 5 && last.HardLockSet {
			t.Fatalf("hard lock set early at failure %d", 5+i)
		}
	}
	f := w.active(w.a1)
	if !last.HardLockSet || f.HardLockedAt == nil || f.Failed24h != 10 {
		t.Fatalf("the 10th failure must set the hard lock: %+v / %+v", last, f)
	}
	// A hard-locked factor refuses even the correct code, after any soft lock window.
	later := at2.Add(48 * time.Hour)
	vr, err := w.verify(w.a1, codeAt(t, secret, later), later)
	if err != nil || vr.Outcome != staffmfa.OutcomeHardLocked {
		t.Fatalf("hard-locked verify: %+v %v", vr, err)
	}
	// The runtime role cannot clear the hard lock (SF002: write-once), by SQL.
	err = w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET hard_locked_at = NULL, failed_24h_count = 0, failed_24h_start = NULL
			WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID)
		return e
	})
	wantCode(t, "clearing a hard lock by SQL", err, "SF002")
	// Only a reset (a new factor) restores access: the hard-locked one is revoked, a new one enrols clean.
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = $2, revoked_reason = 'reset'
			WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID, later)
		return e
	})
	id2, secret2 := w.enrolAt(w.a1, later)
	if got := w.active(w.a1); got.ID != id2 || got.HardLockedAt != nil || got.Failed24h != 0 {
		t.Fatalf("a reset factor must be clean: %+v", got)
	}
	vr, _ = w.verify(w.a1, codeAt(t, secret2, later.Add(31*time.Second)), later.Add(31*time.Second))
	if !vr.Accepted() {
		t.Fatalf("the new factor must verify: %+v", vr)
	}
}

// enrolAt is enrol (kept separate for readability at call sites that reuse a staff member).
func (w *world) enrolAt(ref staffmfa.StaffRef, at time.Time) (uuid.UUID, []byte) {
	return w.enrol(ref, at)
}

func TestFactor_Lockout_24hWindowResets(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	at := t0.Add(31 * time.Second)
	bad := wrongCode(t, secret, at)
	for i := 0; i < 3; i++ {
		_, _ = w.verify(w.a1, bad, at)
	}
	later := at.Add(staffmfa.HardLockWindow + time.Second) // both windows have passed
	_, _ = w.verify(w.a1, wrongCode(t, secret, later), later)
	f := w.active(w.a1)
	if f.FailedCount != 1 || f.Failed24h != 1 {
		t.Fatalf("windows must restart: %+v", f)
	}
	// A success resets the short window but NOT the 24 hour count (section 6.5).
	ok := later.Add(31 * time.Second)
	vr, _ := w.verify(w.a1, codeAt(t, secret, ok), ok)
	f = w.active(w.a1)
	if !vr.Accepted() || f.FailedCount != 0 || f.Failed24h != 1 {
		t.Fatalf("success: %+v / %+v", vr, f)
	}
}

// 17.4 parallel guessing: 50 concurrent wrong verifies leave the counters exact and never exceed 5 comparisons.
func TestFactor_Lockout_ParallelGuessingSerialised(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	at := t0.Add(31 * time.Second)
	bad := wrongCode(t, secret, at)
	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	outs := make([]staffmfa.Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			vr, err := w.verify(w.a1, bad, at)
			if err != nil {
				t.Errorf("verifier %d: %v", i, err)
			}
			outs[i] = vr.Outcome
		}(i)
	}
	close(start)
	wg.Wait()
	rej, locked := 0, 0
	for _, o := range outs {
		switch o {
		case staffmfa.OutcomeRejected:
			rej++
		case staffmfa.OutcomeLocked:
			locked++
		}
	}
	f := w.active(w.a1)
	if rej != staffmfa.SoftLockThreshold || locked != n-staffmfa.SoftLockThreshold {
		t.Fatalf("exactly %d comparisons may happen before the lock: rejected=%d locked=%d", staffmfa.SoftLockThreshold, rej, locked)
	}
	if f.Failed24h != staffmfa.SoftLockThreshold || f.LockedUntil == nil || f.FailedCount != 0 {
		t.Fatalf("counters must be exact after 50 parallel guesses: %+v", f)
	}
}

// 17.6: ciphertext written by SQL or moved to another row fails to open: OutcomeIntegrity, never accepted, never absent.
func TestFactor_Integrity_PlantedAndMovedCiphertext(t *testing.T) {
	w := newWorld(t)
	_, secretA := w.enrol(w.a1, t0)
	now := t0.Add(31 * time.Second)

	// Read a1's real sealed secret (the attacker as igaming_runtime can read ciphertext inside its own scope).
	var kid string
	var nonce, ct []byte
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT secret_kid, secret_nonce, secret_ct FROM staff_mfa_factors WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID).
			Scan(&kid, &nonce, &ct)
	})
	plant := func(ref staffmfa.StaffRef, kid string, nonce, ct []byte) {
		w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
				VALUES ($1, $2, 'totp', 'active', $3, $4, $5, $6)`, tenantArg(ref.TenantID), ref.StaffID, kid, nonce, ct, now)
			return e
		})
	}
	garbage := make([]byte, len(ct))
	copy(garbage, ct)
	garbage[0] ^= 0xff
	cases := map[string]struct {
		ref       staffmfa.StaffRef
		kid       string
		nonce, ct []byte
	}{
		"moved to another staff member of the tenant": {w.a2, kid, nonce, ct},
		"moved to platform staff":                     {w.platform, kid, nonce, ct},
		"flipped ciphertext":                          {w.b1, kid, nonce, garbage},
		"unknown kid":                                 {w.newStaff(&w.tenantA, "support"), "no-such-kid", nonce, ct},
	}
	for name, c := range cases {
		plant(c.ref, c.kid, c.nonce, c.ct)
		// Even the victim's REAL code (the attacker knows the secret of the copied row) must not verify.
		vr, err := w.verify(c.ref, codeAt(t, secretA, now), now)
		if err != nil || vr.Outcome != staffmfa.OutcomeIntegrity || !vr.IntegrityFailure || vr.Accepted() {
			t.Fatalf("%s: %+v %v", name, vr, err)
		}
		// The attempt counts as a failure; it is never "factor absent".
		if got := w.active(c.ref).FailedCount; got != 1 {
			t.Fatalf("%s: an integrity failure must count, failed_count = %d", name, got)
		}
	}
	// The victim is unaffected.
	vr, _ := w.verify(w.a1, codeAt(t, secretA, now), now)
	if !vr.Accepted() {
		t.Fatalf("victim: %+v", vr)
	}
}

// Tenant isolation of the factor paths: another tenant's session, or a wrong StaffRef tenant, finds no factor.
func TestFactor_TenantIsolation(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	now := t0.Add(31 * time.Second)
	code := codeAt(t, secret, now)
	for name, c := range map[string]struct{ scope, target staffmfa.StaffRef }{
		"tenant B session, tenant A staff":             {w.b1, w.a1},
		"tenant B session, A's staff id with B tenant": {w.b1, staffmfa.StaffRef{StaffID: w.a1.StaffID, TenantID: w.tenantB}},
		"tenant A session, A's staff id as platform":   {w.a1, staffmfa.StaffRef{StaffID: w.a1.StaffID}},
		"platform session, tenant A staff":             {w.platform, w.a1},
	} {
		err := w.run(c.scope, func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.factor.VerifyDetailed(ctx, tx, c.target, staffmfa.NewTOTPAssertion(code), now)
			return e
		})
		if !errors.Is(err, ErrNoActiveFactor) {
			t.Fatalf("%s: want ErrNoActiveFactor, got %v", name, err)
		}
	}
	if w.active(w.a1).LastUsedStep != stepOf(t, t0) {
		t.Fatal("a cross-scope attempt burned the victim's step")
	}
}
