//go:build integration

// PRH-2 C: PAY-DEP-REF-VALIDATE-1 (deposit half of PROVIDER-REF-BOUND-1
// security condition C1), LF-5 (sync amount evidence), LF-6 (same-tenant
// reference-binding pre-check) and S-9 (empty/invalid Pending reference).
//
// Every scenario drives the REAL InitiateDepositAttempt path against the
// MOCK adapter wrapped in depRefProvider, which rewrites the adapter's return
// value (the only way a MOCK can return a hostile reference or a wrong
// amount echo). All assertions read committed state in fresh transactions, so
// "the tx commits" is part of every one of them.
package payments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// depRefProvider wraps the MOCK and lets a test rewrite what Deposit returns.
type depRefProvider struct {
	*MockProvider
	mu        sync.Mutex
	script    func(req DepositRequest) DepositResult
	errScript func(req DepositRequest) (DepositResult, error) // takes precedence: result AND error
	status    map[string]StatusResult                         // QueryStatus overrides by reference
	queried   []string
}

func (p *depRefProvider) Deposit(ctx context.Context, req DepositRequest) (DepositResult, error) {
	res, err := p.MockProvider.Deposit(ctx, req)
	p.mu.Lock()
	script, errScript := p.script, p.errScript
	p.mu.Unlock()
	if err != nil {
		return res, err
	}
	if errScript != nil {
		return errScript(req)
	}
	if script == nil {
		return res, nil
	}
	return script(req), nil
}

func (p *depRefProvider) QueryStatus(ctx context.Context, ref string) (StatusResult, error) {
	p.mu.Lock()
	p.queried = append(p.queried, ref)
	ov, ok := p.status[ref]
	p.mu.Unlock()
	if ok {
		return ov, nil
	}
	return p.MockProvider.QueryStatus(ctx, ref)
}

func (p *depRefProvider) queriedRefs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queried...)
}

func (p *depRefProvider) setScript(s func(req DepositRequest) DepositResult) {
	p.mu.Lock()
	p.script = s
	p.mu.Unlock()
}

type depRefEnv struct {
	pool *db.Pool
	f    orchFixture
	p    *depRefProvider
	orch *Orchestrator
	id   string
}

func newDepRefEnv(t *testing.T, pool *db.Pool, providerID string) *depRefEnv {
	t.Helper()
	mp := NewMockProvider(providerID, "EUR")
	mp.SetManifest(OperationManifest{SupportsDeposit: true, StatusQuery: "by_provider_reference", IdempotentSubmission: true, SyncSuccessPossible: true})
	p := &depRefProvider{MockProvider: mp}
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{providerID: p}, MultiWebhookCredentialResolver{providerID: NewMockWebhookCredentials(mp)})
	return &depRefEnv{pool: pool, f: f, p: p, orch: orch, id: providerID}
}

// addTenant registers a second tenant on the same provider and orchestrator.
func (e *depRefEnv) addTenant(t *testing.T) orchFixture {
	t.Helper()
	f2 := seedOrchFixture(t, e.pool)
	registerCapability(t, e.pool, f2, e.p, 100)
	return f2
}

func scriptOutcome(o Outcome, ref string) func(DepositRequest) DepositResult {
	return func(req DepositRequest) DepositResult {
		r := DepositResult{Outcome: o, ProviderReference: ref, Amount: req.Amount, AssetCode: req.AssetCode}
		// A hostile or buggy adapter may hand back a redirect/token on ANY
		// outcome; a park must never forward them (security C-1).
		r.RedirectURL, r.HostedFieldToken = "https://mock-psp.invalid/pay/x", "hosted-tok"
		if o == OutcomeDeclined {
			r.DeclineReason, r.Cascadable = "provider_unavailable", false
		}
		return r
	}
}

func scriptSyncEcho(ref string, amount int64, asset string) func(DepositRequest) DepositResult {
	return func(req DepositRequest) DepositResult {
		return DepositResult{Outcome: OutcomeSucceeded, ProviderReference: ref, Amount: amount, AssetCode: asset,
			RedirectURL: "https://mock-psp.invalid/pay/x", HostedFieldToken: "hosted-tok"}
	}
}

func depScan[T any](t *testing.T, pool *db.Pool, tenantID uuid.UUID, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&v)
	}); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func depLedgerTxCount(t *testing.T, e *depRefEnv, f orchFixture) int64 {
	return depScan[int64](t, e.pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID)
}

