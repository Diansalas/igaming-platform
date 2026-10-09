//go:build integration

package payments

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// The K3 fix batch (security PM-S1..S4, LF F-1..F-6, code-review F-2).

var k3Functions = []string{
	"payment_reserved_ref_prefix", "payment_m2_admits", "payment_manual_resolution_execution_status",
	"payment_manual_resolutions_guard", "payment_manual_resolutions_beneficiary_guard",
	"payment_manual_resolutions_no_executing_commit", "payment_manual_resolution_approvals_guard",
	"payment_manual_resolution_approvals_apply_reject", "payment_attempt_reference_evidence_guard",
	"payment_attempt_reference_evidence_bound_to_park", "ledger_transactions_reserved_prefix_guard",
	"payment_attempts_guard", "payment_attempts_operator_column_discipline", "ledger_governed_fence_allows",
	"ledger_entries_governed_fence", "ledger_adjustment_payload_refusal",
	"ledger_adjustment_requests_step_b_person_sep", "ledger_adjustment_approvals_step_b_person_sep",
}

// PM-S1 / code-review F-1: EVERY function 0115 creates or replaces pins search_path
// (a trigger function resolving a bare table name through a session-controlled path
// can be shadowed by a TEMP table). The catalogue is exact: the live proconfig of
// each listed function AND a static count over the migration text, so a new
// function without the SET fails here.
func TestK3_Y01_EveryFunctionPinsSearchPath(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(realMigrationsDir(t), "0115_payment_force_resolution.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	created := regexp.MustCompile(`(?m)^CREATE (?:OR REPLACE )?FUNCTION (\w+)`).FindAllStringSubmatch(string(src), -1)
	if len(created) != len(k3Functions) {
		t.Fatalf("0115 creates %d functions, the pinned list has %d: update k3Functions", len(created), len(k3Functions))
	}
	if n := strings.Count(string(src), "SET search_path = pg_catalog, public, pg_temp"); n != len(created) {
		t.Fatalf("%d functions but %d SET search_path clauses", len(created), n)
	}
	w := newK3World(t, k3Opts{base: 1})
	for _, name := range k3Functions {
		rows := w.sysQuery(`SELECT array_to_string(proconfig, ',') AS cfg FROM pg_proc WHERE proname = $1 AND pronamespace = 'public'::regnamespace`, name)
		if len(rows) != 1 {
			t.Errorf("%s: %d catalogue rows", name, len(rows))
			continue
		}
		cfg, _ := rows[0]["cfg"].(string)
		if !strings.Contains(cfg, "search_path=pg_catalog, public, pg_temp") {
			t.Errorf("%s: proconfig = %q, want search_path=pg_catalog, public, pg_temp", name, cfg)
		}
	}
}

// PM-S1: a session-created TEMP table named like a table the trigger reads must NOT
// shadow it. (PRH-2 R2: this probe runs on the scratch OWNER pool, which holds TEMP;
// the runtime role can no longer create TEMP objects at all - migration 0116, ADR 0108.)
// With a fake `executing` resolution of this very
// txid in a TEMP table, a reserved-prefix withdrawal_completed is still MR020.
func TestK3_Y02_TempTableShadowCannotDefeatTheReservedNamespace(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	reserved := ReservedDeclaredTxID(uuid.New())
	key := w.provider + ":" + reserved
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE payment_manual_resolutions (tenant_id uuid, kind text, state text, executed_txid bigint,
			withdrawal_request_id uuid, provider_id text, reserved_provider_tx_id text, attempt_id uuid, target_state text) ON COMMIT DROP`); err != nil {
			t.Fatalf("create temp table: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.payment_manual_resolutions VALUES ($1, 'm2_declare_paid', 'executing', txid_current(), $2, $3, $4, $5, 'succeeded')`,
			w.f.tenantID, wr.ID, w.provider, reserved, a.ID); err != nil {
			t.Fatalf("insert fake executing row: %v", err)
		}
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "withdrawal_completed", key, &w.provider, &reserved, wr.ID)
		})
		if k3Code(err) != "MR020" {
			t.Errorf("a TEMP-table shadow admitted the reserved namespace: %v", err)
		}
		return nil
	})
}

