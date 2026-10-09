//go:build integration

package payments

// B13-B (ADR 0111 section 16; ADR 0095 section 44 decisions 1-8): the payments side of the payout
// destination binding, driven through the real T1p / phase B / phase C / T2 / T12 / poll / callback paths
// against MOCK providers (every provider here is MOCK or a test double; nothing is a statement about a real
// PSP). Each test has its own scratch database. Money invariants are asserted after every path that could
// touch a hold: SUM(D)=SUM(C), projection = rebuild, and "nothing posts" on every refusal and park.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// b13bProvider is a MOCK PaymentProvider with scripted Withdraw / QueryStatus results, a recorded request
// log (so a test can prove NO provider call happened, and what Destination reached the adapter), and the
// manifest knobs the destination cells depend on. It embeds *MockProvider, so it is Synthetic.
type b13bProvider struct {
	*MockProvider
	mu       sync.Mutex
	wres     WithdrawResult
	sres     StatusResult
	reqs     []WithdrawRequest
	declared bool // manifest EchoesDestinationFingerprint
	idem     bool // manifest IdempotentSubmission
}

func (p *b13bProvider) Withdraw(_ context.Context, r WithdrawRequest) (WithdrawResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, r)
	return p.wres, nil
}

func (p *b13bProvider) QueryStatus(_ context.Context, ref string) (StatusResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sres
	if s.ProviderReference == "" {
		s.ProviderReference = ref
	}
	return s, nil
}

func (p *b13bProvider) Capabilities() AdapterCapability {
	c := p.MockProvider.Capabilities()
	c.Manifest.EchoesDestinationFingerprint = p.declared
	c.Manifest.IdempotentSubmission = p.idem
	return c
}

func (p *b13bProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *b13bProvider) set(w WithdrawResult, s StatusResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wres, p.sres = w, s
}

type b13bW struct {
	t    *testing.T
	pool *db.Pool
	f    payoutFixture
	prov *b13bProvider
	orch *Orchestrator
	svc  *payoutinstrument.Service
	pid  string
}

func newB13bW(t *testing.T, id string, mut ...func(*b13bProvider)) *b13bW {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	pid := "mock-b13b-" + id
	p := &b13bProvider{MockProvider: NewMockProvider(pid, "EUR")}
	for _, m := range mut {
		m(p)
	}
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{pid: p}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(p.MockProvider)}).
		WithPayoutDestinations(pitest.Shared())
	return &b13bW{t: t, pool: pool, f: f, prov: p, orch: orch, svc: pitest.Shared(), pid: pid}
}

func (w *b13bW) ctx() context.Context { return context.Background() }

func (w *b13bW) approved(amount int64, key string) withdrawal.WithdrawalRequest {
	return approvedWithdrawal(w.t, w.pool, w.f, amount, key)
}

func (w *b13bW) claim(wr withdrawal.WithdrawalRequest, method string) (ClaimResult, error) {
	return w.orch.ClaimForDispatch(w.ctx(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, method, testSubmitActor())
}

func (w *b13bW) mustClaim(wr withdrawal.WithdrawalRequest) ClaimResult {
	w.t.Helper()
	c, err := w.claim(wr, "bank_transfer")
	if err != nil {
		w.t.Fatalf("ClaimForDispatch: %v", err)
	}
	return c
}

func (w *b13bW) dispatch(c ClaimResult) GateResult[WithdrawResult] {
	adapter, ok := w.orch.Provider(c.Capability.ProviderID)
	if !ok {
		w.t.Fatal("provider not registered")
	}
	return DispatchWithdraw(w.ctx(), w.pool, MockCredentialResolver{}, adapter, c.Attempt, w.orch.PayoutOptions()...)
}

func (w *b13bW) apply(wr withdrawal.WithdrawalRequest, c ClaimResult, gr GateResult[WithdrawResult]) error {
	return ApplyPayoutResult(w.ctx(), w.pool, w.f.tenantID, wr.ID, c.Attempt, gr, EvidenceSync, w.orch.PayoutOptions()...)
}

func (w *b13bW) attempt(id uuid.UUID) PaymentAttempt {
	w.t.Helper()
	var a PaymentAttempt
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = GetAttemptByID(ctx, tx, id)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *b13bW) wr(id uuid.UUID) withdrawal.WithdrawalRequest {
	w.t.Helper()
	var r withdrawal.WithdrawalRequest
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = withdrawal.GetByID(ctx, tx, id)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *b13bW) snapshot(attemptID uuid.UUID) payoutinstrument.Snapshot {
	w.t.Helper()
	var s payoutinstrument.Snapshot
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = w.svc.LoadSnapshot(ctx, tx, w.f.tenantID, attemptID)
		return err
	}); err != nil {
		w.t.Fatalf("load snapshot: %v", err)
	}
	return s
}

func (w *b13bW) goodEcho(attemptID uuid.UUID) *payoutinstrument.DestinationEcho {
	s := w.snapshot(attemptID)
	return &payoutinstrument.DestinationEcho{Fingerprint: s.Fingerprint, Kid: s.FingerprintKID}
}

func (w *b13bW) badEcho(attemptID uuid.UUID) *payoutinstrument.DestinationEcho {
	s := w.snapshot(attemptID)
	return &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ab", 32), Kid: s.FingerprintKID}
}

func (w *b13bW) count(q string, args ...any) int {
	w.t.Helper()
	return fpCount(w.t, w.pool, w.f.tenantID, q, args...)
}

func (w *b13bW) payoutDecisions() int {
	return w.count(`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND operation=$2`, w.f.tenantID, string(kyc.EnforcementWithdrawalPayout))
}

func (w *b13bW) ledgerTx() int { return fpLedgerTx(w.t, w.pool, w.f) }

func (w *b13bW) balanced() {
	w.t.Helper()
	loAssertBalanced(w.t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(w.t, w.pool, w.f.tenantID)
}

// tamper simulates arbitrary SQL by the table owner (the attack the Go-side seals exist for).
func (w *b13bW) tamper(table, stmt string, args ...any) {
	w.t.Helper()
	err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{`ALTER TABLE ` + table + ` DISABLE TRIGGER USER`, `ALTER TABLE ` + table + ` NO FORCE ROW LEVEL SECURITY`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, stmt, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() < 1 {
			return fmt.Errorf("tamper statement affected no row")
		}
		for _, q := range []string{`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`, `ALTER TABLE ` + table + ` ENABLE TRIGGER USER`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.t.Fatalf("tamper %s: %v", table, err)
	}
}

func (w *b13bW) suspend(instrumentID uuid.UUID) {
	w.t.Helper()
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: w.f.tenantID, InstrumentID: instrumentID,
			Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
		return err
	}); err != nil {
		w.t.Fatalf("suspend: %v", err)
	}
}