func depDisputeAudits(t *testing.T, e *depRefEnv, f orchFixture, attemptID uuid.UUID) int64 {
	return depScan[int64](t, e.pool, f.tenantID,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`, f.tenantID, attemptID.String())
}

func depIntentStatus(t *testing.T, e *depRefEnv, f orchFixture, intentID uuid.UUID) string {
	return depScan[string](t, e.pool, f.tenantID, `SELECT status FROM deposit_intents WHERE id = $1`, intentID)
}

func depIntentRef(t *testing.T, e *depRefEnv, f orchFixture, intentID uuid.UUID) *string {
	return depScan[*string](t, e.pool, f.tenantID, `SELECT provider_reference FROM deposit_intents WHERE id = $1`, intentID)
}

func depAttemptCount(t *testing.T, e *depRefEnv, f orchFixture, intentID uuid.UUID) int64 {
	return depScan[int64](t, e.pool, f.tenantID, `SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND deposit_intent_id = $2`, f.tenantID, intentID)
}

func depNextActionNull(t *testing.T, e *depRefEnv, f orchFixture, attemptID uuid.UUID) bool {
	return depScan[bool](t, e.pool, f.tenantID, `SELECT next_action_at IS NULL FROM payment_attempts WHERE id = $1`, attemptID)
}

func depTerminalReason(a PaymentAttempt) string {
	if a.TerminalReason == nil {
		return "<nil>"
	}
	return *a.TerminalReason
}

// assertParkedNoMoney asserts the shared post-conditions of every C park: the
// attempt is terminal 'disputed' with the expected reason, exactly one
// payment.attempt_disputed audit exists, no ledger transaction of any kind
// was written, the player's balance is untouched, the ledger balances, the
// intent is 'ambiguous' (a dispute never projects 'declined': funds may be
// captured), no cascade child was created, and the attempt carries no
// next_action_at (nothing for the sweeper to retry - no error loop).
func assertParkedNoMoney(t *testing.T, e *depRefEnv, f orchFixture, res InitiateDepositAttemptResult, wantReason string) PaymentAttempt {
	t.Helper()
	a := mustGetAttempt(t, e.pool, f.tenantID, res.Attempt.ID)
	if a.State != AttemptDisputed || depTerminalReason(a) != wantReason {
		t.Fatalf("attempt state=%s reason=%s, want disputed/%s", a.State, depTerminalReason(a), wantReason)
	}
	if n := depDisputeAudits(t, e, f, a.ID); n != 1 {
		t.Errorf("payment.attempt_disputed audits = %d, want exactly 1", n)
	}
	if n := depLedgerTxCount(t, e, f); n != 0 {
		t.Errorf("a park must never produce a ledger transaction, found %d", n)
	}
	if b := cashBalance(t, e.pool, f); b != 0 {
		t.Errorf("player balance = %d, want 0", b)
	}
	assertLedgerBalanced(t, e.pool, f.tenantID)
	if s := depIntentStatus(t, e, f, res.Intent.ID); s != string(DepositIntentAmbiguous) {
		t.Errorf("intent status = %q, want ambiguous", s)
	}
	if res.Intent.Status != DepositIntentAmbiguous {
		t.Errorf("the intent RETURNED to the caller has status %q, want ambiguous (not a stale pre-park snapshot)", res.Intent.Status)
	}
	if n := depAttemptCount(t, e, f, res.Intent.ID); n != 1 {
		t.Errorf("attempts for the intent = %d, want 1 (a park must not cascade)", n)
	}
	if !depNextActionNull(t, e, f, a.ID) {
		t.Errorf("a parked attempt must have no next_action_at")
	}
	// F3: nothing for the sweeper to claim or touch.
	sw := NewSweeper(e.pool, e.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	if st := sw.RunOnce(context.Background(), []uuid.UUID{f.tenantID}); len(st.Errors) != 0 {
		t.Errorf("sweeper errors after a park: %v", st.Errors)
	}
	if after := mustGetAttempt(t, e.pool, f.tenantID, a.ID); after.State != AttemptDisputed || !after.UpdatedAt.Equal(a.UpdatedAt) {
		t.Errorf("the sweeper touched a parked attempt: %s", after.State)
	}
	// Security C-1: a parked attempt never hands the player a PSP session.
	if res.RedirectURL != "" || res.HostedFieldToken != "" {
		t.Errorf("a parked attempt must return no redirect/token, got %q %q", res.RedirectURL, res.HostedFieldToken)
	}
	return a
}

// --- PAY-DEP-REF-VALIDATE-1 / S-9 -------------------------------------------

func TestDepRef_InvalidReferenceOnAnyOutcomeParks_NoPersistenceNoRedirect(t *testing.T) {
	pool := testPool(t)
	long := strings.Repeat("a", providerrefMax+1)
	cases := []struct {
		name   string
		script func(DepositRequest) DepositResult
		reason string
		probe  string // substring that must not appear anywhere in this tenant's audit metadata
	}{
		{"pending oversize", scriptOutcome(OutcomePending, long), "too_long", long[:200]},
		{"pending control char", scriptOutcome(OutcomePending, "ref\x07bellX"), "control_char", "bellX"},
		{"pending invalid utf8", scriptOutcome(OutcomePending, "\xff\xfe"), "invalid_utf8", ""},
		{"pending empty (S-9)", scriptOutcome(OutcomePending, ""), "empty", ""},
		{"sync success oversize with a correct echo", scriptSyncEcho(long, 5000, "EUR"), "too_long", long[:200]},
		{"declined oversize (no cascade)", scriptOutcome(OutcomeDeclined, long), "too_long", long[:200]},
		{"ambiguous control char", scriptOutcome(OutcomeAmbiguous, "x\x01\x02yyZ"), "control_char", "yyZ"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-dr-inv"+string(rune('a'+i)))
			e.p.setScript(c.script)
			res := rvInit(t, pool, e.orch, e.f, 5000, "dr-inv")
			a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonInvalidProviderReference+":"+c.reason)
			if a.ProviderReference != nil {
				t.Errorf("an invalid reference must never be stored on the attempt")
			}
			if r := depIntentRef(t, e, e.f, res.Intent.ID); r != nil {
				t.Errorf("an invalid reference must never be stored on the intent")
			}
			if res.RedirectURL != "" || res.HostedFieldToken != "" {
				t.Errorf("a parked attempt must not hand a redirect/token to the player: %q %q", res.RedirectURL, res.HostedFieldToken)
			}
			if c.probe != "" {
				if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND strpos(metadata::text, $2) > 0`, e.f.tenantID, c.probe); n != 0 {
					t.Errorf("the raw invalid reference leaked into %d audit record(s)", n)
				}
			}
			// The audit carries only the closed reason, length and hash prefix.
			if got := depScan[string](t, pool, e.f.tenantID,
				`SELECT metadata->>'ref_reason' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				e.f.tenantID, a.ID.String()); got != c.reason {
				t.Errorf("audit ref_reason = %q, want %q", got, c.reason)
			}
		})
	}
}

// providerrefMax mirrors providerref.MaxBytes without importing it into every
// test case literal.
const providerrefMax = 255

// The park never turns the hostile result into an error that rolls back:
// replaying the identical hostile result for the same intent shape must give
// the identical outcome (deterministic), and nothing is left for the sweeper.
func TestDepRef_ParkedAttemptIsNotAnErrorLoop_SweeperHasNothingToDo(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-loop")
	e.p.setScript(scriptOutcome(OutcomePending, strings.Repeat("z", providerrefMax+1)))
	res := rvInit(t, pool, e.orch, e.f, 5000, "dr-loop")
	assertParkedNoMoney(t, e, e.f, res, TerminalReasonInvalidProviderReference+":too_long")

	// (A terminal attempt cannot even carry next_action_at: payment_attempts_check9.)
	sw := NewSweeper(pool, e.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	st := sw.RunOnce(context.Background(), []uuid.UUID{e.f.tenantID})
	if len(st.Errors) != 0 || st.Claimed != 0 {
		t.Fatalf("a parked attempt must be invisible to the sweeper: claimed=%d errors=%v", st.Claimed, st.Errors)
	}
	a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptDisputed {
		t.Fatalf("state changed after a sweep: %s", a.State)
	}
}

func TestDepRef_BoundaryAndNonParkingPaths(t *testing.T) {
	pool := testPool(t)

	t.Run("pending with exactly 255 bytes is accepted and bound", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-dr-b1")
		ref := strings.Repeat("k", providerrefMax)
		e.p.setScript(scriptOutcome(OutcomePending, ref))
		res := rvInit(t, pool, e.orch, e.f, 5000, "dr-b1")
		a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
		if a.State != AttemptPending || a.ProviderReference == nil || *a.ProviderReference != ref {
			t.Fatalf("state=%s ref=%v, want pending bound to the 255-byte reference", a.State, a.ProviderReference)
		}
		if depDisputeAudits(t, e, e.f, a.ID) != 0 {
			t.Errorf("a valid reference must not produce a dispute audit")
		}
		// Positive control: an accepted (pending) attempt still gets its redirect/token.
		if res.RedirectURL == "" || res.HostedFieldToken == "" {
			t.Errorf("a pending attempt must keep its redirect/token, got %q %q", res.RedirectURL, res.HostedFieldToken)
		}
	})

	t.Run("sync success with an empty reference keeps the ambiguous path", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-dr-b2")
		e.p.setScript(scriptSyncEcho("", 5000, "EUR"))
		res := rvInit(t, pool, e.orch, e.f, 5000, "dr-b2")
		a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
		if a.State != AttemptAmbiguous {
			t.Fatalf("state=%s, want ambiguous (NOT parked)", a.State)
		}
		if depDisputeAudits(t, e, e.f, a.ID) != 0 || depLedgerTxCount(t, e, e.f) != 0 {
			t.Errorf("an empty sync reference must neither dispute nor post")
		}
	})

	t.Run("declined with an empty reference is an ordinary decline", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-dr-b3")
		e.p.setScript(scriptOutcome(OutcomeDeclined, ""))
		res := rvInit(t, pool, e.orch, e.f, 5000, "dr-b3")
		a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
		if a.State != AttemptDeclined {
			t.Fatalf("state=%s, want declined", a.State)
		}
	})
}

// --- LF-5: sync amount evidence ---------------------------------------------

func TestDepSyncAmount_MatchPostsOnce(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-am-ok")
	e.p.setScript(scriptSyncEcho("sync-ok-1", 5000, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "am-ok")
	a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptSucceeded || a.LedgerTransactionID == nil {
		t.Fatalf("state=%s ledgerTx=%v, want succeeded with a posting", a.State, a.LedgerTransactionID)
	}
	if b := cashBalance(t, pool, e.f); b != 5000 {
		t.Fatalf("balance=%d, want 5000", b)
	}
	if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, e.f.tenantID); n != 1 {
		t.Fatalf("deposit postings=%d, want exactly 1", n)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

func TestDepSyncAmount_MissingEvidenceIsAmbiguousNotPostedNotDisputed(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name  string
		amt   int64
		asset string
	}{
		{"no amount", 0, "EUR"},
		{"no asset", 5000, ""},
		{"neither", 0, ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-dr-am-miss"+string(rune('a'+i)))
			e.p.setScript(scriptSyncEcho("sync-miss-"+uuid.NewString(), c.amt, c.asset))
			res := rvInit(t, pool, e.orch, e.f, 5000, "am-miss")
			a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
			if a.State != AttemptAmbiguous {
				t.Fatalf("state=%s, want ambiguous (the poll decides)", a.State)
			}
			if a.TerminalReason != nil {
				t.Errorf("a missing echo is not a dispute, terminal_reason=%s", depTerminalReason(a))
			}
			if depLedgerTxCount(t, e, e.f) != 0 || cashBalance(t, pool, e.f) != 0 {
				t.Errorf("missing amount evidence must never post")
			}
			if depDisputeAudits(t, e, e.f, a.ID) != 0 {
				t.Errorf("missing amount evidence must not write a dispute audit")
			}
		})
	}
}

func TestDepSyncAmount_MismatchDisputesNoPosting(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name  string
		amt   int64
		asset string
	}{
		{"one under", 4999, "EUR"},
		{"one over", 5001, "EUR"},
		{"wrong asset", 5000, "USD"},
		{"negative", -5000, "EUR"},
		{"both wrong", 1, "USD"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-dr-am-mm"+string(rune('a'+i)))
			e.p.setScript(scriptSyncEcho("sync-mm-"+uuid.NewString(), c.amt, c.asset))
			res := rvInit(t, pool, e.orch, e.f, 5000, "am-mm")
			a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonSyncAmountMismatch)
			if gotAmt := depScan[int64](t, pool, e.f.tenantID,
				`SELECT (metadata->>'provider_amount')::bigint FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				e.f.tenantID, a.ID.String()); gotAmt != c.amt {
				t.Errorf("audit provider_amount = %d, want %d", gotAmt, c.amt)
			}
		})
	}
}

