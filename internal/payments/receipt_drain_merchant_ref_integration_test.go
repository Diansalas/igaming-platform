//go:build integration

// PAY-RECEIPT-DRAIN-MERCHANT-REF-1 (ADR 0095 section 42.8, ledger-finance I-1; INV-IO-14).
//
// The deferred-receipt drain used to match on (provider, provider reference, event type) only and apply
// the evidence even when the receipt's merchant reference named a DIFFERENT attempt. It now applies the
// live path's merchant-reference consistency check: a merchant reference resolving to another provider's
// (or an unrouted) attempt closes the receipt as anomaly_cross_provider, one resolving to another attempt
// of the same provider closes it as anomaly_reference_conflict (strict ResolveReceipt, attempt_id NULL
// like the live path), and in both cases nothing is applied. Matching or absent merchant references
// behave as before.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// dmDrain runs the drain for the attempt exactly as a binding caller does (parent lock, re-read, drain).
func (e t4DrainEnv) dmDrain(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var applied int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, e.intent); err != nil {
			return err
		}
		a, err := GetAttemptByID(ctx, tx, id)
		if err != nil {
			return err
		}
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, e.orch, a)
		return err
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return applied
}

func dmDepositEvent(ref, merchant string) ReceiptEvidence {
	return ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: merchant,
		Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
}

func (e t4DrainEnv) dmAttemptState(t *testing.T, id uuid.UUID) AttemptState {
	return mustGetAttempt(t, e.pool, e.f.tenantID, id).State
}

// A second attempt of ANOTHER provider in the same tenant (ref-less ambiguous, provider_id set).
func (e t4DrainEnv) dmOtherProviderAttempt(t *testing.T, otherID string) PaymentAttempt {
	t.Helper()
	mp := NewMockProvider(otherID, "EUR")
	registerCapability(t, e.pool, e.f, mp, 1)
	orch2 := NewOrchestrator(map[string]PaymentProvider{otherID: &refLessAmbiguousProvider{mp}},
		MultiWebhookCredentialResolver{otherID: NewMockWebhookCredentials(mp)})
	res := rvInit(t, e.pool, orch2, e.f, MockAmountAmbiguous, "dm-other-"+otherID)
	if res.Attempt.ProviderID == nil || *res.Attempt.ProviderID != otherID {
		t.Fatalf("setup: second attempt provider = %v, want %s", res.Attempt.ProviderID, otherID)
	}
	return res.Attempt
}

func TestDrainMerchantRef_Deposit_MatchingMerchantRef_Applies(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-match", "dm-match")
	ref := "dm-match-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	e.orPlant(t, e.provider, dmDepositEvent(ref, e.attempt.MerchantReference))
	if got := e.dmDrain(t, e.attempt.ID); got != 1 {
		t.Fatalf("applied = %d, want 1", got)
	}
	t4AssertConverged(t, e, ref)
	row := e.orReceipt(t, e.provider, ref)
	if row.resolution != string(ResolutionApplied) || row.attemptID == nil || *row.attemptID != e.attempt.ID {
		t.Fatalf("receipt = %+v", row)
	}
}

func TestDrainMerchantRef_Deposit_AbsentMerchantRef_Applies(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-absent", "dm-absent")
	ref := "dm-absent-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	e.orPlant(t, e.provider, dmDepositEvent(ref, ""))
	if got := e.dmDrain(t, e.attempt.ID); got != 1 {
		t.Fatalf("applied = %d, want 1", got)
	}
	t4AssertConverged(t, e, ref)
}

// A merchant reference that resolves to nothing is behaviour-unchanged (the live path resolves by reference).
func TestDrainMerchantRef_Deposit_UnknownMerchantRef_Applies(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-unk", "dm-unk")
	ref := "dm-unk-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	e.orPlant(t, e.provider, dmDepositEvent(ref, "dm-no-such-merchant-ref"))
	if got := e.dmDrain(t, e.attempt.ID); got != 1 {
		t.Fatalf("applied = %d, want 1", got)
	}
	t4AssertConverged(t, e, ref)
}

type dmSnap struct {
	a, c     AttemptState
	ledgerTx int64
	entries  int64
	cash     int64
	intent   string
}

func (e t4DrainEnv) dmSnap(t *testing.T, other uuid.UUID) dmSnap {
	t.Helper()
	var st string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id=$1`, e.intent).Scan(&st)
	}); err != nil {
		t.Fatal(err)
	}
	return dmSnap{a: e.dmAttemptState(t, e.attempt.ID), c: e.dmAttemptState(t, other),
		ledgerTx: e.count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, e.f.tenantID),
		entries:  e.count(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id=$1`, e.f.tenantID),
		cash:     cashBalance(t, e.pool, e.f), intent: st}
}