// PM-S3 (Z17): the payload, actor and pinned policy of a pending resolution are
// immutable under direct UPDATE.
func TestK3_Y03_PendingResolutionPayloadIsImmutable(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	sets := map[string]string{
		"requested_by":            "requested_by = gen_random_uuid()",
		"requested_by_person_id":  "requested_by_person_id = gen_random_uuid()",
		"required_at_submission":  "required_at_submission = 9",
		"expires_at":              "expires_at = now() + interval '30 days'",
		"reserved_provider_tx_id": "reserved_provider_tx_id = 'k3-forged'",
		"contributing_policy_ids": "contributing_policy_ids = '{}'",
		"provider_id":             "provider_id = 'k3-other-provider'",
		"reason_code":             "reason_code = 'forged'",
		"amount":                  "amount = amount + 1",
	}
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		for name, set := range sets {
			set := set
			err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET `+set+` WHERE id = $1`, r.ID)
				return err
			})
			if k3Code(err) != "MR030" {
				t.Errorf("direct UPDATE of %s: want MR030, got %v", name, err)
			} else if msg := err.Error(); strings.Contains(msg, "invalid transition") {
				// the refusal must come from the immutability / hash check, not from the
				// state machine's fall-through (which would hide a dropped guard)
				t.Errorf("direct UPDATE of %s was refused only by the state-machine fall-through: %v", name, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// PM-S4 (Z05): a resolution submitted under a higher required count still needs the
// pinned count after a platform policy lowers it.
func TestK3_Y04_PinnedRequiredCountSurvivesALowerPolicy(t *testing.T) {
	w := newK3World(t, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	if r.RequiredAtSubmission != 2 {
		t.Fatalf("setup: required at submission = %d", r.RequiredAtSubmission)
	}
	w.approvePolicy(adjustment.PolicyChangeInput{
		ChangeKind: adjustment.ChangeKindPolicy, OperationKind: OperationKindForceResolve, Level: adjustment.LevelPlatform,
		AssetCode: k3StrPtr("EUR"), BaseRequiredApprovals: k3IntPtr(1),
	}, w.adminA, w.adminB)
	// Non-vacuity: a NEW request now pins the lower count.
	_, a2 := w.ambiguousPayout(100)
	if r2 := w.mustRequest(w.f1, w.m2In(a2.ID, ResolutionM2DeclareNotPaid)); r2.RequiredAtSubmission != 1 {
		t.Fatalf("setup: the lower policy did not take effect (new request pins %d)", r2.RequiredAtSubmission)
	}
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || out.Executed || out.Required != 2 {
		t.Fatalf("one approval executed (or the required count dropped) under a lowered policy: %v %+v", err, out)
	}
	if w.attempt(a.ID).State != AttemptAmbiguous {
		t.Fatal("the attempt moved")
	}
}

// F-1 regression (C-42b, LF P1; security PM-S2): ANOTHER live payout's statement line
// that borrows A's merchant reference (but carries that payout's own reference) must
// never clear A's pay_declared_paid_unconfirmed; A's own line does.
func TestK3_C42b_AnotherPayoutsLineCannotConfirm(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.executeM2(a.ID, ResolutionM2DeclarePaid)
	_, b := w.payout(500) // another live payout with its own reference
	if b.ProviderReference == nil || *b.ProviderReference == *a.ProviderReference {
		t.Fatal("setup: B needs its own reference")
	}
	borrowed := w.payoutLine(*b.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)
	for i := 0; i < 2; i++ { // the line is persisted: it must not clear in later runs either
		src := w.source(false, borrowed)
		if i == 1 {
			src = w.pastSource(false)
		}
		ms := w.stmtRun(src)
		w.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "B's line borrowing A's merchant reference (run "+string(rune('0'+i))+")")
	}
	// Non-vacuity: A's own line (by reference) confirms.
	ms := w.stmtRun(w.source(false, w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)))
	w.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "A's own confirming line")
	// A's own line by REFERENCE alone (no merchant reference on the line) confirms.
	w3 := newK3World(t, k3Opts{base: 1})
	_, a3 := w3.ambiguousPayout(500)
	w3.executeM2(a3.ID, ResolutionM2DeclarePaid)
	w3.requireOne(w3.stmtRun(w3.source(false)), reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a3.ID, "no line yet")
	ms = w3.stmtRun(w3.source(false, w3.payoutLine(*a3.ProviderReference, "", statement.PaymentStatusSucceeded, 500)))
	w3.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a3.ID, "a line resolved to A by its provider reference alone")
	// A reference-less line that resolves to A by merchant reference alone (no other
	// attempt holds its reference) also confirms.
	w2 := newK3World(t, k3Opts{base: 1})
	_, a2 := w2.ambiguousPayout(500)
	w2.executeM2(a2.ID, ResolutionM2DeclarePaid)
	ms = w2.stmtRun(w2.source(false, w2.payoutLine("k3-nobody-holds-"+uuid.NewString(), a2.MerchantReference, statement.PaymentStatusSucceeded, 500)))
	w2.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a2.ID, "merchant-reference resolution when nobody holds the line's reference")
}

// F-2 (GK): an unrelated executed compensating debit (different causation) never
// clears (d) pay_declared_not_paid_but_paid or (c2) pay_declared_paid_compensated_but_paid.
func TestK3_Y05_UnrelatedCompensatingDebitDoesNotClear(t *testing.T) {
	// (d)
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(300)
	resA := w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	_, b := w.ambiguousPayout(300)
	resB := w.executeM2(b.ID, ResolutionM2DeclareNotPaid)
	line := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 300)
	w.requireOne(w.stmtRun(w.source(false, line)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "late success after not paid")
	if out, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 300, *resB.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("unrelated debit: %v %+v", err, out)
	}
	w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "an unrelated debit must not clear (d)")
	// A's own full recovery does clear it (non-vacuity).
	if out, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 300, *resA.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("A's recovery: %v %+v", err, out)
	}
	w.requireNone(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "A's own recovery clears (d)")

	// (c2)
	w2 := newK3World(t, k3Opts{base: 1})
	w2.k2Setup()
	_, pa := w2.ambiguousPayout(500)
	resPA := w2.executeM2(pa.ID, ResolutionM2DeclarePaid)
	_, pb := w2.ambiguousPayout(300)
	resPB := w2.executeM2(pb.ID, ResolutionM2DeclareNotPaid)
	if out, err := w2.k2Compensate(adjustment.DirectionCreditPlayer, 200, *resPA.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("compensating credit: %v %+v", err, out)
	}
	pl := w2.payoutLine(*pa.ProviderReference, pa.MerchantReference, statement.PaymentStatusSucceeded, 500)
	w2.requireOne(w2.stmtRun(w2.source(false, pl)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, pa.ID, "credited then paid")
	if out, err := w2.k2Compensate(adjustment.DirectionDebitPlayer, 300, *resPB.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("unrelated debit: %v %+v", err, out)
	}
	w2.requireOne(w2.stmtRun(w2.source(false)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, pa.ID, "an unrelated debit must not clear (c2)")
}

// F-3 (GM): (c2) is raised by ANY succeeded line from any import (a different amount;
// a MOCK line after a real import), not only by confirming lines.
func TestK3_Y06_CompensatedButPaidIsRaisedByAnySucceededLine(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)
	if out, err := w.k2Compensate(adjustment.DirectionCreditPlayer, 200, *res.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("compensating credit: %v %+v", err, out)
	}
	w.requireOne(w.stmtRun(w.source(false, w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 499))),
		reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a.ID, "a different-amount succeeded line")

	w2 := newK3World(t, k3Opts{base: 1})
	w2.k2Setup()
	_, a2 := w2.ambiguousPayout(500)
	res2 := w2.executeM2(a2.ID, ResolutionM2DeclarePaid)
	if out, err := w2.k2Compensate(adjustment.DirectionCreditPlayer, 200, *res2.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("compensating credit: %v %+v", err, out)
	}
	w2.stmtRun(w2.source(true)) // a non-MOCK import exists: MOCK evidence is ineligible to CLEAR or CONFIRM ...
	w2.requireOne(w2.stmtRun(w2.source(false, w2.payoutLine(*a2.ProviderReference, a2.MerchantReference, statement.PaymentStatusSucceeded, 500))),
		reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a2.ID, "... but a MOCK succeeded line still RAISES (c2)")
}

// F-4 (GD): the executor's A8 L1 locks (staff and grants FOR SHARE) make a revoke of
// a counted approver's grant WAIT for the execution transaction instead of racing it.
func TestK3_Y07_RevokeVersusExecutionIsSerialisedByTheShareLocks(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	gid := w.grantIDs[k3GrantKey(w.f2.ID, capability.CapabilityPaymentForceResolveApprove)]
	var mu sync.Mutex
	var revokeErr, suspendErr error
	hooked := false
	testHookResolutionAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
		mu.Lock()
		defer mu.Unlock()
		hooked = true
		rctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		revokeErr = w.pool.WithPrincipalScope(rctx, w.f.tenantID, w.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
			return capability.RevokeGrant(ctx, tx, w.f.tenantID, gid, "k3-race")
		})
		// The approver's STAFF row is share-locked too: a suspension waits as well.
		sctx, scancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer scancel()
		suspendErr = w.pool.WithTenant(sctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, w.f2.ID)
			return err
		})
	}
	out, err := w.decide(w.f2, r, ResolutionApprove)
	testHookResolutionAfterShareLocks = nil
	if err != nil || !out.Executed {
		t.Fatalf("execution: %v %+v", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !hooked {
		t.Fatal("the after-share-locks hook never ran")
	}
	isTimeout := func(err error) bool {
		return err != nil && (errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") ||
			strings.Contains(err.Error(), "canceling statement") || k3Code(err) == "57014")
	}
	if !isTimeout(revokeErr) {
		t.Fatalf("a revoke must WAIT for the execution's share locks and time out; got %v (nil = it completed: the A8 L1 locks are missing)", revokeErr)
	}
	if !isTimeout(suspendErr) {
		t.Fatalf("a staff suspension must WAIT for the share locks and time out; got %v (nil = the staff lock is missing)", suspendErr)
	}
	// After the execution commits, the revoke goes through.
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, w.f.tenantID, gid, "k3-race-after")
	}); err != nil {
		t.Fatalf("revoke after the execution: %v", err)
	}
	// ... and so does the suspension (it was refused only by the lock wait).
	w.setStaff(w.f2.ID, "status", "suspended")
}

// code-review F-2 (K3-S2): the requester's Person must be UNCHANGED since submission.
// Scratch database: the owner re-links the staff rows (the append-only trigger is
// disabled for that one transaction).
func TestK3_Y08_RequesterPersonMustBeUnchangedAtExecution(t *testing.T) {
	pool := depositV2ScratchPool(t) // B13-B: the payments code reads the B13 columns, so a payout world needs the head schema
	w := newK3WorldOn(t, pool, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	p1 := w.f1.PersonID
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid)) // requested_by_person_id = P1
	if out, err := w.decide(w.f2, r, ResolutionApprove); err != nil || out.Executed || out.Counted != 1 {
		t.Fatalf("first approval: %v %+v", err, out)
	}
	p2 := uuid.New()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p2); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE staff_users DISABLE TRIGGER staff_users_person_id_append_only`); err != nil {
			t.Fatalf("scratch DB: cannot disable the append-only trigger: %v", err)
		}
		// The requester moves to P2; the first approver takes over P1.
		if _, err := tx.Exec(ctx, `UPDATE staff_users SET person_id = $2 WHERE id = $1`, w.f1.ID, p2); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE staff_users SET person_id = $2 WHERE id = $1`, w.f2.ID, p1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE staff_users ENABLE TRIGGER staff_users_person_id_append_only`)
		return err
	})
	// A grant is bound to its request-time Person snapshot, so the relinked staff
	// first lose their old grants; fresh grants (snapshot = the NEW live Person) make
	// both principals fully eligible again. Only the "requester's Person unchanged"
	// term of the recount then stops the requester-of-record P1 approving.
	w.revokeGrant(w.f1.ID, capability.CapabilityPaymentForceResolveRequest)
	w.revokeGrant(w.f2.ID, capability.CapabilityPaymentForceResolveApprove)
	w.grantTenant(w.f1, capability.CapabilityPaymentForceResolveRequest)
	w.grantTenant(w.f2, capability.CapabilityPaymentForceResolveApprove)
	out, err := w.decide(w.f3, r, ResolutionApprove)
	if err == nil && out.Executed {
		t.Fatalf("executed although the requester's Person changed since submission (P1 is now requester-of-record AND an approver): %+v", out)
	}
	if got := w.attempt(a.ID).State; got != AttemptAmbiguous {
		t.Fatalf("attempt %s", got)
	}
	if got := w.resolution(r.ID).State; got != ResolutionPending {
		t.Fatalf("resolution %s", got)
	}
}

