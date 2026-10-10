//go:build integration

// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 section 6): the DATABASE-level controls of
// migration 0124 - signed actor proof coverage (eleven governed tables), the CT-R3
// key/correlation/shape guards, the deferred two-leg check, the approved-hold
// freeze, the kill-switch non-interaction, grants and the migration round trip.
// Runtime role, private scratch databases, synthetic data.
package payments

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func hsrRawRequest(ctx context.Context, tx pgx.Tx, h *hsr, wrID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO withdrawal_hold_resolutions
			(tenant_id, withdrawal_request_id, kind, brand_id, player_account_id, wallet_id, amount, asset_code,
			 withdrawal_state_at_submission, tenant_status_at_submission, brand_status_at_submission, reason_code, evidence_ref_hash,
			 payload_hash, requested_by, requested_by_scope, requested_by_person_id, required_at_submission, contributing_policy_ids, expires_at)
		VALUES ($1, $2, 'release_hold_to_player', $3, $3, $3, 1, '-', '-', '-', '-', $4, $5, '-', $3, 'platform_acting', $3, 1, '{}', now())`,
		h.f.tenantID, wrID, uuid.Nil, hsrReason, k3EvidenceHash())
	return err
}

func hsrRawApprove(ctx context.Context, tx pgx.Tx, h *hsr, r HoldResolution, decision string) error {
	_, err := tx.Exec(ctx, `INSERT INTO withdrawal_hold_resolution_approvals
		(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'platform_acting', $5, 0, 'attack_test')`, h.f.tenantID, r.ID, decision, r.PayloadHash, uuid.Nil)
	return err
}

func hsrRequestDigest(h *hsr, wrID uuid.UUID) string {
	return actorproof.Digest(actorproof.S(h.f.tenantID.String()), actorproof.S(wrID.String()), actorproof.S(string(HoldReleaseToPlayer)),
		actorproof.S(k3EvidenceHash()), actorproof.S(hsrReason))
}

// attackAs is arbitrary SQL as the runtime role in an ACTING session of actor, with
// whatever proof the attacker chooses (empty = none).
func (h *hsr) attackAs(actor k3Staff, proof string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return h.acting(actor, func(ctx context.Context, tx pgx.Tx) error {
		if proof != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
}

// SIGNED-ACTOR-PROOF for approve/reject/request: GUC impersonation of eligible platform
// staff fails closed at every binding; the exactly-bound proof is accepted (control).
func TestHSEC_HoldRelease_ActorProof_Matrix_FailsClosed(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(100)
	wr2 := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	r2 := h.mustRequest(h.reqA, wr2.ID)
	tid := h.f.tenantID
	f := h.forger
	nowU := time.Now().Unix()
	const op = "withdrawal_hold_resolution:approve"
	const scope = "platform_acting"

	badKey := make([]byte, 32)
	_, _ = rand.Read(badKey)
	cases := []struct {
		name  string
		proof string
		want  string
	}{
		{"no proof (GUC impersonation only)", "", "AP001"},
		{"garbage proof", "x", "AP001"},
		{"wrong signing key", prooftest.Forger{KID: f.KID, Key: badKey}.Valid(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash), "AP002"},
		{"unknown kid", prooftest.Forger{KID: "nokid", Key: f.Key}.Valid(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash), "AP002"},
		{"stale (expired)", f.Token(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash, nowU-120, nowU-60), "AP003"},
		{"future", f.Token(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash, nowU+600, nowU+630), "AP003"},
		{"lifetime > 60s", f.Token(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash, nowU, nowU+3600), "AP003"},
		{"different operation (reject for approve)", f.Valid(t, h.apprB.ID, scope, tid, "withdrawal_hold_resolution:reject", r.ID.String(), r.PayloadHash), "AP004"},
		{"different operation (K3 force-resolve)", f.Valid(t, h.apprB.ID, scope, tid, "payment_force_resolve:approve", r.ID.String(), r.PayloadHash), "AP004"},
		{"different target", f.Valid(t, h.apprB.ID, scope, tid, op, r2.ID.String(), r.PayloadHash), "AP004"},
		{"different actor", f.Valid(t, h.apprC.ID, scope, tid, op, r.ID.String(), r.PayloadHash), "AP004"},
		{"different tenant", f.Valid(t, h.apprB.ID, scope, uuid.New(), op, r.ID.String(), r.PayloadHash), "AP004"},
		{"tenant scope", f.Valid(t, h.apprB.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), "AP004"},
		{"platform scope, no tenant", f.Valid(t, h.apprB.ID, "platform", uuid.Nil, op, r.ID.String(), r.PayloadHash), "AP004"},
		{"different payload", f.Valid(t, h.apprB.ID, scope, tid, op, r.ID.String(), strings.Repeat("0", 64)), "AP004"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := h.attackAs(h.apprB, c.proof, func(ctx context.Context, tx pgx.Tx) error { return hsrRawApprove(ctx, tx, h, r, "approve") })
			if err == nil {
				t.Fatal("the approval was ACCEPTED without a valid proof for exactly this write")
			}
			k3RequireCode(t, err, c.want)
		})
	}
	t.Run("replayed nonce", func(t *testing.T) {
		tok := f.Valid(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash)
		consume := func() error {
			return h.attackAs(h.apprB, tok, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT actor_proof_require($1, 'platform_acting', $2, $3, $4, $5)`, h.apprB.ID, tid, op, r.ID.String(), r.PayloadHash)
				return err
			})
		}
		if err := consume(); err != nil {
			t.Fatalf("first consumption: %v", err)
		}
		err := h.attackAs(h.apprB, tok, func(ctx context.Context, tx pgx.Tx) error { return hsrRawApprove(ctx, tx, h, r, "approve") })
		k3RequireCode(t, err, "AP005")
		k3RequireCode(t, consume(), "AP005")
	})

	if got := h.rowsApprovals(r.ID); got != 0 {
		t.Fatalf("approval rows exist after the attack matrix: %d", got)
	}
	if h.res(r.ID).State != ResolutionPending {
		t.Fatalf("resolution %s", h.res(r.ID).State)
	}

	// CONTROL (non-vacuity): the exactly-bound proof is accepted.
	if err := h.attackAs(h.apprB, f.Valid(t, h.apprB.ID, scope, tid, op, r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
		return hsrRawApprove(ctx, tx, h, r, "approve")
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if got := h.rowsApprovals(r.ID); got != 1 {
		t.Fatalf("control: want 1 approval, got %d", got)
	}
	if h.res(r.ID).State != ResolutionPending {
		t.Fatal("control: required=2, one approval must leave the resolution pending")
	}
	h.assertInvariants()
}

func (h *hsr) rowsApprovals(id uuid.UUID) int {
	h.t.Helper()
	var n int
	if err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_hold_resolution_approvals WHERE resolution_id = $1`, id).Scan(&n)
	}); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// Request and cancel are proof-bound too; the request digest binds the caller-supplied columns.
func TestHSEC_HoldRelease_ActorProof_Request_And_Cancel(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	other := h.hold(101)
	h.suspendTenant()
	tid := h.f.tenantID
	f := h.forger
	const scope = "platform_acting"
	digest := hsrRequestDigest(h, wr.ID)

	for _, c := range []struct {
		name  string
		proof string
		want  string
	}{
		{"no proof", "", "AP001"},
		{"digest of another withdrawal", f.Valid(t, h.reqA.ID, scope, tid, "withdrawal_hold_resolution:request", "new", hsrRequestDigest(h, other.ID)), "AP004"},
		{"target is not 'new'", f.Valid(t, h.reqA.ID, scope, tid, "withdrawal_hold_resolution:request", uuid.NewString(), digest), "AP004"},
		{"approve op for a request", f.Valid(t, h.reqA.ID, scope, tid, "withdrawal_hold_resolution:approve", "new", digest), "AP004"},
		{"actor is not the requester", f.Valid(t, h.apprB.ID, scope, tid, "withdrawal_hold_resolution:request", "new", digest), "AP004"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := h.attackAs(h.reqA, c.proof, func(ctx context.Context, tx pgx.Tx) error { return hsrRawRequest(ctx, tx, h, wr.ID) })
			k3RequireCode(t, err, c.want)
		})
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_requested'`); n != 0 {
		t.Fatalf("attacks created %d resolutions", n)
	}
	// Control: the exactly-bound proof creates the resolution.
	if err := h.attackAs(h.reqA, f.Valid(t, h.reqA.ID, scope, tid, "withdrawal_hold_resolution:request", "new", digest), func(ctx context.Context, tx pgx.Tx) error {
		return hsrRawRequest(ctx, tx, h, wr.ID)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	rs, err := h.svc.List(k3Ctx(h.reqA), h.target(h.reqA), 10, h.meta())
	if err != nil || len(rs) != 1 {
		t.Fatalf("list: %v (%d)", err, len(rs))
	}
	r := rs[0]
	// Cancel: no proof / replay.
	err = h.attackAs(h.reqA, "", func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'cancelled' WHERE id = $1`, r.ID)
		return e
	})
	k3RequireCode(t, err, "AP001")
	tok := f.Valid(t, h.reqA.ID, scope, tid, "withdrawal_hold_resolution:cancel", r.ID.String(), r.PayloadHash)
	if err := h.attackAs(h.reqA, tok, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'cancelled' WHERE id = $1`, r.ID)
		return e
	}); err != nil {
		t.Fatalf("cancel control: %v", err)
	}
	if h.res(r.ID).State != ResolutionCancelled {
		t.Fatal("not cancelled")
	}
}

