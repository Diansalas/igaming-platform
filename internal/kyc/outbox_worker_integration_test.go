//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.1, 10.2, 10.6): the outbox state machine, the
// outcomes, IC F3 and the crash/retry paths, driven through the real worker as
// the runtime role.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// 1. Create happy path: phase A leaves the orphan and a pending create row; the
// worker binds the reference and moves the row to sent; the audit trail is the
// §3.5 shape.
func TestOutbox_01_CreateHappyPath(t *testing.T) {
	r := newRig(t)
	v := r.create()
	if v.Status != StatusUnverified || mustGetProviderReference(t, r.pool, r.f.tenantID, v.ID) != "" {
		t.Fatal("phase A must leave the orphan shape (unverified, NULL reference)")
	}
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxPending || row.IdempotencyKey != "kv:"+v.ID.String() || row.PlayerAccount != r.f.playerID || row.ProviderID != "mock" {
		t.Fatalf("unexpected phase-A row %+v", row)
	}
	if c, s := r.calls(); c != 0 || s != 0 {
		t.Fatalf("phase A must make no vendor call, got %d/%d", c, s)
	}

	st := r.pass()
	if st.Results[resultSent] != 1 {
		t.Fatalf("expected one sent item, got %+v", st.Results)
	}
	row = onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxSent || row.Claims != 1 || row.ClaimToken == nil {
		t.Fatalf("create row after the pass = %+v", row)
	}
	got := r.reload(v.ID)
	if got.Status != StatusPending || got.ProviderReference == "" {
		t.Fatalf("expected a bound reference and pending status, got %q / %q", got.Status, got.ProviderReference)
	}

	a := r.auditFor(v.ID)
	if auditCount(a, "kyc.verification_requested") != 1 || auditCount(a, auditActionSubmissionEnqueued) != 1 || auditCount(a, "kyc.verification_submitted") != 1 {
		t.Fatalf("expected one requested, one enqueued and one submitted audit row, got %+v", a)
	}
	sub := auditByAction(a, "kyc.verification_submitted")[0]
	if sub.Actor != "system" || sub.Meta["platform_service"] != workerPlatformService || sub.Meta["outbox_id"] != row.ID.String() || sub.Meta["operation"] != "create" {
		t.Fatalf("the worker-written row must be a system row with platform_service and outbox context, got %+v", sub)
	}
	for k, val := range sub.Meta {
		if s, ok := val.(string); ok && strings.Contains(s, got.ProviderReference) {
			t.Fatalf("audit metadata %q carries the vendor reference", k)
		}
	}
}

// 2. Submit happy path: the pinned ids and the key reach the vendor.
func TestOutbox_02_SubmitHappyPath(t *testing.T) {
	r := newRig(t)
	v := r.createSent()
	d1 := seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
	d2 := seedDocument(t, r.pool, r.f, v.ID, DocumentSelfie, "s.png")

	var gotDocs []SubmittedDocument
	r.submitImpl = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		gotDocs = docs
		return ProviderResult{ProviderReference: ref, Outcome: ProviderReviewRequired, Reason: "manual_review"}, nil
	}
	passUntilQuiet(t, r.w)
	if _, s := r.calls(); s != 1 {
		t.Fatalf("expected exactly one vendor submit (the first row was superseded), got %d", s)
	}
	want := independentSubmissionIdempotencyKey(v.ID, []uuid.UUID{d1, d2})
	if r.submitKeys[0] != want || len(gotDocs) != 2 {
		t.Fatalf("pinned set / key not delivered: key=%q want=%q docs=%d", r.submitKeys[0], want, len(gotDocs))
	}
	rows := readOutbox(t, r.pool, r.f.tenantID, v.ID)
	var sent *obRow
	for i := range rows {
		if rows[i].Operation == OpSubmit && rows[i].State == OutboxSent {
			sent = &rows[i]
		}
	}
	if sent == nil || len(sent.DocumentIDs) != 2 {
		t.Fatalf("expected a sent submit row pinning 2 documents, got %+v", rows)
	}
	if got := r.reload(v.ID); got.Status != StatusReviewRequired {
		t.Fatalf("expected the definitive vendor outcome applied, got %q", got.Status)
	}
	a := r.auditFor(v.ID)
	if n := auditCount(a, "kyc.verification_submitted_to_provider"); n != 1 {
		t.Fatalf("expected one submitted_to_provider audit row, got %d", n)
	}
}