// --- LF-6: same-tenant binding pre-check ------------------------------------

// seedPayoutAttemptBoundTo creates a payout attempt in tenant f, accepted
// with the given provider reference under providerID.
func seedPayoutAttemptBoundTo(t *testing.T, pool *db.Pool, f orchFixture, providerID, ref string) uuid.UUID {
	t.Helper()
	attemptID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		wr := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'submitted',$6)`, wr, f.tenantID, f.brandID, f.playerAccountID, f.walletID, "dr-payout-idem-"+wr.String()); err != nil {
			return err
		}
		if _, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: attemptID, TenantID: f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &wr, AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		}); err != nil {
			return err
		}
		if err := ClaimCreatedForSubmission(ctx, tx, attemptID, providerID, uuid.New(), "dr-seed", time.Now().Add(time.Minute)); err != nil {
			return err
		}
		return MarkAccepted(ctx, tx, attemptID, EvidencePlatform, ref, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("seed payout attempt: %v", err)
	}
	return attemptID
}

func TestDepRefConflict_ReferenceBoundToAnotherDepositAttempt_ParksEveryOutcome(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name   string
		script func(ref string) func(DepositRequest) DepositResult
	}{
		{"pending", func(ref string) func(DepositRequest) DepositResult { return scriptOutcome(OutcomePending, ref) }},
		{"sync success with a correct echo", func(ref string) func(DepositRequest) DepositResult { return scriptSyncEcho(ref, 5000, "EUR") }},
		{"declined", func(ref string) func(DepositRequest) DepositResult { return scriptOutcome(OutcomeDeclined, ref) }},
		{"ambiguous", func(ref string) func(DepositRequest) DepositResult { return scriptOutcome(OutcomeAmbiguous, ref) }},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-dr-cf"+string(rune('a'+i)))
			// Intent 1: an ordinary Pending deposit that binds reference R.
			first := rvInit(t, pool, e.orch, e.f, 5000, "cf-first")
			r := *first.Attempt.ProviderReference
			firstBefore := mustGetAttempt(t, pool, e.f.tenantID, first.Attempt.ID)

			// Intent 2 (a different intent, same tenant and provider): the
			// adapter returns R again.
			e.p.setScript(c.script(r))
			second := rvInit(t, pool, e.orch, e.f, 5000, "cf-second")
			a := mustGetAttempt(t, pool, e.f.tenantID, second.Attempt.ID)
			if a.State != AttemptDisputed || depTerminalReason(a) != TerminalReasonProviderReferenceConflict {
				t.Fatalf("second attempt state=%s reason=%s, want disputed/provider_reference_conflict", a.State, depTerminalReason(a))
			}
			if second.RedirectURL != "" || second.HostedFieldToken != "" {
				t.Errorf("a conflict park must return no redirect/token, got %q %q", second.RedirectURL, second.HostedFieldToken)
			}
			if a.ProviderReference != nil {
				t.Errorf("the conflicting reference must not be bound to the second attempt")
			}
			if depDisputeAudits(t, e, e.f, a.ID) != 1 {
				t.Errorf("want exactly one dispute audit for the conflicting attempt")
			}
			if got := depScan[string](t, pool, e.f.tenantID,
				`SELECT metadata->>'bound_to_operation' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				e.f.tenantID, a.ID.String()); got != "deposit" {
				t.Errorf("audit bound_to_operation = %q, want deposit", got)
			}
			if depLedgerTxCount(t, e, e.f) != 0 || cashBalance(t, pool, e.f) != 0 {
				t.Errorf("a binding conflict must never post")
			}
			assertLedgerBalanced(t, pool, e.f.tenantID)
			if s := depIntentStatus(t, e, e.f, second.Intent.ID); s != string(DepositIntentAmbiguous) {
				t.Errorf("second intent status=%q, want ambiguous", s)
			}
			if depAttemptCount(t, e, e.f, second.Intent.ID) != 1 {
				t.Errorf("a conflict must not cascade")
			}
			// The FIRST attempt is untouched.
			firstAfter := mustGetAttempt(t, pool, e.f.tenantID, first.Attempt.ID)
			if firstAfter.State != firstBefore.State || firstAfter.ProviderReference == nil || *firstAfter.ProviderReference != r || !firstAfter.UpdatedAt.Equal(firstBefore.UpdatedAt) {
				t.Errorf("the first attempt changed: before=%+v after=%+v", firstBefore, firstAfter)
			}
		})
	}
}