// A FRESH proof per attempt: every governed write of a real two-person execution
// consumed exactly one distinct nonce (request + two approvals).
func TestHSEC_HoldRelease_ActorProof_FreshProofPerAttempt(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(100)
	h.suspendTenant()
	before := h.holdNonces()
	r := h.mustRequest(h.reqA, wr.ID)
	h.mustDecide(h.apprB, r, ResolutionApprove)
	out := h.mustDecide(h.apprC, r, ResolutionApprove)
	if !out.Executed {
		t.Fatalf("not executed: %+v", out)
	}
	if got := h.holdNonces() - before; got != 3 {
		t.Fatalf("nonces consumed = %d, want 3 (request + 2 approvals)", got)
	}
}

func (h *hsr) holdNonces() int {
	h.t.Helper()
	var n int
	if err := h.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(DISTINCT nonce) FROM actor_proof_nonces WHERE tenant = $1 AND operation LIKE 'withdrawal\_hold\_resolution:%'`, h.f.tenantID).Scan(&n)
	}); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// ADR 0110 verifier operation table (M-10): withdrawal_hold_resolution:* is provable
// ONLY for scope platform_acting with a tenant. A correctly signed token for any other
// scope is refused; the same token shape is accepted for another operation (control).
func TestHSEC_HoldRelease_VerifierOperationTable_ScopeRestricted(t *testing.T) {
	h := newHSR(t, 1)
	tid := h.f.tenantID
	f := h.forger
	target := uuid.New().String()
	payload := strings.Repeat("a", 64)
	for _, op := range []string{"request", "cancel", "approve", "reject"} {
		full := "withdrawal_hold_resolution:" + op
		tg := target
		if op == "request" {
			tg = "new"
		}
		for _, sc := range []string{"tenant", "platform"} {
			tenantForTok, tenantArg := tid, any(tid)
			if sc == "platform" {
				tenantForTok, tenantArg = uuid.Nil, nil
			}
			tok := f.Valid(t, h.reqA.ID, sc, tenantForTok, full, tg, payload)
			err := h.attackAs(h.reqA, tok, func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `SELECT actor_proof_require($1, $2, $3, $4, $5, $6)`, h.reqA.ID, sc, tenantArg, full, tg, payload)
				return e
			})
			if !hsrIs(err, "AP004") {
				t.Errorf("%s with scope %s: want AP004, got %v", full, sc, err)
			}
		}
		tok := f.Valid(t, h.reqA.ID, "platform_acting", tid, full, tg, payload)
		if err := h.attackAs(h.reqA, tok, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `SELECT actor_proof_require($1, 'platform_acting', $2, $3, $4, $5)`, h.reqA.ID, tid, full, tg, payload)
			return e
		}); err != nil {
			t.Errorf("%s with scope platform_acting must verify: %v", full, err)
		}
	}
	// Control: the SAME tenant-scope token shape is accepted for an operation the table
	// does not restrict (so the refusals above are the table's, not a malformed token's).
	tok := f.Valid(t, h.f1.ID, "tenant", tid, "payment_force_resolve:request", "new", payload)
	if err := h.rt.WithPrincipalScope(context.Background(), tid, h.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `SELECT actor_proof_require($1, 'tenant', $2, 'payment_force_resolve:request', 'new', $3)`, h.f1.ID, tid, payload)
		return e
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	// And the Go signer refuses the same claims before they reach the database.
	for _, sc := range []string{actorproof.ScopeTenant, actorproof.ScopePlatform} {
		tn := tid
		if sc == actorproof.ScopePlatform {
			tn = uuid.Nil
		}
		if _, err := prooftest.Issuer(t).Sign(actorproof.Claims{Actor: h.reqA.ID, Scope: sc, Tenant: tn,
			Operation: actorproof.OpHoldResolutionApprove, Target: target, PayloadHash: payload}); !errors.Is(err, actorproof.ErrInvalidClaims) {
			t.Errorf("Go signer accepted scope %s: %v", sc, err)
		}
	}
}

// The catalog pin: zz_actor_proof_guard is the LAST BEFORE ROW trigger on every one of
// the ELEVEN governed tables (nine + the two hold-resolution tables).
func TestHSEC_HoldRelease_ActorProofTriggerOnElevenTables_Last(t *testing.T) {
	h := newHSR(t, 1)
	tables := []string{
		"ledger_adjustment_requests", "ledger_adjustment_approvals", "payment_manual_resolutions", "payment_manual_resolution_approvals",
		"staff_capability_grant_requests", "staff_capability_grant_approvals", "staff_capability_grants",
		"financial_approval_policy_changes", "financial_approval_policy_change_approvals",
		"withdrawal_hold_resolutions", "withdrawal_hold_resolution_approvals",
	}
	var total int
	if err := h.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT tgrelid) FROM pg_trigger WHERE tgname = 'zz_actor_proof_guard' AND NOT tgisinternal`).Scan(&total); err != nil {
			return err
		}
		for _, tbl := range tables {
			rows, err := tx.Query(ctx, `SELECT tgname FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal
				AND (tgtype & 1) = 1 AND (tgtype & 2) = 2 ORDER BY tgname COLLATE "C"`, tbl)
			if err != nil {
				return err
			}
			var names []string
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					rows.Close()
					return err
				}
				names = append(names, n)
			}
			rows.Close()
			if len(names) == 0 || names[len(names)-1] != "zz_actor_proof_guard" {
				t.Errorf("%s: zz_actor_proof_guard must sort LAST among the BEFORE ROW triggers, got %v", tbl, names)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if total != 11 {
		t.Fatalf("zz_actor_proof_guard is on %d tables, want 11", total)
	}
}