// 3. Ambiguous (each kind): the status is unchanged (IC condition 2), the row is
// pending with failed_attempts+1 and a DATABASE-clock backoff, one audit row.
func TestOutbox_03_AmbiguousKinds_StatusUnchanged_ICCondition2(t *testing.T) {
	cases := []struct {
		name string
		impl func(ref string) (ProviderResult, error)
	}{
		{"transport error", func(ref string) (ProviderResult, error) { return ProviderResult{}, errors.New("simulated timeout") }},
		{"ProviderError outcome", func(ref string) (ProviderResult, error) {
			return ProviderResult{ProviderReference: ref, Outcome: ProviderError, Reason: "vendor_ambiguous"}, nil
		}},
		{"unrecognized outcome", func(ref string) (ProviderResult, error) {
			return ProviderResult{ProviderReference: ref, Outcome: ProviderOutcome("???"), Reason: "x"}, nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			v := r.createSent()
			seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
			before := r.snapshot(v.ID)
			ref := v.ProviderReference
			r.submitImpl = func(ctx context.Context, _ string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
				return c.impl(ref)
			}
			passUntilQuiet(t, r.w)
			if after := r.snapshot(v.ID); after != before {
				t.Fatalf("IC condition 2: the verification row changed\nbefore %s\nafter  %s", before, after)
			}
			row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpSubmit)
			if row.State != OutboxPending || row.FailedAttempts != 1 || row.LastErrorClass != string(ClassAmbiguous) {
				t.Fatalf("submit row = %+v, want pending / 1 / ambiguous", row)
			}
			if d := nextAttemptIn(t, r.pool, r.f.tenantID, row.ID); !within(d, 30*time.Second, 10*time.Second) {
				t.Fatalf("next_attempt_at is %s ahead of the database clock, want about 30s (BackoffBase)", d)
			}
			if n := auditCount(r.auditFor(v.ID), auditActionSubmissionRetryScheduled); n != 1 {
				t.Fatalf("expected exactly one retry_scheduled audit row, got %d", n)
			}
			// A retry that is not yet due is neither claimed nor even attempted (M4).
			if st := r.pass(); st.Claimed != 0 || st.ClaimError {
				t.Fatalf("a not-yet-due row must not be claimed, got %+v", st)
			}
		})
	}
}

// 4. Not sent: no vendor call, class not_sent, status unchanged.
func TestOutbox_04_NotSent_NoCall(t *testing.T) {
	cases := []struct {
		name     string
		outbound OutboundCredentialResolver
	}{
		{"credential unavailable", failingResolver{err: providercred.ErrOutboundCredentialUnavailable}},
		{"call refused", failingResolver{err: ErrProviderCallRefused}},
		{"no resolver", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			v := r.create()
			r.w.Outbound = c.outbound
			r.pass()
			if cc, _ := r.calls(); cc != 0 {
				t.Fatalf("a not-sent result must make no vendor call, got %d", cc)
			}
			row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
			if row.State != OutboxPending || row.LastErrorClass != string(ClassNotSent) || row.FailedAttempts != 1 {
				t.Fatalf("create row = %+v, want pending / not_sent / 1", row)
			}
			if r.reload(v.ID).Status != StatusUnverified {
				t.Fatal("the orphan must stay unverified")
			}
		})
	}
}

type failingResolver struct{ err error }

func (f failingResolver) Resolve(context.Context, providercred.TenantTxRunner, uuid.UUID, string) (providercred.OutboundCredential, error) {
	return providercred.OutboundCredential{}, f.err
}