func TestDepRefConflict_ReferenceBoundToAPayoutAttempt_Parks(t *testing.T) {
	pool := testPool(t)
	for i, name := range []string{"pending", "sync success"} {
		t.Run(name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-dr-cfp"+string(rune('a'+i)))
			const payoutRef = "payout-bound-ref-1"
			payoutID := seedPayoutAttemptBoundTo(t, pool, e.f, e.id, payoutRef)
			before := mustGetAttempt(t, pool, e.f.tenantID, payoutID)
			if name == "pending" {
				e.p.setScript(scriptOutcome(OutcomePending, payoutRef))
			} else {
				e.p.setScript(scriptSyncEcho(payoutRef, 5000, "EUR"))
			}
			res := rvInit(t, pool, e.orch, e.f, 5000, "cfp")
			a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonProviderReferenceConflict)
			if got := depScan[string](t, pool, e.f.tenantID,
				`SELECT metadata->>'bound_to_operation' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				e.f.tenantID, a.ID.String()); got != "payout" {
				t.Errorf("audit bound_to_operation = %q, want payout", got)
			}
			if depLedgerTxCount(t, e, e.f) != 0 || cashBalance(t, pool, e.f) != 0 {
				t.Errorf("a binding conflict must never post")
			}
			after := mustGetAttempt(t, pool, e.f.tenantID, payoutID)
			if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("the payout attempt changed: before=%s after=%s", before.State, after.State)
			}
		})
	}
}

func TestDepRefConflict_ReferenceBoundToAnotherIntent_Parks(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-cfi")
	// A legacy-shaped intent that holds the reference with no attempt row.
	const boundRef = "intent-bound-ref-1"
	rawIntent := insertRawDepositIntent(t, pool, e.f, "pending")
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deposit_intents SET provider_id = $2, provider_reference = $3 WHERE id = $1`, rawIntent, e.id, boundRef)
		return err
	}); err != nil {
		t.Fatalf("bind raw intent: %v", err)
	}
	e.p.setScript(scriptOutcome(OutcomePending, boundRef))
	res := rvInit(t, pool, e.orch, e.f, 5000, "cfi")
	a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonProviderReferenceConflict)
	if got := depScan[string](t, pool, e.f.tenantID,
		`SELECT metadata->>'bound_to_operation' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
		e.f.tenantID, a.ID.String()); got != "deposit_intent" {
		t.Errorf("audit bound_to_operation = %q, want deposit_intent", got)
	}
	if depLedgerTxCount(t, e, e.f) != 0 {
		t.Errorf("a binding conflict must never post")
	}
}

// Cross-tenant (S-9, LF-6): there is no cross-tenant read. Tenant B reusing
// the SAME reference string as tenant A's attempt neither detects, disturbs
// nor is disturbed by it: B's attempt binds the string in B's own namespace
// and B's sync success posts only to B's ledger.
func TestDepRefConflict_CrossTenantSameStringHasNoEffectOnTheOtherTenant(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-xt")
	fA := e.f
	fB := e.addTenant(t)

	resA := rvInit(t, pool, e.orch, fA, 5000, "xt-a")
	r := *resA.Attempt.ProviderReference
	aBefore := mustGetAttempt(t, pool, fA.tenantID, resA.Attempt.ID)
	ledgerA := depScan[int64](t, pool, fA.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, fA.tenantID)
	auditA := depScan[int64](t, pool, fA.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, fA.tenantID)

	// Tenant B: the adapter returns A's string as a sync success.
	e.p.setScript(scriptSyncEcho(r, 5000, "EUR"))
	resB := rvInit(t, pool, e.orch, fB, 5000, "xt-b")
	bAttempt := mustGetAttempt(t, pool, fB.tenantID, resB.Attempt.ID)
	if bAttempt.State != AttemptSucceeded || bAttempt.ProviderReference == nil || *bAttempt.ProviderReference != r {
		t.Fatalf("tenant B: state=%s ref=%v, want succeeded bound to its own copy of the string", bAttempt.State, bAttempt.ProviderReference)
	}
	if b := cashBalance(t, pool, fB); b != 5000 {
		t.Fatalf("tenant B balance=%d, want 5000", b)
	}
	if n := depDisputeAudits(t, e, fB, bAttempt.ID); n != 0 {
		t.Errorf("tenant B must not be disputed by a reference it cannot see elsewhere, found %d dispute audits", n)
	}

	// Tenant A is entirely unaffected.
	aAfter := mustGetAttempt(t, pool, fA.tenantID, resA.Attempt.ID)
	if aAfter.State != aBefore.State || aAfter.ProviderReference == nil || *aAfter.ProviderReference != r || !aAfter.UpdatedAt.Equal(aBefore.UpdatedAt) {
		t.Errorf("tenant A attempt changed: before=%+v after=%+v", aBefore, aAfter)
	}
	if got := depScan[int64](t, pool, fA.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, fA.tenantID); got != ledgerA {
		t.Errorf("tenant A ledger changed: %d -> %d", ledgerA, got)
	}
	if got := depScan[int64](t, pool, fA.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, fA.tenantID); got != auditA {
		t.Errorf("tenant A audit changed: %d -> %d", auditA, got)
	}
	if b := cashBalance(t, pool, fA); b != 0 {
		t.Errorf("tenant A balance=%d, want 0", b)
	}

	// And in the other direction: A's own verified success for R posts to A
	// only; B's ledger and balance do not move.
	e.p.setScript(nil)
	if _, err := rvCallback(pool, e.orch, fA, e.id, e.p.CallbackPayload(fA.tenantID, CallbackEventDeposit, r, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("tenant A success callback: %v", err)
	}
	if b := cashBalance(t, pool, fA); b != 5000 {
		t.Errorf("tenant A balance=%d, want 5000", b)
	}
	if b := cashBalance(t, pool, fB); b != 5000 {
		t.Errorf("tenant B balance moved: %d, want 5000", b)
	}
	assertLedgerBalanced(t, pool, fA.tenantID)
	assertLedgerBalanced(t, pool, fB.tenantID)
}

// Callback-versus-sync-success, as it can actually be forced.
//
// A sync success binds its reference to the attempt only in phase C's own
// commit (ApplySuccess). Before that commit a verified callback for the same
// reference cannot be resolved (nothing is bound on deposit_intents or
// payment_attempts yet), so the ONLY reachable orderings are: (a) callback
// during phase B -> stored deferred_unresolved; (b) callback after phase C ->
// duplicate_effect. A "callback queued behind phase C's lock" ordering does not
// exist for sync success (it would still be unresolved), so unlike the
// Pending race (TestRVLF_P8) no lock-held interleaving can make the callback
// resolve first; both orderings are therefore driven sequentially and
// deterministically, with the disposition asserted explicitly.
func TestDepRef_SyncSuccess_CallbackDuringPhaseB_DefersAndPostsExactlyOnce(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-race-b")
	ref := "race-ref-" + uuid.NewString()
	var cbRes ReceiveCallbackResult
	var cbErr error
	e.p.setScript(func(req DepositRequest) DepositResult {
		// Runs inside phase B (no tx held by the caller): the reference is not bound yet.
		cbRes, cbErr = rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
		return DepositResult{Outcome: OutcomeSucceeded, ProviderReference: ref, Amount: req.Amount, AssetCode: req.AssetCode}
	})
	res := rvInit(t, pool, e.orch, e.f, 5000, "race-b")
	if cbErr != nil {
		t.Fatalf("callback returned %v", cbErr)
	}
	if cbRes.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("callback during phase B: disposition=%s, want deferred_unresolved (reference not bound yet)", cbRes.Disposition)
	}
	a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded", a.State)
	}
	if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, e.f.tenantID); n != 1 {
		t.Fatalf("deposit postings=%d, want exactly 1", n)
	}
	if b := cashBalance(t, pool, e.f); b != 5000 {
		t.Fatalf("balance=%d, want 5000", b)
	}
	if depDisputeAudits(t, e, e.f, a.ID) != 0 {
		t.Fatalf("a deferred callback followed by the sync success must not dispute")
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	t.Logf("deferred receipts still unresolved after the sync success (known residual, see report): %d",
		depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2 AND resolved_at IS NULL`, e.f.tenantID, ref))
}