func (w *b13bW) providerRevoke(instrumentID uuid.UUID) {
	w.t.Helper()
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.ApplyProviderBlock(ctx, tx, w.f.tenantID, instrumentID, payoutinstrument.MockVerifierID, payoutinstrument.EventRevoke, "provider_revoked")
		return err
	}); err != nil {
		w.t.Fatalf("provider revoke: %v", err)
	}
}

func (w *b13bW) alertFor(attemptID uuid.UUID, reason string) (alertinject.Row, bool) {
	w.t.Helper()
	want := "payout_attempt:" + attemptID.String() + ":reason:" + reason
	for _, r := range alertinject.ForSubject(w.t, w.pool, w.f.tenantID) {
		if r.Discriminator == want {
			return r, true
		}
	}
	return alertinject.Row{}, false
}

func (w *b13bW) sweeper() *Sweeper {
	return &Sweeper{Pool: w.pool, Orchestrator: w.orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
}

func (w *b13bW) touch(id uuid.UUID) {
	w.t.Helper()
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return Touch(ctx, tx, id) }); err != nil {
		w.t.Fatalf("touch: %v", err)
	}
}

func (w *b13bW) wantNotSent(gr GateResult[WithdrawResult], what string) {
	w.t.Helper()
	if gr.Class != ErrorClassNotSent || gr.Err == nil {
		w.t.Fatalf("%s: class=%s err=%v, want NotSent with an error", what, gr.Class, gr.Err)
	}
	if w.prov.calls() != 0 {
		w.t.Fatalf("%s: the provider was called %d time(s)", what, w.prov.calls())
	}
}

// =============================================================================================
// T1p
// =============================================================================================

// Binding resolved server-side: the rail is the instrument's, the snapshot is written in the claim
// transaction and equals the withdrawal, the attempt and the instrument.
func TestB13B_T1p_WritesTheSnapshot_RailFromTheInstrument(t *testing.T) {
	w := newB13bW(t, "a")
	wr := w.approved(500, "b13b-t1p")
	if !wr.Bound() {
		t.Fatal("the fixture withdrawal must be bound")
	}
	c, err := w.claim(wr, "") // the staff body names NO payment method
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if c.Attempt.PaymentMethod != "bank_transfer" {
		t.Fatalf("the attempt's method must be the instrument's rail, got %q", c.Attempt.PaymentMethod)
	}
	s := w.snapshot(c.Attempt.ID)
	if s.WithdrawalRequestID != wr.ID || s.InstrumentID != *wr.PayoutInstrumentID || s.Fingerprint != *wr.PayoutInstrumentFingerprint ||
		s.Amount != "500" || s.AssetCode != "EUR" || s.Rail != "bank_transfer" || s.Kind != payoutinstrument.KindSyntheticTest ||
		s.VerificationSource != payoutinstrument.SourceSynthetic {
		t.Fatalf("snapshot does not equal the binding: %+v", s)
	}
	if w.prov.calls() != 0 {
		t.Fatal("T1p must not call the provider")
	}
	w.balanced()
}

// The staff submit body can never influence the destination or the route (A-10).
func TestB13B_T1p_StaffBodyCannotInfluenceTheDestination(t *testing.T) {
	w := newB13bW(t, "b")
	wr := w.approved(500, "b13b-body")
	before := w.payoutDecisions()
	_, err := w.claim(wr, "card")
	if !errors.Is(err, ErrPaymentMethodMismatch) {
		t.Fatalf("a body naming another method: %v, want ErrPaymentMethodMismatch", err)
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("state = %s, want approved", got.State)
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
	if w.payoutDecisions() != before {
		t.Fatal("a mismatching body must write no decision row")
	}
	// the matching value and the empty value both work (the latter proven above)
	if _, err := w.claim(wr, "bank_transfer"); err != nil {
		t.Fatalf("a body equal to the rail: %v", err)
	}
}

// The destination gate refuses at T1p: only the denial audit commits (no allow decision row, no state
// change, no attempt, no snapshot, no provider call, no ledger movement) and the request stays approved.
func TestB13B_T1p_DestinationGateRefusals(t *testing.T) {
	type tc struct {
		name      string
		arm       func(w *b13bW, wr withdrawal.WithdrawalRequest)
		integrity bool
		gate      string
	}
	cases := []tc{
		{"suspended by compliance", func(w *b13bW, wr withdrawal.WithdrawalRequest) { w.suspend(*wr.PayoutInstrumentID) }, false, payoutinstrument.ReasonNotVerified},
		{"revoked by the provider", func(w *b13bW, wr withdrawal.WithdrawalRequest) { w.providerRevoke(*wr.PayoutInstrumentID) }, false, payoutinstrument.ReasonNotVerified},
		{"instrument row tampered (seal)", func(w *b13bW, wr withdrawal.WithdrawalRequest) {
			w.tamper("payout_instruments", `UPDATE payout_instruments SET display_mask = 'XX****0000' WHERE id = $1`, *wr.PayoutInstrumentID)
		}, true, payoutinstrument.ReasonSealInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newB13bW(t, "c")
			wr := w.approved(500, "b13b-refuse")
			c.arm(w, wr)
			decisionsBefore, ledgerBefore := w.payoutDecisions(), w.ledgerTx()
			cl, err := w.claim(wr, "bank_transfer")
			if !errors.Is(err, ErrPayoutDestinationNotUsable) {
				t.Fatalf("err = %v, want ErrPayoutDestinationNotUsable", err)
			}
			g, ok := payoutinstrument.IsGateRefusal(err)
			if !ok || g.Reason != c.gate || g.Integrity() != c.integrity {
				t.Fatalf("gate refusal = %+v, want %q integrity=%v", g, c.gate, c.integrity)
			}
			if cl.Attempt.ID != uuid.Nil {
				t.Fatal("no attempt may be returned")
			}
			if got := w.wr(wr.ID); got.State != withdrawal.StateApproved || got.ProviderID != nil {
				t.Fatalf("state=%s provider=%v: the request must stay approved", got.State, got.ProviderID)
			}
			if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
				t.Fatalf("attempts = %d", n)
			}
			if n := w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots`); n != 0 {
				t.Fatalf("snapshots = %d", n)
			}
			if w.payoutDecisions() != decisionsBefore {
				t.Fatal("no ALLOW decision row may commit for a refused claim")
			}
			want := alertReasonPayoutDestinationNotUsable
			if c.integrity {
				want = TerminalReasonDestinationIntegrityFailure
			}
			if n := w.count(`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='withdrawal.submit.http' AND outcome='denied'
				AND metadata->>'denied_by_destination' = $2 AND metadata->>'gate_reason' = $3 AND target_id = $4`,
				w.f.tenantID, want, c.gate, wr.ID.String()); n != 1 {
				t.Fatalf("denial audit rows = %d, want exactly 1", n)
			}
			if w.prov.calls() != 0 || w.ledgerTx() != ledgerBefore {
				t.Fatal("no provider call and no ledger posting on a refusal")
			}
			w.balanced()
			// Retry-stable: the same refusal again, still exactly the same shape.
			if _, err := w.claim(wr, "bank_transfer"); !errors.Is(err, ErrPayoutDestinationNotUsable) {
				t.Fatalf("retry: %v", err)
			}
			if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
				t.Fatal("a retried refusal created an attempt")
			}
		})
	}
}

// A legacy (NULL-binding) withdrawal: a Synthetic adapter still dispatches (the MOCK path, no snapshot
// needed); a non-Synthetic adapter is refused by the tiering predicate with Bound=false (A-11).
func TestB13B_T1p_LegacyUnbound_SyntheticOnly(t *testing.T) {
	w := newB13bW(t, "d")
	legacy := w.legacyApproved(500, "b13b-legacy")
	c, err := w.claim(legacy, "bank_transfer")
	if err != nil {
		t.Fatalf("legacy + Synthetic adapter must dispatch: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, c.Attempt.ID); n != 0 {
		t.Fatalf("a legacy withdrawal needs (and gets) no snapshot, got %d", n)
	}
	// Same legacy shape against a non-Synthetic adapter value.
	l2 := w.legacyApproved(500, "b13b-legacy-2")
	real := &Orchestrator{providers: map[string]PaymentProvider{w.pid: unmarkedPaymentsAdapter{w.prov}}, breaker: NewBreaker(), destinations: w.svc}
	if _, err := real.ClaimForDispatch(w.ctx(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, l2.ID, "bank_transfer", testSubmitActor()); !errors.Is(err, ErrPayoutDestinationNotUsable) {
		t.Fatalf("legacy + non-Synthetic adapter: %v, want ErrPayoutDestinationNotUsable", err)
	} else if g, _ := payoutinstrument.IsGateRefusal(err); g == nil || g.Reason != payoutinstrument.ReasonTierRefused {
		t.Fatalf("refusal = %+v, want tier_refused", g)
	}
	if got := w.wr(l2.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("state = %s", got.State)
	}
}

// legacyApproved plants a pre-B13-B shaped (NULL/NULL) approved withdrawal.
func (w *b13bW) legacyApproved(amount int64, key string) withdrawal.WithdrawalRequest {
	w.t.Helper()
	id := uuid.New()
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
				VALUES ($1,$2,$3,$4,$5,'EUR',$6,'approved',$7)`, id, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, amount, key)
			return err
		})
	}); err != nil {
		w.t.Fatalf("plant legacy withdrawal: %v", err)
	}
	return w.wr(id)
}

