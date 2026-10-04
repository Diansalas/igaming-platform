//go:build integration

package kyc

// PRH-2 E1 fix round (code review F3/F4/F5, identity-compliance C-2, security
// L-1/L-2, and the optional N-series mutants). Every test names the finding or
// mutant it exists to kill. Fixtures never delete outbox rows and every worker
// is scoped to the test's own tenants (code review F1).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// ---- F4: a late submit row must never reach the vendor with an empty reference ----

// The create ended (decided_concurrently, or failed_terminal) and its cascade
// already ran; a submit row committed AFTER that (the race the code review
// proved: an upload that read the create as live before the staff decision)
// must be cancelled by prepare, never sent with provider_reference "".
func TestOutbox_38b_LateSubmitAfterCreateEnded_NeverReachesVendor_F4(t *testing.T) {
	for _, mode := range []string{"decided_concurrently", "create_failed_terminal"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			v := r.create()
			d1 := seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
			d2 := seedDocument(t, r.pool, r.f, v.ID, DocumentSelfie, "s.png")
			if mode == "decided_concurrently" {
				r.staffReview(v.ID, StatusReviewRequired)
				passUntilQuiet(t, r.w)
				if row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxCancelled || row.CancelReason != string(CancelDecidedConcurrently) {
					t.Fatalf("setup: create row = %s / %q", row.State, row.CancelReason)
				}
			} else {
				passUntilQuiet(t, workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy))
				if row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxFailedTerminal {
					t.Fatalf("setup: create row = %s", row.State)
				}
			}
			_ = d1
			// The late row: a NEW key (the set {d2} was never enqueued), committed
			// after the cascade.
			var late uuid.UUID
			if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				cur, err := GetVerificationByID(ctx, tx, v.ID)
				if err != nil {
					return err
				}
				var inserted bool
				late, inserted, err = enqueueSubmitRow(ctx, tx, cur, []SubmittedDocument{{DocumentID: d2}}, workerEnqueueActor())
				if err == nil && !inserted {
					t.Fatal("setup: the late submit row must be inserted")
				}
				return err
			}); err != nil {
				t.Fatalf("late submit row: %v", err)
			}
			passUntilQuiet(t, r.w)
			if _, s := r.calls(); s != 0 {
				t.Fatalf("a submit for a verification with no vendor reference reached the vendor %d time(s) (F4)", s)
			}
			var got obRow
			for _, row := range readOutbox(t, r.pool, r.f.tenantID, v.ID) {
				if row.ID == late {
					got = row
				}
			}
			if got.State != OutboxCancelled || got.CancelReason != string(CancelVerificationNotSubmitted) {
				t.Fatalf("the late submit row = %s / %q, want cancelled / verification_not_submitted", got.State, got.CancelReason)
			}
			if after := r.reload(v.ID); after.ProviderReference != "" {
				t.Fatalf("no reference may ever be bound, got %q", after.ProviderReference)
			}
		})
	}
}

// F4 defence in depth: phase B itself refuses a submit against an empty reference.
func TestOutbox_32c_SubmitWithEmptyReferenceNeverReachesVendor_F4(t *testing.T) {
	r := newRig(t)
	v := r.create() // an orphan: provider_reference ""
	row := claimedRow{
		ID: uuid.New(), TenantID: r.f.tenantID, VerificationID: v.ID, Operation: OpSubmit,
		ProviderID: "mock", IdempotencyKey: "ks:" + v.ID.String() + ":" + strings.Repeat("0", 64), ClaimToken: uuid.New(),
	}
	out := r.w.callVendor(context.Background(), row, &prepared{verification: v, docs: []SubmittedDocument{{DocumentID: uuid.New()}}})
	if out.kind != outcomeNotSent {
		t.Fatalf("a submit with an empty vendor reference must be not-sent, got %v", out.kind)
	}
	if _, s := r.calls(); s != 0 {
		t.Fatalf("the adapter was called %d time(s) with an empty reference", s)
	}
}

// ---- F3: the create race with an OLDER sent create ----