// Z10: the REQUESTER's policy-author exclusion is re-checked at execution (a
// requester who becomes a contributing-policy author before execution no longer
// qualifies).
func TestK3_Y09_RequesterWhoLaterAuthorsAPolicyNoLongerQualifies(t *testing.T) {
	w := newK3World(t, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid)) // platform acting requester
	if out, err := w.decide(w.f2, r, ResolutionApprove); err != nil || out.Executed || out.Counted != 1 {
		t.Fatalf("first approval: %v %+v", err, out)
	}
	w.approvePolicy(adjustment.PolicyChangeInput{
		ChangeKind: adjustment.ChangeKindPolicy, OperationKind: OperationKindForceResolve, Level: adjustment.LevelPlatform,
		AssetCode: k3StrPtr("EUR"), BaseRequiredApprovals: k3IntPtr(2),
	}, w.adminB, w.acting) // the requester approves a new policy row
	out, err := w.decide(w.f3, r, ResolutionApprove)
	if err == nil && out.Executed {
		t.Fatalf("executed although the requester authored a contributing policy before execution: %+v", out)
	}
	if w.attempt(a.ID).State != AttemptAmbiguous {
		t.Fatal("the attempt moved")
	}
}

// Z16: pending -> refused_at_execution is only legal in the final approval's own
// transaction.
func TestK3_Y10_RefusedAtExecutionNeedsASameTransactionApproval(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f2.ID, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'refused_at_execution', refusal_code = 'forged' WHERE id = $1`, r.ID)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("a refused_at_execution without a same-transaction approval: want MR030, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// F-6: the (d) finding's resolution hint no longer promises an off-platform
// recording path that does not exist.
func TestK3_Y11_NotPaidButPaidHintTextIsHonest(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(300)
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	ms := w.stmtRun(w.source(false, w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 300)))
	m := w.requireOne(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "late success")
	if strings.Contains(m.ExpectedValue, "recorded as such") || !strings.Contains(m.ExpectedValue, "has no clearing path") {
		t.Fatalf("hint text: %q", m.ExpectedValue)
	}
}
