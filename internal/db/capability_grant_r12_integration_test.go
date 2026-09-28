//go:build integration

// PRH-2 K1 R12 round (architect ruling k1-architect-ruling-r12.md, security
// confirmation appended to the same file, and k1-architect-ruling-gp1.md
// for I-5): A-20..A-23, the A-10 extension, R12-a and I-5. New file per the
// task's own instruction - internal/db/capability_grant_integration_test.go
// is owned by a parallel branch and must not be touched here.
//
// R-11 background (why several scenarios below need a scratch database
// with the R-11 partial unique index dropped): the request-guard's own
// unique index (staff_capability_grant_requests_one_pending) allows at
// most ONE 'pending' request per (tenant, grantee, capability) at a time.
// That is a genuinely correct, load-bearing invariant in production - but
// it also means that, via the normal CreateRequest/DecideAndGrant flow
// alone, no second request for the same key can ever be created while an
// earlier one for that key is still pending, and by the time the earlier
// one resolves (or fails), a resulting grant is already fully committed
// and visible - so the request-time R-12 pre-check (CG010) always sees it
// first. That means the approval-time pre-check (CG011) and the grants
// table's own binding overlap+lock check (CG012) can only ever be
// EXERCISED, in isolation, either (a) sequentially, in a scratch database
// with R-11's index temporarily dropped so two requests for the same key
// can coexist as 'pending' (this does not weaken production - R-11 stays
// exactly as migration 0112 defines it there; dropping it here is a
// test-only technique to isolate R-12's own, independent mechanism from
// R-11's), or (b) via genuine concurrency at the grants-table trigger
// itself (R12-c), which is what actually matters for the advisory-lock
// mutant.
package db

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// dropRequestsOnePendingIndex removes the R-11 partial unique index in a
// scratch database ONLY, so two requests for the identical (tenant,
// grantee, capability) key can be 'pending' at once - isolating R-12's own
// mechanism (the grants table's advisory lock + overlap check) from R-11's
// independent, and in production always-on, one-pending-request guard.
func dropRequestsOnePendingIndex(t *testing.T, pool *Pool) {
	t.Helper()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DROP INDEX staff_capability_grant_requests_one_pending`)
		return err
	}); err != nil {
		t.Fatalf("drop R-11 index (test isolation technique): %v", err)
	}
}

// r12Fixture is a minimal (tenant, two distinct-Person tenant staff, one
// distinct-Person platform approver) set for G-T request/approve flows in
// this file's tests.
type r12Fixture struct {
	TenantID  uuid.UUID
	Requester uuid.UUID
	Grantee   uuid.UUID
	Approver  uuid.UUID
}

func newR12Fixture(t *testing.T, pool *Pool) r12Fixture {
	t.Helper()
	tenantID := createTestTenant(t, pool)
	return r12Fixture{
		TenantID:  tenantID,
		Requester: cgStaff(t, pool, tenantID, "tenant_admin"),
		Grantee:   cgStaff(t, pool, tenantID, "finance"),
		Approver:  cgStaff(t, pool, uuid.Nil, "platform_admin"),
	}
}

func (f r12Fixture) createRequest(t *testing.T, pool *Pool, validFrom time.Time, validUntil *time.Time) (uuid.UUID, error) {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.WithPrincipalScope(ctx, f.TenantID, f.Requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, f.TenantID, capability.NewRequestInput{
			GranteeStaffID: f.Grantee, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: validFrom, ValidUntil: validUntil, ReasonCode: "r12-test",
		})
		id = req.ID
		return err
	})
	return id, err
}

func (f r12Fixture) approve(t *testing.T, pool *Pool, requestID uuid.UUID) (*capability.Grant, error) {
	t.Helper()
	ctx := context.Background()
	var grant *capability.Grant
	err := pool.WithPlatformAdmin(ctx, f.Approver, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, f.TenantID, requestID, "approve", "r12-test")
		grant = g
		return err
	})
	return grant, err
}

// A-20: after a G-P2 grant expires, a new grant for the same key is
// approved with no revoke. Uses a real (short) wall-clock wait so the
// first grant is genuinely, unambiguously past its valid_until by the
// time the second is created - not merely "non-overlapping by
// construction".
func TestCapabilityGrant_A20_ExpiredThenNewGrantApprovedNoRevoke(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, uuid.Nil, "platform_admin")
	granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")
	approver := cgStaff(t, pool, uuid.Nil, "platform_admin")

	shortUntil := time.Now().Add(1200 * time.Millisecond)
	var reqID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: &shortUntil, ReasonCode: "test",
		})
		reqID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create first G-P2 request: %v", err)
	}
	var grant1 *capability.Grant
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID, "approve", "test")
		grant1 = g
		return err
	}); err != nil {
		t.Fatalf("approve first G-P2 request: %v", err)
	}
	if grant1.RevokedAt != nil {
		t.Fatalf("expected the first grant to start unrevoked")
	}

	// Wait past the first grant's valid_until (real wall-clock time).
	time.Sleep(1500 * time.Millisecond)

	// The first grant is STILL UNREVOKED (nobody revoked it - expiry is
	// derived, never a written fact, ADR 0099's rejected option (b)).
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		g, err := capability.GetGrant(ctx, tx, tenantID, grant1.ID)
		if err != nil {
			return err
		}
		if g.RevokedAt != nil {
			t.Fatalf("expected the expired grant to remain unrevoked (no auto-supersede)")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify first grant still unrevoked: %v", err)
	}

	newUntil := time.Now().Add(1 * time.Hour)
	var reqID2 uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: &newUntil, ReasonCode: "test",
		})
		reqID2 = req.ID
		return err
	}); err != nil {
		t.Fatalf("create second (renewal) request: %v", err)
	}
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID2, "approve", "test")
		if err != nil {
			return err
		}
		if g == nil {
			t.Fatal("expected a second grant")
		}
		return nil
	}); err != nil {
		t.Fatalf("approve renewal after expiry (no revoke) should succeed: %v", err)
	}
}

// A-21: an adjacent renewal (valid_from == previous valid_until) is
// accepted. An overlapping window is refused at request-creation time
// (CG010, the common case) and - using the R-11-dropped scratch technique
// documented at the top of this file - also at approval time (CG011).
func TestCapabilityGrant_A21_AdjacentAcceptedOverlapRefused(t *testing.T) {
	pool, _ := scratchPoolThrough0112(t, "cap0112r12a21")
	f := newR12Fixture(t, pool)

	t0 := time.Now().Add(10 * time.Minute)
	t1 := t0.Add(1 * time.Hour)

	reqA, err := f.createRequest(t, pool, t0, &t1)
	if err != nil {
		t.Fatalf("create request A: %v", err)
	}
	grantA, err := f.approve(t, pool, reqA)
	if err != nil {
		t.Fatalf("approve request A: %v", err)
	}

	// Adjacent renewal: valid_from == grantA.ValidUntil exactly. Accepted.
	t2 := grantA.ValidUntil.Add(1 * time.Hour)
	reqB, err := f.createRequest(t, pool, *grantA.ValidUntil, &t2)
	if err != nil {
		t.Fatalf("create adjacent renewal request B: %v", err)
	}
	grantB, err := f.approve(t, pool, reqB)
	if err != nil {
		t.Fatalf("approve adjacent renewal request B: expected success, got %v", err)
	}
	if grantB == nil {
		t.Fatal("expected an adjacent-renewal grant")
	}

	// Overlapping (into grantA's own tail): refused at request-creation
	// (CG010) - the common, non-racy path.
	overlapStart := grantA.ValidFrom.Add(5 * time.Minute)
	overlapEnd := overlapStart.Add(10 * time.Minute)
	_, err = f.createRequest(t, pool, overlapStart, &overlapEnd)
	if !cgIsCode(err, "CG010") {
		t.Fatalf("overlapping request: expected CG010, got %v", err)
	}

	// Overlapping-at-approval-time (CG011): construct two requests for the
	// SAME key that are BOTH 'pending' simultaneously (only possible here
	// because R-11's index was dropped for this scratch database - see
	// the file-level comment). Neither overlaps any EXISTING GRANT at its
	// own creation time (grantA/grantB already occupy [t0,t2); pick a
	// disjoint pair of windows beyond t2, then approve the first of the
	// pair - producing a grant that the SECOND, still-pending request's
	// own window overlaps).
	dropRequestsOnePendingIndex(t, pool)

	wa0, wa1 := t2.Add(1*time.Hour), t2.Add(2*time.Hour)
	wb0, wb1 := t2.Add(90*time.Minute), t2.Add(3*time.Hour) // overlaps [wa0,wa1)

	reqC, err := f.createRequest(t, pool, wb0, &wb1) // created FIRST, left pending
	if err != nil {
		t.Fatalf("create request C (pending): %v", err)
	}
	reqD, err := f.createRequest(t, pool, wa0, &wa1) // created SECOND - no grant yet overlaps, so this passes its own pre-check
	if err != nil {
		t.Fatalf("create request D (pending, would not overlap anything yet): %v", err)
	}
	if _, err := f.approve(t, pool, reqD); err != nil {
		t.Fatalf("approve request D (no overlap yet): %v", err)
	}
	// Now request C's own window (wb0,wb1) overlaps D's freshly-granted
	// window (wa0,wa1). Approving C must be refused by the APPROVAL
	// GUARD's own pre-check (CG011) - C's own request-creation-time check
	// already passed (before D existed as a grant).
	_, err = f.approve(t, pool, reqC)
	if !cgIsCode(err, "CG011") {
		t.Fatalf("approve request C (overlaps D's grant): expected CG011, got %v", err)
	}
}

// A-22: backdating beyond 5 minutes is refused at request creation
// (CG010); an already-ended window is refused at request creation and at
// approval; a past valid_from within the 5-minute tolerance is accepted
// and clamped to granted_at (R-14's binding INSERT-time clamp).
func TestCapabilityGrant_A22_BackdatingAndEndedWindow(t *testing.T) {
	pool := testPool(t)
	f := newR12Fixture(t, pool)

	// Beyond the 5-minute tolerance: refused at request creation.
	tooOld := time.Now().Add(-10 * time.Minute)
	_, err := f.createRequest(t, pool, tooOld, nil)
	if !cgIsCode(err, "CG010") {
		t.Fatalf("backdated beyond 5min: expected CG010, got %v", err)
	}

	// An already-ended window (valid_until <= now()) is refused at
	// request creation.
	ended := time.Now().Add(-1 * time.Minute)
	_, err = f.createRequest(t, pool, time.Now().Add(-2*time.Minute), &ended)
	if !cgIsCode(err, "CG010") {
		t.Fatalf("already-ended window at request: expected CG010, got %v", err)
	}

	// An already-ended window caught only at APPROVAL time: construct a
	// request whose valid_until is barely in the future at creation
	// (passes the request guard), wait for it to pass, then approve.
	almostOver := time.Now().Add(700 * time.Millisecond)
	reqID, err := f.createRequest(t, pool, time.Now(), &almostOver)
	if err != nil {
		t.Fatalf("create short-lived request: %v", err)
	}
	time.Sleep(900 * time.Millisecond)
	_, err = f.approve(t, pool, reqID)
	if !cgIsCode(err, "CG011") {
		t.Fatalf("approve after window ended: expected CG011, got %v", err)
	}
	// The refused request stays 'pending' forever (nobody moved it out of
	// that status) - cancel it so R-11 (one pending request per key)
	// doesn't block the next scenario below, which reuses the same fixture
	// grantee/capability.
	if err := pool.WithPrincipalScope(context.Background(), f.TenantID, f.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return capability.CancelRequest(ctx, tx, f.TenantID, reqID)
	}); err != nil {
		t.Fatalf("cancel expired-window request: %v", err)
	}

	// A past valid_from WITHIN the 5-minute tolerance is accepted, and
	// the grant INSERT trigger clamps it forward to granted_at (never
	// backdated).
	withinTolerance := time.Now().Add(-2 * time.Minute)
	reqID2, err := f.createRequest(t, pool, withinTolerance, nil)
	if err != nil {
		t.Fatalf("create within-tolerance backdated request: %v", err)
	}
	grant, err := f.approve(t, pool, reqID2)
	if err != nil {
		t.Fatalf("approve within-tolerance backdated request: %v", err)
	}
	if grant.ValidFrom.Before(grant.GrantedAt) {
		t.Fatalf("expected valid_from (%v) >= granted_at (%v) after the R-14 clamp", grant.ValidFrom, grant.GrantedAt)
	}
	if grant.ValidFrom.Before(withinTolerance) {
		t.Fatalf("clamp should never move valid_from further into the past")
	}
}

// A-23 / R12-c: the binding control. Two concurrent transactions attempt
// to insert an overlapping grant for the SAME (tenant, grantee,
// capability) key. A manual blocker transaction holds the per-key
// pg_advisory_xact_lock first; the test polls pg_stat_activity until BOTH
// competing transactions are queued waiting on it (proving neither
// short-circuited earlier), then releases the blocker. Exactly one grant
// must result; the loser is refused CG012 by the GRANTS TABLE'S OWN
// insert trigger (never CG010/CG011 - both requests' own non-binding
// pre-checks already passed, since at each request's own creation time no
// grant yet existed for the key). This is also A-23's own scenario: a
// direct grant-insert path that bypasses the earlier, non-binding
// pre-checks still hits the overlap refusal.
//
// Run with -race -count=20 (task requirement).
func TestCapabilityGrant_A23_R12c_ConcurrentOverlappingGrantsExactlyOne(t *testing.T) {
	pool, _ := scratchPoolThrough0112(t, "cap0112r12c")
	dropRequestsOnePendingIndex(t, pool) // see file-level comment
	f := newR12Fixture(t, pool)

	w0 := time.Now().Add(1 * time.Hour)
	w1 := w0.Add(1 * time.Hour)
	// Overlapping windows for the identical key.
	reqA, err := f.createRequest(t, pool, w0, &w1)
	if err != nil {
		t.Fatalf("create request A: %v", err)
	}
	reqB, err := f.createRequest(t, pool, w0.Add(30*time.Minute), timePtr(w1.Add(30*time.Minute)))
	if err != nil {
		t.Fatalf("create request B (overlapping A): %v", err)
	}

	lockKeyQuery := `SELECT hashtextextended('staff_capability_grant:' || $1::text || ':' || $2::text || ':' || $3, 0)`
	var lockKey int64
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, lockKeyQuery, f.TenantID, f.Grantee, string(capability.CapabilityLedgerAdjustmentInitiate)).Scan(&lockKey)
	}); err != nil {
		t.Fatalf("compute lock key: %v", err)
	}

	// Blocker: holds the advisory lock for the key in its own transaction.
	blockerConn, err := pool.pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	defer blockerConn.Release()
	blockerTx, err := blockerConn.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	if _, err := blockerTx.Exec(context.Background(), `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		t.Fatalf("blocker take lock: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	grants := make([]*capability.Grant, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, g, err := runDecideAndGrant(pool, f.TenantID, f.Approver, reqA)
		results[0], grants[0] = err, g
	}()
	go func() {
		defer wg.Done()
		_, g, err := runDecideAndGrant(pool, f.TenantID, f.Approver, reqB)
		results[1], grants[1] = err, g
	}()

	// Poll pg_stat_activity until both are waiting on our advisory lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				 WHERE datname = current_database()
				   AND wait_event_type = 'Lock' AND wait_event = 'advisory'
				   AND pid <> pg_backend_pid()
			`).Scan(&waiting)
		}); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for both approvals to queue on the advisory lock (saw %d)", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := blockerTx.Commit(context.Background()); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	wg.Wait()

	successes, refusals := 0, 0
	for i, err := range results {
		if err == nil {
			successes++
			if grants[i] == nil {
				t.Fatalf("goroutine %d: nil error but nil grant", i)
			}
		} else if cgIsCode(err, "CG012") {
			refusals++
		} else {
			t.Fatalf("goroutine %d: expected nil or CG012, got %v", i, err)
		}
	}
	if successes != 1 || refusals != 1 {
		t.Fatalf("expected exactly one success and one CG012 refusal, got %d successes and %d other-refusals (errs: %v)", successes, refusals, results)
	}
}

func runDecideAndGrant(pool *Pool, tenantID, approver, requestID uuid.UUID) (uuid.UUID, *capability.Grant, error) {
	var approvalID uuid.UUID
	var grant *capability.Grant
	err := pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		id, g, err := capability.DecideAndGrant(ctx, tx, tenantID, requestID, "approve", "r12c-test")
		approvalID, grant = id, g
		return err
	})
	return approvalID, grant, err
}

// A-10 extension: a clamped G-P2 grant still ends at or before
// requested_from + max_lifetime, even though its ACTUAL valid_from was
// clamped forward to now() (R-14) rather than the originally requested
// (backdated-within-tolerance) start.
func TestCapabilityGrant_A10Ext_ClampedGP2StillBoundedByMaxLifetime(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, uuid.Nil, "platform_admin")
	granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")
	approver := cgStaff(t, pool, uuid.Nil, "platform_admin")

	// Requested from 2 minutes ago (within the 5-minute tolerance, so it
	// is accepted at request time and clamped at grant-insert time), with
	// valid_until requested at exactly requested_from + 4h (the maximum).
	requestedFrom := time.Now().Add(-2 * time.Minute)
	requestedUntil := requestedFrom.Add(4 * time.Hour)

	var reqID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: requestedFrom, ValidUntil: &requestedUntil, ReasonCode: "test",
		})
		reqID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}

	var grant *capability.Grant
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID, "approve", "test")
		grant = g
		return err
	}); err != nil {
		t.Fatalf("approve request: %v", err)
	}

	if !grant.ValidFrom.After(requestedFrom) {
		t.Fatalf("expected valid_from to be clamped forward past the requested (backdated) start")
	}
	if grant.ValidUntil == nil || grant.ValidUntil.After(requestedFrom.Add(4*time.Hour).Add(time.Second)) {
		t.Fatalf("expected valid_until to stay at/under requested_from + max_lifetime, got %v (requested_from=%v)", grant.ValidUntil, requestedFrom)
	}
	// The clamp makes the ACTUAL window strictly shorter than 4h, never
	// longer, so R-13 still holds by construction.
	if grant.ValidUntil.Sub(grant.ValidFrom) > 4*time.Hour {
		t.Fatalf("clamped window %v exceeds the 4h max lifetime", grant.ValidUntil.Sub(grant.ValidFrom))
	}
}

// R12-a: under REPEATABLE READ, the grant INSERT is refused (CG012)
// outright, regardless of whether an overlap actually exists - write skew
// across snapshots is exactly what this refusal is protecting against.
// R12-a (CORRECTED by security's own re-check of the integrated head,
// 5a27be6): only READ COMMITTED is accepted. SERIALIZABLE alone is NOT
// safe either - a SERIALIZABLE approver that took its snapshot before a
// concurrent READ COMMITTED approver commits can still acquire the
// advisory lock afterward (locks are not part of a snapshot), run its own
// overlap SELECT against its own pre-commit snapshot, see no conflict, and
// insert an overlapping grant; Postgres's SSI only tracks
// SERIALIZABLE-vs-SERIALIZABLE conflicts. Both REPEATABLE READ and
// SERIALIZABLE are refused outright (CG012), regardless of whether an
// overlap actually exists.
func TestCapabilityGrant_R12a_NonReadCommittedRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		iso  pgx.TxIsoLevel
	}{
		{"repeatable_read", pgx.RepeatableRead},
		{"serializable", pgx.Serializable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			f := newR12Fixture(t, pool)

			reqID, err := f.createRequest(t, pool, time.Now(), nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}

			tx, err := pool.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: tc.iso})
			if err != nil {
				t.Fatalf("begin %s tx: %v", tc.name, err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, f.Approver.String()); err != nil {
				t.Fatalf("set platform admin context: %v", err)
			}
			_, _, err = capability.DecideAndGrant(ctx, tx, f.TenantID, reqID, "approve", "test")
			if !cgIsCode(err, "CG012") {
				t.Fatalf("approve under %s: expected CG012 (R12-a), got %v", tc.name, err)
			}
		})
	}
}

// R12-a mixed-isolation race (security's own reproduction of the original
// "READ COMMITTED or SERIALIZABLE" bug, now as a regression test): one
// approver runs under SERIALIZABLE, one under READ COMMITTED, racing an
// overlapping grant for the same key. In a scratch DB with the R-11 index
// dropped (the A-23 technique - two requests for the same key can coexist
// as pending) exactly one grant must result: either the SERIALIZABLE
// transaction is refused outright by R12-a before it ever reaches the
// lock (the current, corrected behavior), or - if R12-a's guard were ever
// weakened back to accepting SERIALIZABLE - the SSI gap this test exists
// to catch would let both commit. Today's correct code refuses the
// SERIALIZABLE side deterministically, so this also serves as an
// end-to-end demonstration that mixing isolation levels can never produce
// two overlapping grants.
func TestCapabilityGrant_R12a_MixedIsolationRaceExactlyOne(t *testing.T) {
	pool, _ := scratchPoolThrough0112(t, "cap0112r12amix")
	dropRequestsOnePendingIndex(t, pool)
	f := newR12Fixture(t, pool)

	w0 := time.Now().Add(1 * time.Hour)
	w1 := w0.Add(1 * time.Hour)
	reqRC, err := f.createRequest(t, pool, w0, &w1)
	if err != nil {
		t.Fatalf("create request A (read committed side): %v", err)
	}
	reqSER, err := f.createRequest(t, pool, w0.Add(30*time.Minute), timePtr(w1.Add(30*time.Minute)))
	if err != nil {
		t.Fatalf("create request B (serializable side, overlapping A): %v", err)
	}

	// The SERIALIZABLE approval: takes its snapshot now, before the READ
	// COMMITTED approval below commits.
	serTx, err := pool.pool.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin serializable tx: %v", err)
	}
	defer func() { _ = serTx.Rollback(context.Background()) }()
	if _, err := serTx.Exec(context.Background(), `SELECT set_config('app.platform_admin_principal_id', $1, true)`, f.Approver.String()); err != nil {
		t.Fatalf("set platform admin context (serializable): %v", err)
	}
	// Touch the requests table under this snapshot so a real SERIALIZABLE
	// snapshot is actually established before the concurrent commit below.
	if _, err := serTx.Exec(context.Background(), `SELECT 1 FROM staff_capability_grant_requests WHERE id = $1`, reqSER); err != nil {
		t.Fatalf("prime serializable snapshot: %v", err)
	}

	// The READ COMMITTED approval: runs and commits to completion first.
	rcGrant, err := f.approve(t, pool, reqRC)
	if err != nil {
		t.Fatalf("approve request A (read committed): %v", err)
	}
	if rcGrant == nil {
		t.Fatal("expected a grant from the read-committed approval")
	}

	// The SERIALIZABLE approval now proceeds, in the transaction whose
	// snapshot predates A's commit. R12-a must refuse it (CG012) -
	// exactly one grant results.
	_, _, serErr := capability.DecideAndGrant(context.Background(), serTx, f.TenantID, reqSER, "approve", "test")
	if !cgIsCode(serErr, "CG012") {
		t.Fatalf("approve request B under SERIALIZABLE racing a committed overlap: expected CG012 (R12-a), got %v", serErr)
	}
}

// I-5: a G-P2 approval is refused when the grantee's LIVE staff_users row
// is no longer active, no longer platform_admin, or (forced into a
// scratch DB, since migration 0034 makes person_id append-only once set)
// has a different person_id than the snapshot taken at request time.
func TestCapabilityGrant_I5_PlatformGranteeLiveReRead(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Case 1: grantee suspended between request and approval. Plain
	// UPDATE, no RLS obstacle (a platform-scoped session updating a
	// platform-scoped row satisfies staff_users' own dual_scope_isolation
	// policy on both USING and WITH CHECK).
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, uuid.Nil, "platform_admin")
	granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")
	approver := cgStaff(t, pool, uuid.Nil, "platform_admin")
	validUntil := time.Now().Add(1 * time.Hour)

	var reqID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: &validUntil, ReasonCode: "test",
		})
		reqID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create G-P2 request: %v", err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, granteeP2)
		return err
	}); err != nil {
		t.Fatalf("suspend grantee: %v", err)
	}
	err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID, "approve", "test")
		return err
	})
	if !cgIsCode(err, "CG011") {
		t.Fatalf("approve G-P2 for a now-suspended grantee: expected CG011 (I-5), got %v", err)
	}

	// Cases 2 and 3 need to mutate staff_users in ways ordinary RLS/CHECK
	// rules structurally refuse for any real session (moving a row's own
	// tenant_id, and - per migration 0034 - re-linking a non-NULL
	// person_id): a scratch database with the 0034 append-only trigger
	// removed, and staff_users' RLS toggled OFF only for the instant of
	// each mutation itself, purely to construct this otherwise-
	// unreachable test state. This does not claim either guarantee is
	// weakened in production. L-b (code review of `5a27be6`): RLS is
	// re-enabled BEFORE each approval below, so the real production I-5
	// arm - reading staff_users under normal RLS, from the platform
	// approver's own session - is what actually gets exercised, not a
	// bypassed one.
	scratchPool, _ := scratchPoolThrough0112(t, "cap0112i5c23")
	if err := scratchPool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DROP TRIGGER IF EXISTS staff_users_person_id_append_only ON staff_users`)
		return err
	}); err != nil {
		t.Fatalf("test isolation setup (drop 0034 trigger): %v", err)
	}

	// Case 2: grantee demoted away from platform_admin between request
	// and approval.
	tenantID2 := createTestTenant(t, scratchPool)
	requester2 := cgStaff(t, scratchPool, uuid.Nil, "platform_admin")
	granteeP2b := cgStaff(t, scratchPool, uuid.Nil, "platform_admin")
	approver2 := cgStaff(t, scratchPool, uuid.Nil, "platform_admin")
	validUntil2 := time.Now().Add(1 * time.Hour)
	var reqID2 uuid.UUID
	if err := scratchPool.WithPlatformAdmin(ctx, requester2, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID2, capability.NewRequestInput{
			GranteeStaffID: granteeP2b, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: &validUntil2, ReasonCode: "test",
		})
		reqID2 = req.ID
		return err
	}); err != nil {
		t.Fatalf("create second G-P2 request: %v", err)
	}
	if err := scratchPool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE staff_users DISABLE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		// staff_users' own CHECK constraint (migration 0011) requires
		// role='platform_admin' IFF tenant_id IS NULL, so demoting away
		// from platform_admin must move tenant_id to a real tenant in the
		// same statement.
		if _, err := tx.Exec(ctx, `UPDATE staff_users SET role = 'compliance', tenant_id = $2 WHERE id = $1`, granteeP2b, tenantID2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE staff_users ENABLE ROW LEVEL SECURITY`)
		return err
	}); err != nil {
		t.Fatalf("demote grantee: %v", err)
	}
	// L-b note: in production this exercises a DIFFERENT observable path
	// than case 1's suspension - a demotion also moves tenant_id to a real
	// tenant, and staff_users' own dual_scope_isolation policy hides any
	// tenant-scoped row from a plain platform session entirely (the same
	// fact this table's own grantee_person_id column doc comment relies
	// on). So the I-5 live re-read's SELECT finds NOT FOUND (the row is
	// invisible under RLS), not "found but ineligible" - a different
	// reason, the same CG011 refusal. Both are asserted as just CG011
	// here, since that is what the real arm actually returns.
	err = scratchPool.WithPlatformAdmin(ctx, approver2, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID2, reqID2, "approve", "test")
		return err
	})
	if !cgIsCode(err, "CG011") {
		t.Fatalf("approve G-P2 for a demoted grantee: expected CG011 (I-5), got %v", err)
	}

	// Case 3: grantee's live person_id no longer matches the snapshot.
	tenantID3 := createTestTenant(t, scratchPool)
	requester3 := cgStaff(t, scratchPool, uuid.Nil, "platform_admin")
	granteeP2c := cgStaff(t, scratchPool, uuid.Nil, "platform_admin")
	approver3 := cgStaff(t, scratchPool, uuid.Nil, "platform_admin")
	validUntil3 := time.Now().Add(1 * time.Hour)
	var reqID3 uuid.UUID
	if err := scratchPool.WithPlatformAdmin(ctx, requester3, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID3, capability.NewRequestInput{
			GranteeStaffID: granteeP2c, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: &validUntil3, ReasonCode: "test",
		})
		reqID3 = req.ID
		return err
	}); err != nil {
		t.Fatalf("create third G-P2 request: %v", err)
	}
	newPerson := uuid.New()
	if err := scratchPool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, newPerson); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE staff_users DISABLE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE staff_users SET person_id = $1 WHERE id = $2`, newPerson, granteeP2c); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE staff_users ENABLE ROW LEVEL SECURITY`)
		return err
	}); err != nil {
		t.Fatalf("re-link grantee to a different person (forced, test-only): %v", err)
	}
	// L-b note: unlike case 2, this row's tenant_id is unchanged (still
	// NULL), so it stays visible to the platform approver's session under
	// normal RLS - the live re-read here really does find the row and
	// compare person_id, exercising the person_id-mismatch branch
	// specifically (not a NOT FOUND masking it).
	err = scratchPool.WithPlatformAdmin(ctx, approver3, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID3, reqID3, "approve", "test")
		return err
	})
	if !cgIsCode(err, "CG011") {
		t.Fatalf("approve G-P2 whose grantee's live person_id no longer matches the snapshot: expected CG011 (I-5), got %v", err)
	}
}

// RC-2 (code review of 5a27be6, mutant M12 survived): A-8's suspended-
// actor coverage extends to the PLATFORM branch of financial_actor_session
// specifically - a suspended platform requester (a G-P2 request), a
// suspended platform approver, and a suspended platform revoker, all via
// db.WithPlatformAdmin, each expected to be refused CG001 by
// financial_actor_session's own "platform principal % is not an active
// platform staff member" check. Kills M12 (dropping that check's
// `rec.status <> 'active'` arm).
func TestCapabilityGrant_RC2_SuspendedPlatformActorRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	suspend := func(id uuid.UUID) {
		t.Helper()
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, id)
			return err
		}); err != nil {
			t.Fatalf("suspend platform staff %s: %v", id, err)
		}
	}

	t.Run("suspended_platform_requester_gp2", func(t *testing.T) {
		tenantID := createTestTenant(t, pool)
		requester := cgStaff(t, pool, uuid.Nil, "platform_admin")
		granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")
		suspend(requester)
		validUntil := time.Now().Add(1 * time.Hour)
		err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
				GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
				ValidFrom: time.Now(), ValidUntil: &validUntil, ReasonCode: "test",
			})
			return err
		})
		if !cgIsCode(err, "CG001") {
			t.Fatalf("suspended platform requester: expected CG001, got %v", err)
		}
	})

	t.Run("suspended_platform_approver", func(t *testing.T) {
		f := newR12Fixture(t, pool)
		reqID, err := f.createRequest(t, pool, time.Now(), nil)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		suspend(f.Approver)
		err = pool.WithPlatformAdmin(ctx, f.Approver, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := capability.DecideAndGrant(ctx, tx, f.TenantID, reqID, "approve", "test")
			return err
		})
		if !cgIsCode(err, "CG001") {
			t.Fatalf("suspended platform approver: expected CG001, got %v", err)
		}
	})

	t.Run("suspended_platform_revoker", func(t *testing.T) {
		f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
		revoker := cgStaff(t, pool, uuid.Nil, "platform_admin")
		suspend(revoker)
		err := pool.WithPlatformAdmin(ctx, revoker, func(ctx context.Context, tx pgx.Tx) error {
			return capability.RevokeGrant(ctx, tx, f.TenantID, f.GrantID, "test-revoke")
		})
		if !cgIsCode(err, "CG001") {
			t.Fatalf("suspended platform revoker: expected CG001, got %v", err)
		}
	})
}

// L-a (code review of 5a27be6): the grant-time R-13 re-check must bound
// valid_until by the REQUESTED v_req.valid_from + max_lifetime, not by the
// clamped NEW.valid_from - using the clamped value makes the bound more
// permissive than intended (the clamp only ever moves valid_from forward),
// so a request that sat pending long enough for its clamp to shift
// valid_from far forward could otherwise get a grant whose actual duration,
// measured from the ORIGINALLY requested start, exceeds
// acting_grant_max_lifetime. This is simulated (rather than requiring a
// real multi-hour wait) in a scratch DB by directly rewriting the pending
// request's own valid_from backward via raw SQL, with the request guard
// trigger temporarily disabled for that one UPDATE only - approximating
// "this request has been sitting pending for a while" without an actual
// wait.
func TestCapabilityGrant_La_R13BoundedByRequestedValidFromNotClamped(t *testing.T) {
	pool, _ := scratchPoolThrough0112(t, "cap0112la")
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, uuid.Nil, "platform_admin")
	granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")
	approver := cgStaff(t, pool, uuid.Nil, "platform_admin")

	validUntil := time.Now().Add(4 * time.Hour) // exactly the max lifetime from "now"
	var reqID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: &validUntil, ReasonCode: "test",
		})
		reqID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create G-P2 request: %v", err)
	}

	// Simulate "this request has been pending for 2 hours": rewrite its
	// own valid_from 2 hours into the past, bypassing the immutability
	// guard for this one test-only UPDATE (valid_until is untouched, so
	// R-14's approval-time "already ended" check is unaffected).
	// Platform scope (not WithoutTenant): the platform_scope_update RLS
	// policy on this table requires app.platform_admin_principal_id to be
	// set (any value) with tenant_id/player/service unset - WithoutTenant
	// sets none of those, so the UPDATE would silently affect 0 rows.
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE staff_capability_grant_requests DISABLE TRIGGER staff_capability_grant_requests_guard`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE staff_capability_grant_requests SET valid_from = valid_from - interval '2 hours' WHERE id = $1`, reqID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to rewrite exactly 1 row, affected %d", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `ALTER TABLE staff_capability_grant_requests ENABLE TRIGGER staff_capability_grant_requests_guard`)
		return err
	}); err != nil {
		t.Fatalf("simulate long-pending request (test-only rewrite): %v", err)
	}

	// Approve now: the clamp moves valid_from forward to ~now(), so the
	// ACTUAL grant window (valid_from=now, valid_until=original now()+4h)
	// is a full 4h - but measured from the request's OWN (rewritten)
	// valid_from (now-2h), the total span is 6h, exceeding the 4h max.
	// The L-a-fixed check catches this; the pre-fix check (bounding by the
	// clamped valid_from) would not have.
	err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID, "approve", "test")
		return err
	})
	if !cgIsCode(err, "CG012") {
		t.Fatalf("approve a grant whose requested-from-based duration exceeds max lifetime: expected CG012 (R-13/L-a), got %v", err)
	}
}