// The tiering predicate at T1p: a non-Synthetic adapter needs a non-synthetic verification. A MOCK
// (synthetic-source) instrument never satisfies it; a verification by a registered non-Synthetic verifier does
// (sandbox / real behaviour follows the strongest model, decision 8).
type b13bSandboxVerifier struct{}

func (b13bSandboxVerifier) ID() string { return "b13b-sandbox-verifier" }
func (b13bSandboxVerifier) DeclaredSource() payoutinstrument.VerificationSource {
	return payoutinstrument.SourcePSPAccount
}
func (b13bSandboxVerifier) Supports(kind, _ string) bool {
	return kind == payoutinstrument.KindBankAccount
}
func (b13bSandboxVerifier) Verify(context.Context, payoutinstrument.VerifyRequest) (payoutinstrument.VerifyResult, error) {
	return payoutinstrument.VerifyResult{Verified: true, AccountHolderMatchesVerifiedIdentity: true, SourceExpiry: time.Now().Add(48 * time.Hour)}, nil
}

func TestB13B_T1p_NonSyntheticAdapter_TieringMatrix(t *testing.T) {
	w := newB13bW(t, "e")
	real := func(svc *payoutinstrument.Service) *Orchestrator {
		return &Orchestrator{providers: map[string]PaymentProvider{w.pid: unmarkedPaymentsAdapter{w.prov}}, breaker: NewBreaker(), destinations: svc}
	}
	// (1) bound to a MOCK (synthetic-source) instrument: refused for a non-Synthetic adapter - the verifier that
	// produced it is Synthetic, so it is not "a registered non-Synthetic verifier".
	wr := w.approved(500, "b13b-tier-mock")
	_, err := real(w.svc).ClaimForDispatch(w.ctx(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if g, _ := payoutinstrument.IsGateRefusal(err); !errors.Is(err, ErrPayoutDestinationNotUsable) || g == nil || g.Reason != payoutinstrument.ReasonVerifierNotRegd {
		t.Fatalf("non-Synthetic adapter + MOCK-source instrument: %v (%+v), want verifier_not_registered", err, g)
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("state = %s", got.State)
	}

	// (2) the same adapter with an instrument verified by a registered non-Synthetic verifier: allowed, and phase B
	// delivers the destination.
	keys, err := payoutinstrument.NewKeys("m1", map[string][]byte{"m1": bytes32(1)}, "f1", map[string][]byte{"f1": bytes32(2)})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := payoutinstrument.NewService(keys, payoutinstrument.DefaultKinds(), b13bSandboxVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	w.licenceWithMaxAge(48 * time.Hour)
	var inst payoutinstrument.Instrument
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := sandbox.Register(ctx, tx, payoutinstrument.RegisterParams{TenantID: w.f.tenantID, PlayerAccountID: w.f.playerAccountID,
			Kind: payoutinstrument.KindBankAccount, Rail: "bank_transfer", AssetCodes: []string{"EUR"}, Detail: json.RawMessage(`{"iban":"GB82WEST12345698765432"}`)})
		inst = r.Instrument
		return err
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := sandbox.Verify(w.ctx(), w.pool, w.f.tenantID, inst.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	mustSetWithdrawalPolicy(t, w.pool, w.f.tenantID, "EUR", 1_000_000, 2)
	var wr2 withdrawal.WithdrawalRequest
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr2, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{TenantID: w.f.tenantID, BrandID: w.f.brandID,
			PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID, WalletID: w.f.walletID, AssetCode: "EUR", Amount: 700,
			IdempotencyKey: "b13b-tier-real", PayoutInstrumentID: inst.ID, Destinations: sandbox})
		if err != nil {
			return err
		}
		if err := withdrawal.MoveToPendingReview(ctx, tx, wr2.ID); err != nil {
			return err
		}
		_, err = withdrawal.Approve(ctx, tx, wr2.ID, uuid.New(), true, nil, nil)
		return err
	}); err != nil {
		t.Fatalf("request/approve with the sandbox instrument: %v", err)
	}
	orch := real(sandbox)
	c, err := orch.ClaimForDispatch(w.ctx(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr2.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("non-Synthetic adapter + sandbox-verified instrument must be allowed: %v", err)
	}
	w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: "sandbox-ref-1"}, StatusResult{})
	if gr := DispatchWithdraw(w.ctx(), w.pool, MockCredentialResolver{}, unmarkedPaymentsAdapter{w.prov}, c.Attempt, orch.PayoutOptions()...); gr.Class != ErrorClassPending {
		t.Fatalf("phase B for the non-Synthetic adapter: class=%s err=%v", gr.Class, gr.Err)
	}
	if w.prov.calls() != 1 {
		t.Fatalf("calls = %d", w.prov.calls())
	}
	got := w.prov.reqs[0]
	if got.Destination.Empty() || got.Destination.Kind != payoutinstrument.KindBankAccount || !strings.Contains(string(got.Destination.Detail), "GB82WEST12345698765432") {
		t.Fatalf("the non-Synthetic adapter must receive the decrypted destination, got %v", got.Destination)
	}
	// (3) The gate refuses the same sandbox-verified instrument once its verifier is no longer a REGISTERED
	// non-Synthetic verifier (a service that does not know it), and refuses it for a Synthetic-only service.
	bare, err := payoutinstrument.NewService(keys, payoutinstrument.DefaultKinds())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := bare.EvaluateGate(ctx, tx, payoutinstrument.GateParams{TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID,
			PersonID: w.f.personID, InstrumentID: inst.ID, AssetCode: "EUR", Adapter: unmarkedPaymentsAdapter{w.prov}})
		return err
	}); err == nil {
		t.Fatal("an unregistered verifier must not satisfy a non-Synthetic adapter")
	} else if g, ok := payoutinstrument.IsGateRefusal(err); !ok || g.Reason != payoutinstrument.ReasonVerifierNotRegd {
		t.Fatalf("refusal = %v, want verifier_not_registered", err)
	}
}