// executing opens a raw acting session as approver (base-1 world), records the approver's
// approval with a genuine proof, moves r to `executing`, runs fn and ROLLS BACK.
func (h *hsr) inExecuting(r HoldResolution, approver k3Staff, fn func(ctx context.Context, tx pgx.Tx) error) error {
	h.t.Helper()
	err := h.acting(approver, func(ctx context.Context, tx pgx.Tx) error {
		if err := h.proof(ctx, tx, "withdrawal_hold_resolution:approve", r.ID.String(), r.PayloadHash); err != nil {
			return err
		}
		if err := hsrRawApprove(ctx, tx, h, r, "approve"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return errK3Rollback
	})
	if errors.Is(err, errK3Rollback) {
		return nil
	}
	return err
}

func hsrPost(ctx context.Context, tx pgx.Tx, h *hsr, wr withdrawal.WithdrawalRequest, typ ledger.TransactionType, key string, corr uuid.UUID,
	holdDebit, cashCredit int64) error {
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode})
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: wr.TenantID, TransactionType: typ, IdempotencyKey: key, CorrelationID: corr, ReversesTransactionID: wr.HoldLedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: holdDebit},
			{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: cashCredit},
		}})
	return err
}

// CT-R3: the governed key, correlation, type, entry shape and withdrawal link, in every session.
func TestHSEC_HoldRelease_CTR3_KeyCorrelationShapeLinkForgeries(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(400)
	wr2 := h.hold(401)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	key := wr.ID.String() + ":governed_hold_released"

	otherTenant := seedPayoutFixture(t, h.pool, 1000, true)
	// A second player and wallet of the SAME tenant (for the cross-wallet shape attacks).
	person2, player2, wallet2 := uuid.New(), uuid.New(), uuid.New()
	if err := h.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var hold2, cash2 uuid.UUID
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			player2, h.f.tenantID, h.f.brandID, person2, player2.String()+"@hsr.invalid"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			wallet2, h.f.tenantID, h.f.brandID, player2); err != nil {
			return err
		}
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, h.f.tenantID,
			ledger.AccountSpec{WalletID: &wallet2, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
			ledger.AccountSpec{WalletID: &wallet2, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"})
		hold2, cash2 = ids[0], ids[1]
		return err
	})
	crossWallet := func(creditOther bool) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			own, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
				ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
				ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"})
			if err != nil {
				return err
			}
			debit, credit := own[0], own[1]
			if creditOther {
				credit = cash2
			} else {
				debit = hold2
			}
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: wr.TenantID, TransactionType: ledger.TxWithdrawalRejected, IdempotencyKey: key,
				CorrelationID: wr.ID, ReversesTransactionID: wr.HoldLedgerTransactionID,
				Entries: []ledger.EntryInput{{LedgerAccountID: debit, Direction: ledger.Debit, Amount: 400}, {LedgerAccountID: credit, Direction: ledger.Credit, Amount: 400}}})
			return err
		}
	}

	// ACTING session, executing resolution of THIS transaction.
	attacks := []struct {
		name string
		fn   func(ctx context.Context, tx pgx.Tx) error
		want []string
	}{
		{"wrong correlation", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalRejected, key, uuid.New(), 400, 400)
		}, []string{"CG030", "HR020"}},
		{"wrong transaction type", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalFailed, key, wr.ID, 400, 400)
		}, []string{"CG030", "HR020"}},
		{"another withdrawal's governed key", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr2, ledger.TxWithdrawalRejected, wr2.ID.String()+":governed_hold_released", wr2.ID, 401, 401)
		}, []string{"CG030", "HR020"}},
		{"an ordinary key under the resolution", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalRejected, wr.ID.String()+":rejected", wr.ID, 400, 400)
		}, []string{"CG030"}},
		{"the credit leg on another wallet's cash", crossWallet(true), []string{"CG030"}},
		{"the debit leg on another wallet's hold", crossWallet(false), []string{"CG030"}},
		{"a wrong amount (entry shape)", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalRejected, key, wr.ID, 399, 399)
		}, []string{"CG030"}},
		{"the state edge to submitted", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'submitted' WHERE id = $1`, wr.ID)
			return err
		}, []string{"HR050", "HR030", "42501"}},
		// state 'rejected' passes the acting policy WITH CHECK, so only the trigger can stop it.
		{"rejected with an unrelated link", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected', release_ledger_transaction_id = hold_ledger_transaction_id WHERE id = $1`, wr.ID)
			return err
		}, []string{"HR030"}},
		{"cancelled", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'cancelled' WHERE id = $1`, wr.ID)
			return err
		}, []string{"HR050"}},
		{"failed", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'failed' WHERE id = $1`, wr.ID)
			return err
		}, []string{"HR050"}},
	}
	for _, a := range attacks {
		t.Run(a.name, func(t *testing.T) {
			err := h.inExecuting(r, h.apprB, func(ctx context.Context, tx pgx.Tx) error { return k3Try(ctx, tx, a.fn) })
			if err == nil {
				// k3Try always rolls its savepoint back and returns fn's error: a nil
				// error from inExecuting means fn itself succeeded.
				t.Fatal("the forgery was ACCEPTED")
			}
			if !hsrIs(err, a.want...) {
				t.Fatalf("want one of %v, got %v", a.want, err)
			}
		})
	}
	// Each of the two ledger-header layers must hold ALONE: with the acting fence lifted
	// for ONE statement the all-sessions key guard still refuses the forgeries (HR020), and
	// with the key guard lifted the acting fence still refuses them (CG030).
	ctx0 := context.Background()
	ddl := func(sql string) {
		if err := h.pool.WithoutTenant(ctx0, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	headerForgeries := []struct {
		name string
		fn   func(ctx context.Context, tx pgx.Tx) error
	}{
		{"wrong correlation", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalRejected, key, uuid.New(), 400, 400)
		}},
		{"wrong transaction type", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalFailed, key, wr.ID, 400, 400)
		}},
		{"another withdrawal's governed key", func(ctx context.Context, tx pgx.Tx) error {
			return hsrPost(ctx, tx, h, wr2, ledger.TxWithdrawalRejected, wr2.ID.String()+":governed_hold_released", wr2.ID, 401, 401)
		}},
		// The tenant binding: a header for ANOTHER tenant carrying this tenant's executing key.
		{"raw header, another tenant", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id)
				VALUES ($1, 'withdrawal_rejected', $2, $3)`, otherTenant.tenantID, key, wr.ID)
			return err
		}},
		// A header-only insert (no entries): only the header layers can stop it.
		{"raw header, wrong correlation", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id)
				VALUES ($1, 'withdrawal_rejected', $2, $3)`, wr.TenantID, key, uuid.New())
			return err
		}},
	}
	for _, c := range []struct{ layer, off, on, want string }{
		{"key guard alone", "ledger_transactions_governed_fence", "ledger_transactions_governed_fence", "HR020"},
		{"acting fence alone", "ledger_transactions_hold_release_key_guard", "ledger_transactions_hold_release_key_guard", "CG030"},
	} {
		ddl(`ALTER TABLE ledger_transactions DISABLE TRIGGER ` + c.off)
		for _, a := range headerForgeries {
			err := h.inExecuting(r, h.apprB, func(ctx context.Context, tx pgx.Tx) error { return k3Try(ctx, tx, a.fn) })
			if !hsrIs(err, c.want) {
				ddl(`ALTER TABLE ledger_transactions ENABLE TRIGGER ` + c.on)
				t.Fatalf("%s / %s: want %s, got %v", c.layer, a.name, c.want, err)
			}
		}
		ddl(`ALTER TABLE ledger_transactions ENABLE TRIGGER ` + c.on)
	}

	// The key guard on its own: a resolution that is only PENDING (not executing) does not
	// admit the key even where the acting fence is lifted.
	ddl(`ALTER TABLE ledger_transactions DISABLE TRIGGER ledger_transactions_governed_fence`)
	pendErr := h.acting(h.apprB, func(ctx context.Context, tx pgx.Tx) error {
		var hold, cash uuid.UUID
		q := `SELECT id FROM ledger_accounts WHERE tenant_id = $1 AND wallet_id = $2 AND account_type = $3 AND asset_code = 'EUR'`
		if err := tx.QueryRow(ctx, q, wr.TenantID, wr.WalletID, "player_withdrawal_hold").Scan(&hold); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, q, wr.TenantID, wr.WalletID, "player_cash").Scan(&cash); err != nil {
			return err
		}
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: wr.TenantID, TransactionType: ledger.TxWithdrawalRejected, IdempotencyKey: key,
			CorrelationID: wr.ID, ReversesTransactionID: wr.HoldLedgerTransactionID,
			Entries: []ledger.EntryInput{{LedgerAccountID: hold, Direction: ledger.Debit, Amount: 400}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 400}}})
		return err
	})
	ddl(`ALTER TABLE ledger_transactions ENABLE TRIGGER ledger_transactions_governed_fence`)
	if !hsrIs(pendErr, "HR020") {
		t.Fatalf("key guard vs a pending resolution (fence lifted): want HR020, got %v", pendErr)
	}

	// CONTROL (non-vacuity): the exact key/correlation/type/shape posting and the
	// governed state change both succeed in the same executing context.
	if err := h.inExecuting(r, h.apprB, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.ReleaseForGovernedResolution(ctx, tx, wr.ID)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}

	// Without an executing resolution, in every session: the governed key is refused.
	forged := func(name string, run func(fn func(ctx context.Context, tx pgx.Tx) error) error) {
		t.Run("no resolution/"+name, func(t *testing.T) {
			err := run(func(ctx context.Context, tx pgx.Tx) error {
				return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalRejected, key, wr.ID, 400, 400)
			})
			// An acting session without an executing resolution is refused earlier still: by the
			// acting ledger_accounts WITH CHECK (42501) or the acting ledger fence (CG030).
			if !hsrIs(err, "HR020", "CG030", "42501") {
				t.Fatalf("want HR020 (or CG030/42501 for an acting session), got %v", err)
			}
		})
	}
	forged("tenant session", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return h.rt.WithTenant(context.Background(), h.f.tenantID, fn)
	})
	forged("tenant principal session", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return h.rt.WithPrincipalScope(context.Background(), h.f.tenantID, h.f1.ID, fn)
	})
	forged("acting session, resolution pending", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return h.acting(h.apprB, fn)
	})

	// After a genuine execution: another withdrawal cannot borrow the governed posting as
	// its release link, and the governed key cannot be re-posted.
	out := h.mustDecide(h.apprB, r, ResolutionApprove)
	if !out.Executed {
		t.Fatalf("execute: %+v", out)
	}
	h.activateTenant()
	err := h.rt.WithTenant(context.Background(), h.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected', release_ledger_transaction_id = $2 WHERE id = $1`, wr2.ID, *out.Resolution.LedgerTransactionID)
		return e
	})
	if !hsrIs(err, "HR020") {
		t.Fatalf("borrowing a governed release link: want HR020, got %v", err)
	}
	err = h.rt.WithTenant(context.Background(), h.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return hsrPost(ctx, tx, h, wr, ledger.TxWithdrawalRejected, key, wr.ID, 400, 400)
	})
	if !hsrIs(err, "HR020") {
		t.Fatalf("re-posting the governed key: want HR020, got %v", err)
	}
	h.assertInvariants()
}