func TestDrainMerchantRef_Deposit_ForeignAttemptSameProvider_ClosedAsReferenceConflict(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-conflict", "dm-conflict")
	c := e.orSecondAmbiguous(t, "dm-conflict-c")
	ref := "dm-conflict-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	e.orPlant(t, e.provider, dmDepositEvent(ref, c.MerchantReference))
	if n := e.orUnapplied(t, e.provider); n != 1 {
		t.Fatalf("setup: cap count = %d, want 1", n)
	}
	before := e.dmSnap(t, c.ID)
	if got := e.dmDrain(t, e.attempt.ID); got != 0 {
		t.Fatalf("applied = %d, want 0 (closed, not applied)", got)
	}
	if after := e.dmSnap(t, c.ID); after != before {
		t.Fatalf("state/ledger changed: before=%+v after=%+v", before, after)
	}
	row := e.orReceipt(t, e.provider, ref)
	if !row.resolved || row.resolution != string(ResolutionAnomalyReferenceConflict) || row.attemptID != nil {
		t.Fatalf("receipt = %+v, want reference-conflict, attempt_id NULL", row)
	}
	if n := e.orUnapplied(t, e.provider); n != 0 {
		t.Fatalf("cap count after drain = %d, want 0", n)
	}
	// Replay: nothing left to drain, nothing changes.
	if got := e.dmDrain(t, e.attempt.ID); got != 0 {
		t.Fatalf("replay applied = %d", got)
	}
	if again := e.orReceipt(t, e.provider, ref); again.resolvedAt == nil || row.resolvedAt == nil || !again.resolvedAt.Equal(*row.resolvedAt) || again.resolution != row.resolution {
		t.Fatalf("replay changed the receipt: %+v vs %+v", row, again)
	}
	if after := e.dmSnap(t, c.ID); after != before {
		t.Fatalf("replay changed state: %+v", after)
	}
	assertLedgerBalanced(t, e.pool, e.f.tenantID)
}

func TestDrainMerchantRef_Deposit_OtherProviderAttempt_ClosedAsCrossProvider(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-xp", "dm-xp")
	x := e.dmOtherProviderAttempt(t, "mock-dm-xp-other")
	ref := "dm-xp-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	e.orPlant(t, e.provider, dmDepositEvent(ref, x.MerchantReference))
	before := e.dmSnap(t, x.ID)
	if got := e.dmDrain(t, e.attempt.ID); got != 0 {
		t.Fatalf("applied = %d, want 0", got)
	}
	if after := e.dmSnap(t, x.ID); after != before {
		t.Fatalf("state/ledger changed: before=%+v after=%+v", before, after)
	}
	row := e.orReceipt(t, e.provider, ref)
	if !row.resolved || row.resolution != string(ResolutionAnomalyCrossProvider) || row.attemptID != nil {
		t.Fatalf("receipt = %+v, want cross-provider, attempt_id NULL", row)
	}
	if n := e.orUnapplied(t, e.provider); n != 0 {
		t.Fatalf("cap count = %d", n)
	}
}

// Ordering: the predates-submission close still comes first and keeps its label (the receipt is stored
// BEFORE the attempt it is drained against is created, and names another attempt's merchant reference).
func TestDrainMerchantRef_Deposit_PredatesSubmissionStillWinsOverMerchantCheck(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-pre", "dm-pre")
	c := e.orSecondAmbiguous(t, "dm-pre-c")
	ref := "dm-pre-" + uuid.NewString()
	e.orPlant(t, e.provider, dmDepositEvent(ref, c.MerchantReference))
	time.Sleep(50 * time.Millisecond)
	d := e.orSecondAmbiguous(t, "dm-pre-d")
	e.orBind(t, d.ID, ref)
	if got := e.dmDrain(t, d.ID); got != 0 {
		t.Fatalf("applied = %d", got)
	}
	row := e.orReceipt(t, e.provider, ref)
	if row.resolution != string(ResolutionAnomalyPredatesSubmission) {
		t.Fatalf("resolution = %q, want predates-submission", row.resolution)
	}
}