// 5. Exhaustion: MaxFailedAttempts failures end failed_terminal with an alert
// (subject tenant, discriminator, platform-owned) and an audit row; the
// verification row is byte-identical (IC F3).
func TestOutbox_05_Exhaustion_FailedTerminal_AlertAndVerificationIdentical(t *testing.T) {
	r := newRig(t)
	r.w.Config.MaxFailedAttempts = 3
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{}, errors.New("vendor down")
	}
	v := r.create()
	before := r.snapshot(v.ID)
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	for i := 1; i <= 3; i++ {
		if st := r.pass(); st.Claimed != 1 {
			t.Fatalf("attempt %d: expected one claim, got %+v", i, st)
		}
		if i < 3 {
			if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxPending || got.FailedAttempts != i {
				t.Fatalf("attempt %d: row = %+v", i, got)
			}
			makeDueNow(t, row.ID)
		}
	}
	got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if got.State != OutboxFailedTerminal || got.LastErrorClass != string(ClassAmbiguous) || got.FailedAttempts != 3 {
		t.Fatalf("create row = %+v, want failed_terminal / ambiguous / 3", got)
	}
	if after := r.snapshot(v.ID); after != before {
		t.Fatalf("the verification row must be byte-identical after a vendor outage\nbefore %s\nafter  %s", before, after)
	}
	al := r.alerts()
	if len(al) != 1 || al[0].Discriminator != "create:mock" || al[0].Severity != "p2" || al[0].Occurrences != 1 {
		t.Fatalf("expected one p2 alert with discriminator create:mock, got %+v", al)
	}
	for k := range al[0].Attributes {
		if k != "operation" && k != "provider_id" && k != "last_error_class" && k != "outbox_id" {
			t.Fatalf("alert attribute %q is outside the allowlist", k)
		}
	}
	if al[0].Attributes["outbox_id"] != row.ID.String() || al[0].Attributes["last_error_class"] != "ambiguous" {
		t.Fatalf("unexpected alert attributes %v", al[0].Attributes)
	}
	a := r.auditFor(v.ID)
	if auditCount(a, auditActionSubmissionFailedTerminal) != 1 || auditCount(a, auditActionSubmissionRetryScheduled) != 2 {
		t.Fatalf("expected 2 retry rows and 1 terminal row, got %+v", a)
	}
}

// 7a. Prepare: a suspended tenant defers without consuming an attempt, makes no
// vendor call and never writes the verification; it resumes when active.
func TestOutbox_07a_TenantInactive_DefersWithoutAttempt_ThenResumes_T_H(t *testing.T) {
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			r := newRig(t)
			v := r.create()
			before := r.snapshot(v.ID)
			row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
			setTenantStatus(t, r.pool, r.f.tenantID, status)
			t.Cleanup(func() { setTenantStatus(t, r.pool, r.f.tenantID, "active") })

			if st := r.pass(); st.Results[resultDeferred] != 1 {
				t.Fatalf("expected one deferral, got %+v", st.Results)
			}
			if c, _ := r.calls(); c != 0 {
				t.Fatalf("a non-active tenant must see no vendor call, got %d", c)
			}
			got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
			if got.State != OutboxPending || got.FailedAttempts != 0 || got.LastErrorClass != string(ClassDeferredTenantInactive) || got.Claims != 1 {
				t.Fatalf("deferred row = %+v, want pending / 0 attempts / deferred_tenant_inactive / claims 1", got)
			}
			if r.snapshot(v.ID) != before {
				t.Fatal("a deferral must not write the verification")
			}
			// Every pass defers again; claims counts the cycles, failed_attempts does not move.
			makeDueNow(t, row.ID)
			r.pass()
			got = onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
			if got.FailedAttempts != 0 || got.Claims != 2 {
				t.Fatalf("after a second deferral: attempts=%d claims=%d, want 0 / 2 (claims counts worker activity, IC C5)", got.FailedAttempts, got.Claims)
			}
			// Resume on reactivation.
			setTenantStatus(t, r.pool, r.f.tenantID, "active")
			makeDueNow(t, row.ID)
			r.pass()
			if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxSent {
				t.Fatalf("expected the row to resume and be sent once the tenant is active, got %s", got.State)
			}
		})
	}
}

