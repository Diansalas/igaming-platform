//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// confirmCommit runs Confirm and COMMITS a refusal (its counters persist), as the S3 flows do.
func (w *world) confirmCommit(ref staffmfa.StaffRef, id uuid.UUID, code string, at time.Time) error {
	var out error
	err := w.run(ref, func(ctx context.Context, tx pgx.Tx) error {
		out = w.factor.Confirm(ctx, tx, ref, id, staffmfa.NewTOTPAssertion(code), at)
		if out != nil && !Refused(out) {
			return out
		}
		return nil
	})
	wantOK(w.t, "confirm transaction", err)
	return out
}

// Confirm shares the lockout budget: wrong codes on a PENDING factor soft-lock and hard-lock it, and a locked
// pending factor refuses even the right code without activating.
func TestFactor_Confirm_LockoutSharesCounters(t *testing.T) {
	w := newWorld(t)
	id, secret := w.beginPending(w.a1, t0)
	bad := wrongCode(t, secret, t0)
	for i := 0; i < staffmfa.SoftLockThreshold; i++ {
		if err := w.confirmCommit(w.a1, id, bad, t0); !errors.Is(err, ErrRejected) {
			t.Fatalf("wrong confirm %d: %v", i, err)
		}
	}
	if err := w.confirmCommit(w.a1, id, codeAt(t, secret, t0), t0.Add(time.Second)); !errors.Is(err, ErrLocked) {
		t.Fatalf("a soft-locked pending factor must answer ErrLocked: %v", err)
	}
	if fs := w.factors(w.a1); fs[0].Status != "pending" {
		t.Fatalf("a locked confirm must not activate: %+v", fs[0])
	}

	// The pending window is only 10 minutes, so the hard lock is reached by driving the 24 hour counter to 9.
	id2, secret2 := w.beginPending(w.a2, t0)
	w.mustRun(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET failed_24h_count = 9, failed_24h_start = $2 WHERE id = $1`, id2, t0)
		return e
	})
	if err := w.confirmCommit(w.a2, id2, wrongCode(t, secret2, t0), t0.Add(time.Minute)); !errors.Is(err, ErrRejected) {
		t.Fatalf("10th failure: %v", err)
	}
	if err := w.confirmCommit(w.a2, id2, codeAt(t, secret2, t0), t0.Add(2*time.Minute)); !errors.Is(err, ErrHardLocked) {
		t.Fatalf("a hard-locked pending factor must answer ErrHardLocked: %v", err)
	}
	if fs := w.factors(w.a2); fs[0].Status != "pending" || fs[0].HardLockedAt == nil {
		t.Fatalf("hard-locked pending factor: %+v", fs[0])
	}
}

// A pending factor whose ciphertext was planted by SQL fails integrity: counted, never activated.
func TestFactor_Confirm_PlantedCiphertextFailsIntegrity(t *testing.T) {
	w := newWorld(t)
	planted := w.newStaff(&w.tenantA, "support")
	pid := uuid.New()
	w.mustRun(planted, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (id, tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, pending_expires_at, created_at)
			VALUES ($1, $2, $3, 'totp', 'pending', 'f1', '\x000000000000000000000000', '\x0102030405060708090a0b0c0d0e0f1011121314', $4, $5)`,
			pid, w.tenantA, planted.StaffID, t0.Add(staffmfa.PendingFactorTTL), t0)
		return e
	})
	err := w.confirmCommit(planted, pid, "123456", t0.Add(time.Minute))
	if !errors.Is(err, ErrIntegrity) || !Refused(err) {
		t.Fatalf("a planted pending ciphertext must fail integrity: %v", err)
	}
	if fs := w.factors(planted); fs[0].Status != "pending" || fs[0].Failed24h != 1 {
		t.Fatalf("integrity failure must count and not activate: %+v", fs[0])
	}
}

// A challenge whose attempts were zeroed by SQL (without consuming it) is still unusable, and a revoked
// enrolment token cannot be consumed even by id.
func TestTokens_ZeroAttemptsAndRevokedAreUnusable(t *testing.T) {
	w := newWorld(t)
	raw := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_auth_challenges SET attempts_left = 0 WHERE staff_user_id = $1`, w.a1.StaffID)
		return e
	})
	err := w.lockChallenge(raw, ChallengeMFA, t0, func(context.Context, pgx.Tx, Challenge) error { return nil })
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("a challenge with no attempts left must be invalid: %v", err)
	}
	if _, derr := func() (int, error) {
		var left int
		e := w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
			ref, re := w.st.ResolveChallenge(ctx, w.rt, ChallengeMFA, raw)
			if re != nil {
				return re
			}
			var de error
			left, de = w.st.DecrementChallengeAttempts(ctx, tx, ref.ID, t0)
			return de
		})
		return left, e
	}(); !errors.Is(derr, ErrInvalidChallenge) {
		t.Fatalf("decrementing a challenge with no attempts left: %v", derr)
	}

	w.issueToken(w.a1, EnrolInvite, t0)
	var tokID uuid.UUID
	w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND purpose = 'invite'`, w.a1.StaffID).Scan(&tokID)
	})
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_enrolment_tokens SET revoked_at = $2 WHERE id = $1`, tokID, t0)
		return e
	})
	err = w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error { return w.st.ConsumeEnrolmentToken(ctx, tx, tokID, t0) })
	if !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("a revoked token must not be consumable: %v", err)
	}
}