func TestDrainMerchantRef_Deposit_MixedBatch_OnlyForeignClosed_OrderPreserved(t *testing.T) {
	e := t4DrainSetup(t, "mock-dm-mix", "dm-mix")
	c := e.orSecondAmbiguous(t, "dm-mix-c")
	ref := "dm-mix-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	// Same (provider, ref, type) but distinct fingerprints: one foreign merchant ref, one matching.
	bad := dmDepositEvent(ref, c.MerchantReference)
	bad.Outcome, bad.Amount = OutcomePending, MockAmountAmbiguous
	e.orPlant(t, e.provider, bad)
	e.orPlant(t, e.provider, dmDepositEvent(ref, e.attempt.MerchantReference))
	if got := e.dmDrain(t, e.attempt.ID); got != 1 {
		t.Fatalf("applied = %d, want 1 (only the matching one)", got)
	}
	var conflicts, applied int64
	conflicts = e.count(t, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2 AND resolution=$3 AND attempt_id IS NULL`, e.f.tenantID, ref, string(ResolutionAnomalyReferenceConflict))
	applied = e.count(t, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2 AND resolution=$3 AND attempt_id=$4`, e.f.tenantID, ref, string(ResolutionApplied), e.attempt.ID)
	if conflicts != 1 || applied != 1 {
		t.Fatalf("conflicts=%d applied=%d, want 1 and 1", conflicts, applied)
	}
	if st := e.dmAttemptState(t, c.ID); st != AttemptAmbiguous {
		t.Fatalf("foreign attempt state = %s", st)
	}
}

// Tenant isolation: another tenant's receipt sharing (provider, reference) is not touched by this tenant's
// drain, and a merchant reference naming another tenant's attempt is invisible under RLS (resolves to
// nothing, exactly as on the live path) so it neither classifies nor leaks.
func TestDrainMerchantRef_Deposit_TenantIsolation(t *testing.T) {
	a := t4DrainSetup(t, "mock-dm-ti", "dm-ti-a")
	fB := seedOrchFixture(t, a.pool)
	mp := NewMockProvider("mock-dm-ti", "EUR")
	registerCapability(t, a.pool, fB, mp, 100)
	orchB := NewOrchestrator(map[string]PaymentProvider{"mock-dm-ti": &refLessAmbiguousProvider{mp}},
		MultiWebhookCredentialResolver{"mock-dm-ti": NewMockWebhookCredentials(mp)})
	resB := rvInit(t, a.pool, orchB, fB, MockAmountAmbiguous, "dm-ti-b")
	b := t4DrainEnv{pool: a.pool, orch: orchB, f: fB, attempt: resB.Attempt, intent: resB.Intent.ID, provider: "mock-dm-ti"}

	ref := "dm-ti-shared-" + uuid.NewString()
	a.orBind(t, a.attempt.ID, ref)
	b.orBind(t, b.attempt.ID, ref)
	// Tenant A: foreign merchant reference naming A's other attempt -> conflict. Tenant B: its receipt names
	// tenant A's attempt, which B cannot see -> applied as on the live path.
	c := a.orSecondAmbiguous(t, "dm-ti-a-c")
	a.orPlant(t, a.provider, dmDepositEvent(ref, c.MerchantReference))
	b.orPlant(t, b.provider, dmDepositEvent(ref, a.attempt.MerchantReference))

	if got := a.dmDrain(t, a.attempt.ID); got != 0 {
		t.Fatalf("tenant A applied = %d", got)
	}
	if rb := b.orReceipt(t, b.provider, ref); rb.resolved {
		t.Fatalf("tenant A's drain resolved tenant B's receipt: %+v", rb)
	}
	if got := b.dmDrain(t, b.attempt.ID); got != 1 {
		t.Fatalf("tenant B applied = %d, want 1", got)
	}
	if ra := a.orReceipt(t, a.provider, ref); ra.resolution != string(ResolutionAnomalyReferenceConflict) {
		t.Fatalf("tenant A receipt = %+v", ra)
	}
	if st := a.dmAttemptState(t, a.attempt.ID); st != AttemptPending {
		t.Fatalf("tenant A attempt moved off its bound (pending) state: %s", st)
	}
}

// ---- payout variants ----

type dmPayoutEnv struct {
	pool *db.Pool
	orch *Orchestrator
	f    payoutFixture
	prov string
}

func dmPayoutSetup(t *testing.T, prov string) dmPayoutEnv {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	p := NewMockProvider(prov, "EUR")
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{prov: p}, MultiWebhookCredentialResolver{prov: NewMockWebhookCredentials(p)})
	return dmPayoutEnv{pool: pool, orch: orch, f: f, prov: prov}
}