func bytes32(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// licenceWithMaxAge licenses the fixture tenant under a fresh jurisdiction with a verification max age (the
// owner role writes it; the runtime role cannot) and approves the player's KYC, the preconditions of a
// non-synthetic verification.
func (w *b13bW) licenceWithMaxAge(d time.Duration) {
	w.t.Helper()
	jid, lid := uuid.New(), uuid.New()
	if err := w.pool.WithPlatformAdmin(w.ctx(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'B13-B test jurisdiction')`, jid, "B13B-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1,$2,'platform',$3)`, lid, jid, "L-"+lid.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, w.f.tenantID)
		return err
	}); err != nil {
		w.t.Fatalf("licence tenant: %v", err)
	}
	if err := w.pool.WithoutTenant(w.ctx(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verification_max_age (jurisdiction_id, max_age) VALUES ($1, make_interval(secs => $2))`, jid, d.Seconds())
		return err
	}); err != nil {
		w.t.Fatalf("max age: %v", err)
	}
}

// =============================================================================================
// phase B
// =============================================================================================

// The destination reaches the adapter, built from the snapshot plus the decrypted immutable detail, and every
// rendering of it is redacted.
func TestB13B_PhaseB_DestinationReachesTheAdapter_Redacted(t *testing.T) {
	w := newB13bW(t, "f")
	wr := w.approved(500, "b13b-pb")
	c := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-pb-1"}, StatusResult{})
	gr := w.dispatch(c)
	if gr.Class != ErrorClassPending {
		t.Fatalf("class = %s err = %v", gr.Class, gr.Err)
	}
	if w.prov.calls() != 1 {
		t.Fatalf("calls = %d", w.prov.calls())
	}
	req := w.prov.reqs[0]
	if req.PaymentMethod != "bank_transfer" || req.Destination.Empty() || req.Destination.InstrumentID != *wr.PayoutInstrumentID ||
		req.Destination.Kind != payoutinstrument.KindSyntheticTest || req.Destination.FingerprintKid != w.snapshot(c.Attempt.ID).FingerprintKID {
		t.Fatalf("request does not carry the snapshot's destination: %+v", req)
	}
	if !strings.Contains(string(req.Destination.Detail), `"label"`) {
		t.Fatalf("the adapter must receive the decrypted detail, got %q", req.Destination.Detail)
	}
	for _, rendered := range []string{fmt.Sprint(req.Destination), fmt.Sprintf("%+v", req.Destination), fmt.Sprintf("%#v", req.Destination), fmt.Sprintf("%v", req)} {
		if strings.Contains(rendered, "label") && strings.Contains(rendered, `"t`) && strings.Contains(rendered, "{\"") {
			t.Fatalf("the destination Detail leaked into a rendering: %s", rendered)
		}
	}
	b, _ := json.Marshal(req.Destination)
	if strings.Contains(string(b), "label") && !strings.Contains(string(b), "REDACTED") {
		t.Fatalf("JSON rendering leaked: %s", b)
	}
	if err := w.apply(wr, c, gr); err != nil {
		t.Fatal(err)
	}
	w.balanced()
}