func TestOutbox_20e_CreateRace_WinnerTerminal_WithOlderSentCreate_F3(t *testing.T) {
	r := newRig(t)
	old := r.createSent() // the player's older, sent create
	winner := r.create()  // the newest create, live
	bad := workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy)
	t.Cleanup(func() { requestVerificationTestHook = nil })
	requestVerificationTestHook = func(stage string, attempt int) {
		if stage == "after_conflict" && attempt == 0 {
			requestVerificationTestHook = nil
			passUntilQuiet(t, bad) // the winner ends failed_terminal in exactly this window
		}
	}
	v, existing, err := tryRequestCreate(r.pool, r.f, "mock")
	if err != nil {
		t.Fatalf("expected the insert to be retried, got %v", err)
	}
	if existing || v.ID == winner.ID || v.ID == old.ID {
		t.Fatalf("expected a NEW verification (existing=%v sameAsWinner=%v sameAsOld=%v): the older sent create must never be returned for a terminal winner", existing, v.ID == winner.ID, v.ID == old.ID)
	}
	if row := onlyRow(t, r.pool, r.f.tenantID, winner.ID, OpCreate); row.State != OutboxFailedTerminal {
		t.Fatalf("setup: the winner must be failed_terminal, got %s", row.State)
	}
}

// N11: a repeat create audits duplicate=true (and the first duplicate=false).
func TestOutbox_20f_DuplicateCreateAudit_DuplicateTrue_N11(t *testing.T) {
	r := newRig(t)
	v := r.create()
	again, existing := requestCreate(t, r.pool, r.f, "mock")
	if !existing || again.ID != v.ID {
		t.Fatalf("setup: expected the existing verification, got existing=%v", existing)
	}
	var dupTrue, dupFalse int
	for _, a := range auditByAction(r.auditFor(v.ID), auditActionSubmissionEnqueued) {
		switch a.Meta["duplicate"] {
		case true:
			dupTrue++
		case false:
			dupFalse++
		}
	}
	if dupTrue != 1 || dupFalse != 1 {
		t.Fatalf("expected one duplicate=false and one duplicate=true enqueue audit row, got false=%d true=%d", dupFalse, dupTrue)
	}
}

// ---- F5: the credential binding check, arm by arm ----

type fixedBindingResolver struct{ domain, provider string }

func (f fixedBindingResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	domain, provider := f.domain, f.provider
	if domain == "" {
		domain = "kyc"
	}
	if provider == "" {
		provider = providerID
	}
	return providercred.NewMockOutboundCredential(tenantID, domain, provider), nil // the RIGHT tenant
}

func TestOutbox_09b_BindingMismatch_ProviderArmAndDomainArm_N4_N5(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  fixedBindingResolver
	}{
		{"wrong provider id (right tenant, right domain)", fixedBindingResolver{provider: "some-other-provider"}},
		{"wrong domain (right tenant, right provider)", fixedBindingResolver{domain: "payments"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.w.Outbound = tc.res
			v := r.create()
			r.pass()
			row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
			if row.State != OutboxFailedTerminal || row.LastErrorClass != string(ClassCredentialBindingMismatch) {
				t.Fatalf("create row = %s / %q, want failed_terminal / credential_binding_mismatch", row.State, row.LastErrorClass)
			}
			if c, _ := r.calls(); c != 0 {
				t.Fatalf("a credential bound to the wrong %s must never reach the adapter, got %d call(s)", tc.name, c)
			}
		})
	}
	// Positive control: the same resolver shape with the CORRECT binding sends.
	r := newRig(t)
	r.w.Outbound = fixedBindingResolver{}
	v := r.create()
	r.pass()
	if row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxSent {
		t.Fatalf("positive control: a correctly bound credential must send, got %s", row.State)
	}
}

// ---- per-pass per-tenant cap (N6, N7) ----

func TestOutbox_16c_PerTenantCap_OnePassClaimsTwoOfThreeAndTheOtherTenant_N6_N7(t *testing.T) {
	r := newRig(t)
	r.w.Config.PerTenantCap = 2
	a2 := seedSecondAccount(t, r.pool, r.f)
	a3 := seedSecondAccount(t, r.pool, r.f)
	b := seedFixture(t, r.pool)
	va1 := r.create()
	va2, _ := requestCreate(t, r.pool, a2, "mock")
	va3, _ := requestCreate(t, r.pool, a3, "mock")
	vb, _ := requestCreate(t, r.pool, b, "mock")

	st := r.pass()
	if st.Claimed != 3 {
		t.Fatalf("one pass must claim two rows of tenant A (the cap) and one of tenant B, got %+v", st)
	}
	state := func(tenant uuid.UUID, id uuid.UUID) OutboxState {
		return onlyRow(t, r.pool, tenant, id, OpCreate).State
	}
	if state(r.f.tenantID, va1.ID) != OutboxSent || state(r.f.tenantID, va2.ID) != OutboxSent {
		t.Fatal("the first two tenant-A rows must be sent in the first pass")
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, va3.ID, OpCreate); got.State != OutboxPending || got.Claims != 0 {
		t.Fatalf("the third tenant-A row must wait for the next pass (cap 2), got %+v", got)
	}
	if state(b.tenantID, vb.ID) != OutboxSent {
		t.Fatal("tenant B must not be starved by tenant A's backlog")
	}
	if st := r.pass(); st.Claimed != 1 || state(r.f.tenantID, va3.ID) != OutboxSent {
		t.Fatalf("the next pass must send the remaining tenant-A row, got %+v", st)
	}
}