// The deferred two-leg check holds in ALL sessions even if the acting entry fence were
// bypassed: with the fence lifted for ONE statement in this throwaway scratch database,
// a three-entry (or wrong-account) "release" cannot commit.
func TestHSEC_HoldRelease_DeferredShapeCheck_CatchesFenceBypass(t *testing.T) {
	for _, variant := range []string{"three_entries", "credit_to_house_not_cash", "four_entries_extra_pair", "no_reversal_link", "provider_ids_set", "credit_to_other_wallet_cash", "wrong_amount_both_legs"} {
		t.Run(variant, func(t *testing.T) {
			h := newHSR(t, 1)
			wr := h.hold(400)
			h.suspendTenant()
			r := h.mustRequest(h.reqA, wr.ID)
			var houseID uuid.UUID
			h.tx(func(ctx context.Context, tx pgx.Tx) error {
				var err error
				houseID, err = ledger.GetOrCreateAccount(ctx, tx, h.f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
				return err
			})
			var otherCash uuid.UUID
			h.tx(func(ctx context.Context, tx pgx.Tx) error {
				person2, player2, wallet2 := uuid.New(), uuid.New(), uuid.New()
				if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, (SELECT person_id FROM player_accounts WHERE id = $4), $5, 'x', 'active')`,
					player2, h.f.tenantID, h.f.brandID, h.f.playerAccountID, player2.String()+"@hsr.invalid"); err != nil {
					return err
				}
				_ = person2
				if _, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
					wallet2, h.f.tenantID, h.f.brandID, player2); err != nil {
					return err
				}
				var err error
				otherCash, err = ledger.GetOrCreateAccount(ctx, tx, h.f.tenantID, &wallet2, ledger.AccountPlayerCash, "EUR")
				return err
			})
			ctx := context.Background()
			ddl := func(sql string) {
				if err := h.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
					t.Fatal(err)
				}
			}
			ddl(`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_governed_fence`)
			defer ddl(`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_governed_fence`)

			err := h.acting(h.apprB, func(ctx context.Context, tx pgx.Tx) error {
				if err := h.proof(ctx, tx, "withdrawal_hold_resolution:approve", r.ID.String(), r.PayloadHash); err != nil {
					return err
				}
				if err := hsrRawApprove(ctx, tx, h, r, "approve"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
					return err
				}
				accts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
					ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
					ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"})
				if err != nil {
					return err
				}
				entries := []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: 400}}
				reverses, pid, ptx := wr.HoldLedgerTransactionID, (*string)(nil), (*string)(nil)
				switch variant {
				case "three_entries":
					entries = append(entries,
						ledger.EntryInput{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 399},
						ledger.EntryInput{LedgerAccountID: houseID, Direction: ledger.Credit, Amount: 1})
				case "credit_to_house_not_cash":
					entries = append(entries, ledger.EntryInput{LedgerAccountID: houseID, Direction: ledger.Credit, Amount: 400})
				case "four_entries_extra_pair":
					entries = append(entries, ledger.EntryInput{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 400},
						ledger.EntryInput{LedgerAccountID: houseID, Direction: ledger.Debit, Amount: 5},
						ledger.EntryInput{LedgerAccountID: houseID, Direction: ledger.Credit, Amount: 5})
				case "no_reversal_link":
					entries = append(entries, ledger.EntryInput{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 400})
					reverses = nil
				case "credit_to_other_wallet_cash":
					entries = append(entries, ledger.EntryInput{LedgerAccountID: otherCash, Direction: ledger.Credit, Amount: 400})
				case "wrong_amount_both_legs":
					entries = []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: 399},
						{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 399}}
				case "provider_ids_set":
					entries = append(entries, ledger.EntryInput{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: 400})
					a, b := "hsr-forged-provider", "hsr-forged-"+uuid.NewString()
					pid, ptx = &a, &b
				}
				res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: wr.TenantID, TransactionType: ledger.TxWithdrawalRejected,
					IdempotencyKey: wr.ID.String() + ":governed_hold_released", CorrelationID: wr.ID, ReversesTransactionID: reverses,
					ProviderID: pid, ProviderTxID: ptx, Entries: entries})
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected', release_ledger_transaction_id = $2, updated_at = now() WHERE id = $1`, wr.ID, res.TransactionID); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, res.TransactionID)
				return err
			})
			if !hsrIs(err, "HR041") {
				t.Fatalf("a malformed release must not commit: want HR041, got %v", err)
			}
			if h.wr(wr.ID).State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
				t.Fatal("the malformed release committed")
			}
		})
	}
}