// Phase B refuses with NO provider call when the destination cannot be proven at the moment of the call.
func TestB13B_PhaseB_Refusals_MakeNoProviderCall(t *testing.T) {
	type tc struct {
		name string
		arm  func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (adapter PaymentProvider, opts []PayoutOption)
	}
	std := func(w *b13bW) (PaymentProvider, []PayoutOption) {
		a, _ := w.orch.Provider(w.pid)
		return a, w.orch.PayoutOptions()
	}
	cases := []tc{
		{"instrument suspended after T1p", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			w.suspend(*wr.PayoutInstrumentID)
			return std(w)
		}},
		{"instrument revoked by the provider after T1p", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			w.providerRevoke(*wr.PayoutInstrumentID)
			return std(w)
		}},
		{"adapter swapped for a non-Synthetic value between T1p and phase B", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			_, o := std(w)
			return unmarkedPaymentsAdapter{w.prov}, o
		}},
		{"no destination service supplied", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			a, _ := std(w)
			return a, nil
		}},
		{"snapshot deleted (missing on a bound withdrawal)", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			w.tamper("payout_attempt_destination_snapshots", `DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, c.Attempt.ID)
			return std(w)
		}},
		{"snapshot tampered (seal)", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			w.tamper("payout_attempt_destination_snapshots", `UPDATE payout_attempt_destination_snapshots SET display_mask = 'ZZ****9999' WHERE attempt_id = $1`, c.Attempt.ID)
			return std(w)
		}},
		{"instrument tampered (seal)", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) (PaymentProvider, []PayoutOption) {
			w.tamper("payout_instruments", `UPDATE payout_instruments SET display_mask = 'XX****0000' WHERE id = $1`, *wr.PayoutInstrumentID)
			return std(w)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newB13bW(t, "g")
			wr := w.approved(500, "b13b-pbref")
			cl := w.mustClaim(wr)
			adapter, opts := c.arm(w, wr, cl)
			gr := DispatchWithdraw(w.ctx(), w.pool, MockCredentialResolver{}, adapter, cl.Attempt, opts...)
			w.wantNotSent(gr, c.name)
			// phase C for a NotSent result returns the never-sent attempt to `created`: nothing else changes.
			if err := w.apply(wr, cl, gr); err != nil {
				t.Fatalf("apply NotSent: %v", err)
			}
			a := w.attempt(cl.Attempt.ID)
			if a.State != AttemptCreated || a.EverPossiblySent {
				t.Fatalf("attempt = %s everPossiblySent=%v, want created/false", a.State, a.EverPossiblySent)
			}
			if got := w.wr(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
				t.Fatalf("withdrawal = %s (release=%v): the hold must stay", got.State, got.ReleaseLedgerTransactionID)
			}
			w.balanced()
		})
	}
}

// The snapshot must equal what is ABOUT TO BE SENT: an attempt whose amount (or asset) no longer equals its sealed
// snapshot is refused at phase B even though the snapshot itself still verifies.
func TestB13B_PhaseB_SnapshotMustEqualWhatIsSent(t *testing.T) {
	for _, tc := range []struct{ name, set string }{
		{"amount differs", `UPDATE payment_attempts SET amount = 501 WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newB13bW(t, "g2")
			wr := w.approved(500, "b13b-pb-eq")
			cl := w.mustClaim(wr)
			w.tamper("payment_attempts", tc.set, cl.Attempt.ID)
			reloaded := w.attempt(cl.Attempt.ID)
			adapter, _ := w.orch.Provider(w.pid)
			gr := DispatchWithdraw(w.ctx(), w.pool, MockCredentialResolver{}, adapter, reloaded, w.orch.PayoutOptions()...)
			w.wantNotSent(gr, tc.name)
			if g, ok := payoutinstrument.IsGateRefusal(gr.Err); !ok || g.Reason != payoutinstrument.ReasonSnapshotMismatch || !g.Integrity() {
				t.Fatalf("refusal = %+v, want an integrity snapshot_mismatch (err %v)", g, gr.Err)
			}
		})
	}
}

// =============================================================================================
// T2 / T12
// =============================================================================================

// T2: the re-claim of a `created` attempt re-checks the destination. A blocked instrument escalates (T16) with a
// closed reason, an audit row and a B12 P1; nothing is resent or released; a repeat only reschedules.
func TestB13B_T2_Reclaim_EscalatesOnBlockedDestination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		arm    func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult)
		reason string
	}{
		{"suspended", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) { w.suspend(*wr.PayoutInstrumentID) }, alertReasonPayoutDestinationNotUsable},
		{"snapshot missing", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) {
			w.tamper("payout_attempt_destination_snapshots", `DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, c.Attempt.ID)
		}, TerminalReasonDestinationIntegrityFailure},
		{"snapshot sealed wrongly", func(w *b13bW, wr withdrawal.WithdrawalRequest, c ClaimResult) {
			w.tamper("payout_attempt_destination_snapshots", `UPDATE payout_attempt_destination_snapshots SET asset_code = 'EUR', display_mask = 'ZZ****9999' WHERE attempt_id = $1`, c.Attempt.ID)
		}, TerminalReasonDestinationIntegrityFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newB13bW(t, "h")
			wr := w.approved(500, "b13b-t2")
			cl := w.mustClaim(wr)
			// The call "never reached the provider": the attempt returns to `created` (T5) so T2 owns it.
			if err := w.apply(wr, cl, GateResult[WithdrawResult]{Class: ErrorClassNotSent, Err: errors.New("never sent")}); err != nil {
				t.Fatal(err)
			}
			tc.arm(w, wr, cl)
			ledgerBefore := w.ledgerTx()
			w.touch(cl.Attempt.ID)
			st := w.sweeper().RunOnce(w.ctx(), []uuid.UUID{w.f.tenantID})
			if len(st.Errors) != 0 {
				t.Fatalf("sweep errors: %v", st.Errors)
			}
			a := w.attempt(cl.Attempt.ID)
			if a.State != AttemptCreated || a.EscalatedAt == nil {
				t.Fatalf("attempt = %s escalated=%v, want created + escalated (T16)", a.State, a.EscalatedAt != nil)
			}
			if w.prov.calls() != 0 {
				t.Fatalf("a re-claim of a blocked destination must not call the provider (%d)", w.prov.calls())
			}
			if got := w.wr(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil || w.ledgerTx() != ledgerBefore {
				t.Fatal("no release, no posting on a destination escalation")
			}
			if n := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_destination_gate_denied' AND target_id=$1 AND metadata->>'reason'=$2`, cl.Attempt.ID.String(), tc.reason); n != 1 {
				t.Fatalf("escalation audit rows = %d, want 1", n)
			}
			al, ok := w.alertFor(cl.Attempt.ID, tc.reason)
			if !ok || al.Severity != "p1" || al.Occurrences != 1 {
				t.Fatalf("B12 P1 missing or wrong: %+v ok=%v", al, ok)
			}
			// Repeat tick: an already-escalated attempt only reschedules (no second audit row, no second occurrence).
			w.touch(cl.Attempt.ID)
			if st := w.sweeper().RunOnce(w.ctx(), []uuid.UUID{w.f.tenantID}); len(st.Errors) != 0 {
				t.Fatalf("second sweep errors: %v", st.Errors)
			}
			if n := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_destination_gate_denied' AND target_id=$1`, cl.Attempt.ID.String()); n != 1 {
				t.Fatalf("a repeat tick wrote another audit row (%d)", n)
			}
			if al2, _ := w.alertFor(cl.Attempt.ID, tc.reason); al2.Occurrences != 1 {
				t.Fatalf("a repeat tick raised again: %d occurrences", al2.Occurrences)
			}
			if w.prov.calls() != 0 {
				t.Fatal("provider called on the repeat tick")
			}
			w.balanced()
		})
	}
}

