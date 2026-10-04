//go:build integration

// PRH-2 K3 (ADR 0101 revision 4 + section 26) shared fixtures. Every world is
// a fully isolated, fully migrated PRIVATE scratch database (its own tenant,
// players, staff, grants and platform baseline policy), so a "no platform
// baseline -> disabled" case never depends on another test. Every RLS, guard
// and fence assertion runs as the non-superuser, non-BYPASSRLS application
// owner role scratchdb connects as; each world asserts that role (T-1,
// vacuity) before any test uses it. All policy values are synthetic and
// test-only (HD-PRH2-3).
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func k3Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func k3RequireCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := k3Code(err); got != code {
		t.Fatalf("expected SQLSTATE %s, got %q (%v)", code, got, err)
	}
}

// k3Staff is one staff principal with its own distinct Person.
type k3Staff struct {
	ID       uuid.UUID
	PersonID uuid.UUID
	TenantID uuid.UUID // uuid.Nil for a platform principal
	Role     string
}

const k3Hash = "ab" // repeated 32 times: a lowercase-hex SHA-256 stand-in

func k3EvidenceHash() string { return strings.Repeat(k3Hash, 32) }

var k3Counter atomic.Int64

type k3Opts struct {
	// base is the synthetic baseline requirement of the platform row. 0 means
	// "no baseline" (the operation is DISABLED, HD-PRH2-3).
	base int
	// noStatementSource leaves the provider out of the statement-source
	// registry (an m2_declare_not_paid is then refused, LF O-4).
	noStatementSource bool
	// noClearing funds the wallet by a casino win instead of a deposit, so the
	// tenant has NO psp_clearing account until an M2 "declare paid" creates it
	// (T-2: the first-ever psp_clearing creation for a tenant and asset).
	noClearing bool
}

type k3World struct {
	t    *testing.T
	pool *db.Pool
	f    payoutFixture
	svc  *ManualResolutionService
	reg  *StatementSourceRegistry

	provider string
	mock     *MockProvider
	prov     *k3Provider // the scripted wrapper the orchestrator calls
	orch     *Orchestrator

	// Platform principals: two policy authors, a K1 grant approver and two
	// acting principals (G-P2 grants for the tenant).
	adminA, adminB, grantApprover, acting, acting2 k3Staff
	tenantAdmin                                    k3Staff
	// Tenant finance staff with request+approve grants.
	f1, f2, f3, f4 k3Staff
	financeNoGrant k3Staff
	grantIDs       map[string]uuid.UUID
}

func k3MkStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID, role string, person uuid.UUID) k3Staff {
	t.Helper()
	ctx := context.Background()
	s := k3Staff{ID: uuid.New(), PersonID: person, TenantID: tenantID, Role: role}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1) ON CONFLICT DO NOTHING`, person)
		return err
	}); err != nil {
		t.Fatalf("insert person: %v", err)
	}
	insert := func(ctx context.Context, tx pgx.Tx) error {
		var tid any
		if tenantID != uuid.Nil {
			tid = tenantID
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', $4, 'active', $5)`, s.ID, tid, "k3-"+s.ID.String()+"@test.invalid", role, person)
		return err
	}
	var err error
	if tenantID == uuid.Nil {
		err = pool.WithoutTenant(ctx, insert)
	} else {
		err = pool.WithTenant(ctx, tenantID, insert)
	}
	if err != nil {
		t.Fatalf("insert staff %s: %v", role, err)
	}
	return s
}

// fundViaCasino posts a casino win (house_gaming -> player_cash): funds without
// creating a psp_clearing account.
func (w *k3World) fundViaCasino(amount int64) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.f.walletID
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.f.tenantID,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		pid, ptx := "k3-mock-casino", uuid.NewString()
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxCasinoWin, IdempotencyKey: "k3-fixture:" + ptx,
			ProviderID: &pid, ProviderTxID: &ptx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
}