// The approved-hold freeze (security M-6), in tenant, principal, acting sessions and the
// brand-only variant; the normal KYC denial is unaffected (the H-SEC gate runs first and
// an ACTIVE tenant still denies).
func TestHSEC_HoldRelease_FreezeTrigger(t *testing.T) {
	for _, variant := range []string{"tenant_suspended", "brand_suspended", "tenant_closed"} {
		t.Run(variant, func(t *testing.T) {
			h := newHSR(t, 1)
			wr := h.hold(100)
			// Active tenant and brand: the same edges are NOT frozen (control, rolled back).
			err := h.rt.WithTenant(context.Background(), h.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, e := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'cancelled' WHERE id = $1`, wr.ID); e != nil {
					return e
				}
				return errK3Rollback
			})
			if !errors.Is(err, errK3Rollback) {
				t.Fatalf("control: an active tenant must not freeze approved -> cancelled: %v", err)
			}
			switch variant {
			case "tenant_suspended":
				h.suspendTenant()
			case "tenant_closed":
				h.setTenantStatus("closed")
			default:
				h.suspendBrand()
			}
			for _, to := range []string{"rejected", "cancelled", "failed", "submitted", "requested"} {
				sessions := map[string]func(fn func(ctx context.Context, tx pgx.Tx) error) error{
					"tenant": func(fn func(ctx context.Context, tx pgx.Tx) error) error {
						return h.rt.WithTenant(context.Background(), h.f.tenantID, fn)
					},
					"principal": func(fn func(ctx context.Context, tx pgx.Tx) error) error {
						return h.rt.WithPrincipalScope(context.Background(), h.f.tenantID, h.f1.ID, fn)
					},
					"acting": func(fn func(ctx context.Context, tx pgx.Tx) error) error { return h.acting(h.reqA, fn) },
					"owner": func(fn func(ctx context.Context, tx pgx.Tx) error) error {
						return h.pool.WithTenant(context.Background(), h.f.tenantID, fn)
					},
				}
				for name, run := range sessions {
					err := run(func(ctx context.Context, tx pgx.Tx) error {
						tag, e := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = $2 WHERE id = $1`, wr.ID, to)
						if e == nil && tag.RowsAffected() == 0 {
							return errors.New("no row matched")
						}
						return e
					})
					// An acting session without an executing resolution may also be stopped by RLS; the
					// freeze trigger fires BEFORE the WITH CHECK, so HR050 is the answer everywhere.
					if !hsrIs(err, "HR050") {
						t.Errorf("%s session approved -> %s: want HR050, got %v", name, to, err)
					}
				}
			}
			if got := h.wr(wr.ID); got.State != withdrawal.StateApproved {
				t.Fatalf("state %s", got.State)
			}

			// DenyForCompliance (the ledger-bearing edge approved -> rejected) is frozen too ...
			params := kyc.EnforcementParams{TenantID: h.f.tenantID, BrandID: h.f.brandID, PlayerAccountID: h.f.playerAccountID, PersonID: h.f.personID,
				Operation: kyc.EnforcementWithdrawalPayout, AssetCode: "EUR", Amount: 100, CorrelationID: wr.ID}
			decision := kyc.EnforcementDecision{Outcome: kyc.OutcomeFailed, Allowed: false, Code: "kyc_withdrawal_payout:failed", PolicyVersion: kyc.PolicyVersion}
			err = h.pool.WithTenant(context.Background(), h.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, e := withdrawal.LockApprovedForSubmission(ctx, tx, wr.ID); e != nil {
					return e
				}
				_, e := withdrawal.DenyForCompliance(ctx, tx, wr.ID, decision, params)
				return e
			})
			if !hsrIs(err, "HR050") {
				t.Fatalf("DenyForCompliance on a non-active tenant/brand: want HR050, got %v", err)
			}
			// ... but the real path never gets there: the H-SEC gate refuses first, with the KYC
			// verification revoked (a deny would otherwise release the hold).
			revokeVerification(t, h.pool, h.f)
			_, cerr := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			if !errors.Is(cerr, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("the H-SEC gate must run before the KYC denial: %v", cerr)
			}
			if h.wr(wr.ID).State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
				t.Fatal("the hold moved")
			}

			if variant == "tenant_closed" {
				h.assertInvariants()
				return // closed is terminal (ADR 0112 3.1): there is no reactivation
			}
			// Reactivated: the normal KYC denial works and releases the hold (the freeze does not
			// break it).
			h.activateTenant()
			h.activateBrand()
			claim, cerr := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			if cerr != nil || !claim.Denied {
				t.Fatalf("after reactivation the KYC denial must work: %+v %v", claim, cerr)
			}
			if got := h.wr(wr.ID); got.State != withdrawal.StateRejected || got.ReleaseLedgerTransactionID == nil {
				t.Fatalf("kyc deny: %+v", got)
			}
			h.assertInvariants()
		})
	}
}

