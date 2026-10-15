//go:build integration

// MANUAL-ADJ-LINK-1 (migration 0153, ADR 0100 section 21): the session-independent manual_adjustment link fence.
//
// Every test here runs against the real schema with the fence ENABLED; none uses the owner-only fixture exemption
// (adjlinkfix) except where a test says so explicitly to isolate one check from the older 0113 controls. The
// sessions are the real ones: the table owner (TEST_DATABASE_URL) and the runtime role (TEST_RUNTIME_DATABASE_URL),
// each with no GUCs, the system-tenant shape (app.tenant_id only), the tenant-principal shape, forged tenant GUCs and
// forged acting GUCs.
package adjustment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// errLF153Rollback is returned from a probe transaction so it always rolls back.
var errLF153Rollback = errors.New("lf153: rollback")

// lf153InsTx inserts one ledger_transactions row directly (below ledger.Post, so the database is the only control).
func lf153InsTx(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, typ, key string, corr uuid.UUID, reason *string,
	causation *uuid.UUID, providerID, providerTx *string, reverses *uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id,
		correlation_id, causation_id, reverses_transaction_id, reason_code) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, tenantID, typ, key, providerID, providerTx, corr, causation, reverses, reason)
	return err
}

// lf153InsEntry inserts one ledger_entries row directly.
func lf153InsEntry(ctx context.Context, tx pgx.Tx, txID, account, tenantID uuid.UUID, dir string, amount int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
		VALUES ($1, $2, $3, (SELECT asset_code FROM ledger_accounts WHERE id = $2), $4, $5)`, txID, account, tenantID, dir, amount)
	return err
}

// lf153Probe runs fn inside a savepoint, rolls the savepoint back, and records a mismatch between fn's SQLSTATE and
// want (want "" = must succeed). msg, when set, must appear in the error text (pins WHICH check fired).
type lf153Prober struct {
	ctx      context.Context
	tx       pgx.Tx
	failures []string
}

func (p *lf153Prober) probe(name, want, msg string, fn func() error) {
	if _, err := p.tx.Exec(p.ctx, `SAVEPOINT lf153`); err != nil {
		p.failures = append(p.failures, name+": savepoint: "+err.Error())
		return
	}
	err := fn()
	got := pgCode(err)
	switch {
	case want == "" && err != nil:
		p.failures = append(p.failures, fmt.Sprintf("%s: want success, got %v", name, err))
	case want != "" && got != want:
		p.failures = append(p.failures, fmt.Sprintf("%s: want %s, got %q (%v)", name, want, got, err))
	case want != "" && msg != "" && !strings.Contains(err.Error(), msg):
		p.failures = append(p.failures, fmt.Sprintf("%s: want %s with %q, got %v", name, want, msg, err))
	}
	if _, err := p.tx.Exec(p.ctx, `ROLLBACK TO SAVEPOINT lf153`); err != nil {
		p.failures = append(p.failures, name+": rollback to savepoint: "+err.Error())
	}
}

// inExecutingTx runs fn in ONE tenant-principal (non-acting) transaction of approver, after a proof-backed final
// approval and the 0113 guarded "-> executing" transition (the DB re-verifies the count there), i.e. exactly the
// state the real executor is in just before ledger.Post. The transaction always rolls back.
func (w *world) inExecutingTx(t *testing.T, r Request, approver staffMember, fn func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error) error {
	t.Helper()
	var fnErr error
	err := w.tenantTx(approver, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'lf153')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		wallet := r.WalletID
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: r.AssetCode},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: r.AssetCode})
		if err != nil {
			return err
		}
		fnErr = fn(ctx, tx, ids[0], ids[1])
		if fnErr != nil {
			return fnErr
		}
		return errLF153Rollback
	})
	if fnErr != nil {
		return fnErr
	}
	if errors.Is(err, errLF153Rollback) {
		return nil
	}
	return err
}

func (w *world) reconLedger(t *testing.T) []reconciliation.Mismatch {
	t.Helper()
	var ms []reconciliation.Mismatch
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, ms, err = reconciliation.RunLedgerVsProjection(ctx, tx, w.Tenant, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil {
		t.Fatalf("recon: %v", err)
	}
	return ms
}

func (w *world) accounts(t *testing.T) (pc, house uuid.UUID) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: w.Asset})
		if err != nil {
			return err
		}
		pc, house = ids[0], ids[1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return pc, house
}

func (w *world) governedTxCount(t *testing.T, requestID uuid.UUID) int {
	t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND
			(correlation_id = $2 OR idempotency_key = $3)`, w.Tenant, requestID, "manual_adjustment:"+requestID.String()).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// (1) The governed path still posts - credit and debit, through the real Service, a tenant (non-acting) session -