func (w *k3World) staffMember(tenantID uuid.UUID, role string) k3Staff {
	w.t.Helper()
	return k3MkStaff(w.t, w.pool, tenantID, role, uuid.New())
}

func newK3World(t *testing.T, opts k3Opts) *k3World {
	t.Helper()
	pool := depositV2ScratchPool(t)
	return newK3WorldOn(t, pool, opts)
}

func newK3WorldOn(t *testing.T, pool *db.Pool, opts k3Opts) *k3World {
	t.Helper()
	ctx := context.Background()
	n := k3Counter.Add(1)
	w := &k3World{t: t, pool: pool, grantIDs: map[string]uuid.UUID{}}
	initial := int64(1_000_000)
	if opts.noClearing {
		initial = 0
	}
	w.f = seedPayoutFixture(t, pool, initial, true)
	if opts.noClearing {
		w.fundViaCasino(1_000_000)
	}
	w.provider = fmt.Sprintf("k3-psp-%d-%s", n, w.f.tenantID.String()[:6])
	w.mock = NewMockProvider(w.provider, "EUR")
	w.prov = &k3Provider{MockProvider: w.mock}
	registerCapability(t, pool, w.f.orchFixture, w.prov, 100)
	w.orch = NewOrchestrator(map[string]PaymentProvider{w.provider: w.prov},
		MultiWebhookCredentialResolver{w.provider: NewMockWebhookCredentials(w.mock)})
	w.reg = &StatementSourceRegistry{}
	if !opts.noStatementSource {
		w.reg.Register(w.provider)
	}
	w.svc = NewManualResolutionService(pool, w.reg)

	w.adminA = w.staffMember(uuid.Nil, "platform_admin")
	w.adminB = w.staffMember(uuid.Nil, "platform_admin")
	w.grantApprover = w.staffMember(uuid.Nil, "platform_admin")
	w.acting = w.staffMember(uuid.Nil, "platform_admin")
	w.acting2 = w.staffMember(uuid.Nil, "platform_admin")
	w.tenantAdmin = w.staffMember(w.f.tenantID, "tenant_admin")
	w.f1 = w.staffMember(w.f.tenantID, "finance")
	w.f2 = w.staffMember(w.f.tenantID, "finance")
	w.f3 = w.staffMember(w.f.tenantID, "finance")
	w.f4 = w.staffMember(w.f.tenantID, "finance")
	w.financeNoGrant = w.staffMember(w.f.tenantID, "finance")
	for _, s := range []k3Staff{w.f1, w.f2, w.f3, w.f4} {
		w.grantTenant(s, capability.CapabilityPaymentForceResolveRequest)
		w.grantTenant(s, capability.CapabilityPaymentForceResolveApprove)
	}
	w.grantActing(w.acting, capability.CapabilityPaymentForceResolveRequest)
	w.grantActing(w.acting, capability.CapabilityPaymentForceResolveApprove)
	w.grantActing(w.acting2, capability.CapabilityPaymentForceResolveApprove)

	if opts.base > 0 {
		w.approvePolicy(adjustment.PolicyChangeInput{
			ChangeKind: adjustment.ChangeKindPolicy, OperationKind: OperationKindForceResolve, Level: adjustment.LevelPlatform,
			AssetCode: k3StrPtr("EUR"), BaseRequiredApprovals: k3IntPtr(opts.base),
		}, w.adminA, w.adminB)
	}
	// T-1 vacuity: the session role the whole world runs as must be neither a
	// superuser nor BYPASSRLS, or every RLS/guard/fence assertion would pass
	// for the wrong reason.
	k3AssertUnprivilegedRole(t, pool, w.f.tenantID)
	_ = ctx
	return w
}

// k3AssertUnprivilegedRole asserts the T-1 vacuity condition.
func k3AssertUnprivilegedRole(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var super, bypass bool
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatalf("role probe: %v", err)
	}
	if super || bypass {
		t.Fatalf("T-1 vacuity: the test session role is rolsuper=%v rolbypassrls=%v - every RLS assertion would be vacuous", super, bypass)
	}
}

