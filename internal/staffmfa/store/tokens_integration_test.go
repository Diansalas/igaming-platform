//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

func (w *world) issueChallenge(ref staffmfa.StaffRef, purpose ChallengePurpose, via ChallengeOrigin, at time.Time) staffmfa.Token {
	w.t.Helper()
	var tok staffmfa.Token
	w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tok, err = w.st.IssueChallenge(ctx, tx, ref, purpose, via, 0, at)
		return err
	})
	return tok
}

// lockChallenge resolves (pre-tenant) then locks in the resolved scope: the S3 call sequence.
func (w *world) lockChallenge(raw string, purpose ChallengePurpose, at time.Time, fn func(ctx context.Context, tx pgx.Tx, c Challenge) error) error {
	ref, err := w.st.ResolveChallenge(context.Background(), w.rt, purpose, raw)
	if err != nil {
		return err
	}
	tok, err := staffmfa.ParseToken(raw)
	if err != nil {
		return err
	}
	return WithScope(context.Background(), w.rt, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
		c, err := w.st.LockChallenge(ctx, tx, purpose, tok, at)
		if err != nil {
			return err
		}
		return fn(ctx, tx, c)
	})
}

func TestChallenge_IssueResolveLockConsume(t *testing.T) {
	w := newWorld(t)
	for _, ref := range []staffmfa.StaffRef{w.a1, w.platform} {
		tok := w.issueChallenge(ref, ChallengeMFA, OriginLogin, t0)
		raw := tok.RevealForDelivery()

		// Stored as a keyed MAC only: the token (and its random part) is nowhere in the row.
		var dump string
		w.mustOwner(ref, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT string_agg(t::text, ' ') FROM staff_auth_challenges t WHERE staff_user_id = $1`, ref.StaffID).Scan(&dump)
		})
		if strings.Contains(dump, raw) || strings.Contains(dump, raw[22:]) {
			t.Fatal("the challenge token is stored in clear text")
		}

		// Phase 1 resolves the scope without any tenant context; phase 2 locks and validates.
		pre, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, raw)
		if err != nil || pre.Staff != ref {
			t.Fatalf("resolve: %+v %v", pre, err)
		}
		var got Challenge
		err = w.lockChallenge(raw, ChallengeMFA, t0.Add(time.Minute), func(ctx context.Context, tx pgx.Tx, c Challenge) error {
			got = c
			return nil
		})
		if err != nil || got.Staff != ref || got.Purpose != ChallengeMFA || got.Origin != OriginLogin || got.AttemptsLeft != staffmfa.ChallengeAttempts ||
			!got.ExpiresAt.Equal(t0.Add(staffmfa.ChallengeTTL)) {
			t.Fatalf("lock: %+v %v", got, err)
		}

		// One identical error for the wrong purpose, a malformed token, an unknown token.
		for name, c := range map[string]struct {
			raw     string
			purpose ChallengePurpose
		}{
			"wrong purpose": {raw, ChallengeEnrol},
			"malformed":     {"not-a-token", ChallengeMFA},
			"unknown":       {mustNewToken(t, ref).RevealForDelivery(), ChallengeMFA},
			"flipped char":  {flip(raw), ChallengeMFA},
		} {
			if _, err := w.st.ResolveChallenge(context.Background(), w.rt, c.purpose, c.raw); !errors.Is(err, ErrInvalidChallenge) {
				t.Fatalf("%s: %v", name, err)
			}
		}

		// Expiry (inclusive of the boundary), consumption, exhaustion.
		err = w.lockChallenge(raw, ChallengeMFA, t0.Add(staffmfa.ChallengeTTL), func(context.Context, pgx.Tx, Challenge) error { return nil })
		if !errors.Is(err, ErrInvalidChallenge) {
			t.Fatalf("expired: %v", err)
		}
		err = w.lockChallenge(raw, ChallengeMFA, t0.Add(time.Minute), func(ctx context.Context, tx pgx.Tx, c Challenge) error {
			return w.st.ConsumeChallenge(ctx, tx, c.ID, t0.Add(time.Minute))
		})
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		err = w.lockChallenge(raw, ChallengeMFA, t0.Add(2*time.Minute), func(context.Context, pgx.Tx, Challenge) error { return nil })
		if !errors.Is(err, ErrInvalidChallenge) {
			t.Fatalf("a consumed challenge must be invalid: %v", err)
		}
	}
}

func mustNewToken(t testing.TB, ref staffmfa.StaffRef) staffmfa.Token {
	t.Helper()
	tok, err := staffmfa.NewToken(ref.StaffID)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// flip changes one character of the random part of a wire token.
func flip(raw string) string {
	b := []byte(raw)
	i := len(b) - 3
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestChallenge_AttemptsExhaustAndConsume(t *testing.T) {
	w := newWorld(t)
	raw := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	for i := staffmfa.ChallengeAttempts - 1; i >= 0; i-- {
		err := w.lockChallenge(raw, ChallengeMFA, t0, func(ctx context.Context, tx pgx.Tx, c Challenge) error {
			left, err := w.st.DecrementChallengeAttempts(ctx, tx, c.ID, t0)
			if left != i {
				t.Errorf("attempts left = %d, want %d", left, i)
			}
			return err
		})
		if err != nil {
			t.Fatalf("attempt: %v", err)
		}
	}
	// The last attempt consumed the challenge.
	err := w.lockChallenge(raw, ChallengeMFA, t0, func(context.Context, pgx.Tx, Challenge) error { return nil })
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("an exhausted challenge must be invalid: %v", err)
	}
	var consumed bool
	w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT bool_and(consumed_at IS NOT NULL AND attempts_left = 0) FROM staff_auth_challenges WHERE staff_user_id = $1`, w.a1.StaffID).Scan(&consumed)
	})
	if !consumed {
		t.Fatal("the exhausted challenge must be consumed")
	}
}