// Kill-switch semantics intact (owner decision 18): a release is not a dispatch - it is
// neither blocked nor enabled by the switch and calls no provider - and the dispatch path
// stays blocked by the switch after reactivation.
func TestHSEC_HoldRelease_KillSwitchUntouched(t *testing.T) {
	h := newHSR(t, 1)
	released := h.hold(300)
	kept := h.hold(301)
	if err := h.pool.WithPrincipalScope(context.Background(), h.f.tenantID, h.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, h.f.tenantID, "*", KillSwitchOperationAny, "hsec-incident")
		return err
	}); err != nil {
		t.Fatalf("engage the kill switch: %v", err)
	}
	h.suspendTenant()
	r := h.mustRequest(h.reqA, released.ID)
	if out := h.mustDecide(h.apprB, r, ResolutionApprove); !out.Executed {
		t.Fatalf("the release is not a dispatch and must not be gated by the switch: %+v", out)
	}
	if n := h.count(`SELECT count(*) FROM payment_attempts WHERE tenant_id = $1`, h.f.tenantID); n != 0 {
		t.Fatalf("a release created %d payment attempts", n)
	}
	if h.mock.AttemptCount() != 0 {
		t.Fatalf("a release called the provider %d times", h.mock.AttemptCount())
	}
	// Resume after reactivation is the EXISTING submit path, and the switch still blocks it.
	h.activateTenant()
	_, err := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, kept.ID, "bank_transfer", testSubmitActor())
	if !errors.Is(err, ErrPayoutKillSwitchEngaged) {
		t.Fatalf("the kill switch must still block the dispatch: %v", err)
	}
	if h.wr(kept.ID).State != withdrawal.StateApproved || h.mock.AttemptCount() != 0 {
		t.Fatal("the switch did not hold the payout")
	}
	h.assertInvariants()
}