func k3StrPtr(s string) *string { return &s }
func k3IntPtr(i int) *int       { return &i }

func k3GrantKey(staff uuid.UUID, c capability.Capability) string {
	return staff.String() + "|" + string(c)
}

// grantTenant issues a G-T grant: tenant_admin requests, an independent
// platform principal co-approves (ADR 0099 4).
func (w *k3World) grantTenant(s k3Staff, c capability.Capability) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	var reqID uuid.UUID
	if err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := capability.CreateRequest(ctx, tx, w.f.tenantID, capability.NewRequestInput{GranteeStaffID: s.ID, Capability: c, ReasonCode: "k3-fixture"})
		reqID = r.ID
		return err
	}); err != nil {
		w.t.Fatalf("grant request %s: %v", c, err)
	}
	var gid uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.grantApprover.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, w.f.tenantID, reqID, "approve", "k3-fixture")
		if err == nil {
			gid = g.ID
		}
		return err
	}); err != nil {
		w.t.Fatalf("grant approve %s: %v", c, err)
	}
	w.grantIDs[k3GrantKey(s.ID, c)] = gid
	return gid
}

// grantActing issues a G-P2 grant (a platform principal acting in the tenant).
func (w *k3World) grantActing(p k3Staff, c capability.Capability) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	until := time.Now().Add(2 * time.Hour)
	var reqID uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := capability.CreateRequest(ctx, tx, w.f.tenantID, capability.NewRequestInput{GranteeStaffID: p.ID, Capability: c, ValidUntil: &until, ReasonCode: "k3-fixture"})
		reqID = r.ID
		return err
	}); err != nil {
		w.t.Fatalf("G-P2 request %s: %v", c, err)
	}
	var gid uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.grantApprover.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, w.f.tenantID, reqID, "approve", "k3-fixture")
		if err == nil {
			gid = g.ID
		}
		return err
	}); err != nil {
		w.t.Fatalf("G-P2 approve %s: %v", c, err)
	}
	w.grantIDs[k3GrantKey(p.ID, c)] = gid
	return gid
}

func (w *k3World) revokeGrant(staff uuid.UUID, c capability.Capability) {
	w.t.Helper()
	gid, ok := w.grantIDs[k3GrantKey(staff, c)]
	if !ok {
		w.t.Fatalf("no grant recorded for %s %s", staff, c)
	}
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, w.f.tenantID, gid, "k3-test-revoke")
	}); err != nil {
		w.t.Fatalf("revoke: %v", err)
	}
}

func (w *k3World) approvePolicy(in adjustment.PolicyChangeInput, requester, approver k3Staff) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	var c adjustment.PolicyChange
	if err := w.pool.WithPlatformAdmin(ctx, requester.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = adjustment.ProposePolicyChangeInTx(ctx, tx, adjustment.PolicyCall{ActorID: requester.ID}, in)
		return err
	}); err != nil {
		w.t.Fatalf("propose policy: %v", err)
	}
	if err := w.pool.WithPlatformAdmin(ctx, approver.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := adjustment.DecidePolicyChangeInTx(ctx, tx, adjustment.PolicyCall{ActorID: approver.ID}, c.ID, adjustment.DecisionApprove, c.ContentHash, "k3-test")
		return err
	}); err != nil {
		w.t.Fatalf("approve policy: %v", err)
	}
	var id uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM financial_approval_policies WHERE change_id = $1`, c.ID).Scan(&id)
	}); err != nil {
		w.t.Fatalf("read policy row: %v", err)
	}
	return id
}

func (w *k3World) setTenantStatus(status string) {
	w.t.Helper()
	if err := w.pool.WithPlatformAdmin(context.Background(), w.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = $2 WHERE id = $1`, w.f.tenantID, status)
		return err
	}); err != nil {
		w.t.Fatalf("set tenant status %s: %v", status, err)
	}
}

