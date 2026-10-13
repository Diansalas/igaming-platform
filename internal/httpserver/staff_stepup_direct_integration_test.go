//go:build integration

package httpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// ADR 0119 S4b section 6.6: auth.StepUpStaffSession's own refusals, called directly as the runtime role. Over HTTP the
// liveness gate refuses a bad seal or an absolute-expired session BEFORE the step-up handler runs, so these guards are
// otherwise only exercised on the race between liveness and the step-up transaction (mutation pass: H03 / H02b). Each
// refusal leaves mfa_at and the seal untouched; the control case re-seals and moves mfa_at.
func TestStepUpStaffSession_DirectRefusals(t *testing.T) {
	w := newMFAW(t)
	ctx := context.Background()
	call := func(s mfaStaff, sid uuid.UUID, at func(dbNow time.Time) time.Time) (auth.StepUpResult, error) {
		var res auth.StepUpResult
		var stepErr error
		if err := store.WithScope(ctx, w.rt, s.ref, func(ctx context.Context, tx pgx.Tx) error {
			var dbNow time.Time
			if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
				return err
			}
			res, stepErr = auth.StepUpStaffSession(ctx, tx, testStaffMFAKeys(), sid, s.ref.StaffID, s.ref.TenantID, at(dbNow))
			return nil
		}); err != nil {
			t.Fatalf("scope: %v", err)
		}
		return res, stepErr
	}
	dbClock := func(n time.Time) time.Time { return n }
	unchanged := func(t *testing.T, s mfaStaff, sid uuid.UUID, before mfaSessionRow) {
		t.Helper()
		after := w.session(s, sid)
		if !after.MFAAt.Equal(*before.MFAAt) || string(after.Seal) != string(before.Seal) {
			t.Fatal("a refused step-up wrote the session row")
		}
	}

	t.Run("control_fresh_head_is_stepped_up", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		sid := w.sidOf(e.pair.AccessToken)
		before := w.session(s, sid)
		time.Sleep(10 * time.Millisecond)
		res, err := call(s, sid, dbClock)
		if err != nil {
			t.Fatalf("control step-up: %v", err)
		}
		after := w.session(s, sid)
		if !after.MFAAt.After(*before.MFAAt) || !after.MFAAt.Equal(res.MFAAt) || !sealOK(after) {
			t.Fatalf("control: mfa_at %v -> %v (result %v), seal ok %v", before.MFAAt, after.MFAAt, res.MFAAt, sealOK(after))
		}
	})

	t.Run("tampered_seal", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		sid := w.sidOf(e.pair.AccessToken)
		row := w.session(s, sid)
		bad := append([]byte(nil), row.Seal...)
		bad[0] ^= 0xff
		if err := w.ownerSessionExec(s, sid, `UPDATE sessions SET assurance_seal = $2 WHERE id = $1`, bad); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		before := w.session(s, sid)
		res, err := call(s, sid, dbClock)
		if !errors.Is(err, auth.ErrStepUpSealInvalid) || res.Legacy {
			t.Fatalf("a tampered seal must be ErrStepUpSealInvalid (not legacy): %v legacy=%v", err, res.Legacy)
		}
		unchanged(t, s, sid, before)
	})

	t.Run("absolute_expired", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		sid := w.sidOf(e.pair.AccessToken)
		before := w.session(s, sid)
		abs := *before.AbsExpires
		for _, at := range []time.Time{abs, abs.Add(time.Second)} {
			if _, err := call(s, sid, func(time.Time) time.Time { return at }); !errors.Is(err, auth.ErrStepUpNotChainHead) {
				t.Fatalf("now=%v (absolute %v): want ErrStepUpNotChainHead, got %v", at, abs, err)
			}
		}
		unchanged(t, s, sid, before)
	})

	t.Run("revoked_and_wrong_owner", func(t *testing.T) {
		w.t = t
		s := w.staff(&w.tenant, "support")
		other := w.staff(&w.tenant, "support")
		e := w.enrol(s)
		sid := w.sidOf(e.pair.AccessToken)
		before := w.session(s, sid)
		if _, err := call(other, sid, dbClock); !errors.Is(err, auth.ErrStepUpNotChainHead) {
			t.Fatalf("another staff member's session: want ErrStepUpNotChainHead, got %v", err)
		}
		if err := w.ownerSessionExec(s, sid, `UPDATE sessions SET revoked_at = now() WHERE id = $1`); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := call(s, sid, dbClock); !errors.Is(err, auth.ErrStepUpNotChainHead) {
			t.Fatalf("revoked: want ErrStepUpNotChainHead, got %v", err)
		}
		unchanged(t, s, sid, before)
	})
}
