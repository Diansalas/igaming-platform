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

// A recovery success resets the SHORT failure window only; the 24 hour count is untouched (ADR 0119 section 6.5).
func TestRecovery_SuccessResetsShortWindowOnly(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	codes := w.issueCodes(w.a1, t0)
	at := t0.Add(time.Minute)
	for i := 0; i < 3; i++ {
		if vr, _ := w.recover(w.a1, "00000-00000", at); vr.Outcome != staffmfa.OutcomeRejected {
			t.Fatalf("wrong recovery %d: %+v", i, vr)
		}
	}
	if f := w.active(w.a1); f.FailedCount != 3 || f.Failed24h != 3 {
		t.Fatalf("after three failures: %+v", f)
	}
	if vr, _ := w.recover(w.a1, codes[0], at); !vr.Accepted() {
		t.Fatalf("correct code: %+v", vr)
	}
	if f := w.active(w.a1); f.FailedCount != 0 || f.Failed24h != 3 {
		t.Fatalf("a success resets the short window only: %+v", f)
	}
}

// The purpose of a challenge / token is bound inside its keyed MAC, not only in the purpose column: a row whose
// MAC was computed for ANOTHER purpose is not found even though its purpose column names the requested one.
// (A runtime attacker cannot compute any MAC; the test uses the real keys to build the mismatched row, which is
// what a mix-up bug in the store would produce.)
func TestPurposeIsBoundInsideTheKeyedMAC(t *testing.T) {
	w := newWorld(t)
	now := t0.Add(time.Minute)

	tok, err := staffmfa.NewToken(w.a1.StaffID)
	if err != nil {
		t.Fatal(err)
	}
	mfaMAC, err := w.keys.HashToken(staffmfa.MACChallengeMFA, tok)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
			VALUES ($1, $2, 'enrol', $3, $4, 0, 5, $5, 'invite')`, w.tenantA, w.a1.StaffID, mfaMAC.KID, mfaMAC.MAC, t0.Add(time.Hour))
		return e
	})
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeEnrol, tok.RevealForDelivery()); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("an mfa-purpose MAC under an enrol row must not resolve as enrol: %v", err)
	}
	err = w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.LockChallenge(ctx, tx, ChallengeEnrol, tok, now)
		return e
	})
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("LockChallenge: %v", err)
	}
	// The mirror image: the MAC is right for mfa but the ROW says enrol - the purpose column is checked as well.
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, tok.RevealForDelivery()); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("a row whose purpose column says enrol must not resolve as mfa even with an mfa MAC: %v", err)
	}

	tok2, err := staffmfa.NewToken(w.a2.StaffID)
	if err != nil {
		t.Fatal(err)
	}
	inviteMAC, err := w.keys.HashToken(staffmfa.MACEnrolInvite, tok2)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_enrolment_tokens (tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at)
			VALUES ($1, $2, 'bootstrap', $3, $4, $5)`, w.tenantA, w.a2.StaffID, inviteMAC.KID, inviteMAC.MAC, t0.Add(time.Hour))
		return e
	})
	if _, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, []EnrolmentPurpose{EnrolBootstrap}, tok2.RevealForDelivery()); !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("an invite-purpose MAC under a bootstrap row must not resolve as bootstrap: %v", err)
	}
	err = w.run(w.a2, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.LockEnrolmentToken(ctx, tx, EnrolBootstrap, tok2, now)
		return e
	})
	if !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("LockEnrolmentToken: %v", err)
	}
	if _, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, []EnrolmentPurpose{EnrolInvite}, tok2.RevealForDelivery()); !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("a row whose purpose column says bootstrap must not resolve as invite even with an invite MAC: %v", err)
	}
}