// k3Ctx returns a context carrying actor's verified-token identity, as the HTTP
// auth middleware would build it.
func k3Ctx(actor k3Staff) context.Context {
	return tenant.WithContext(context.Background(), tenant.Context{TenantID: actor.TenantID, Subject: actor.ID.String(), Role: actor.Role})
}

func (w *k3World) target(actor k3Staff) ResolutionTarget {
	tg, err := NewResolutionTarget(tenant.Context{TenantID: actor.TenantID}, w.f.tenantID)
	if err != nil {
		w.t.Fatal(err)
	}
	return tg
}

func (w *k3World) request(actor k3Staff, in ResolutionRequestInput) (ManualResolution, error) {
	return w.svc.Request(k3Ctx(actor), w.target(actor), in, ResolutionMeta{RequestID: "k3-test", IPAddress: "203.0.113.7", UserAgent: "k3-test-agent"})
}

func (w *k3World) decide(actor k3Staff, r ManualResolution, d ResolutionDecision) (ResolutionOutcome, error) {
	return w.svc.Decide(k3Ctx(actor), w.target(actor), r.ID,
		ResolutionDecisionInput{Decision: d, PayloadHash: r.PayloadHash, ReasonCode: "k3-test"},
		ResolutionMeta{RequestID: "k3-test", IPAddress: "203.0.113.7", UserAgent: "k3-test-agent"})
}

// m2In builds an M2 request for an attempt.
func (w *k3World) m2In(attemptID uuid.UUID, kind ResolutionKind) ResolutionRequestInput {
	return ResolutionRequestInput{AttemptID: attemptID, Kind: kind, BasisCode: BasisProviderConfirmedOutOfBand,
		EvidenceRefHash: k3EvidenceHash(), ReasonCode: "k3-test", Note: "k3 M2 test"}
}

func (w *k3World) m1In(attemptID uuid.UUID, finding string) ResolutionRequestInput {
	return ResolutionRequestInput{AttemptID: attemptID, Kind: ResolutionM1DepositEvidence, FindingCode: finding, ReasonCode: "k3-test", Note: "k3 M1 test"}
}

// executeM2 runs request -> approve(s) to execution as finance staff (the
// shortest governed path) and returns the executed resolution.
func (w *k3World) executeM2(attemptID uuid.UUID, kind ResolutionKind) ManualResolution {
	w.t.Helper()
	r, err := w.request(w.f1, w.m2In(attemptID, kind))
	if err != nil {
		w.t.Fatalf("request M2: %v", err)
	}
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil {
		w.t.Fatalf("approve M2: %v", err)
	}
	if !out.Executed {
		w.t.Fatalf("M2 did not execute (counted %d of %d, refused=%v)", out.Counted, out.Required, out.Refused)
	}
	return out.Resolution
}

// --- fixtures ---------------------------------------------------------------

func (w *k3World) tx(fn func(ctx context.Context, tx pgx.Tx) error) {
	w.t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, fn); err != nil {
		w.t.Fatalf("tenant tx: %v", err)
	}
}

// approveWithdrawal drives a fresh withdrawal to `approved` (a single
// automated below-threshold approval, mirroring approvedWithdrawal without
// re-inserting the policy row).
func (w *k3World) approveWithdrawal(amount int64, idemKey string) withdrawal.WithdrawalRequest {
	w.t.Helper()
	var wr withdrawal.WithdrawalRequest
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID,
			WalletID: w.f.walletID, AssetCode: "EUR", Amount: amount, IdempotencyKey: idemKey,
		})
		return err
	})
	w.tx(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.MoveToPendingReview(ctx, tx, wr.ID) })
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		ok, err := withdrawal.Approve(ctx, tx, wr.ID, uuid.New(), true, nil, nil)
		if err == nil && !ok {
			err = fmt.Errorf("a single automated approval did not approve the request")
		}
		return err
	})
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	})
	return wr
}