// Grants pin: exactly SELECT/INSERT/UPDATE and SELECT/INSERT for the runtime role, equal
// to deploy/init-app-role.sql; the catalogue and classification rows.
func TestHSEC_HoldRelease_GrantsCatalogueAndClassification(t *testing.T) {
	h := newHSR(t, 1)
	want := map[string]string{
		"withdrawal_hold_resolutions":          "INSERT,SELECT,UPDATE",
		"withdrawal_hold_resolution_approvals": "INSERT,SELECT",
	}
	for table, privs := range want {
		var got string
		h.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COALESCE(string_agg(privilege_type, ',' ORDER BY privilege_type), '') FROM information_schema.role_table_grants
				WHERE table_schema = 'public' AND table_name = $1 AND grantee = 'igaming_runtime'`, table).Scan(&got)
		})
		if got != privs {
			t.Errorf("%s: igaming_runtime privileges = %q, want %q", table, got, privs)
		}
	}
	b, err := os.ReadFile(filepath.Join(realMigrationsDir(t), "..", "deploy", "init-app-role.sql"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\('(withdrawal_hold_[a-z_]+)', '([A-Z, ]+)'\)`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		got[m[1]] = strings.ReplaceAll(m[2], " ", "")
	}
	for table, privs := range want {
		g := strings.Split(got[table], ",")
		sort.Strings(g)
		if strings.Join(g, ",") != privs {
			t.Errorf("deploy/init-app-role.sql %s = %q, want %q", table, got[table], privs)
		}
	}
	var class string
	var roles []string
	var platformOK bool
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT class FROM financial_control_classifications WHERE operation_kind = 'withdrawal_hold_resolution'`).Scan(&class); err != nil {
			return err
		}
		return nil
	})
	if class != "mandatory_four_eyes" {
		t.Errorf("classification = %q", class)
	}
	for _, c := range []string{"withdrawal_hold_resolution:request", "withdrawal_hold_resolution:approve"} {
		h.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT eligible_tenant_roles, platform_grantee_allowed FROM financial_capability_catalogue WHERE capability = $1`, c).Scan(&roles, &platformOK)
		})
		if len(roles) != 0 || !platformOK {
			t.Errorf("%s: eligible_tenant_roles=%v platform_grantee_allowed=%v, want {} and true", c, roles, platformOK)
		}
	}
	// DELETE and TRUNCATE are refused for the runtime role / by trigger.
	err = h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM withdrawal_hold_resolutions`)
		return e
	})
	if err == nil {
		t.Error("DELETE on withdrawal_hold_resolutions was accepted")
	}
}

const hsecMigrationVersion = 124

// Migration up/down/up on a scratch DB migrated to N-1: the whole-schema snapshot after
// down equals the one before up (restoring the 0113/0115/0120 bodies, policies and
// CHECKs byte for byte), and re-up reproduces the first up. The down REFUSES while
// resolution rows exist.
func TestHSEC_HoldRelease_Migration0124UpDownUp_WholeSchema(t *testing.T) {
	pool, _ := scratchThrough(t, "hsec0124_", hsecMigrationVersion-1)
	preSnap := schemaSnapshot15(t, pool)
	dir := migration0101Dir(t, hsecMigrationVersion)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	upSnap := schemaSnapshot15(t, pool)
	if upSnap == preSnap {
		t.Fatal("0124 changed nothing?")
	}
	for _, must := range []string{"table:withdrawal_hold_resolutions:", "table:withdrawal_hold_resolution_approvals:",
		"trigger:withdrawal_requests:withdrawal_requests_approved_hold_freeze", "trigger:ledger_transactions:ledger_transactions_hold_release_key_guard",
		"policy:brands:acting_lock", "trigger:withdrawal_hold_resolutions:zz_actor_proof_guard"} {
		if !strings.Contains(upSnap, must) {
			t.Errorf("the 0124 schema lacks %q", must)
		}
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down on an empty 0124: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != preSnap {
		t.Fatalf("0124 down did not restore the N-1 schema exactly:\n%s", snapDiff(preSnap, got))
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != upSnap {
		t.Fatalf("0124 re-up differs from the first up:\n%s", snapDiff(upSnap, got))
	}
}

func TestHSEC_HoldRelease_Migration0124DownRefusals(t *testing.T) {
	// The world is migrated to the latest version; every migration above 0124
	// (0125 PAY-PAYOUT-UNBOUND-RESOLVE-1, 0126 B13-B, 0127 GOV-R32, ...) is first
	// rolled back (they hold no data here), so the step that refuses is 0124's own down.
	downTo0124 := func(t *testing.T, h *hsr) error {
		t.Helper()
		// 0128 (ADR 0112) refuses its own down while a governed transition exists (S13, covered by
		// internal/tenant/migration_0128_integration_test.go). This test is about 0124's down, and the
		// world's status flips are governed, so on this THROWAWAY scratch database the launch history is
		// removed first, inside one owner transaction (transactional DDL: an error leaves everything as it was).
		if err := h.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			for _, stmt := range []string{
				`ALTER TABLE launch_status_transitions NO FORCE ROW LEVEL SECURITY`,
				`ALTER TABLE launch_authorisation_approvals NO FORCE ROW LEVEL SECURITY`,
				`ALTER TABLE launch_authorisation_requests NO FORCE ROW LEVEL SECURITY`,
				`ALTER TABLE launch_status_transitions DISABLE TRIGGER USER`,
				`ALTER TABLE launch_authorisation_approvals DISABLE TRIGGER USER`,
				`ALTER TABLE launch_authorisation_requests DISABLE TRIGGER USER`,
				`DELETE FROM launch_status_transitions WHERE kind = 'governed'`,
				`DELETE FROM launch_authorisation_approvals`,
				`DELETE FROM launch_authorisation_requests`,
				`ALTER TABLE launch_status_transitions ENABLE TRIGGER USER`,
				`ALTER TABLE launch_authorisation_approvals ENABLE TRIGGER USER`,
				`ALTER TABLE launch_authorisation_requests ENABLE TRIGGER USER`,
			} {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return errors.New(stmt + ": " + err.Error())
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("clear launch history on the scratch world: %v", err)
		}
		dir := realMigrationsDir(t)
		if _, err := h.pool.MigrateDown(context.Background(), dir, migrationsAbove(t, hsecMigrationVersion)); err != nil {
			t.Fatalf("down above 0124 (empty): %v", err)
		}
		_, err := h.pool.MigrateDown(context.Background(), migration0101Dir(t, hsecMigrationVersion), 1)
		return err
	}
	t.Run("resolution_row", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(100)
		h.suspendTenant()
		h.mustRequest(h.reqA, wr.ID)
		k3RequireCode(t, downTo0124(t, h), "HR099")
		if h.count(`SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_requested'`) != 1 {
			t.Fatal("whole rollback expected")
		}
	})
	t.Run("policy_row", func(t *testing.T) {
		h := newHSR(t, 1)
		k3RequireCode(t, downTo0124(t, h), "HR099")
	})
}

// Security C-1: the deferred check FAILS CLOSED when the resolution row is not visible to the
// committing session (here the acting settings are cleared before commit).
func TestHSEC_HoldRelease_DeferredCheck_RaisesWhenRowNotVisibleAtCommit(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		if err := h.proof(ctx, tx, "withdrawal_hold_resolution:request", "new", hsrRequestDigest(h, wr.ID)); err != nil {
			return err
		}
		if err := hsrRawRequest(ctx, tx, h, wr.ID); err != nil {
			return err
		}
		// Control inside the txn: the row is visible now. Then the session settings change.
		_, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', '', true), set_config('app.acting_platform_principal_id', '', true)`)
		return err
	})
	if !hsrIs(err, "HR041") {
		t.Fatalf("an invisible-at-commit resolution must raise HR041, got %v", err)
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_requested'`); n != 0 {
		t.Fatalf("audit rows: %d", n)
	}
}