func TestDepRef_SyncSuccess_CallbackAfterPhaseC_IsDuplicateEffectNoSecondPosting(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-race-a")
	ref := "race-ref-" + uuid.NewString()
	e.p.setScript(scriptSyncEcho(ref, 5000, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "race-a")
	cb, err := rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if cb.Disposition != DispositionDuplicateEffect {
		t.Fatalf("callback after phase C: disposition=%s, want duplicate_effect", cb.Disposition)
	}
	if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, e.f.tenantID); n != 1 {
		t.Fatalf("deposit postings=%d, want exactly 1", n)
	}
	if a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID); a.State != AttemptSucceeded || depDisputeAudits(t, e, e.f, a.ID) != 0 {
		t.Fatalf("state=%s, want succeeded and undisputed", a.State)
	}
	if b := cashBalance(t, pool, e.f); b != 5000 {
		t.Fatalf("balance=%d, want 5000", b)
	}
}

// F3: a late verified callback after a LF-6 conflict park. The reference
// belongs to the OTHER attempt, so the callback legitimately applies there and
// posts to the first intent only; the parked attempt stays disputed.
func TestDepRefConflict_LateCallbackAfterConflictPark_AppliesToTheBoundAttemptOnly(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-late-cf")
	first := rvInit(t, pool, e.orch, e.f, 5000, "late-cf-1")
	r := *first.Attempt.ProviderReference
	e.p.setScript(scriptOutcome(OutcomePending, r))
	second := rvInit(t, pool, e.orch, e.f, 5000, "late-cf-2")
	parked := mustGetAttempt(t, pool, e.f.tenantID, second.Attempt.ID)
	if parked.State != AttemptDisputed || depTerminalReason(parked) != TerminalReasonProviderReferenceConflict {
		t.Fatalf("setup: parked attempt %s/%s", parked.State, depTerminalReason(parked))
	}

	cb, err := rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, r, "", OutcomeSucceeded, 5000, "EUR", "", false))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if cb.Disposition != DispositionApplied {
		t.Fatalf("callback disposition=%s, want applied (to the attempt that owns the reference)", cb.Disposition)
	}
	if n := ledgerDepositTxCount(t, pool, e.f.tenantID, first.Intent.ID); n != 1 {
		t.Errorf("first intent postings=%d, want 1", n)
	}
	if n := ledgerDepositTxCount(t, pool, e.f.tenantID, second.Intent.ID); n != 0 {
		t.Errorf("parked intent postings=%d, want 0", n)
	}
	a := mustGetAttempt(t, pool, e.f.tenantID, second.Attempt.ID)
	if a.State != AttemptDisputed || depTerminalReason(a) != TerminalReasonProviderReferenceConflict {
		t.Errorf("parked attempt changed: %s/%s", a.State, depTerminalReason(a))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

// F3: a late verified callback after a sync_amount_mismatch park. The parked
// attempt bound the reference at the park (LF F-C1), so the callback resolves to
// the disputed attempt and is recorded only (duplicate_effect). Nothing posts
// and the attempt stays disputed.
func TestDepSyncAmount_LateCallbackAfterMismatchPark_RecordedOnlyNoPosting(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-late-mm")
	ref := "late-mm-" + uuid.NewString()
	e.p.setScript(scriptSyncEcho(ref, 4999, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "late-mm")
	assertParkedNoMoney(t, e, e.f, res, TerminalReasonSyncAmountMismatch)

	cb, err := rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if cb.Disposition != DispositionDuplicateEffect {
		t.Fatalf("callback disposition=%s, want duplicate_effect (the park bound the reference; success on a disputed attempt is recorded only)", cb.Disposition)
	}
	if depLedgerTxCount(t, e, e.f) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Errorf("a late callback after a mismatch park must not post")
	}
	a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptDisputed || depTerminalReason(a) != TerminalReasonSyncAmountMismatch {
		t.Errorf("parked attempt changed: %s/%s", a.State, depTerminalReason(a))
	}
}

// Security C-1 through the cascade driver (driveCreatedAttempt): provider A
// declines cascadably, the cascade child is routed to provider B, whose
// Pending result (with a redirect and token) names a reference already bound
// to a payout attempt in the same tenant. The child parks; the player gets no
// redirect from the request that drove it.
func TestDepRefConflict_CascadeDrivenParkReturnsNoRedirect(t *testing.T) {
	pool := testPool(t)
	ma := NewMockProvider("mock-dr-cas-a", "EUR")
	mb := NewMockProvider("mock-dr-cas-b", "EUR")
	pa := &depRefProvider{MockProvider: ma}
	pb := &depRefProvider{MockProvider: mb}
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-dr-cas-a": pa, "mock-dr-cas-b": pb},
		MultiWebhookCredentialResolver{"mock-dr-cas-a": NewMockWebhookCredentials(ma), "mock-dr-cas-b": NewMockWebhookCredentials(mb)})
	const boundRef = "cascade-bound-ref-1"
	seedPayoutAttemptBoundTo(t, pool, f, "mock-dr-cas-b", boundRef)
	pa.setScript(func(req DepositRequest) DepositResult {
		return DepositResult{Outcome: OutcomeDeclined, DeclineReason: "provider_unavailable", Cascadable: true, Amount: req.Amount, AssetCode: req.AssetCode}
	})
	pb.setScript(scriptOutcome(OutcomePending, boundRef))

	res := rvInit(t, pool, orch, f, 5000, "cas")
	if res.RedirectURL != "" || res.HostedFieldToken != "" {
		t.Fatalf("a cascade-driven conflict park must return no redirect/token, got %q %q", res.RedirectURL, res.HostedFieldToken)
	}
	if res.Attempt.AttemptNo != 2 {
		t.Fatalf("expected the driven attempt to be the cascade child (attempt_no 2), got %d", res.Attempt.AttemptNo)
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if a.State != AttemptDisputed || depTerminalReason(a) != TerminalReasonProviderReferenceConflict {
		t.Fatalf("child state=%s reason=%s, want disputed/provider_reference_conflict", a.State, depTerminalReason(a))
	}
	if n := depScan[int64](t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID); n != 0 {
		t.Errorf("no ledger transaction may exist, found %d", n)
	}
}

// Code review F1: the adapter returns an ERROR together with a reference. The
// reference is validated before the error return, so a hostile one parks like
// any other outcome (before, it reached the binding query and the intent write
// raw, hit the 0099 CHECK, and the attempt looped forever as 'submitting').
func TestDepRef_ErrorPathWithInvalidReference_ParksNothingPersisted(t *testing.T) {
	pool := testPool(t)
	long := strings.Repeat("e", providerrefMax+1)
	cases := []struct {
		name, ref, reason string
	}{
		{"error with oversize reference", long, "too_long"},
		{"error with control-character reference", "err\x07refQ", "control_char"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-dr-err"+string(rune('a'+i)))
			e.p.errScript = func(req DepositRequest) (DepositResult, error) {
				return DepositResult{Outcome: OutcomePending, ProviderReference: c.ref, RedirectURL: "https://mock-psp.invalid/pay/x", HostedFieldToken: "hosted-tok"},
					errors.New("psp transport blew up after answering")
			}
			res := rvInit(t, pool, e.orch, e.f, 5000, "dr-err")
			a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonInvalidProviderReference+":"+c.reason)
			if a.ProviderReference != nil || depIntentRef(t, e, e.f, res.Intent.ID) != nil {
				t.Errorf("an invalid reference must never be stored")
			}
		})
	}
}