// dmNewPayout creates a payout attempt claimed for submission under providerID; when ref != "" it is accepted
// (reference bound), exactly as N3 does.
func (e dmPayoutEnv) dmNewPayout(t *testing.T, orch *Orchestrator, key, providerID, ref string) (withdrawalID uuid.UUID, a PaymentAttempt) {
	t.Helper()
	wr, att := notSentPayoutAttempt(t, e.pool, orch, e.f, 5000, key)
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, att.ID, providerID, uuid.New(), "dm-"+key, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ref != "" {
		if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return MarkAccepted(ctx, tx, att.ID, EvidencePlatform, ref, time.Now().Add(time.Minute))
		}); err != nil {
			t.Fatalf("accept: %v", err)
		}
	}
	return wr.ID, mustGetAttempt(t, e.pool, e.f.tenantID, att.ID)
}

func (e dmPayoutEnv) dmDrain(t *testing.T, wrID, attemptID uuid.UUID) int {
	t.Helper()
	var applied int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, wrID); err != nil {
			return err
		}
		a, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, e.orch, a)
		return err
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return applied
}

func (e dmPayoutEnv) dmPlant(t *testing.T, provider string, ev ReceiptEvidence) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, e.f.tenantID, provider, ev, DispositionDeferredUnresolved)
		if err == nil && dup {
			t.Fatalf("setup: duplicate plant")
		}
		return err
	}); err != nil {
		t.Fatalf("plant: %v", err)
	}
}

type dmPayoutSnap struct {
	a, c       AttemptState
	wrA, wrC   string
	ledgerTx   int64
	ledgerEnts int64
	unresolved int64
}