// 7b. Prepare: a provider no longer configured -> cancelled/provider_deconfigured,
// zero vendor calls, never redirected (F4); the cascade cancels pending submits.
func TestOutbox_07b_ProviderDeconfigured_CancelsWithoutACall_F4(t *testing.T) {
	r := newRig(t)
	v := r.create()
	// Two registered synthetic adapters: the sole-synthetic fallback no longer
	// selects "mock", and the tenant holds no credential handle for it.
	second := NewMockKYCProvider()
	orch := NewOrchestrator(map[string]KYCProvider{"mock": r.spy, "mock2": renamedMock{second, "mock2"}}, nil)
	w := newScopedWorker(r.pool, orch, NewMockOutboundResolver())
	st := w.RunPass(context.Background())
	if st.Results[resultCancelled] != 1 {
		t.Fatalf("expected one cancellation, got %+v", st.Results)
	}
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxCancelled || row.CancelReason != string(CancelProviderDeconfigured) {
		t.Fatalf("create row = %s / %q, want cancelled / provider_deconfigured", row.State, row.CancelReason)
	}
	if c, _ := r.calls(); c != 0 {
		t.Fatalf("no vendor call may happen for a de-configured provider, got %d", c)
	}
	if second.created == nil || len(second.created) != 0 {
		t.Fatal("the row must never be redirected to another provider")
	}
	a := r.auditFor(v.ID)
	if auditCount(a, auditActionSubmissionCancelled) != 1 {
		t.Fatalf("expected one cancelled audit row, got %+v", a)
	}
}

// renamedMock is a second synthetic adapter under another provider id.
type renamedMock struct {
	*MockKYCProvider
	id string
}

func (r renamedMock) ID() string { return r.id }

// 7c. Prepare: a re-claim whose retry budget is exhausted ends failed_terminal
// (lease_expired) with an alert and no vendor call.
func TestOutbox_07c_ExhaustedReclaim_FailedTerminalLeaseExpired(t *testing.T) {
	r := newRig(t)
	r.w.Config.MaxFailedAttempts = 2
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	// A first claim that is never completed (crash), lease expired by fixture,
	// with the budget one short of exhausted.
	claimed := claimOne(t, r.w)
	if claimed.ID != row.ID {
		t.Fatal("claimed the wrong row")
	}
	outboxFixtureUpdate(t, row.ID, `lease_expires_at = now() - interval '1 second', failed_attempts = 1`)
	st := r.pass() // re-claim: failed_attempts 1 -> 2 == Max -> terminal at P, no call
	if st.Results[resultFailedTerminal] != 1 {
		t.Fatalf("expected a terminal item, got %+v", st.Results)
	}
	got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if got.State != OutboxFailedTerminal || got.LastErrorClass != string(ClassLeaseExpired) || got.FailedAttempts != 2 {
		t.Fatalf("create row = %+v, want failed_terminal / lease_expired / 2", got)
	}
	if c, _ := r.calls(); c != 0 {
		t.Fatalf("no vendor call for an exhausted re-claim, got %d", c)
	}
	if al := r.alerts(); len(al) != 1 || al[0].Discriminator != "create:mock" || al[0].Attributes["last_error_class"] != "lease_expired" {
		t.Fatalf("expected one lease_expired alert, got %+v", al)
	}
}

// 7d. Prepare: a staff decision on the orphan before the send -> cancelled /
// decided_concurrently, no vendor call, the staff decision is never overwritten.
func TestOutbox_07d_DecidedBeforeSend_Cancels_NoCall(t *testing.T) {
	r := newRig(t)
	v := r.create()
	staffID := r.staffReview(v.ID, StatusRejected)
	r.pass()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxCancelled || row.CancelReason != string(CancelDecidedConcurrently) {
		t.Fatalf("create row = %s / %q, want cancelled / decided_concurrently", row.State, row.CancelReason)
	}
	if c, _ := r.calls(); c != 0 {
		t.Fatalf("no vendor call for a decided orphan, got %d", c)
	}
	got := r.reload(v.ID)
	if got.Status != StatusRejected || got.ReviewedBy != staffID {
		t.Fatalf("the staff decision must be intact, got %q reviewed_by %s", got.Status, got.ReviewedBy)
	}
}