// Orchestrator ruling: an ambiguous result on the error path returns NO
// redirect and NO token even when the reference is valid (a player-facing
// redirect exists only for an attempt phase C leaves pending). The valid
// reference IS bound on the ambiguous attempt (LF F-C1).
func TestDepRef_ErrorPathWithValidReference_AmbiguousNoRedirectReferenceBound(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-err-ok")
	ref := "err-valid-" + uuid.NewString()
	e.p.errScript = func(req DepositRequest) (DepositResult, error) {
		return DepositResult{Outcome: OutcomePending, ProviderReference: ref, RedirectURL: "https://mock-psp.invalid/pay/x", HostedFieldToken: "hosted-tok"},
			errors.New("psp transport blew up after answering")
	}
	res := rvInit(t, pool, e.orch, e.f, 5000, "dr-err-ok")
	if res.RedirectURL != "" || res.HostedFieldToken != "" {
		t.Fatalf("an ambiguous error-path result must return no redirect/token, got %q %q", res.RedirectURL, res.HostedFieldToken)
	}
	a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptAmbiguous {
		t.Fatalf("state=%s, want ambiguous", a.State)
	}
	if a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Fatalf("the validated reference must be bound on the ambiguous attempt, got %v", a.ProviderReference)
	}
	if depLedgerTxCount(t, e, e.f) != 0 {
		t.Errorf("nothing may post")
	}
}