func (e dmPayoutEnv) dmSnap(t *testing.T, wrA, wrC, aID, cID uuid.UUID) dmPayoutSnap {
	t.Helper()
	var s dmPayoutSnap
	s.a = mustGetAttempt(t, e.pool, e.f.tenantID, aID).State
	s.c = mustGetAttempt(t, e.pool, e.f.tenantID, cID).State
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id=$1`, wrA).Scan(&s.wrA); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id=$1`, wrC).Scan(&s.wrC); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, e.f.tenantID).Scan(&s.ledgerTx); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id=$1`, e.f.tenantID).Scan(&s.ledgerEnts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND resolved_at IS NULL`, e.f.tenantID).Scan(&s.unresolved)
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e dmPayoutEnv) dmReceipt(t *testing.T, ref string) (resolved bool, resolution string, attemptID *uuid.UUID) {
	t.Helper()
	var res *string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolved_at IS NOT NULL, resolution, attempt_id FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2`,
			e.f.tenantID, ref).Scan(&resolved, &res, &attemptID)
	}); err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if res != nil {
		resolution = *res
	}
	return
}

func TestDrainMerchantRef_Payout_Matrix(t *testing.T) {
	cases := []struct {
		name      string
		otherProv string // provider the "other" payout is claimed under
		merchant  func(a, c PaymentAttempt) string
		want      ReceiptResolution // "" means applied
	}{
		{"matching", "mock-dm-pay", func(a, c PaymentAttempt) string { return a.MerchantReference }, ""},
		{"absent", "mock-dm-pay", func(a, c PaymentAttempt) string { return "" }, ""},
		{"foreign_same_provider", "mock-dm-pay", func(a, c PaymentAttempt) string { return c.MerchantReference }, ResolutionAnomalyReferenceConflict},
		{"foreign_other_provider", "mock-dm-pay-other", func(a, c PaymentAttempt) string { return c.MerchantReference }, ResolutionAnomalyCrossProvider},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := dmPayoutSetup(t, "mock-dm-pay")
			orchC := e.orch
			if tc.otherProv != e.prov {
				// The other provider outranks the first (priority 1) and is the only adapter of its own
				// orchestrator, so the second payout is routed to it.
				op := NewMockProvider(tc.otherProv, "EUR")
				registerCapability(t, e.pool, e.f.orchFixture, op, 1)
				orchC = NewOrchestrator(map[string]PaymentProvider{tc.otherProv: op}, MultiWebhookCredentialResolver{tc.otherProv: NewMockWebhookCredentials(op)})
			}
			ref := "dm-pay-" + tc.name + "-" + uuid.NewString()
			wrA, a := e.dmNewPayout(t, e.orch, "dm-pay-a", e.prov, ref)
			wrC, c := e.dmNewPayout(t, orchC, "dm-pay-c", tc.otherProv, "")
			ev := ReceiptEvidence{EventType: "payout", ProviderReference: ref, MerchantReference: tc.merchant(a, c),
				Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}
			e.dmPlant(t, e.prov, ev)
			before := e.dmSnap(t, wrA, wrC, a.ID, c.ID)
			got := e.dmDrain(t, wrA, a.ID)
			after := e.dmSnap(t, wrA, wrC, a.ID, c.ID)
			resolved, resolution, attemptID := e.dmReceipt(t, ref)
			if !resolved {
				t.Fatalf("receipt left unresolved")
			}
			if tc.want == "" {
				if got != 1 || resolution != string(ResolutionApplied) || attemptID == nil || *attemptID != a.ID {
					t.Fatalf("applied=%d resolution=%q attempt=%v", got, resolution, attemptID)
				}
				if after.a != AttemptSucceeded || after.c != before.c || after.wrC != before.wrC {
					t.Fatalf("snapshot after apply: %+v (before %+v)", after, before)
				}
				return
			}
			if got != 0 || resolution != string(tc.want) || attemptID != nil {
				t.Fatalf("applied=%d resolution=%q attempt=%v, want closed as %s with NULL attempt", got, resolution, attemptID, tc.want)
			}
			before.unresolved = 0
			if after != before {
				t.Fatalf("state/ledger changed: before=%+v after=%+v", before, after)
			}
			if got := e.dmDrain(t, wrA, a.ID); got != 0 {
				t.Fatalf("replay applied = %d", got)
			}
			if again := e.dmSnap(t, wrA, wrC, a.ID, c.ID); again != after {
				t.Fatalf("replay changed state: %+v", again)
			}
		})
	}
}

// An UNROUTED attempt (provider_id NULL, 'created', never sent) named by the merchant reference is treated as
// the live path treats it (ResolveAttemptForEvidence: "a different (or unrouted) provider"): cross_provider.
func TestDrainMerchantRef_Deposit_UnroutedForeignAttempt_ClosedAsCrossProvider(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := newDepRefEnv(t, pool, "mock-dm-unrouted")
	e.p.setScript(scriptOutcome(OutcomeDeclined, ""))
	res1 := rvInit(t, pool, e.orch, e.f, 5000, "dm-un-1") // terminal decline, so a second attempt may be inserted
	var created PaymentAttempt
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: e.f.tenantID, Operation: AttemptOperationDeposit, DepositIntentID: &res1.Intent.ID,
			AttemptNo: 2, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		return err
	}); err != nil {
		t.Fatalf("setup: insert created attempt: %v", err)
	}
	if created.ProviderID != nil {
		t.Fatalf("setup: the created attempt must be unrouted")
	}
	ref := "dm-unrouted-" + uuid.NewString()
	e.p.setScript(scriptOutcome(OutcomePending, ref))
	res2 := rvInit(t, pool, e.orch, e.f, 5000, "dm-un-2")
	if res2.Attempt.ProviderReference == nil || *res2.Attempt.ProviderReference != ref {
		t.Fatalf("setup: second attempt must hold the scripted reference, got %v", res2.Attempt.ProviderReference)
	}
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, e.f.tenantID, e.id, ReceiptEvidence{EventType: "deposit", ProviderReference: ref,
			MerchantReference: created.MerchantReference, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}, DispositionDeferredUnresolved)
		if err == nil && dup {
			t.Fatalf("setup: duplicate plant")
		}
		return err
	}); err != nil {
		t.Fatalf("plant: %v", err)
	}
	var applied int
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res2.Intent.ID); err != nil {
			return err
		}
		a, err := GetAttemptByID(ctx, tx, res2.Attempt.ID)
		if err != nil {
			return err
		}
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, e.orch, a)
		return err
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0", applied)
	}
	var resolution string
	var attemptID *uuid.UUID
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolution, attempt_id FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2`, e.f.tenantID, ref).Scan(&resolution, &attemptID)
	}); err != nil {
		t.Fatal(err)
	}
	if resolution != string(ResolutionAnomalyCrossProvider) || attemptID != nil {
		t.Fatalf("resolution=%q attempt=%v, want cross-provider with NULL attempt", resolution, attemptID)
	}
	if st := mustGetAttempt(t, pool, e.f.tenantID, res2.Attempt.ID).State; st != res2.Attempt.State {
		t.Fatalf("bound attempt moved: %s -> %s", res2.Attempt.State, st)
	}
	if st := mustGetAttempt(t, pool, e.f.tenantID, created.ID).State; st != AttemptCreated {
		t.Fatalf("unrouted attempt moved: %s", st)
	}
}