// 7e. Prepare: a document rejected (or removed) after enqueue changes the current
// set: cancelled / document_set_changed and the CURRENT set is re-enqueued in the
// same tx; sent under its own key.
func TestOutbox_07e_DocumentSetChanged_CancelsAndReenqueues(t *testing.T) {
	r := newRig(t)
	v := r.createSent()
	d1 := seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
	d2 := seedDocument(t, r.pool, r.f, v.ID, DocumentSelfie, "s.png")
	staffID := seedComplianceStaff(t, r.pool, r.f)
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewDocument(ctx, tx, ReviewDocumentParams{DocumentID: d2, StaffID: staffID, NewStatus: DocumentRejected, Reason: "blurry"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	passUntilQuiet(t, r.w)

	var cancelledChanged, sent int
	for _, row := range readOutbox(t, r.pool, r.f.tenantID, v.ID) {
		if row.Operation != OpSubmit {
			continue
		}
		if row.State == OutboxCancelled && row.CancelReason == string(CancelDocumentSetChanged) {
			cancelledChanged++
		}
		if row.State == OutboxSent {
			sent++
			if len(row.DocumentIDs) != 1 || row.DocumentIDs[0] != d1 {
				t.Fatalf("the re-enqueued set must be the current one {d1}, got %v", row.DocumentIDs)
			}
		}
	}
	// The rows for sets {d1} (superseded by {d1,d2}) and {d1,d2} (document set changed).
	if cancelledChanged != 1 || sent != 1 {
		t.Fatalf("expected one document_set_changed cancel and one sent re-enqueued row, got %d / %d", cancelledChanged, sent)
	}
	if _, s := r.calls(); s != 1 {
		t.Fatalf("expected exactly one vendor submit, got %d", s)
	}
}

// 8. Backoff table (pure function).
func TestOutbox_08_BackoffTable(t *testing.T) {
	base, ceiling := 30*time.Second, 30*time.Minute
	want := map[int]time.Duration{
		0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute,
		5: 8 * time.Minute, 6: 16 * time.Minute, 7: 30 * time.Minute, 8: 30 * time.Minute, 60: 30 * time.Minute, 1 << 30: 30 * time.Minute,
	}
	for n, w := range want {
		if got := backoff(n, base, ceiling); got != w {
			t.Errorf("backoff(%d) = %s, want %s", n, got, w)
		}
	}
	if backoff(3, 0, ceiling) != 0 {
		t.Error("a zero base is zero")
	}
}

// 9. Binding mismatch: terminal at once (no retry), distinct discriminator.
func TestOutbox_09_BindingMismatch_TerminalAtOnce_DistinctDiscriminator(t *testing.T) {
	r := newRig(t)
	r.w.Outbound = mismatchedKYCOutboundResolver{}
	v := r.create()
	r.pass()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxFailedTerminal || row.LastErrorClass != string(ClassCredentialBindingMismatch) || row.FailedAttempts != 1 {
		t.Fatalf("create row = %+v, want failed_terminal / credential_binding_mismatch / 1", row)
	}
	al := r.alerts()
	if len(al) != 1 || al[0].Discriminator != "create:mock:binding_mismatch" {
		t.Fatalf("expected the :binding_mismatch discriminator, got %+v", al)
	}
	if c, _ := r.calls(); c != 0 {
		t.Fatalf("no vendor call, got %d", c)
	}
	if st := r.pass(); st.Claimed != 0 {
		t.Fatalf("a terminal row is never retried, got %+v", st)
	}
}

// 10. apply_conflict: a deterministic 23505 in phase C (the vendor reference is
// already bound elsewhere) ends failed_terminal after the in-item retries with NO
// re-send loop; the audit row names the unbound reference, never its value (IC R3).
func TestOutbox_10_ApplyConflict_NoResendLoop_ReferenceNeverRecorded_ICR3(t *testing.T) {
	r := newRig(t)
	second := seedSecondAccount(t, r.pool, r.f)
	const dupRef = "dup-ref-SECRET-aaaa1111"
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{ProviderReference: dupRef, Outcome: ProviderPending, Reason: "created"}, nil
	}
	v1 := r.create()
	passUntilQuiet(t, r.w) // first create binds dupRef
	if r.reload(v1.ID).ProviderReference != dupRef {
		t.Fatal("setup: first create must bind the reference")
	}
	v2, _ := requestCreate(t, r.pool, second, "mock")
	before := r.snapshot(v2.ID)
	cBefore, _ := r.calls()
	st := r.pass()
	if st.Results[resultApplyConflict] != 1 {
		t.Fatalf("expected an apply_conflict item, got %+v", st.Results)
	}
	if cAfter, _ := r.calls(); cAfter-cBefore != 1 {
		t.Fatalf("apply_conflict must NOT re-send: vendor calls for the second create = %d, want 1", cAfter-cBefore)
	}
	row := onlyRow(t, r.pool, r.f.tenantID, v2.ID, OpCreate)
	if row.State != OutboxFailedTerminal || row.LastErrorClass != string(ClassApplyConflict) {
		t.Fatalf("create row = %s / %q, want failed_terminal / apply_conflict", row.State, row.LastErrorClass)
	}
	if r.snapshot(v2.ID) != before {
		t.Fatal("the orphan must be untouched")
	}
	a := r.auditFor(v2.ID)
	term := auditByAction(a, auditActionSubmissionFailedTerminal)
	if len(term) != 1 || term[0].Meta["vendor_reference_unbound"] != true || term[0].Meta["error_class"] != "apply_conflict" {
		t.Fatalf("the terminal audit row must carry vendor_reference_unbound=true, got %+v", term)
	}
	for _, ar := range a {
		for k, val := range ar.Meta {
			if s, ok := val.(string); ok && strings.Contains(s, dupRef) {
				t.Fatalf("audit %s.%s carries the vendor reference value", ar.Action, k)
			}
		}
	}
	found := false
	for _, al := range r.alerts() {
		if al.Discriminator == "create:mock:apply_conflict" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the :apply_conflict discriminator, got %+v", r.alerts())
	}
}

// 11. Crash A -> B: the row is pending, the verification an orphan; the next
// pass sends.
func TestOutbox_11_CrashAfterPhaseA_NextPassSends(t *testing.T) {
	r := newRig(t)
	v := r.create() // "crash" here: no worker has run
	if row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxPending || row.Claims != 0 {
		t.Fatalf("row = %+v, want pending with no claim", row)
	}
	if r.reload(v.ID).Status != StatusUnverified {
		t.Fatal("the verification must be the orphan")
	}
	r.pass()
	if row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxSent {
		t.Fatalf("row = %s, want sent", row.State)
	}
}

// 12. Crash B -> C: the vendor accepted but phase C never ran; after lease expiry
// (fixture) the row is re-claimed (lease_expired, counted once) and re-sent with
// the SAME key; exactly one success audit row.
func TestOutbox_12_CrashAfterVendorCall_ReclaimSameKey(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)

	claimed := claimOne(t, r.w)
	po, err := r.w.prepare(context.Background(), claimed)
	if err != nil || po.kind != prepProceed {
		t.Fatalf("prepare: %v %+v", err, po)
	}
	out := r.w.callVendor(context.Background(), claimed, po.prep) // vendor accepted; "crash" before phase C
	if out.kind != outcomeDefinitive {
		t.Fatalf("setup: vendor outcome %v", out.kind)
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxClaimed {
		t.Fatalf("row = %s, want claimed", got.State)
	}
	if st := r.pass(); st.Claimed != 0 || st.ClaimError {
		t.Fatalf("an unexpired lease must not be re-claimed (and the claim must not even be attempted: M4), got %+v", st)
	}
	expireLease(t, row.ID)
	r.pass()

	got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if got.State != OutboxSent || got.Claims != 2 || got.FailedAttempts != 1 || got.LastErrorClass != string(ClassLeaseExpired) {
		t.Fatalf("row = %+v, want sent / claims 2 / failed_attempts 1 / lease_expired", got)
	}
	if len(r.createKeys) != 2 || r.createKeys[0] != r.createKeys[1] || r.createKeys[0] != "kv:"+v.ID.String() {
		t.Fatalf("the re-send must use the SAME idempotency key, got %v", r.createKeys)
	}
	if n := auditCount(r.auditFor(v.ID), "kyc.verification_submitted"); n != 1 {
		t.Fatalf("expected exactly one success audit row, got %d", n)
	}
}

// 13. A phase-C DB failure: bounded retry inside the item, then the row stays
// claimed; the lease path recovers.
func TestOutbox_13_PhaseCFailure_BoundedRetry_ThenLeasePath(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	r.w.Config.PhaseCTimeout = time.Nanosecond // every phase-C attempt fails on its deadline
	st := r.pass()
	if st.Results[resultPhaseCFailed] != 1 {
		t.Fatalf("expected a phase_c_failed item, got %+v", st.Results)
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxClaimed {
		t.Fatalf("row = %s, want claimed (no state change without a landed phase C)", got.State)
	}
	if r.reload(v.ID).Status != StatusUnverified {
		t.Fatal("the verification must be untouched")
	}
	r.w.Config.PhaseCTimeout = phaseCTimeout
	expireLease(t, row.ID)
	r.pass()
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxSent || got.Claims != 2 {
		t.Fatalf("row = %+v, want sent after the lease path", got)
	}
}

// 15. Lost claim: A's phase C affects 0 rows, rolls back, and A's result is not
// applied; B's own result stands.
func TestOutbox_15_LostClaim_ResultDiscarded(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)

	a := claimOne(t, r.w)
	po, err := r.w.prepare(context.Background(), a)
	if err != nil || po.kind != prepProceed {
		t.Fatalf("A prepare: %v %+v", err, po)
	}
	aOut := r.w.callVendor(context.Background(), a, po.prep)
	// A's lease expires; B re-claims and completes.
	expireLease(t, row.ID)
	if st := r.pass(); st.Results[resultSent] != 1 {
		t.Fatalf("B: expected a sent item, got %+v", st.Results)
	}
	bRef := mustGetProviderReference(t, r.pool, r.f.tenantID, v.ID)
	snapshot := r.snapshot(v.ID)

	if got := r.w.runPhaseC(context.Background(), a, po.prep, aOut, false); got != resultClaimLost {
		t.Fatalf("A's phase C = %q, want claim_lost", got)
	}
	if r.snapshot(v.ID) != snapshot || mustGetProviderReference(t, r.pool, r.f.tenantID, v.ID) != bRef {
		t.Fatal("A's result must not be applied")
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxSent || got.Claims != 2 {
		t.Fatalf("row = %+v", got)
	}
}

// 17. Duplicate submit of the same content is a no-op while a live or sent row
// exists; after failed_terminal the same content may be enqueued again.
func TestOutbox_17_DuplicateSubmit_NoOp_ThenReenqueueAfterTerminal(t *testing.T) {
	r := newRig(t)
	v := r.createSent()
	seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")

	enqueue := func() (bool, error) {
		var inserted bool
		err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			cur, docs, ok, err := gatherSubmissionDocuments(ctx, tx, v.ID)
			if err != nil || !ok {
				return fmt.Errorf("gather: %v ok=%v", err, ok)
			}
			_, inserted, err = enqueueSubmitRow(ctx, tx, cur, docs, playerEnqueueActor(r.f.playerID))
			return err
		})
		return inserted, err
	}
	if ins, err := enqueue(); err != nil || ins {
		t.Fatalf("a duplicate of a pending row must be a no-op, got inserted=%v err=%v", ins, err)
	}
	if n := len(readOutbox(t, r.pool, r.f.tenantID, v.ID)); n != 2 { // create + one submit
		t.Fatalf("expected exactly one submit row, got %d outbox rows", n)
	}
	dups := 0
	for _, a := range r.auditFor(v.ID) {
		if a.Action == auditActionSubmissionEnqueued && a.Meta["duplicate"] == true {
			dups++
		}
	}
	if dups != 1 {
		t.Fatalf("expected one duplicate=true audit row, got %d", dups)
	}
	// Sent: still a no-op.
	passUntilQuiet(t, r.w)
	if ins, err := enqueue(); err != nil || ins {
		t.Fatalf("a duplicate of a SENT row must be a no-op, got inserted=%v err=%v", ins, err)
	}
	// failed_terminal (fixture) -> the same content may be enqueued again.
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpSubmit)
	outboxFixtureUpdate(t, row.ID, `state = 'failed_terminal', last_error_class = 'ambiguous', terminal_at = now()`)
	if ins, err := enqueue(); err != nil || !ins {
		t.Fatalf("after failed_terminal the same content may be enqueued again, got inserted=%v err=%v", ins, err)
	}
}