// payout dispatches a fresh withdrawal through the real claim/dispatch/apply
// path (the mock adapter answers Pending with a reference) and returns the
// withdrawal (submitted) and its PENDING attempt (reference bound).
func (w *k3World) payout(amount int64) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	w.t.Helper()
	w.ensureWithdrawalPolicy()
	wr := w.approveWithdrawal(amount, "k3-"+uuid.NewString())
	claim, err := w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		w.t.Fatalf("ClaimForDispatch: %v", err)
	}
	adapter, _ := w.orch.Provider(claim.Capability.ProviderID)
	gr := DispatchWithdraw(context.Background(), nil, MockCredentialResolver{}, adapter, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		w.t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var a PaymentAttempt
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		wr, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	})
	if a.State != AttemptPending || a.ProviderReference == nil || wr.State != withdrawal.StateSubmitted {
		w.t.Fatalf("setup: want a pending payout attempt with a reference and a submitted withdrawal, got %s %v / %s", a.State, a.ProviderReference, wr.State)
	}
	return wr, a
}

func (w *k3World) ensureWithdrawalPolicy() {
	w.t.Helper()
	var n int
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_policies WHERE tenant_id = $1`, w.f.tenantID).Scan(&n)
	})
	if n == 0 {
		mustSetWithdrawalPolicy(w.t, w.pool, w.f.tenantID, "EUR", 1_000_000, 2)
	}
}

// ambiguousPayout returns a payout attempt moved pending -> ambiguous by the
// real T11 writer.
func (w *k3World) ambiguousPayout(amount int64) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	w.t.Helper()
	wr, a := w.payout(amount)
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return MarkAmbiguousFromPending(ctx, tx, a.ID, EvidenceSweeper, time.Now().Add(time.Hour))
	})
	return wr, w.attempt(a.ID)
}

// disputedPayout returns a payout attempt parked by the real T10 writer with
// the given reason.
func (w *k3World) disputedPayout(amount int64, reason string) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	w.t.Helper()
	wr, a := w.payout(amount)
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, reason)
	})
	return wr, w.attempt(a.ID)
}

// disputedDeposit returns a deposit attempt parked through the real receipt path
// (a verified callback whose amount differs: T10 callback_amount_asset_mismatch,
// which holds the captured reference).
func (w *k3World) disputedDeposit(amount int64) PaymentAttempt {
	w.t.Helper()
	res := rvInit(w.t, w.pool, w.orch, w.f.orchFixture, amount, "k3-dep-"+uuid.NewString())
	if res.Attempt.ProviderReference == nil {
		w.t.Fatalf("setup: deposit attempt has no reference (state %s)", res.Attempt.State)
	}
	if _, err := rvApplyReceipt(w.pool, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{
		EventType: "deposit", ProviderReference: *res.Attempt.ProviderReference,
		Outcome: OutcomeSucceeded, Amount: amount - 1, AssetCode: "EUR",
	}); err != nil {
		w.t.Fatalf("deposit mismatch callback: %v", err)
	}
	a := w.attempt(res.Attempt.ID)
	if a.State != AttemptDisputed {
		w.t.Fatalf("setup: want a disputed deposit attempt, got %s", a.State)
	}
	return a
}

func (w *k3World) attempt(id uuid.UUID) PaymentAttempt {
	w.t.Helper()
	return mustGetAttempt(w.t, w.pool, w.f.tenantID, id)
}

func (w *k3World) withdrawalOf(id uuid.UUID) withdrawal.WithdrawalRequest {
	w.t.Helper()
	var wr withdrawal.WithdrawalRequest
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.GetByID(ctx, tx, id)
		return err
	})
	return wr
}

func (w *k3World) resolution(id uuid.UUID) ManualResolution {
	w.t.Helper()
	var r ManualResolution
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = GetResolutionInTx(ctx, tx, w.f.tenantID, id)
		return err
	}); err != nil {
		w.t.Fatalf("read resolution: %v", err)
	}
	return r
}

// ownerQuery runs a read as the (non-superuser, FORCE-RLS-bound) owner role with
// the tenant GUC only - the system session shape.
func (w *k3World) sysQuery(sql string, args ...any) []map[string]any {
	w.t.Helper()
	var out []map[string]any
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return err
			}
			m := map[string]any{}
			for i, f := range rows.FieldDescriptions() {
				m[f.Name] = vals[i]
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out
}

func (w *k3World) countRows(sql string, args ...any) int {
	w.t.Helper()
	var n int
	w.tx(func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) })
	return n
}

func (w *k3World) ledgerTxCount(txType string) int {
	return w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = $2`, w.f.tenantID, txType)
}