// with the posting keys, the audit trail and a clean reconciliation; a replay of the posted key is an idempotent
// no-op (it conflicts on the key BEFORE the AFTER-INSERT fence), a changed replay is a payload mismatch, and a
// second decision on an executed request posts nothing.
func TestLinkFence0153_GovernedPathPostsIdempotentlyAndAudited(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(1_000)

	r, err := w.submit(w.F1, w.credit(250, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed || out.Request.LedgerTransactionID == nil {
		t.Fatalf("governed credit must execute under the fence: %v %+v", err, out)
	}
	w.assertPostingKeys(out.Request)
	if got := w.playerCash(); got != 1_250 {
		t.Fatalf("player_cash after the credit: %d", got)
	}
	actions := strings.Join(w.auditActions(r.ID), ",")
	if !strings.Contains(actions, "ledger_adjustment.submitted") || !strings.Contains(actions, "ledger_adjustment.executed") {
		t.Fatalf("audit trail incomplete: %s", actions)
	}

	// Idempotent replay of the posted key (fence enabled, plain tenant session).
	pc, house := w.accounts(t)
	in := buildPosting(out.Request, pc, house)
	if err := w.tenantTx(w.F2, func(ctx context.Context, tx pgx.Tx) error {
		res, err := ledger.Post(ctx, tx, in)
		if err != nil {
			return err
		}
		if !res.AlreadyPosted || res.TransactionID != *out.Request.LedgerTransactionID {
			return fmt.Errorf("replay: want AlreadyPosted of %s, got %+v", *out.Request.LedgerTransactionID, res)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed := in
	changed.Entries = []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 251}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: 251}}
	if err := w.tenantTx(w.F2, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, changed)
		return err
	}); !errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
		t.Fatalf("a changed replay of the governed key must be a payload mismatch, got %v", err)
	}
	if _, err := w.decide(w.F3, out.Request, DecisionApprove); err == nil {
		t.Fatal("a decision on an executed request was accepted")
	}

	d, err := w.submit(w.F1, w.debit(100, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	dout, err := w.decide(w.F3, d, DecisionApprove)
	if err != nil || !dout.Executed {
		t.Fatalf("governed debit must execute under the fence: %v %+v", err, dout)
	}
	w.assertPostingKeys(dout.Request)
	if got := w.playerCash(); got != 1_150 {
		t.Fatalf("player_cash after credit and debit: %d", got)
	}
	if got := w.adjustmentLedgerTxCount(); got != 2 {
		t.Fatalf("want exactly 2 manual_adjustment transactions, got %d", got)
	}
	if ms := w.reconLedger(t); len(ms) != 0 {
		t.Fatalf("governed adjustments must reconcile clean: %+v", ms)
	}
	w.assertInvariants()
}

// (2) Concurrency: two final approvals of one request race; it executes exactly once.
func TestLinkFence0153_ConcurrentFinalApprovalsPostOnce(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(77, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outs := make([]Outcome, 2)
	errs := make([]error, 2)
	for i, f := range []staffMember{w.F2, w.F3} {
		wg.Add(1)
		go func(i int, f staffMember) {
			defer wg.Done()
			outs[i], errs[i] = w.decide(f, r, DecisionApprove)
		}(i, f)
	}
	wg.Wait()
	executed := 0
	for i := range outs {
		if errs[i] == nil && outs[i].Executed {
			executed++
		}
	}
	if executed != 1 {
		t.Fatalf("want exactly one execution, got %d (errs %v)", executed, errs)
	}
	if n := w.governedTxCount(t, r.ID); n != 1 {
		t.Fatalf("want exactly one governed posting, got %d", n)
	}
	if got := w.playerCash(); got != 77 {
		t.Fatalf("player_cash: %d", got)
	}
	w.assertInvariants()
}

// (3) Partial failure: a failure after '-> executing' and before the posting rolls the whole decision back (nothing
// posted, request pending, no executed audit); the retry then posts exactly once.
func TestLinkFence0153_PartialFailureRollsBackAndRetryPostsOnce(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(60, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("lf153: injected failure before the posting")
	testHookBeforePost = func(context.Context, uuid.UUID) error { return boom }
	_, err = w.decide(w.F2, r, DecisionApprove)
	testHookBeforePost = nil
	if !errors.Is(err, boom) {
		t.Fatalf("want the injected failure, got %v", err)
	}
	if got := w.request(r.ID); got.State != StatePending || got.LedgerTransactionID != nil {
		t.Fatalf("the failed decision left state behind: %+v", got)
	}
	if n := w.governedTxCount(t, r.ID); n != 0 {
		t.Fatalf("the failed decision posted %d transaction(s)", n)
	}
	if strings.Contains(strings.Join(w.auditActions(r.ID), ","), "ledger_adjustment.executed") {
		t.Fatal("an executed audit row survived the rollback")
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("retry: %v %+v", err, out)
	}
	if _, err := w.decide(w.F3, r, DecisionApprove); err == nil {
		t.Fatal("a second retry was accepted after execution")
	}
	if n := w.governedTxCount(t, r.ID); n != 1 {
		t.Fatalf("want exactly one governed posting after the retry, got %d", n)
	}
	w.assertInvariants()
}

// (4) Reversal: a compensating_entry (the only correction path - a compensating entry, never an edit) of an executed
// credit posts under the fence and nets the player back; over-compensation is refused.
func TestLinkFence0153_CompensatingEntryReversesExecutedCredit(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(400, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("credit: %v", err)
	}
	comp := w.debit(400, ReasonCompensatingEntry)
	comp.CausationTransactionID = out.Request.LedgerTransactionID
	c, err := w.submit(w.F1, comp)
	if err != nil {
		t.Fatalf("compensating submit: %v", err)
	}
	cout, err := w.decide(w.F3, c, DecisionApprove)
	if err != nil || !cout.Executed {
		t.Fatalf("compensating entry must execute under the fence: %v %+v", err, cout)
	}
	w.assertPostingKeys(cout.Request)
	if got := w.playerCash(); got != 0 {
		t.Fatalf("player_cash after the compensating entry: %d", got)
	}
	over := w.debit(1, ReasonCompensatingEntry)
	over.CausationTransactionID = out.Request.LedgerTransactionID
	if _, err := w.submit(w.F1, over); pgCode(err) != "MA022" {
		t.Fatalf("over-compensation: want MA022, got %v", err)
	}
	w.assertInvariants()
}

// (5) An unlinked manual_adjustment is refused for the RUNTIME role and for the TABLE OWNER alike, in every
// non-acting session shape, through ledger.Post and through a raw INSERT. With no GUCs at all the ledger RLS
// refuses first (42501); every shape that RLS admits is refused by the fence (MA050).
func TestLinkFence0153_UnlinkedInsertRefusedForRuntimeAndOwner(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	rt := tempRevokeRuntimePool(t)
	pc, house := w.accounts(t)
	reason := "unlinked"
	for _, role := range []struct {
		name string
		pool *db.Pool
	}{{"owner", w.pool}, {"runtime", rt}} {
		post := func(ctx context.Context, tx pgx.Tx) error {
			id := uuid.New()
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
				IdempotencyKey: "manual_adjustment:" + id.String(), CorrelationID: id, ReasonCode: &reason,
				Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 3}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: 3}}})
			return err
		}
		raw := func(ctx context.Context, tx pgx.Tx) error {
			id := uuid.New()
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", "manual_adjustment:"+id.String(), id, &reason, nil, nil, nil, nil)
		}
		for _, shape := range []struct {
			name string
			want string
			run  func(fn func(ctx context.Context, tx pgx.Tx) error) error
		}{
			{"no GUCs", "42501", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
				return role.pool.WithoutTenant(context.Background(), fn)
			}},
			{"system tenant (app.tenant_id only)", "MA050", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
				return role.pool.WithTenant(context.Background(), w.Tenant, fn)
			}},
			{"tenant principal", "MA050", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
				return role.pool.WithPrincipalScope(context.Background(), w.Tenant, w.F2.ID, fn)
			}},
		} {
			for _, v := range []struct {
				name string
				fn   func(ctx context.Context, tx pgx.Tx) error
			}{{"ledger.Post", post}, {"raw INSERT", raw}} {
				err := shape.run(v.fn)
				if shape.want == "42501" && v.name == "ledger.Post" {
					// With no GUCs, ledger.Post cannot even see the accounts it must lock (RLS), so it may fail
					// before its INSERT with a non-SQLSTATE error: any refusal is correct; the raw INSERT pins 42501.
					if err == nil {
						t.Errorf("%s / %s / %s: want a refusal, got success", role.name, shape.name, v.name)
					}
					continue
				}
				if pgCode(err) != shape.want {
					t.Errorf("%s / %s / %s: want %s, got %v", role.name, shape.name, v.name, shape.want, err)
				} else if shape.want == "MA050" && !strings.Contains(err.Error(), "is not linked to a governed adjustment request") {
					t.Errorf("%s / %s / %s: MA050 from the wrong check: %v", role.name, shape.name, v.name, err)
				}
			}
		}
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("an unlinked posting went through: %d", got)
	}
	w.assertInvariants()
}