// ---- N1 / N18 / N2 / N13 / N17 ----

// N1: the second retry waits longer (attemptsAfter, not the pre-increment count).
func TestOutbox_08b_RetryBackoffGrowsWithFailedAttempts_N1(t *testing.T) {
	r := newRig(t)
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{}, errors.New("vendor down")
	}
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	r.pass()
	first := nextAttemptIn(t, r.pool, r.f.tenantID, row.ID)
	makeDueNow(t, row.ID)
	r.pass()
	second := nextAttemptIn(t, r.pool, r.f.tenantID, row.ID)
	if !within(first, backoff(1, r.w.Config.BackoffBase, r.w.Config.BackoffCap), 5*time.Second) {
		t.Fatalf("first retry waits %s, want about %s", first, backoff(1, r.w.Config.BackoffBase, r.w.Config.BackoffCap))
	}
	if !within(second, backoff(2, r.w.Config.BackoffBase, r.w.Config.BackoffCap), 5*time.Second) {
		t.Fatalf("second retry waits %s, want about %s (it must grow with failed_attempts)", second, backoff(2, r.w.Config.BackoffBase, r.w.Config.BackoffCap))
	}
}

// N18 and N13: a deferral for an inactive tenant grows with claims, and the
// deferred-age gauge reflects the pass (set by a deferral, cleared by the next
// pass that defers nothing).
func TestOutbox_07f_DeferralBackoffGrowsWithClaims_AndGauge_N13_N18(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	outboxFixtureUpdate(t, row.ID, `created_at = now() - interval '2 hours'`) // an old row, so the age gauge is distinguishable from 0
	setTenantStatus(t, r.pool, r.f.tenantID, "suspended")
	t.Cleanup(func() { setTenantStatus(t, r.pool, r.f.tenantID, "active") })

	if st := r.pass(); st.Results[resultDeferred] != 1 {
		t.Fatalf("expected a deferred item, got %+v", st.Results)
	}
	if age := outboxOldestDeferredSecs.Load(); age < 2*3600-60 {
		t.Fatalf("the deferred-age gauge must reflect the deferral of a 2-hour-old row, got %d s (N13)", age)
	}
	first := nextAttemptIn(t, r.pool, r.f.tenantID, row.ID)
	makeDueNow(t, row.ID)
	r.pass()
	second := nextAttemptIn(t, r.pool, r.f.tenantID, row.ID)
	if !within(first, backoff(1, r.w.Config.BackoffBase, r.w.Config.BackoffCap), 5*time.Second) ||
		!within(second, backoff(2, r.w.Config.BackoffBase, r.w.Config.BackoffCap), 5*time.Second) {
		t.Fatalf("deferral waits %s then %s, want about %s then %s (growing with claims, N18)", first, second,
			backoff(1, r.w.Config.BackoffBase, r.w.Config.BackoffCap), backoff(2, r.w.Config.BackoffBase, r.w.Config.BackoffCap))
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.FailedAttempts != 0 {
		t.Fatalf("a deferral must not consume an attempt, got %d", got.FailedAttempts)
	}
	// A pass that defers nothing clears the gauge (per-pass maximum, F8).
	setTenantStatus(t, r.pool, r.f.tenantID, "active")
	r.pass() // the row is not due (deferred into the future): nothing claimed
	if age := outboxOldestDeferredSecs.Load(); age != 0 {
		t.Fatalf("a pass that defers nothing must clear the gauge, got %d", age)
	}
}

// N2: the inactive-tenant deferral is decided BEFORE the exhausted re-claim
// terminal: a suspended tenant's row whose budget is spent is deferred, never
// failed_terminal, never alerted.
func TestOutbox_07g_InactiveTenantDefersBeforeExhaustedTerminal_N2(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	claimOne(t, r.w)
	outboxFixtureUpdate(t, row.ID, `lease_expires_at = now() - interval '1 second', failed_attempts = 8`)
	setTenantStatus(t, r.pool, r.f.tenantID, "suspended")
	t.Cleanup(func() { setTenantStatus(t, r.pool, r.f.tenantID, "active") })
	r.pass()
	got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if got.State != OutboxPending || got.LastErrorClass != string(ClassDeferredTenantInactive) {
		t.Fatalf("row = %s / %q, want pending / deferred_tenant_inactive (a suspended tenant is never terminal)", got.State, got.LastErrorClass)
	}
	if n := len(r.alerts()); n != 0 {
		t.Fatalf("a deferral raises no alert, got %d", n)
	}
}

// N17: phase B is bounded: a provider that blocks until its context is done is
// released at the call timeout and the row is retried as ambiguous.
func TestOutbox_32d_PhaseBIsBounded_N17(t *testing.T) {
	r := newRig(t)
	r.w.Config.CallTimeout = 50 * time.Millisecond
	r.createImpl = func(ctx context.Context, _ CreateVerificationInput) (ProviderResult, error) {
		<-ctx.Done()
		return ProviderResult{}, ctx.Err()
	}
	v := r.create()
	done := make(chan PassStats, 1)
	go func() { done <- r.pass() }()
	select {
	case st := <-done:
		if st.Results[resultRetry] != 1 {
			t.Fatalf("expected one retry item, got %+v", st.Results)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("phase B did not return: the vendor call is unbounded (N17)")
	}
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxPending || row.LastErrorClass != string(ClassAmbiguous) {
		t.Fatalf("row = %s / %q, want pending / ambiguous", row.State, row.LastErrorClass)
	}
}

// ---- IC C-2: submit-side apply_conflict (scratch database: it adds a constraint) ----

func TestOutbox_10b_SubmitApplyConflict_NoResend_StatusUnchanged_VendorStateUnreflected_ICC2(t *testing.T) {
	pool, _ := scratchThrough0114(t, "kyc0114subconf")
	r := newRigOn(t, pool, pool)
	const vendorRef = "mock-ref-SUBMIT-CONFLICT-5151"
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{ProviderReference: vendorRef, Outcome: ProviderPending, Reason: "created"}, nil
	}
	r.submitImpl = func(_ context.Context, ref string, _ []SubmittedDocument, _ CallContext) (ProviderResult, error) {
		return ProviderResult{ProviderReference: ref, Outcome: ProviderApproved, Reason: "approved by the vendor"}, nil
	}
	v := r.createSent()
	seedDocument(t, pool, r.f, v.ID, DocumentPassport, "p.png")
	// A deterministic apply conflict: the verification may not become approved
	// (scratch database only; never the shared one).
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE kyc_verifications ADD CONSTRAINT zz_no_approved_for_test CHECK (status <> 'approved') NOT VALID`)
		return err
	}); err != nil {
		t.Fatalf("scratch constraint: %v", err)
	}
	before := r.snapshot(v.ID)
	_, submitsBefore := r.calls()
	st := r.pass()
	if st.Results[resultApplyConflict] != 1 {
		t.Fatalf("expected an apply_conflict item, got %+v", st.Results)
	}
	if _, submitsAfter := r.calls(); submitsAfter-submitsBefore != 1 {
		t.Fatalf("apply_conflict must NOT re-send: vendor submit calls = %d, want 1", submitsAfter-submitsBefore)
	}
	row := onlyRow(t, pool, r.f.tenantID, v.ID, OpSubmit)
	if row.State != OutboxFailedTerminal || row.LastErrorClass != string(ClassApplyConflict) {
		t.Fatalf("submit row = %s / %q, want failed_terminal / apply_conflict", row.State, row.LastErrorClass)
	}
	if r.snapshot(v.ID) != before {
		t.Fatal("the verification must be untouched (status unchanged)")
	}
	a := r.auditFor(v.ID)
	term := auditByAction(a, auditActionSubmissionFailedTerminal)
	if len(term) != 1 || term[0].Meta["vendor_state_unreflected"] != true || term[0].Meta["error_class"] != "apply_conflict" {
		t.Fatalf("the terminal audit row must carry vendor_state_unreflected=true, got %+v", term)
	}
	if _, bad := term[0].Meta["vendor_reference_unbound"]; bad {
		t.Fatal("a submit conflict must not carry the create-side vendor_reference_unbound key")
	}
	for _, ar := range a {
		for k, val := range ar.Meta {
			if s, ok := val.(string); ok && strings.Contains(s, vendorRef) {
				t.Fatalf("audit %s.%s carries the vendor reference value", ar.Action, k)
			}
		}
	}
	found := false
	for _, al := range r.alerts() {
		if al.Discriminator == "submit:mock:apply_conflict" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the submit:mock:apply_conflict discriminator, got %+v", r.alerts())
	}
}

// ---- security L-1: a TEMP table cannot shadow what the guard trigger reads ----

func TestOutboxGuard_SearchPath_TempTenantsTableCannotAlterTheTrigger_L1(t *testing.T) {
	r := newRig(t)
	r.create()
	claimed := claimOne(t, r.w) // a claimed row of an ACTIVE tenant
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE tenants (id uuid, status text) ON COMMIT DROP`); err != nil {
			t.Fatalf("temp table (the runtime role holds TEMP): %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.tenants VALUES ($1, 'suspended')`, r.f.tenantID); err != nil {
			t.Fatal(err)
		}
		// Non-vacuity: an UNQUALIFIED `tenants` in this session now IS the temp table.
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, r.f.tenantID).Scan(&status); err != nil || status != "suspended" {
			t.Fatalf("the shadow must be effective for an unpinned lookup (non-vacuity), got %q %v", status, err)
		}
		// The guard must still read the REAL tenants table: the tenant is active,
		// so a deferral for an inactive tenant is refused.
		_, err := spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'pending', last_error_class = 'deferred_tenant_inactive',
			next_attempt_at = now() + interval '1 minute' WHERE id = $1`, claimed.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("a temp tenants table decided a transition (search_path not pinned): %v", err)
		}
	})
}

// ---- security L-2: log redaction (S5, S5c) ----

func TestOutboxLogs_ResolverFailureAndPhaseCFailure_NeverCarryRawErrorText_L2(t *testing.T) {
	r := newRig(t)
	var logs bytes.Buffer
	r.w.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// S5: the credential resolver fails with secret-shaped text.
	const secret = "RESOLVER-SECRET-text-7788"
	r.w.Outbound = failingResolver{err: errors.New("vault unreachable token=" + secret)}
	r.create()
	r.pass()
	if !strings.Contains(logs.String(), "kyc_outbox_credential_unavailable") {
		t.Fatalf("setup: the resolver-failure log line must exist, got %q", logs.String())
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "vault unreachable") {
		t.Fatalf("the resolver error text reached the log: %s", logs.String())
	}

	// S5c: a phase-C failure logs only the closed redacted detail.
	logs.Reset()
	r2 := newRig(t)
	r2.w.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r2.create()
	r2.w.Config.PhaseCTimeout = time.Nanosecond
	r2.pass()
	out := logs.String()
	if !strings.Contains(out, "kyc_outbox_phase_c_failed") {
		t.Fatalf("setup: the phase-C failure log line must exist, got %q", out)
	}
	if !strings.Contains(out, `"detail":"timeout"`) {
		t.Fatalf("the phase-C failure detail must be the closed redacted value, got %s", out)
	}
	for _, raw := range []string{"context deadline exceeded", "kyc_submission_outbox", "kyc: "} {
		if strings.Contains(out, raw) {
			t.Fatalf("raw error text %q reached the phase-C failure log: %s", raw, out)
		}
	}
}

// ---- security I-1: a new row is always due now ----

func TestOutboxGuard_24c_InsertForcesNextAttemptAtToNow_I1(t *testing.T) {
	r := newRig(t)
	orphan := rawOrphan(t, r.pool, seedSecondAccount(t, r.pool, r.f))
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		var due bool
		err := tx.QueryRow(ctx, `INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, idempotency_key, next_attempt_at)
			VALUES ($1, $2, 'create', 'mock', $3, now() + interval '30 days') RETURNING next_attempt_at <= now()`,
			r.f.tenantID, orphan, "kv:"+orphan.String()).Scan(&due)
		if err != nil || !due {
			t.Errorf("a caller-supplied future next_attempt_at must be forced to now (due=%v err=%v)", due, err)
		}
	})
}

// F4 (FOR SHARE): the upload's read of the live create holds a row lock until
// the upload transaction ends, so a worker phase C that decides or cancels the
// create (FOR UPDATE) cannot slip between the read and the submit insert.
func TestOutbox_38c_UploadHoldsTheLiveCreateRowLock_F4(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	var lockErr error
	err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		live, err := hasLiveCreate(ctx, tx, r.f.tenantID, v.ID)
		if err != nil || !live {
			t.Fatalf("setup: the create must be live: %v %v", live, err)
		}
		probeErr := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx2 context.Context, tx2 pgx.Tx) error {
			var id uuid.UUID
			return tx2.QueryRow(ctx2, `SELECT id FROM kyc_submission_outbox WHERE id = $1 FOR UPDATE NOWAIT`, row.ID).Scan(&id)
		})
		lockErr = probeErr
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgCode(lockErr) != "55P03" {
		t.Fatalf("a concurrent FOR UPDATE on the live create must be refused (55P03) while the upload holds it, got %v", lockErr)
	}
}