func TestChallenge_ConcurrentConsumeExactlyOne(t *testing.T) {
	w := newWorld(t)
	raw := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := w.lockChallenge(raw, ChallengeMFA, t0, func(ctx context.Context, tx pgx.Tx, c Challenge) error {
				return w.st.ConsumeChallenge(ctx, tx, c.ID, t0)
			})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, ErrInvalidChallenge) {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("a challenge must be consumable once: %d successes", won)
	}
}

// ConsumeChallenge's own condition, without the row lock (kills a mutant that drops "consumed_at IS NULL").
func TestConsumeChallenge_ConditionAloneAllowsExactlyOne(t *testing.T) {
	w := newWorld(t)
	raw := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	ref, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, raw)
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
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
				return w.st.ConsumeChallenge(ctx, tx, ref.ID, t0)
			})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("ConsumeChallenge succeeded %d times, want 1", won)
	}
}

func TestChallenge_TenantIsolationAndStaffBinding(t *testing.T) {
	w := newWorld(t)
	rawA := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	tokA, _ := staffmfa.ParseToken(rawA)

	// Tenant B's scope (or platform scope) cannot lock tenant A's challenge even holding the token.
	for name, scope := range map[string]staffmfa.StaffRef{"tenant B": w.b1, "platform": w.platform} {
		err := w.run(scope, func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.st.LockChallenge(ctx, tx, ChallengeMFA, tokA, t0)
			return e
		})
		if !errors.Is(err, ErrInvalidChallenge) {
			t.Fatalf("%s scope: %v", name, err)
		}
	}
	// A token whose embedded staff id is replaced by another staff member's: the keyed MAC binds the staff id.
	forged := mustNewToken(t, w.a2).RevealForDelivery()
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, forged); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("forged: %v", err)
	}
	// A challenge for staff A presented under tenant A's scope but with staff a2's id inside the token: not found.
	raw2 := w.issueChallenge(w.a2, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	mixed := raw2[:22] + rawA[22:] // a2's id prefix, a1's random part (22 base64 chars cover the 16-byte id)
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, mixed); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("mixed token: %v", err)
	}
	// The wrong challenge kind (enrol token used as mfa) is refused: the purpose is inside the MAC.
	rawE := w.issueChallenge(w.a1, ChallengeEnrol, OriginInvite, t0).RevealForDelivery()
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, rawE); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("enrol challenge at mfa: %v", err)
	}
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeEnrol, rawE); err != nil {
		t.Fatalf("enrol challenge at enrol: %v", err)
	}
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeEnrol, rawA); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("mfa challenge at enrol: %v", err)
	}
}