func (w *k3World) auditActions(targetType, targetID string) []string {
	w.t.Helper()
	var out []string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND target_type = $3 ORDER BY created_at, id`,
			w.f.tenantID, targetID, targetType)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out
}

// assertInvariants is the ADR 0101 12 per-test rule: SUM(debits) == SUM(credits)
// per transaction and per tenant, and the projection equal to its recomputation.
func (w *k3World) assertInvariants() {
	w.t.Helper()
	loAssertBalanced(w.t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(w.t, w.pool, w.f.tenantID)
}

// pspClearingBalance is the tenant's psp_clearing credit-positive balance.
func (w *k3World) pspClearing() int64 {
	w.t.Helper()
	var s string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0)::text
			FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
			WHERE a.tenant_id = $1 AND a.account_type = 'psp_clearing' AND a.wallet_id IS NULL`, w.f.tenantID).Scan(&s)
	})
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *k3World) walletBalance(accountType string) int64 {
	w.t.Helper()
	var s string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0)::text
			FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
			WHERE a.tenant_id = $1 AND a.wallet_id = $2 AND a.account_type = $3`, w.f.tenantID, w.f.walletID, accountType).Scan(&s)
	})
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// k3Provider wraps the MockProvider so a test can script a hostile or unusual
// provider answer (a reserved-prefix reference, a different poll echo) without
// touching the mock. With nothing scripted it is the mock.
type k3Provider struct {
	*MockProvider
	mu       sync.Mutex
	deposit  func(req DepositRequest) DepositResult
	withdraw func(req WithdrawRequest) WithdrawResult
	status   map[string]StatusResult
}

func (p *k3Provider) Deposit(ctx context.Context, req DepositRequest) (DepositResult, error) {
	res, err := p.MockProvider.Deposit(ctx, req)
	p.mu.Lock()
	f := p.deposit
	p.mu.Unlock()
	if err != nil || f == nil {
		return res, err
	}
	return f(req), nil
}

func (p *k3Provider) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	res, err := p.MockProvider.Withdraw(ctx, req)
	p.mu.Lock()
	f := p.withdraw
	p.mu.Unlock()
	if err != nil || f == nil {
		return res, err
	}
	return f(req), nil
}

func (p *k3Provider) QueryStatus(ctx context.Context, ref string) (StatusResult, error) {
	p.mu.Lock()
	ov, ok := p.status[ref]
	p.mu.Unlock()
	if ok {
		return ov, nil
	}
	return p.MockProvider.QueryStatus(ctx, ref)
}

func (p *k3Provider) setDeposit(f func(req DepositRequest) DepositResult) {
	p.mu.Lock()
	p.deposit = f
	p.mu.Unlock()
}

func (p *k3Provider) setWithdraw(f func(req WithdrawRequest) WithdrawResult) {
	p.mu.Lock()
	p.withdraw = f
	p.mu.Unlock()
}

func (p *k3Provider) setStatus(ref string, st StatusResult) {
	p.mu.Lock()
	if p.status == nil {
		p.status = map[string]StatusResult{}
	}
	p.status[ref] = st
	p.mu.Unlock()
}
