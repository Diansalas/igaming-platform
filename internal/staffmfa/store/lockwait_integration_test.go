//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// Begin's own staff predicate: a StaffRef whose tenant is not the staff row's tenant is refused as ineligible even
// when the session scope CAN see the row (the database trigger would also refuse, but with an SF001 error).
func TestFactor_Begin_StaffRefTenantMustMatchTheRow(t *testing.T) {
	w := newWorld(t)
	for name, ref := range map[string]staffmfa.StaffRef{
		"another tenant": {StaffID: w.a1.StaffID, TenantID: w.tenantB},
		"platform ref":   {StaffID: w.a1.StaffID},
	} {
		err := w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
			_, _, e := w.factor.Begin(ctx, tx, ref, t0)
			return e
		})
		if !errors.Is(err, ErrStaffNotEligible) {
			t.Fatalf("%s: want ErrStaffNotEligible, got %v", name, err)
		}
	}
	if len(w.factors(w.a1)) != 0 {
		t.Fatal("a refused Begin wrote a factor")
	}
}

// The keyed MAC is verified under the kid the ROW names: a row whose MAC was computed under one kid but that
// names another is never accepted (a kid mix-up must fail closed, not fall back to "found by MAC").
func TestTokens_MACKidMustMatchTheRow(t *testing.T) {
	w := newWorld(t)
	now := t0.Add(time.Minute)
	other := "m0"
	if w.keys.MACKID() == "m0" {
		other = "m1"
	}

	tok, err := staffmfa.NewToken(w.a1.StaffID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := w.keys.HashToken(staffmfa.MACChallengeMFA, tok)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
			VALUES ($1, $2, 'mfa', $3, $4, 0, 5, $5, 'login')`, w.tenantA, w.a1.StaffID, other, h.MAC, t0.Add(time.Hour))
		return e
	})
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, tok.RevealForDelivery()); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("ResolveChallenge with a mismatched row kid: %v", err)
	}
	err = w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.LockChallenge(ctx, tx, ChallengeMFA, tok, now)
		return e
	})
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("LockChallenge with a mismatched row kid: %v", err)
	}

	tok2, _ := staffmfa.NewToken(w.a2.StaffID)
	h2, err := w.keys.HashToken(staffmfa.MACEnrolInvite, tok2)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_enrolment_tokens (tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at)
			VALUES ($1, $2, 'invite', $3, $4, $5)`, w.tenantA, w.a2.StaffID, other, h2.MAC, t0.Add(time.Hour))
		return e
	})
	if _, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, []EnrolmentPurpose{EnrolInvite}, tok2.RevealForDelivery()); !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("ResolveEnrolmentToken with a mismatched row kid: %v", err)
	}
	err = w.run(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.LockEnrolmentToken(ctx, tx, EnrolInvite, tok2, now)
		return e
	})
	if !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("LockEnrolmentToken with a mismatched row kid: %v", err)
	}
}

// waitsForLock runs first in an open transaction, then starts second in another and proves second BLOCKS while
// the first transaction holds its row lock, and proceeds after it ends.
func waitsForLock(t *testing.T, first func(ctx context.Context, tx pgx.Tx) error, second func(ctx context.Context, tx pgx.Tx) error, w *world, ref staffmfa.StaffRef) {
	t.Helper()
	ctx := context.Background()
	held := make(chan struct{})
	release := make(chan struct{})
	done1 := make(chan error, 1)
	go func() {
		done1 <- WithScope(ctx, w.rt, ref, func(ctx context.Context, tx pgx.Tx) error {
			if err := first(ctx, tx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done1:
		t.Fatalf("first transaction ended early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("first transaction never took its lock")
	}
	returned := make(chan error, 1)
	go func() {
		returned <- WithScope(ctx, w.rt, ref, func(ctx context.Context, tx pgx.Tx) error { return second(ctx, tx) })
	}()
	select {
	case err := <-returned:
		close(release)
		<-done1
		t.Fatalf("the second transaction did not wait for the row lock (returned %v)", err)
	case <-time.After(700 * time.Millisecond):
	}
	close(release)
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the second transaction never proceeded after the lock was released")
	}
}

// Every Lock* function holds the row FOR UPDATE: a concurrent locker waits (ADR 0119 sections 6.1, 6.5).
func TestLocks_HoldTheRowUntilTheTransactionEnds(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	now := t0.Add(31 * time.Second)

	raw := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	tok, _ := staffmfa.ParseToken(raw)
	waitsForLock(t,
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.st.LockChallenge(ctx, tx, ChallengeMFA, tok, now)
			return e
		},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.st.LockChallenge(ctx, tx, ChallengeMFA, tok, now)
			return e
		},
		w, w.a1)

	rawT := w.issueToken(w.a1, EnrolInvite, t0).RevealForDelivery()
	tokT, _ := staffmfa.ParseToken(rawT)
	waitsForLock(t,
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.st.LockEnrolmentToken(ctx, tx, EnrolInvite, tokT, now)
			return e
		},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.st.LockEnrolmentToken(ctx, tx, EnrolInvite, tokT, now)
			return e
		},
		w, w.a1)

	// The active factor row (Verify / recovery / regeneration share it).
	waitsForLock(t,
		func(ctx context.Context, tx pgx.Tx) error { _, e := lockActive(ctx, tx, w.a1); return e },
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.factor.VerifyDetailed(ctx, tx, w.a1, staffmfa.NewTOTPAssertion(codeAt(t, secret, now)), now)
			return e
		},
		w, w.a1)

	// A pending factor row (Confirm).
	id, psecret := w.beginPending(w.a2, t0)
	waitsForLock(t,
		func(ctx context.Context, tx pgx.Tx) error { _, e := lockPending(ctx, tx, w.a2, id); return e },
		func(ctx context.Context, tx pgx.Tx) error {
			e := w.factor.Confirm(ctx, tx, w.a2, id, staffmfa.NewTOTPAssertion(codeAt(t, psecret, t0)), t0)
			if Refused(e) {
				return nil
			}
			return e
		},
		w, w.a2)
}