func TestChallenge_IssueRejectsBadInput(t *testing.T) {
	w := newWorld(t)
	err := w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueChallenge(ctx, tx, w.a1, "bogus", OriginLogin, 0, t0)
		return e
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("purpose: %v", err)
	}
	err = w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueChallenge(ctx, tx, w.a1, ChallengeMFA, "bogus", 0, t0)
		return e
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("via: %v", err)
	}
	// A staff member the scope cannot see: the DB trigger refuses (SF001), nothing is written.
	err = w.run(w.b1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueChallenge(ctx, tx, w.a1, ChallengeMFA, OriginLogin, 0, t0)
		return e
	})
	wantCode(t, "issue for another tenant's staff", err, "SF001")
}

// ---- enrolment tokens ---------------------------------------------------------

func (w *world) issueToken(ref staffmfa.StaffRef, p EnrolmentPurpose, at time.Time) staffmfa.Token {
	w.t.Helper()
	var tok staffmfa.Token
	w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tok, err = w.st.IssueEnrolmentToken(ctx, tx, ref, p, nil, nil, at)
		return err
	})
	return tok
}

func (w *world) lockToken(raw string, purposes []EnrolmentPurpose, p EnrolmentPurpose, at time.Time, fn func(ctx context.Context, tx pgx.Tx, e EnrolmentToken) error) error {
	ref, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, purposes, raw)
	if err != nil {
		return err
	}
	tok, err := staffmfa.ParseToken(raw)
	if err != nil {
		return err
	}
	return WithScope(context.Background(), w.rt, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
		e, err := w.st.LockEnrolmentToken(ctx, tx, ref.Purpose, tok, at)
		if err != nil {
			return err
		}
		_ = p
		return fn(ctx, tx, e)
	})
}

func TestEnrolmentToken_IssueResolveConsume_TTLByPurpose(t *testing.T) {
	w := newWorld(t)
	ttl := map[EnrolmentPurpose]time.Duration{EnrolInvite: 72 * time.Hour, EnrolMFAReset: 24 * time.Hour,
		EnrolPasswordReset: 24 * time.Hour, EnrolBootstrap: 24 * time.Hour}
	all := []EnrolmentPurpose{EnrolInvite, EnrolMFAReset, EnrolPasswordReset, EnrolBootstrap}
	for _, ref := range []staffmfa.StaffRef{w.a1, w.platform} {
		for _, p := range all {
			raw := w.issueToken(ref, p, t0).RevealForDelivery()
			var dump string
			var exp, created time.Time
			w.mustOwner(ref, func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, `SELECT string_agg(t::text, ' ') FROM staff_enrolment_tokens t WHERE staff_user_id = $1`, ref.StaffID).Scan(&dump); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `SELECT expires_at, created_at FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND purpose = $2`, ref.StaffID, string(p)).Scan(&exp, &created)
			})
			if strings.Contains(dump, raw) || strings.Contains(dump, raw[22:]) {
				t.Fatalf("%s token stored in clear text", p)
			}
			if exp.Sub(created) != ttl[p] {
				t.Fatalf("%s TTL = %s, want %s", p, exp.Sub(created), ttl[p])
			}
			// The purpose is inside the keyed MAC: only the right purpose list resolves it.
			wrong := []EnrolmentPurpose{}
			for _, q := range all {
				if q != p {
					wrong = append(wrong, q)
				}
			}
			if _, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, wrong, raw); !errors.Is(err, ErrInvalidEnrolmentToken) {
				t.Fatalf("%s resolved under other purposes: %v", p, err)
			}
			got, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, all, raw) // multi-purpose endpoint
			if err != nil || got.Purpose != p || got.Staff != ref {
				t.Fatalf("%s resolve: %+v %v", p, got, err)
			}
			// Expiry boundary, consumption, reuse.
			err = w.lockToken(raw, all, p, t0.Add(ttl[p]), func(context.Context, pgx.Tx, EnrolmentToken) error { return nil })
			if !errors.Is(err, ErrInvalidEnrolmentToken) {
				t.Fatalf("%s expired: %v", p, err)
			}
			err = w.lockToken(raw, all, p, t0.Add(time.Minute), func(ctx context.Context, tx pgx.Tx, e EnrolmentToken) error {
				return w.st.ConsumeEnrolmentToken(ctx, tx, e.ID, t0.Add(time.Minute))
			})
			if err != nil {
				t.Fatalf("%s consume: %v", p, err)
			}
			err = w.lockToken(raw, all, p, t0.Add(2*time.Minute), func(context.Context, pgx.Tx, EnrolmentToken) error { return nil })
			if !errors.Is(err, ErrInvalidEnrolmentToken) {
				t.Fatalf("%s reuse: %v", p, err)
			}
		}
	}
}