// (6) Forged GUCs with no matching approved request: a runtime session claiming a real tenant principal (no actor
// proof) cannot post for a random correlation, cannot post for a PENDING request that already holds a committed
// approval, cannot move that request to 'executing' itself, and cannot append an entry to an executed adjustment;
// forged ACTING GUCs are refused as well.
func TestLinkFence0153_ForgedGUCsWithoutApprovedRequestRefused(t *testing.T) {
	w := newWorld(t, worldOpts{base: 2})
	rt := tempRevokeRuntimePool(t)
	pc, house := w.accounts(t)

	// A pending request with ONE committed approval (counted 1 < required 2).
	pend, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := w.decide(w.F2, pend, DecisionApprove); err != nil || out.Executed {
		t.Fatalf("first of two approvals: %v %+v", err, out)
	}
	// An executed request (both approvals).
	done, err := w.submit(w.F1, w.credit(40, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.decide(w.F2, done, DecisionApprove); err != nil {
		t.Fatal(err)
	}
	dout, err := w.decide(w.F3, done, DecisionApprove)
	if err != nil || !dout.Executed {
		t.Fatalf("executed request: %v %+v", err, dout)
	}
	cashBefore := w.playerCash()

	forged := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			for k, v := range map[string]string{"app.tenant_id": w.Tenant.String(), "app.principal_id": w.F3.ID.String()} {
				if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
					return err
				}
			}
			return fn(ctx, tx)
		})
	}
	reason := string(pend.ReasonCode)
	cases := []struct {
		name, want string
		fn         func(ctx context.Context, tx pgx.Tx) error
	}{
		{"random correlation", "MA050", func(ctx context.Context, tx pgx.Tx) error {
			id := uuid.New()
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", "manual_adjustment:"+id.String(), id, &reason, nil, nil, nil, nil)
		}},
		{"pending request with a committed approval", "MA050", func(ctx context.Context, tx pgx.Tx) error {
			_, err := ledger.Post(ctx, tx, buildPosting(pend, pc, house))
			return err
		}},
		{"move the pending request to executing", "MA030", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, pend.ID)
			return err
		}},
		{"append an entry to an executed adjustment", "MA051", func(ctx context.Context, tx pgx.Tx) error {
			return lf153InsEntry(ctx, tx, *dout.Request.LedgerTransactionID, pc, w.Tenant, "credit", 1)
		}},
	}
	for _, c := range cases {
		if err := forged(c.fn); pgCode(err) != c.want {
			t.Errorf("forged tenant GUCs / %s: want %s, got %v", c.name, c.want, err)
		}
	}
	// Forged ACTING GUCs (a real acting principal, no proof): refused before or by the ledger fences.
	err = rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for k, v := range map[string]string{"app.acting_tenant_id": w.Tenant.String(), "app.acting_platform_principal_id": w.Acting.ID.String()} {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
				return err
			}
		}
		_, err := ledger.Post(ctx, tx, buildPosting(pend, pc, house))
		return err
	})
	if c := pgCode(err); c != "CG030" && c != "MA050" && c != "42501" {
		t.Fatalf("forged acting GUCs: want a refusal (CG030 / MA050 / 42501), got %v", err)
	}
	if got := w.request(pend.ID); got.State != StatePending {
		t.Fatalf("pending request moved: %s", got.State)
	}
	if got := w.playerCash(); got != cashBefore {
		t.Fatalf("player_cash moved: %d -> %d", cashBefore, got)
	}
	if n := w.governedTxCount(t, pend.ID); n != 0 {
		t.Fatalf("the pending request has %d posting(s)", n)
	}
	w.assertInvariants()
}