// T2 positive control: with an intact destination the same re-claim dispatches, carrying the destination.
func TestB13B_T2_Reclaim_IntactDestinationDispatches(t *testing.T) {
	w := newB13bW(t, "i")
	wr := w.approved(500, "b13b-t2ok")
	cl := w.mustClaim(wr)
	if err := w.apply(wr, cl, GateResult[WithdrawResult]{Class: ErrorClassNotSent, Err: errors.New("never sent")}); err != nil {
		t.Fatal(err)
	}
	w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-t2-1"}, StatusResult{})
	w.touch(cl.Attempt.ID)
	if st := w.sweeper().RunOnce(w.ctx(), []uuid.UUID{w.f.tenantID}); len(st.Errors) != 0 {
		t.Fatalf("sweep errors: %v", st.Errors)
	}
	if w.prov.calls() != 1 || w.prov.reqs[0].Destination.Empty() {
		t.Fatalf("T2 must dispatch once with the destination (calls=%d)", w.prov.calls())
	}
	if a := w.attempt(cl.Attempt.ID); a.State != AttemptPending {
		t.Fatalf("attempt = %s, want pending", a.State)
	}
	w.balanced()
}

// T12: a resend of an ambiguous attempt re-checks the destination; a blocked destination escalates and the
// provider is never called again (Withdraw stays at exactly the original call).
func TestB13B_T12_Resend_GatedByTheDestination(t *testing.T) {
	w := newB13bW(t, "j", func(p *b13bProvider) { p.idem = true })
	wr := w.approved(500, "b13b-t12")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeAmbiguous, ProviderReference: ""}, StatusResult{Outcome: OutcomeAmbiguous})
	gr := w.dispatch(cl)
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	if a := w.attempt(cl.Attempt.ID); a.State != AttemptAmbiguous {
		t.Fatalf("attempt = %s, want ambiguous", a.State)
	}
	if w.prov.calls() != 1 {
		t.Fatalf("calls = %d", w.prov.calls())
	}
	w.suspend(*wr.PayoutInstrumentID)
	w.touch(cl.Attempt.ID)
	if st := w.sweeper().RunOnce(w.ctx(), []uuid.UUID{w.f.tenantID}); len(st.Errors) != 0 {
		t.Fatalf("sweep errors: %v", st.Errors)
	}
	if w.prov.calls() != 1 {
		t.Fatalf("the resend must be refused: calls = %d, want 1", w.prov.calls())
	}
	a := w.attempt(cl.Attempt.ID)
	if a.State != AttemptAmbiguous || a.EscalatedAt == nil {
		t.Fatalf("attempt = %s escalated=%v", a.State, a.EscalatedAt != nil)
	}
	if _, ok := w.alertFor(cl.Attempt.ID, alertReasonPayoutDestinationNotUsable); !ok {
		t.Fatal("B12 P1 destination_not_usable missing")
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		t.Fatal("the hold must stay")
	}
	w.balanced()
}

// =============================================================================================
// evidence: sync (phase C), poll, callback
// =============================================================================================

func (w *b13bW) submittedWithResult(res WithdrawResult, key string) (withdrawal.WithdrawalRequest, ClaimResult, GateResult[WithdrawResult]) {
	w.t.Helper()
	wr := w.approved(500, key)
	cl := w.mustClaim(wr)
	w.prov.set(res, StatusResult{})
	return wr, cl, w.dispatch(cl)
}

func (w *b13bW) wantParked(wr withdrawal.WithdrawalRequest, a PaymentAttempt, reason string) {
	w.t.Helper()
	if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != reason {
		w.t.Fatalf("attempt = %s reason=%v, want disputed/%s", a.State, a.TerminalReason, reason)
	}
	got := w.wr(wr.ID)
	if got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		w.t.Fatalf("withdrawal = %s (release=%v): NO payout progression and the hold stays", got.State, got.ReleaseLedgerTransactionID)
	}
	if n := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_parked_destination' AND target_id=$1 AND metadata->>'reason'=$2`, a.ID.String(), reason); n != 1 {
		w.t.Fatalf("park audit rows = %d, want 1", n)
	}
	al, ok := w.alertFor(a.ID, reason)
	if !ok || al.Severity != "p1" || al.State != "open" {
		w.t.Fatalf("B12 P1 %s missing: %+v ok=%v", reason, al, ok)
	}
	if len(al.Attributes) != 1 || al.Attributes["provider_id"] != w.pid {
		w.t.Fatalf("alert attributes = %v, want only provider_id", al.Attributes)
	}
	w.balanced()
}

// Decision 5: a provider-reported destination that differs from the snapshot parks the attempt as `disputed`
// (destination_mismatch) with an audit row and a P1, whatever the outcome, with no payout progression.
func TestB13B_Echo_Sync_Mismatch_ParksEveryOutcome(t *testing.T) {
	for _, o := range []struct {
		name string
		res  WithdrawResult
	}{
		{"success", WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-echo-ok"}},
		{"pending", WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-echo-p"}},
		{"decline", WithdrawResult{Outcome: OutcomeDeclined, ProviderReference: "ref-echo-d", DeclineReason: "account_closed"}},
		{"ambiguous", WithdrawResult{Outcome: OutcomeAmbiguous, ProviderReference: "ref-echo-a"}},
	} {
		for _, kind := range []string{"different fingerprint", "unknown kid"} {
			t.Run(o.name+"/"+kind, func(t *testing.T) {
				w := newB13bW(t, "k")
				wr := w.approved(500, "b13b-echo")
				cl := w.mustClaim(wr)
				echo := w.badEcho(cl.Attempt.ID)
				if kind == "unknown kid" {
					echo = &payoutinstrument.DestinationEcho{Fingerprint: w.snapshot(cl.Attempt.ID).Fingerprint, Kid: "no-such-kid"}
				}
				res := o.res
				res.DestinationEcho = echo
				w.prov.set(res, StatusResult{})
				gr := w.dispatch(cl)
				ledgerBefore := w.ledgerTx()
				if err := w.apply(wr, cl, gr); err != nil {
					t.Fatalf("apply: %v", err)
				}
				w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
				if w.ledgerTx() != ledgerBefore {
					t.Fatal("a destination park must post nothing")
				}
				// Replay: the same evidence again is a no-op (one audit row, one alert occurrence).
				if err := w.apply(wr, cl, gr); err != nil {
					t.Fatalf("replay: %v", err)
				}
				w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
				if al, _ := w.alertFor(cl.Attempt.ID, TerminalReasonDestinationMismatch); al.Occurrences != 1 {
					t.Fatalf("replay raised again: occurrences = %d", al.Occurrences)
				}
			})
		}
	}
}

// Legacy (unbound) withdrawals have no destination to compare: an echo on their evidence is ignored and their
// evidence applies exactly as before B13-B (the legacy path is unaffected, ADR 0111 A-11).
func TestB13B_Echo_LegacyUnboundIsNotCompared(t *testing.T) {
	w := newB13bW(t, "lg")
	legacy := w.legacyApproved(500, "b13b-legacy-echo")
	cl, err := w.claim(legacy, "bank_transfer")
	if err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-legacy-1",
		DestinationEcho: &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("cd", 32), Kid: "any"}}, StatusResult{})
	gr := w.dispatch(cl)
	if gr.Class != ErrorClassPending {
		t.Fatalf("legacy dispatch: class=%s err=%v", gr.Class, gr.Err)
	}
	if err := w.apply(legacy, cl, gr); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if a := w.attempt(cl.Attempt.ID); a.State != AttemptPending {
		t.Fatalf("a legacy attempt must not be parked by an echo it has nothing to compare with, got %s", a.State)
	}
}