func TestEnrolmentToken_ReissueRevokesLiveOne_RevokedIsInvalid(t *testing.T) {
	w := newWorld(t)
	first := w.issueToken(w.a1, EnrolInvite, t0).RevealForDelivery()
	second := w.issueToken(w.a1, EnrolInvite, t0.Add(time.Minute)).RevealForDelivery()
	if first == second {
		t.Fatal("tokens must differ")
	}
	one := []EnrolmentPurpose{EnrolInvite}
	if err := w.lockToken(first, one, EnrolInvite, t0.Add(2*time.Minute), func(context.Context, pgx.Tx, EnrolmentToken) error { return nil }); !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("a superseded token must be revoked: %v", err)
	}
	if err := w.lockToken(second, one, EnrolInvite, t0.Add(2*time.Minute), func(context.Context, pgx.Tx, EnrolmentToken) error { return nil }); err != nil {
		t.Fatalf("the live token: %v", err)
	}
	var live int
	w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND purpose = 'invite' AND consumed_at IS NULL AND revoked_at IS NULL`, w.a1.StaffID).Scan(&live)
	})
	if live != 1 {
		t.Fatalf("exactly one live token per (staff, purpose): %d", live)
	}
	// Different staff members and purposes are independent.
	w.issueToken(w.a2, EnrolInvite, t0)
	w.issueToken(w.a1, EnrolMFAReset, t0)
}

func TestEnrolmentToken_ConcurrentConsumeExactlyOne_AndIsolation(t *testing.T) {
	w := newWorld(t)
	raw := w.issueToken(w.a1, EnrolMFAReset, t0).RevealForDelivery()
	one := []EnrolmentPurpose{EnrolMFAReset}
	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	won := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := w.lockToken(raw, one, EnrolMFAReset, t0, func(ctx context.Context, tx pgx.Tx, e EnrolmentToken) error {
				return w.st.ConsumeEnrolmentToken(ctx, tx, e.ID, t0)
			})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, ErrInvalidEnrolmentToken) {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("a token must be consumable once: %d", won)
	}
	// Tenant isolation: another scope cannot lock it, holding the token.
	raw2 := w.issueToken(w.a2, EnrolInvite, t0).RevealForDelivery()
	tok2, _ := staffmfa.ParseToken(raw2)
	for name, scope := range map[string]staffmfa.StaffRef{"tenant B": w.b1, "platform": w.platform} {
		err := w.run(scope, func(ctx context.Context, tx pgx.Tx) error {
			_, e := w.st.LockEnrolmentToken(ctx, tx, EnrolInvite, tok2, t0)
			return e
		})
		if !errors.Is(err, ErrInvalidEnrolmentToken) {
			t.Fatalf("%s scope: %v", name, err)
		}
	}
	// ConsumeEnrolmentToken's own condition, without the row lock.
	ref, _ := w.st.ResolveEnrolmentToken(context.Background(), w.rt, []EnrolmentPurpose{EnrolInvite}, raw2)
	won = 0
	start = make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := w.run(w.a2, func(ctx context.Context, tx pgx.Tx) error { return w.st.ConsumeEnrolmentToken(ctx, tx, ref.ID, t0) }); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("ConsumeEnrolmentToken succeeded %d times, want 1", won)
	}
}

func TestEnrolmentToken_IssueRejectsBadInput(t *testing.T) {
	w := newWorld(t)
	err := w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueEnrolmentToken(ctx, tx, w.a1, "bogus", nil, nil, t0)
		return e
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("purpose: %v", err)
	}
	if _, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, nil, "x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("no purposes: %v", err)
	}
	// issued_by must name a real staff member (FK); lifecycle_request_id is free.
	ghost := w.a2.StaffID
	ghost[0] ^= 0xff
	err = w.run(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueEnrolmentToken(ctx, tx, w.a1, EnrolInvite, &ghost, nil, t0)
		return e
	})
	wantCode(t, "issued_by FK", err, "23503")
	// Recording the issuer.
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.IssueEnrolmentToken(ctx, tx, w.a1, EnrolInvite, &w.a2.StaffID, nil, t0)
		return e
	})
}