// 18. Create/submit ordering: a submit is not claimable while the verification's
// create is live.
func TestOutbox_18_SubmitNotClaimableWhileCreateLive(t *testing.T) {
	r := newRig(t)
	v := r.create()
	seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png") // accepted on a live-create orphan
	first := claimOne(t, r.w)
	if first.Operation != OpCreate || first.VerificationID != v.ID {
		t.Fatalf("the create must be claimed first, got %s", first.Operation)
	}
	if got, err := r.w.claimNext(context.Background(), nil); err != nil || got != nil {
		t.Fatalf("the submit must not be claimable while the create is claimed, got %+v err=%v", got, err)
	}
	po, _ := r.w.prepare(context.Background(), first)
	out := r.w.callVendor(context.Background(), first, po.prep)
	if res := r.w.runPhaseC(context.Background(), first, po.prep, out, false); res != resultSent {
		t.Fatalf("create phase C = %q", res)
	}
	// Create sent: the submit is now claimable.
	if got := claimOne(t, r.w); got.Operation != OpSubmit {
		t.Fatalf("expected the submit to become claimable once the create is sent, got %s", got.Operation)
	}
}

// 19. P's re-enqueue racing a concurrent enqueue of the same set: ON CONFLICT DO
// NOTHING, never a 23505 abort (F12).
func TestOutbox_19_ReenqueueRacingSameSet_NoAbort_F12(t *testing.T) {
	r := newRig(t)
	v := r.createSent()
	d1 := seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
	rowD1 := onlyRowBySet(t, r, v.ID, 1)
	// Hold row {d1} back so the {d1,d2} row is claimed first.
	outboxFixtureUpdate(t, rowD1.ID, `next_attempt_at = now() + interval '1 hour'`)
	d2 := seedDocument(t, r.pool, r.f, v.ID, DocumentSelfie, "s.png")
	_ = d2
	staffID := seedComplianceStaff(t, r.pool, r.f)
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewDocument(ctx, tx, ReviewDocumentParams{DocumentID: d2, StaffID: staffID, NewStatus: DocumentRejected, Reason: "blurry"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// {d1,d2} is claimed; the current set is {d1}, whose key is already LIVE
	// (rowD1, pending): the re-enqueue must hit ON CONFLICT DO NOTHING.
	st := r.pass()
	if st.Results[resultCancelled] != 1 || st.Results[resultPhaseCFailed] != 0 || st.Results[resultPrepareFailed] != 0 {
		t.Fatalf("expected the {d1,d2} row cancelled without an abort, got %+v", st.Results)
	}
	live := 0
	for _, row := range readOutbox(t, r.pool, r.f.tenantID, v.ID) {
		if row.Operation == OpSubmit && row.State == OutboxPending && len(row.DocumentIDs) == 1 && row.DocumentIDs[0] == d1 {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("expected exactly one live {d1} row (no second row from the re-enqueue), got %d", live)
	}
}

func onlyRowBySet(t *testing.T, r *rig, verificationID uuid.UUID, n int) obRow {
	t.Helper()
	var found []obRow
	for _, row := range readOutbox(t, r.pool, r.f.tenantID, verificationID) {
		if row.Operation == OpSubmit && len(row.DocumentIDs) == n {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one submit row pinning %d document(s), got %d", n, len(found))
	}
	return found[0]
}