// Security C-2: the brands lock-only policy grants no write: an acting UPDATE is refused by
// its WITH CHECK (false), and a locking read still works.
func TestHSEC_HoldRelease_ActingBrandsPolicyIsLockOnly(t *testing.T) {
	h := newHSR(t, 1)
	err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1 AND tenant_id = $2 FOR SHARE`, h.f.brandID, h.f.tenantID).Scan(&st); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("control: the acting FOR SHARE read must work: %v", err)
	}
	err = h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET name = name WHERE id = $1`, h.f.brandID)
		return err
	})
	if !hsrIs(err, "42501") {
		t.Fatalf("an acting UPDATE brands must be refused by the policy WITH CHECK (42501), got %v", err)
	}
	err = h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = 'closed' WHERE id = $1`, h.f.brandID)
		return err
	})
	// ADR 0112 (security S-4): the BEFORE UPDATE launch guard refuses a status change BEFORE the
	// policy WITH CHECK is evaluated, so the status UPDATE is exactly LA020 ...
	if !hsrIs(err, "LA020") {
		t.Fatalf("an acting status UPDATE must be refused by the launch guard (LA020), got %v", err)
	}
	// ... while an acting UPDATE of a NON-status column is still exactly the policy's 42501.
	err = h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET name = 'acting rename' WHERE id = $1`, h.f.brandID)
		return err
	})
	if !hsrIs(err, "42501") {
		t.Fatalf("an acting UPDATE of brands.name must be exactly the policy refusal (42501), got %v", err)
	}
}

// Security F-4: cancel and expire null every execution-time column, even if the UPDATE names values.
func TestHSEC_HoldRelease_CancelAndExpire_NullExecutionColumns(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	wr2 := h.hold(101)
	h.suspendTenant()
	forged := `, tenant_status_at_execution = 'suspended', brand_status_at_execution = 'suspended', required_at_execution = 9, contributing_policy_ids_at_execution = ARRAY[gen_random_uuid()]`
	read := func(id uuid.UUID) (n int) {
		if err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_hold_resolutions WHERE id = $1 AND tenant_status_at_execution IS NULL
				AND brand_status_at_execution IS NULL AND required_at_execution IS NULL AND contributing_policy_ids_at_execution IS NULL`, id).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	r1 := h.mustRequest(h.reqA, wr.ID)
	if err := h.attackAs(h.reqA, h.forger.Valid(t, h.reqA.ID, "platform_acting", h.f.tenantID, "withdrawal_hold_resolution:cancel", r1.ID.String(), r1.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'cancelled'`+forged+` WHERE id = $1`, r1.ID)
		return e
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if h.res(r1.ID).State != ResolutionCancelled || read(r1.ID) != 1 {
		t.Fatal("cancel left execution-time columns set")
	}
	r2 := h.mustRequest(h.reqA, wr2.ID)
	h.backdate(r2.ID)
	if err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'expired'`+forged+` WHERE id = $1`, r2.ID)
		return e
	}); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if h.res(r2.ID).State != ResolutionExpired || read(r2.ID) != 1 {
		t.Fatal("expire left execution-time columns set")
	}
}

// Ledger-finance C-1: the governed-release guard fires on EVERY update. After the release
// (approved -> rejected) inside the executing transaction, a second UPDATE of any column is
// refused; and HR042: an executing resolution with a governed posting cannot be refused.
func TestHSEC_HoldRelease_ExecutingWindow_SecondUpdateAndRefusalRefused(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(400)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	for name, sql := range map[string]string{
		"provider_id":       `UPDATE withdrawal_requests SET provider_id = 'hsr-x' WHERE id = $1`,
		"provider_ref":      `UPDATE withdrawal_requests SET provider_reference = 'hsr-ref' WHERE id = $1`,
		"updated_at":        `UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`,
		"state back":        `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`,
		"state to complete": `UPDATE withdrawal_requests SET state = 'completed' WHERE id = $1`,
	} {
		err := h.inExecuting(r, h.apprB, func(ctx context.Context, tx pgx.Tx) error {
			if err := withdrawal.ReleaseForGovernedResolution(ctx, tx, wr.ID); err != nil {
				return err
			}
			return k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, sql, wr.ID)
				return e
			})
		})
		if !hsrIs(err, "HR030") {
			t.Errorf("%s after the release in the executing txn: want HR030, got %v", name, err)
		}
	}
	// HR042.
	err := h.inExecuting(r, h.apprB, func(ctx context.Context, tx pgx.Tx) error {
		if err := withdrawal.ReleaseForGovernedResolution(ctx, tx, wr.ID); err != nil {
			return err
		}
		return k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'refused_at_execution', refusal_code = 'x' WHERE id = $1`, r.ID)
			return e
		})
	})
	if !hsrIs(err, "HR042") {
		t.Errorf("refusing an executing resolution that already posted: want HR042, got %v", err)
	}
	// Control: the genuine release still commits.
	if out := h.mustDecide(h.apprB, r, ResolutionApprove); !out.Executed {
		t.Fatalf("control: %+v", out)
	}
}

// Ledger-finance C-3: catalog pin - the one-pending and one-executed partial UNIQUE indexes exist
// with the right columns and predicates.
func TestHSEC_HoldRelease_CatalogPin_OnePendingOneExecutedIndexes(t *testing.T) {
	h := newHSR(t, 1)
	for name, state := range map[string]string{"withdrawal_hold_resolutions_one_pending": "pending", "withdrawal_hold_resolutions_one_executed": "executed"} {
		var def string
		h.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name).Scan(&def)
		})
		if !strings.Contains(def, "CREATE UNIQUE INDEX") || !strings.Contains(def, "(withdrawal_request_id)") ||
			!strings.Contains(def, "WHERE (state = '"+state+"'::text)") {
			t.Errorf("%s: unexpected definition %q", name, def)
		}
	}
}

// Security M-6 fail-closed: when the freeze trigger cannot READ the tenant status it refuses, even
// for an active tenant (a restrictive policy hides the tenants row in this scratch database only).
func TestHSEC_HoldRelease_FreezeTrigger_UnreadableStatusFailsClosed(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	ctx := context.Background()
	ddl := func(sql string) {
		if err := h.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	try := func() error {
		return h.rt.WithTenant(ctx, h.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'cancelled' WHERE id = $1`, wr.ID); e != nil {
				return e
			}
			return errK3Rollback
		})
	}
	if err := try(); !errors.Is(err, errK3Rollback) {
		t.Fatalf("control (readable, active): %v", err)
	}
	ddl(`CREATE POLICY hsr_hide ON tenants AS RESTRICTIVE FOR SELECT USING (false)`)
	err := try()
	ddl(`DROP POLICY hsr_hide ON tenants`)
	if !hsrIs(err, "HR050") {
		t.Fatalf("an unreadable tenant status must fail closed with HR050, got %v", err)
	}
}