// LF F-C1: a sync success with no amount echo goes ambiguous WITH the
// reference bound, so the sweeper's very next poll actually calls QueryStatus
// with it, and a polled success then posts exactly once.
func TestDepSyncAmount_MissingEcho_ReferenceBound_SweepPollsAndPostsOnce(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-poll")
	ref := "poll-ref-" + uuid.NewString()
	e.p.setScript(scriptSyncEcho(ref, 0, "EUR")) // amount not echoed
	e.p.status = map[string]StatusResult{ref: {ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}}
	res := rvInit(t, pool, e.orch, e.f, 5000, "dr-poll")
	a := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptAmbiguous || a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Fatalf("state=%s ref=%v, want ambiguous with the reference bound", a.State, a.ProviderReference)
	}
	setNextActionNow(t, pool, e.f.tenantID, a.ID)
	sw := NewSweeper(pool, e.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	if st := sw.RunOnce(context.Background(), []uuid.UUID{e.f.tenantID}); len(st.Errors) != 0 {
		t.Fatalf("sweep errors: %v", st.Errors)
	}
	if q := e.p.queriedRefs(); len(q) != 1 || q[0] != ref {
		t.Fatalf("QueryStatus calls=%v, want exactly one with %q", q, ref)
	}
	final := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if final.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded after the poll", final.State)
	}
	if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, e.f.tenantID); n != 1 {
		t.Fatalf("deposit postings=%d, want exactly 1", n)
	}
	if b := cashBalance(t, pool, e.f); b != 5000 {
		t.Fatalf("balance=%d, want 5000", b)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

// LF F-C1 (mismatch half) and F-C3: the sync_amount_mismatch park binds the
// validated reference, and the audit names the adapter outcome.
func TestDepSyncAmount_MismatchPark_BindsReferenceAndAuditsAdapterOutcome(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-dr-mm-bind")
	ref := "mm-bind-" + uuid.NewString()
	e.p.setScript(scriptSyncEcho(ref, 4999, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "dr-mm-bind")
	a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonSyncAmountMismatch)
	if a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Errorf("the mismatch park must bind the validated reference, got %v", a.ProviderReference)
	}
	if got := depScan[string](t, pool, e.f.tenantID,
		`SELECT metadata->>'adapter_outcome' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
		e.f.tenantID, a.ID.String()); got != string(OutcomeSucceeded) {
		t.Errorf("audit adapter_outcome=%q, want succeeded", got)
	}
}