// (7) Inside the executing transaction (the real executor's state, non-acting tenant session) the fence admits
// ONLY the approved posting: wrong amount, asset, direction, wallet, counter account, a duplicate leg, wrong keys,
// a foreign or pending request's key - each refused, each pinned to its check.
func TestLinkFence0153_ExecutingTxAdmitsOnlyTheApprovedShape(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(321, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	other, err := w.submit(w.F1, w.credit(321, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	_, _, otherWallet := w.newPlayer()
	reason := string(r.ReasonCode)
	wrongReason := "goodwill_credit"
	key := "manual_adjustment:" + r.ID.String()
	pid, ptx := "lf153", "p-"+uuid.NewString()
	var failures []string
	err = w.inExecutingTx(t, r, w.F2, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
		hg, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, nil, ledger.AccountHouseGaming, w.Asset)
		if err != nil {
			return err
		}
		eurHouse, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		ow := otherWallet
		otherPC, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, &ow, ledger.AccountPlayerCash, w.Asset)
		if err != nil {
			return err
		}
		rw := r.WalletID
		lockedCash, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, &rw, ledger.AccountPlayerLockedCash, w.Asset)
		if err != nil {
			return err
		}
		walletScopedAdj, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, &rw, ledger.AccountManualAdjustment, w.Asset)
		if err != nil {
			return err
		}
		causation := uuid.New()
		p := &lf153Prober{ctx: ctx, tx: tx}
		goodTx := func(id uuid.UUID) error {
			return lf153InsTx(ctx, tx, w.Tenant, id, "manual_adjustment", key, r.ID, &reason, nil, nil, nil, nil)
		}
		// Transaction-row checks (MA050).
		p.probe("wrong idempotency key", "MA050", "governed key", func() error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", key+"x", r.ID, &reason, nil, nil, nil, nil)
		})
		p.probe("wrong reason_code", "MA050", "keys do not match", func() error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", key, r.ID, &wrongReason, nil, nil, nil, nil)
		})
		p.probe("causation set", "MA050", "keys do not match", func() error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", key, r.ID, &reason, &causation, nil, nil, nil)
		})
		p.probe("provider reference set", "MA050", "keys do not match", func() error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", key, r.ID, &reason, nil, &pid, &ptx, nil)
		})
		p.probe("reverses set", "MA050", "keys do not match", func() error {
			fund := w.fundIn(ctx, tx)
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", key, r.ID, &reason, nil, nil, nil, &fund)
		})
		p.probe("another (pending) request's key", "MA050", "not executing", func() error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", "manual_adjustment:"+other.ID.String(), other.ID, &reason, nil, nil, nil, nil)
		})
		p.probe("after a refused_insufficient_funds exit in this transaction", "MA050", "not executing", func() error {
			if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_insufficient_funds' WHERE id = $1`, r.ID); err != nil {
				return err
			}
			return goodTx(uuid.New())
		})
		// The entry fence on its own (the transaction fence switched off for the probe by the owner, inside the
		// savepoint): an entry on a manual_adjustment row that is not the executing request's posting is refused.
		txFenceOff := func() error {
			_, err := tx.Exec(ctx, `ALTER TABLE ledger_transactions DISABLE TRIGGER ledger_transactions_manual_adjustment_link`)
			return err
		}
		p.probe("entry fence alone: wrong key on the parent", "MA051", "no request executing", func() error {
			if err := txFenceOff(); err != nil {
				return err
			}
			id := uuid.New()
			if err := lf153InsTx(ctx, tx, w.Tenant, id, "manual_adjustment", key+"x", r.ID, &reason, nil, nil, nil, nil); err != nil {
				return err
			}
			return lf153InsEntry(ctx, tx, id, pc, w.Tenant, "credit", 321)
		})
		p.probe("entry fence alone: request already exited (refused) in this transaction", "MA051", "no request executing", func() error {
			if err := txFenceOff(); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_insufficient_funds' WHERE id = $1`, r.ID); err != nil {
				return err
			}
			id := uuid.New()
			if err := goodTx(id); err != nil {
				return err
			}
			return lf153InsEntry(ctx, tx, id, pc, w.Tenant, "credit", 321)
		})
		// Entry checks (MA051), each on a correct transaction row.
		for _, e := range []struct {
			name    string
			account uuid.UUID
			dir     string
			amount  int64
		}{
			{"wrong amount", pc, "credit", 322},
			{"wrong asset (house in EUR)", eurHouse, "debit", 321},
			{"wrong direction (player debited on a credit)", pc, "debit", 321},
			{"another wallet's player_cash", otherPC, "credit", 321},
			{"house_gaming as the counter account", hg, "debit", 321},
			{"house credited on a credit", house, "credit", 321},
			{"the request wallet's player_locked_cash as the player leg", lockedCash, "credit", 321},
			{"a wallet-scoped manual_adjustment account as the house leg", walletScopedAdj, "debit", 321},
		} {
			e := e
			p.probe(e.name, "MA051", "not a leg", func() error {
				id := uuid.New()
				if err := goodTx(id); err != nil {
					return err
				}
				return lf153InsEntry(ctx, tx, id, e.account, w.Tenant, e.dir, e.amount)
			})
		}
		p.probe("duplicate player leg", "MA051", "already holds", func() error {
			id := uuid.New()
			if err := goodTx(id); err != nil {
				return err
			}
			if err := lf153InsEntry(ctx, tx, id, pc, w.Tenant, "credit", 321); err != nil {
				return err
			}
			return lf153InsEntry(ctx, tx, id, pc, w.Tenant, "credit", 321)
		})
		p.probe("duplicate leg in ONE statement", "MA051", "already holds", func() error {
			id := uuid.New()
			if err := goodTx(id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
				VALUES ($1, $2, $3, $4, 'credit', 321), ($1, $2, $3, $4, 'credit', 321)`, id, pc, w.Tenant, w.Asset)
			return err
		})
		// Control: the approved shape passes (raw and through ledger.Post).
		p.probe("approved shape (raw)", "", "", func() error {
			id := uuid.New()
			if err := goodTx(id); err != nil {
				return err
			}
			if err := lf153InsEntry(ctx, tx, id, house, w.Tenant, "debit", 321); err != nil {
				return err
			}
			return lf153InsEntry(ctx, tx, id, pc, w.Tenant, "credit", 321)
		})
		p.probe("approved shape (ledger.Post)", "", "", func() error {
			_, err := ledger.Post(ctx, tx, buildPosting(r, pc, house))
			return err
		})
		failures = p.failures
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) > 0 {
		t.Fatalf("fence probes failed:\n  %s", strings.Join(failures, "\n  "))
	}
	if n := w.governedTxCount(t, r.ID); n != 0 {
		t.Fatalf("a probe leaked a posting: %d", n)
	}
	w.assertInvariants()
}

// fundIn posts a casino_win inside tx and returns its id (a reversible original for the "reverses set" probe).
func (w *world) fundIn(ctx context.Context, tx pgx.Tx) uuid.UUID {
	wallet := w.Wallet
	ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
		ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: w.Asset})
	if err != nil {
		return uuid.Nil
	}
	pid, ptx := "lf153-casino", uuid.NewString()
	res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxCasinoWin, IdempotencyKey: "lf153:" + ptx,
		ProviderID: &pid, ProviderTxID: &ptx, CorrelationID: uuid.New(),
		Entries: []ledger.EntryInput{{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: 1}, {LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: 1}}})
	if err != nil {
		return uuid.Nil
	}
	return res.TransactionID
}

// (8) The fence's state, payload and count checks hold on their own: with the 0113 request guard disabled in-tx by
// the owner (so the request row can be put into states the guard would never allow), a posting is still refused when
// the request is executing without approvals, executing under another txid, or carries a tampered payload.
func TestLinkFence0153_ChecksHoldWithoutTheRequestGuard(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	const executing = `state = 'executing', executed_txid = txid_current()`
	for _, c := range []struct {
		name, msg, set, want string
		approve, txFenceOff  bool
		before               func(t *testing.T, w *world) *world
	}{
		{name: "executing without counted approvals", msg: "is not approved", set: executing, want: "MA050"},
		{name: "executing under another transaction", msg: "not executing in this transaction", set: `state = 'executing', executed_txid = txid_current() + 1000`, want: "MA050"},
		{name: "tampered payload", msg: "payload_hash", set: executing + `, note_hash = repeat('0', 64)`, want: "MA050"},
		{name: "pending", msg: "not executing in this transaction", set: `note_hash = note_hash`, want: "MA050"},
		{name: "approved, but the policy was disabled after submission", msg: "is not approved", set: executing, want: "MA050", approve: true,
			before: func(t *testing.T, w *world) *world {
				jur := w.newJurisdiction() // a jurisdiction row + a tenant with no resolvable jurisdiction disables (0113)
				w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelJurisdiction,
					JurisdictionID: &jur, AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(1)}, w.AdminA, w.AdminB)
				return w
			}},
		{name: "approved, but the initiator no longer qualifies", msg: "is not approved", set: executing, want: "MA050", approve: true,
			before: func(t *testing.T, w *world) *world {
				w.revokeGrant(w.F1.ID, capability.CapabilityLedgerAdjustmentInitiate)
				return w
			}},
		{name: "entry fence alone: executing under another transaction", msg: "no request executing", set: `state = 'executing', executed_txid = txid_current() + 1000`,
			want: "MA051", txFenceOff: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := w
			if c.before != nil {
				w = newWorld(t, worldOpts{base: 1})
			}
			r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatal(err)
			}
			if c.before != nil {
				w = c.before(t, w)
			}
			pc, house := w.accounts(t)
			var postErr error
			_ = w.tenantTx(w.F2, func(ctx context.Context, tx pgx.Tx) error {
				fail := func(err error) error { postErr = err; return err }
				if _, err := tx.Exec(ctx, `ALTER TABLE ledger_adjustment_requests DISABLE TRIGGER ledger_adjustment_requests_guard`); err != nil {
					return fail(err)
				}
				if c.txFenceOff {
					if _, err := tx.Exec(ctx, `ALTER TABLE ledger_transactions DISABLE TRIGGER ledger_transactions_manual_adjustment_link`); err != nil {
						return fail(err)
					}
				}
				if c.approve {
					if err := prooftest.AttachForSession(ctx, tx, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash); err != nil {
						return fail(err)
					}
					if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
						VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'lf153')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
						return fail(err)
					}
				}
				if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET `+c.set+` WHERE id = $1`, r.ID); err != nil {
					return fail(err)
				}
				_, postErr = ledger.Post(ctx, tx, buildPosting(r, pc, house))
				if postErr == nil {
					return errLF153Rollback
				}
				return postErr
			})
			if pgCode(postErr) != c.want || !strings.Contains(postErr.Error(), c.msg) {
				t.Fatalf("want %s (%q), got %v", c.want, c.msg, postErr)
			}
		})
	}
	var enabled string
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(tgenabled::text, '' ORDER BY tgname) FROM pg_trigger
			WHERE tgname IN ('ledger_adjustment_requests_guard', 'ledger_transactions_manual_adjustment_link')`).Scan(&enabled)
	}); err != nil || enabled != "OO" {
		t.Fatalf("the guard and the fence must be enabled again after the rolled-back probes: %q %v", enabled, err)
	}
	w.assertInvariants()
}

// (9) An executed request can never post again: the same key conflicts (nothing written), any other key is refused,
// and no entry can be appended to its posting - not by the tenant session, not by the owner's system-tenant shape.
func TestLinkFence0153_ExecutedRequestCannotPostAgain(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(90, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatal(err)
	}
	pc, house := w.accounts(t)
	reason := string(r.ReasonCode)
	posted := *out.Request.LedgerTransactionID
	for _, c := range []struct {
		name, want string
		fn         func(ctx context.Context, tx pgx.Tx) error
	}{
		{"same key again", "23505", func(ctx context.Context, tx pgx.Tx) error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", "manual_adjustment:"+r.ID.String(), r.ID, &reason, nil, nil, nil, nil)
		}},
		{"another key for the request", "MA050", func(ctx context.Context, tx pgx.Tx) error {
			return lf153InsTx(ctx, tx, w.Tenant, uuid.New(), "manual_adjustment", "manual_adjustment:"+r.ID.String()+":2", r.ID, &reason, nil, nil, nil, nil)
		}},
		{"append the house leg again", "MA051", func(ctx context.Context, tx pgx.Tx) error {
			return lf153InsEntry(ctx, tx, posted, house, w.Tenant, "debit", 90)
		}},
		{"append a balanced pair", "MA051", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
				VALUES ($1, $2, $4, $5, 'debit', 90), ($1, $3, $4, $5, 'credit', 90)`, posted, house, pc, w.Tenant, w.Asset)
			return err
		}},
	} {
		if err := w.tenantTx(w.F2, c.fn); pgCode(err) != c.want {
			t.Errorf("tenant session / %s: want %s, got %v", c.name, c.want, err)
		}
		if err := w.pool.WithTenant(context.Background(), w.Tenant, c.fn); pgCode(err) != c.want {
			t.Errorf("owner system-tenant session / %s: want %s, got %v", c.name, c.want, err)
		}
	}
	if n := w.governedTxCount(t, r.ID); n != 1 {
		t.Fatalf("want exactly one posting, got %d", n)
	}
	if got := w.playerCash(); got != 90 {
		t.Fatalf("player_cash: %d", got)
	}
	w.assertInvariants()
}