// A matching echo settles normally (the comparison is not a veto on every payout).
func TestB13B_Echo_Sync_Match_Settles(t *testing.T) {
	w := newB13bW(t, "l")
	wr := w.approved(500, "b13b-echo-match")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-match-1", DestinationEcho: w.goodEcho(cl.Attempt.ID)}, StatusResult{})
	if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
		t.Fatal(err)
	}
	if a := w.attempt(cl.Attempt.ID); a.State != AttemptSucceeded {
		t.Fatalf("attempt = %s, want succeeded", a.State)
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("withdrawal = %s, want completed", got.State)
	}
	w.balanced()
}

// A success WITHOUT an echo from an adapter whose manifest declares one is ambiguous, never a success (the poll
// decides); from an adapter that does not declare one it settles (legacy-compatible).
func TestB13B_Echo_AbsentOnSuccess_DependsOnTheManifest(t *testing.T) {
	declared := newB13bW(t, "m", func(p *b13bProvider) { p.declared = true })
	wr, cl, gr := declared.submittedWithResult(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-abs-1"}, "b13b-abs-1")
	if err := declared.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	if a := declared.attempt(cl.Attempt.ID); a.State != AttemptAmbiguous {
		t.Fatalf("declared + absent echo: attempt = %s, want ambiguous (never a success)", a.State)
	}
	if got := declared.wr(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		t.Fatal("no completion without the declared echo")
	}
	declared.balanced()

	plain := newB13bW(t, "n")
	wr2, cl2, gr2 := plain.submittedWithResult(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-abs-2"}, "b13b-abs-2")
	if err := plain.apply(wr2, cl2, gr2); err != nil {
		t.Fatal(err)
	}
	if a := plain.attempt(cl2.Attempt.ID); a.State != AttemptSucceeded {
		t.Fatalf("undeclared + absent echo: attempt = %s, want succeeded", a.State)
	}
	plain.balanced()
}

// Decision 4: a snapshot that is missing (or unsealed) on a bound withdrawal is a destination_integrity_failure
// when evidence tries to settle the attempt: parked, hold kept.
func TestB13B_Evidence_MissingOrTamperedSnapshot_ParksAsIntegrityFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(w *b13bW, a uuid.UUID)
	}{
		{"missing", func(w *b13bW, a uuid.UUID) {
			w.tamper("payout_attempt_destination_snapshots", `DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, a)
		}},
		{"tampered", func(w *b13bW, a uuid.UUID) {
			w.tamper("payout_attempt_destination_snapshots", `UPDATE payout_attempt_destination_snapshots SET display_mask = 'ZZ****9999' WHERE attempt_id = $1`, a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newB13bW(t, "o")
			wr := w.approved(500, "b13b-snap")
			cl := w.mustClaim(wr)
			// The provider call already happened (the destination was intact at phase B); the snapshot is damaged
			// before the success evidence is applied.
			w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-snap-1"}, StatusResult{})
			gr := w.dispatch(cl)
			tc.arm(w, cl.Attempt.ID)
			if err := w.apply(wr, cl, gr); err != nil {
				t.Fatalf("apply: %v", err)
			}
			w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationIntegrityFailure)
		})
	}
	// And with no destination service at all, a bound withdrawal's success cannot settle.
	w := newB13bW(t, "p")
	wr := w.approved(500, "b13b-noservice")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-ns-1"}, StatusResult{})
	gr := w.dispatch(cl)
	if err := ApplyPayoutResult(w.ctx(), w.pool, w.f.tenantID, wr.ID, cl.Attempt, gr, EvidenceSync); err != nil { // no options
		t.Fatal(err)
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationIntegrityFailure)
}

// Poll: the QueryStatus result is compared too. A mismatch parks; a match settles.
func TestB13B_Echo_Poll(t *testing.T) {
	w := newB13bW(t, "q")
	wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-poll-1", DestinationEcho: nil}, "b13b-poll")
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	a := w.attempt(cl.Attempt.ID)
	if a.State != AttemptPending {
		t.Fatalf("attempt = %s", a.State)
	}
	st := StatusResult{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", DestinationEcho: w.badEcho(cl.Attempt.ID)}
	w.prov.set(WithdrawResult{}, st)
	if err := PollPayoutStatus(w.ctx(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, a, time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("poll: %v", err)
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
	// Poll replay on the parked attempt: no further effect.
	if err := PollPayoutStatus(w.ctx(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, w.attempt(cl.Attempt.ID), time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("poll replay: %v", err)
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)

	w2 := newB13bW(t, "r")
	wr2, cl2, gr2 := w2.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-poll-2"}, "b13b-poll-2")
	if err := w2.apply(wr2, cl2, gr2); err != nil {
		t.Fatal(err)
	}
	w2.prov.set(WithdrawResult{}, StatusResult{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", DestinationEcho: w2.goodEcho(cl2.Attempt.ID)})
	if err := PollPayoutStatus(w2.ctx(), w2.pool, w2.orch, MockCredentialResolver{}, w2.f.tenantID, w2.attempt(cl2.Attempt.ID), time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := w2.wr(wr2.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("a matching poll must settle, withdrawal = %s", got.State)
	}
	w2.balanced()
}

func (w *b13bW) callback(a PaymentAttempt, outcome Outcome, ref string, echo *payoutinstrument.DestinationEcho) error {
	pending, err := alerting.InTx(w.ctx(), alerting.NewTenantRunner(w.pool, w.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.pid, ReceiptEvidence{
			EventType: "payout", ProviderReference: ref, MerchantReference: a.MerchantReference,
			Outcome: outcome, Amount: 500, AssetCode: "EUR", DestinationEcho: echo,
		})
		return err
	})
	if err == nil {
		pending.Flush(w.ctx())
	}
	return err
}

// Callback: a payout success callback with a differing destination parks the attempt; it never settles; and
// a callback can never determine a destination (no B13 table is written - pinned statically as well).
func TestB13B_Echo_Callback(t *testing.T) {
	w := newB13bW(t, "s")
	wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-cb-1"}, "b13b-cb")
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	a := w.attempt(cl.Attempt.ID)
	snapsBefore := w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots`)
	if err := w.callback(a, OutcomeSucceeded, "ref-cb-1", w.badEcho(cl.Attempt.ID)); err != nil {
		t.Fatalf("callback: %v", err)
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
	if w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots`) != snapsBefore {
		t.Fatal("a callback must not write any destination snapshot")
	}
	// Redelivery of the same callback: still one park.
	if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-cb-1", w.badEcho(cl.Attempt.ID)); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)

	// A matching callback settles.
	w2 := newB13bW(t, "t")
	wr2, cl2, gr2 := w2.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-cb-2"}, "b13b-cb-2")
	if err := w2.apply(wr2, cl2, gr2); err != nil {
		t.Fatal(err)
	}
	if err := w2.callback(w2.attempt(cl2.Attempt.ID), OutcomeSucceeded, "ref-cb-2", w2.goodEcho(cl2.Attempt.ID)); err != nil {
		t.Fatal(err)
	}
	if got := w2.wr(wr2.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("a matching callback must settle, withdrawal = %s", got.State)
	}
	w2.balanced()
}

// A differing echo on an ALREADY succeeded payout changes nothing but is audited and raised.
func TestB13B_Echo_OnTerminalSucceeded_SignalOnly(t *testing.T) {
	w := newB13bW(t, "u")
	wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-term-1"}, "b13b-term")
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-term-1", w.goodEcho(cl.Attempt.ID)); err != nil {
		t.Fatal(err)
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("withdrawal = %s", got.State)
	}
	ledgerBefore := w.ledgerTx()
	if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-term-1", w.badEcho(cl.Attempt.ID)); err != nil {
		t.Fatal(err)
	}
	a := w.attempt(cl.Attempt.ID)
	if a.State != AttemptSucceeded || w.wr(wr.ID).State != withdrawal.StateCompleted || w.ledgerTx() != ledgerBefore {
		t.Fatal("a terminal payout must not change")
	}
	if n := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_destination_mismatch_terminal' AND target_id=$1`, a.ID.String()); n != 1 {
		t.Fatalf("terminal mismatch audit rows = %d", n)
	}
	if al, ok := w.alertFor(a.ID, alertReasonPayoutDestinationMismatchOnTerminal); !ok || al.Severity != "p1" {
		t.Fatalf("terminal mismatch signal missing: %+v", al)
	}
	w.balanced()
}

// Concurrency: N workers apply the same mismatching evidence at once. Exactly one park (one audit row, one alert
// occurrence), the hold untouched, the ledger balanced.
func TestB13B_Echo_ConcurrentMismatch_ExactlyOnePark(t *testing.T) {
	w := newB13bW(t, "v")
	wr := w.approved(500, "b13b-conc")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-conc-1", DestinationEcho: w.badEcho(cl.Attempt.ID)}, StatusResult{})
	gr := w.dispatch(cl)
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.apply(wr, cl, gr)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("worker %d: %v", i, e)
		}
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
	if al, _ := w.alertFor(cl.Attempt.ID, TerminalReasonDestinationMismatch); al.Occurrences != 1 {
		t.Fatalf("alert occurrences = %d, want 1", al.Occurrences)
	}
}

// Concurrency at T1p: N claims race for one approved withdrawal: exactly one attempt and one snapshot.
func TestB13B_T1p_ConcurrentClaims_OneAttemptOneSnapshot(t *testing.T) {
	w := newB13bW(t, "x")
	wr := w.approved(500, "b13b-claims")
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = w.claim(wr, "bank_transfer")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("successful claims = %d, want exactly 1 (errors: %v)", ok, errs)
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots WHERE withdrawal_request_id = $1`, wr.ID); n != 1 {
		t.Fatalf("snapshots = %d", n)
	}
	w.balanced()
}

// Partial failure at T1p: if the snapshot insert fails (here: the instrument vanishes from under the claim - the
// deferred constraint refuses the commit), the WHOLE claim rolls back: request still approved, no attempt.
func TestB13B_T1p_SnapshotConstraintFailure_RollsBackTheClaim(t *testing.T) {
	w := newB13bW(t, "y")
	wr := w.approved(500, "b13b-partial")
	// Make the database refuse the snapshot (PI054: the in-force verification is no longer the latest): add a newer
	// verification row for the instrument after the Go gate reads it is not reproducible deterministically, so the
	// refusal is provoked at the insert by pointing the snapshot at a wrong amount via a hostile caller.
	err := w.pool.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := withdrawal.LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
			return err
		}
		g, err := w.svc.EvaluateGate(ctx, tx, payoutinstrument.GateParams{TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID,
			PersonID: w.f.personID, InstrumentID: *wr.PayoutInstrumentID, AssetCode: "EUR", Lock: true})
		if err != nil {
			return err
		}
		attemptID := uuid.New()
		if _, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{ID: attemptID, TenantID: w.f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &wr.ID, ProviderID: w.pid, PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: 500, ClaimToken: uuid.New(),
			LeaseOwner: "t", LeaseUntil: time.Now().Add(time.Minute)}); err != nil {
			return err
		}
		// a snapshot whose amount differs from the attempt's: the deferred constraint refuses the commit.
		_, err = w.svc.WriteSnapshot(ctx, tx, g, payoutinstrument.SnapshotParams{AttemptID: attemptID, WithdrawalRequestID: wr.ID, Amount: "499", AssetCode: "EUR"})
		return err
	})
	if err == nil {
		t.Fatal("a snapshot that differs from the attempt must be refused")
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("state = %s", got.State)
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
	w.balanced()
}