// (10) Tenant isolation: another tenant's session can neither post for this tenant's executing request (invisible
// to it: MA050) nor write into this tenant (RLS: 42501), and an entry cannot cross tenants.
func TestLinkFence0153_TenantIsolation(t *testing.T) {
	a := newWorld(t, worldOpts{base: 1})
	b := newWorld(t, worldOpts{base: 1})
	r, err := a.submit(a.F1, a.credit(55, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	reason := string(r.ReasonCode)
	key := "manual_adjustment:" + r.ID.String()
	// B's session, B's tenant id, A's request as the claimed link.
	if err := b.tenantTx(b.F2, func(ctx context.Context, tx pgx.Tx) error {
		return lf153InsTx(ctx, tx, b.Tenant, uuid.New(), "manual_adjustment", key, r.ID, &reason, nil, nil, nil, nil)
	}); pgCode(err) != "MA050" {
		t.Fatalf("B posting for A's request: want MA050, got %v", err)
	}
	// B's session writing into A's tenant.
	if err := b.tenantTx(b.F2, func(ctx context.Context, tx pgx.Tx) error {
		return lf153InsTx(ctx, tx, a.Tenant, uuid.New(), "manual_adjustment", key, r.ID, &reason, nil, nil, nil, nil)
	}); pgCode(err) != "42501" {
		t.Fatalf("B writing into A: want 42501, got %v", err)
	}
	// Inside A's executing transaction: A's approved transaction row, then an entry tagged with B's tenant.
	bpc, _ := b.accounts(t)
	var crossErr error
	if err := a.inExecutingTx(t, r, a.F2, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
		id := uuid.New()
		if err := lf153InsTx(ctx, tx, a.Tenant, id, "manual_adjustment", key, r.ID, &reason, nil, nil, nil, nil); err != nil {
			return err
		}
		crossErr = lf153InsEntry(ctx, tx, id, bpc, b.Tenant, "credit", 55)
		return nil
	}); err != nil && crossErr == nil {
		t.Fatal(err)
	}
	// Refused by whichever layer sees it first: the entry-populate trigger (the foreign account is invisible: P0001),
	// RLS (42501), the FK (23503) or the 0153 entry fence (MA051).
	if c := pgCode(crossErr); c != "MA051" && c != "42501" && c != "23503" && c != "P0001" {
		t.Fatalf("cross-tenant entry: want a refusal, got %v", crossErr)
	}
	if n := a.governedTxCount(t, r.ID); n != 0 {
		t.Fatalf("A has %d posting(s)", n)
	}
	a.assertInvariants()
	b.assertInvariants()
}

// Catalog pins: both fence triggers exist, are enabled, are NOT DEFERRABLE AFTER INSERT row constraint triggers
// (the transaction one keyed on the type), and their functions are invoker-rights with a pinned search_path.
func TestLinkFence0153_CatalogPins(t *testing.T) {
	pool := testPool(t)
	type row struct {
		def, enabled, fn, config string
		deferrable, secdef       bool
	}
	got := map[string]row{}
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.tgname, pg_get_triggerdef(t.oid), t.tgenabled::text, p.proname,
				COALESCE(array_to_string(p.proconfig, ';'), ''), c.condeferrable, p.prosecdef
			FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid JOIN pg_constraint c ON c.oid = t.tgconstraint
			WHERE t.tgname IN ('ledger_transactions_manual_adjustment_link', 'ledger_entries_manual_adjustment_link')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var r row
			if err := rows.Scan(&name, &r.def, &r.enabled, &r.fn, &r.config, &r.deferrable, &r.secdef); err != nil {
				return err
			}
			got[name] = r
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want both 0153 constraint triggers, got %v", got)
	}
	for name, want := range map[string]struct{ fn, def string }{
		"ledger_transactions_manual_adjustment_link": {"ledger_manual_adjustment_link_check",
			"AFTER INSERT ON public.ledger_transactions NOT DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW WHEN ((new.transaction_type = 'manual_adjustment'::text))"},
		"ledger_entries_manual_adjustment_link": {"ledger_manual_adjustment_entry_check",
			"AFTER INSERT ON public.ledger_entries NOT DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE"},
	} {
		r := got[name]
		if r.enabled != "O" || r.deferrable || r.secdef || r.fn != want.fn || !strings.Contains(r.def, want.def) ||
			r.config != "search_path=pg_catalog, public, pg_temp" {
			t.Errorf("%s: unexpected catalog state %+v", name, r)
		}
	}
}

// adjlinkfix.Without restores the ledger tables' deferrable constraints with SET CONSTRAINTS ... DEFERRED, which is
// the default mode only while every one of them is INITIALLY DEFERRED. Pin that.
func TestAdjLinkFix_LedgerDeferrableConstraintsInitiallyDeferred(t *testing.T) {
	pool := testPool(t)
	var bad []string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT conname FROM pg_constraint
			WHERE conrelid IN ('ledger_transactions'::regclass, 'ledger_entries'::regclass) AND condeferrable AND NOT condeferred`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			bad = append(bad, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("deferrable but INITIALLY IMMEDIATE ledger constraints break adjlinkfix's restore: %v", bad)
	}
}
