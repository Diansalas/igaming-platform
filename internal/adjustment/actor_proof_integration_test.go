//go:build integration

// PRH-2 R5 (SIGNED-ACTOR-PROOF, ADR 0110, migration 0120). An authorized
// DEFENSIVE test of this repository's own control, on a private scratch
// database with synthetic data: with arbitrary SQL as the REAL runtime role
// (TEST_RUNTIME_DATABASE_URL, asserted neither superuser nor BYPASSRLS), set
// the transaction-local GUCs that financial_actor_session() trusts and try to
// submit / approve a K2 ledger adjustment WITHOUT a valid server-issued proof,
// and with forged, expired, replayed and mis-bound ones. Every attempt must be
// refused with its own AP* SQLSTATE, leave no approval/request row and no
// ledger posting. The positive tests drive the real Service as the runtime role
// with server-issued proofs: two DIFFERENT real actors, exactly one posting,
// SUM(D) = SUM(C), projection = rebuild, and an idempotent retry.
//
// Non-vacuity: every negative matrix has a control in which the SAME write with
// a proof for exactly that actor/operation/target/payload succeeds.
package adjustment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func processForger(t testing.TB) prooftest.Forger {
	t.Helper()
	return prooftest.ProcessForger(t, envOwnerURL(t))
}

func envOwnerURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

// attackSession is arbitrary SQL as the runtime role: it sets exactly the GUCs
// a compromised credential can set (tenant + principal), plus whatever proof
// string the attacker chooses, then runs fn.
func attackSession(rt *db.Pool, w *world, actor staffMember, proof string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return rt.WithPrincipalScope(context.Background(), w.Tenant, actor.ID, func(ctx context.Context, tx pgx.Tx) error {
		if proof != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
}

func attackSubmitSQL(ctx context.Context, tx pgx.Tx, w *world, id uuid.UUID, in SubmitInput) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ledger_adjustment_requests
			(id, tenant_id, wallet_id, player_account_id, brand_id, asset_code, direction, amount, reason_code,
			 causation_transaction_id, evidence_ref_hash, note_hash, payload_hash, initiated_by, initiated_by_scope,
			 initiated_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7::numeric, $8, $9, $10, ledger_adjustment_note_hash($11),
			'-', $4, 'tenant', $4, '-', 1, '{}', now())`,
		id, w.Tenant, in.WalletID, uuid.Nil, in.AssetCode, string(in.Direction), fmt.Sprint(in.Amount), string(in.ReasonCode),
		in.CausationTransactionID, in.EvidenceRefHash, in.Note)
	return err
}

func attackDecideSQL(ctx context.Context, tx pgx.Tx, w *world, requestID uuid.UUID, decision, payloadHash string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ledger_adjustment_approvals
			(tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'tenant', $5, 0, 'attack')`,
		w.Tenant, requestID, decision, payloadHash, uuid.Nil)
	return err
}

func (w *world) approvalCount(requestID uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithPrincipalScope(context.Background(), w.Tenant, w.F1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_approvals WHERE request_id = $1`, requestID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) requestCount() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithPrincipalScope(context.Background(), w.Tenant, w.F1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_requests WHERE tenant_id = $1`, w.Tenant).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) proofNonceCount() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM actor_proof_nonces WHERE tenant = $1`, w.Tenant).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// (1a) ADVERSARIAL, approve. The attacker impersonates two real eligible
// finance admins (F2, F3) with only GUCs and tries to approve a real pending K2
// request. Required approvals = 2, so two accepted approvals would execute a
// real debit - the asserted outcome is that NONE is accepted without a proof
// that is exactly right.
func TestActorProof_K2_Adversarial_Approve_FailsClosed(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 2})
	w.svc = NewService(rt)
	f := processForger(t)
	causation := w.fund(10_000)

	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in) // genuine, server-issued proof
	if err != nil {
		t.Fatalf("genuine submit: %v", err)
	}
	r2, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("genuine submit (second request): %v", err)
	}
	before := w.playerCash()
	ln := w.adjustmentLedgerTxCount()
	nowU := time.Now().Unix()
	const op = "ledger_adjustment:approve"
	tid := w.Tenant

	cases := []struct {
		name  string
		actor staffMember
		proof func() string
		want  string
	}{
		{"no proof at all", w.F2, func() string { return "" }, "AP001"},
		{"garbage proof", w.F2, func() string { return "not-a-proof" }, "AP001"},
		{"right shape, mac not hex", w.F2, func() string {
			return strings.Join([]string{"v1", f.KID, w.F2.ID.String(), "tenant", tid.String(), op, r.ID.String(), r.PayloadHash, "1", "2", prooftest.Nonce22(t), "zz"}, "|")
		}, "AP001"},
		{"wrong signing key", w.F2, func() string {
			k := make([]byte, 32)
			_, _ = rand.Read(k)
			return prooftest.Forger{KID: f.KID, Key: k}.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash)
		}, "AP002"},
		{"unknown kid", w.F2, func() string {
			return prooftest.Forger{KID: "nokid", Key: f.Key}.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash)
		}, "AP002"},
		{"field tampered after signing (actor swapped)", w.F3, func() string {
			tok := f.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash)
			return strings.Replace(tok, w.F2.ID.String(), w.F3.ID.String(), 1)
		}, "AP002"},
		{"expired proof", w.F2, func() string {
			return f.Token(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash, nowU-120, nowU-60)
		}, "AP003"},
		{"proof issued in the future", w.F2, func() string {
			return f.Token(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash, nowU+600, nowU+630)
		}, "AP003"},
		{"lifetime longer than 60 s", w.F2, func() string {
			return f.Token(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash, nowU, nowU+3600)
		}, "AP003"},
		{"proof for a different operation (reject)", w.F2, func() string {
			return f.Valid(t, w.F2.ID, "tenant", tid, "ledger_adjustment:reject", r.ID.String(), r.PayloadHash)
		}, "AP004"},
		{"proof for a different target request", w.F2, func() string {
			return f.Valid(t, w.F2.ID, "tenant", tid, op, r2.ID.String(), r.PayloadHash)
		}, "AP004"},
		{"proof for a different actor (F3's proof, F2's session)", w.F2, func() string {
			return f.Valid(t, w.F3.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash)
		}, "AP004"},
		{"proof for a different tenant", w.F2, func() string {
			return f.Valid(t, w.F2.ID, "tenant", uuid.New(), op, r.ID.String(), r.PayloadHash)
		}, "AP004"},
		{"proof for a different scope (platform_acting)", w.F2, func() string {
			return f.Valid(t, w.F2.ID, "platform_acting", tid, op, r.ID.String(), r.PayloadHash)
		}, "AP004"},
		{"proof for a different payload hash", w.F2, func() string {
			return f.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), strings.Repeat("0", 64))
		}, "AP004"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := attackSession(rt, w, c.actor, c.proof(), func(ctx context.Context, tx pgx.Tx) error {
				return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
			})
			if err == nil {
				t.Fatal("the approval was ACCEPTED without a valid proof for exactly this write")
			}
			requireCode(t, err, c.want)
		})
	}
	// The decision is bound: an APPROVE proof cannot carry a REJECT decision.
	t.Run("approve proof presented for a reject decision", func(t *testing.T) {
		err := attackSession(rt, w, w.F2, f.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
			return attackDecideSQL(ctx, tx, w, r.ID, "reject", r.PayloadHash)
		})
		requireCode(t, err, "AP004")
	})
	// Retired key (rotation): a proof under a retired kid is refused.
	t.Run("retired key", func(t *testing.T) {
		kid := prooftest.RandomKID(t)
		key := prooftest.NewKey(t)
		prooftest.Provision(t, envOwnerURL(t), kid, key)
		prooftest.Retire(t, envOwnerURL(t), kid)
		err := attackSession(rt, w, w.F2, prooftest.Forger{KID: kid, Key: key}.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
			return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
		})
		requireCode(t, err, "AP002")
	})
	// Replay: a perfectly valid proof whose nonce was already consumed.
	t.Run("replayed nonce", func(t *testing.T) {
		tok := f.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash)
		consume := func() error {
			return attackSession(rt, w, w.F2, tok, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT actor_proof_require($1, 'tenant', $2, $3, $4, $5)`, w.F2.ID, tid, op, r.ID.String(), r.PayloadHash)
				return err
			})
		}
		if err := consume(); err != nil {
			t.Fatalf("first consumption of a valid proof must succeed: %v", err)
		}
		err := attackSession(rt, w, w.F2, tok, func(ctx context.Context, tx pgx.Tx) error {
			return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
		})
		requireCode(t, err, "AP005")
		requireCode(t, consume(), "AP005")
	})

	if got := w.approvalCount(r.ID); got != 0 {
		t.Fatalf("approval rows exist after the attack matrix: %d", got)
	}
	if got := w.request(r.ID).State; got != StatePending {
		t.Fatalf("request left pending: %s", got)
	}
	if got := w.adjustmentLedgerTxCount(); got != ln {
		t.Fatalf("a ledger posting exists after the attack matrix (%d -> %d)", ln, got)
	}
	if got := w.playerCash(); got != before {
		t.Fatalf("player_cash changed %d -> %d", before, got)
	}

	// CONTROL (non-vacuity): the same write with a proof for exactly this
	// actor/operation/target/payload is accepted.
	if err := attackSession(rt, w, w.F2, f.Valid(t, w.F2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
		return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
	}); err != nil {
		t.Fatalf("control: the exactly-bound proof must be accepted: %v", err)
	}
	if got := w.approvalCount(r.ID); got != 1 {
		t.Fatalf("control: want 1 approval row, got %d", got)
	}
	if got := w.adjustmentLedgerTxCount(); got != ln {
		t.Fatalf("control: required=2, one approval must not execute (%d -> %d)", ln, got)
	}
	w.assertInvariants()
}

// (1b) ADVERSARIAL, submit (initiate).
func TestActorProof_K2_Adversarial_Submit_FailsClosed(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	f := processForger(t)
	causation := w.fund(10_000)
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	noteSum := sha256.Sum256([]byte(in.Note))
	digest := func(in SubmitInput) string {
		return actorproof.Digest(actorproof.S(w.Tenant.String()), actorproof.S(in.WalletID.String()), actorproof.S(in.AssetCode),
			actorproof.S(string(in.Direction)), actorproof.S(fmt.Sprint(in.Amount)), actorproof.S(string(in.ReasonCode)),
			actorproof.UUIDP(in.CausationTransactionID), in.EvidenceRefHash, actorproof.S(hex.EncodeToString(noteSum[:])))
	}
	const op = "ledger_adjustment:initiate"
	id := uuid.New()
	other := in
	other.Amount = 9_000
	cases := []struct {
		name  string
		proof string
		id    uuid.UUID
		want  string
	}{
		{"no proof (GUC impersonation of F1 only)", "", id, "AP001"},
		{"garbage proof", "x|y", id, "AP001"},
		{"wrong key", prooftest.Forger{KID: f.KID, Key: make([]byte, 32)}.Valid(t, w.F1.ID, "tenant", w.Tenant, op, id.String(), digest(in)), id, "AP002"},
		{"different amount than the proof binds", f.Valid(t, w.F1.ID, "tenant", w.Tenant, op, id.String(), digest(other)), id, "AP004"},
		{"proof for a different request id", f.Valid(t, w.F1.ID, "tenant", w.Tenant, op, uuid.NewString(), digest(in)), id, "AP004"},
		{"proof for a different actor", f.Valid(t, w.F2.ID, "tenant", w.Tenant, op, id.String(), digest(in)), id, "AP004"},
		{"proof for a different operation (cancel)", f.Valid(t, w.F1.ID, "tenant", w.Tenant, "ledger_adjustment:cancel", id.String(), digest(in)), id, "AP004"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := attackSession(rt, w, w.F1, c.proof, func(ctx context.Context, tx pgx.Tx) error {
				return attackSubmitSQL(ctx, tx, w, c.id, in)
			})
			if err == nil {
				t.Fatal("the submission was ACCEPTED without a valid proof for exactly this write")
			}
			requireCode(t, err, c.want)
		})
	}
	if got := w.requestCount(); got != 0 {
		t.Fatalf("request rows exist after the attack matrix: %d", got)
	}
	// CONTROL, then REPLAY of the same proof/id: the control commits, the replay
	// is refused by the nonce table (the BEFORE trigger runs before the PK check).
	tok := f.Valid(t, w.F1.ID, "tenant", w.Tenant, op, id.String(), digest(in))
	if err := attackSession(rt, w, w.F1, tok, func(ctx context.Context, tx pgx.Tx) error { return attackSubmitSQL(ctx, tx, w, id, in) }); err != nil {
		t.Fatalf("control: the exactly-bound proof must be accepted: %v", err)
	}
	if got := w.requestCount(); got != 1 {
		t.Fatalf("control: want 1 request, got %d", got)
	}
	err := attackSession(rt, w, w.F1, tok, func(ctx context.Context, tx pgx.Tx) error { return attackSubmitSQL(ctx, tx, w, id, in) })
	requireCode(t, err, "AP005")
	if got := w.requestCount(); got != 1 {
		t.Fatalf("replay created a second request: %d", got)
	}
	// Cancel is actor-bound too: the initiator's GUCs alone cannot cancel.
	err = attackSession(rt, w, w.F1, "", func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'cancelled' WHERE id = $1`, id)
		return e
	})
	requireCode(t, err, "AP001")
	if got := w.request(id).State; got != StatePending {
		t.Fatalf("an unproven cancel changed the state to %s", got)
	}
	w.assertInvariants()
}

// (1c) Acting-session shape (a platform principal acting in the tenant): the
// same GUC impersonation is refused without a proof; with an exactly-bound
// platform_acting proof it is accepted (control).
func TestActorProof_K2_Adversarial_ActingSession_FailsClosed(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 2})
	w.svc = NewService(rt)
	f := processForger(t)
	causation := w.fund(10_000)
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	acting := func(proof string) error {
		return rt.WithPlatformActingInTenant(context.Background(), w.Acting.ID, w.Tenant, r.ID, OperationKind, func(ctx context.Context, tx pgx.Tx) error {
			if proof != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
					return err
				}
			}
			return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
		})
	}
	requireCode(t, acting(""), "AP001")
	requireCode(t, acting(f.Valid(t, w.Acting.ID, "tenant", w.Tenant, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash)), "AP004") // wrong scope
	requireCode(t, acting(f.Valid(t, w.Acting2.ID, "platform_acting", w.Tenant, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash)), "AP004")
	if got := w.approvalCount(r.ID); got != 0 {
		t.Fatalf("approval rows after the acting attack: %d", got)
	}
	if err := acting(f.Valid(t, w.Acting.ID, "platform_acting", w.Tenant, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash)); err != nil {
		t.Fatalf("control: an exactly-bound platform_acting proof must be accepted: %v", err)
	}
	if got := w.approvalCount(r.ID); got != 1 {
		t.Fatalf("control: want 1 approval, got %d", got)
	}
}

// (1d) The key table and the nonce table are unreadable and unwritable by the
// runtime role, and nothing the runtime role can execute mints a proof.
func TestActorProof_RuntimeRoleCannotReadKeysNoncesOrMint(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	f := processForger(t)
	_ = w
	ctx := context.Background()
	deny := func(label, sql string, args ...any) {
		t.Helper()
		err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, sql, args...)
			return e
		})
		if err == nil {
			t.Fatalf("%s: the runtime role was ALLOWED: %s", label, sql)
		}
		requireCode(t, err, "42501")
	}
	deny("read keys", `SELECT secret FROM actor_proof_keys`)
	deny("read kids", `SELECT kid FROM actor_proof_keys`)
	deny("read nonces", `SELECT nonce FROM actor_proof_nonces`)
	deny("insert key", `INSERT INTO actor_proof_keys (kid, secret) VALUES ('rt-attacker', decode(repeat('ab', 32), 'hex'))`)
	deny("update key", `UPDATE actor_proof_keys SET status = 'retired', retired_at = now()`)
	deny("delete key", `DELETE FROM actor_proof_keys`)
	deny("insert nonce", `INSERT INTO actor_proof_nonces (nonce, kid, actor, scope, tenant, operation, target, payload_hash, expires_at, consumed_txid)
		VALUES ('aaaaaaaaaaaaaaaaaaaaaa', 'k', gen_random_uuid(), 'tenant', gen_random_uuid(), 'a:b', 'new', repeat('0', 64), now(), 1)`)
	deny("delete nonces (reset replay protection)", `DELETE FROM actor_proof_nonces`)
	deny("truncate nonces", `TRUNCATE actor_proof_nonces`)
	deny("truncate keys", `TRUNCATE actor_proof_keys`)

	// The tables carry no ACL entry for the runtime role and no policy at all.
	var rtPriv bool
	var pol int
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'actor_proof_keys', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			OR has_table_privilege('igaming_runtime', 'actor_proof_nonces', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`).Scan(&rtPriv); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename IN ('actor_proof_keys', 'actor_proof_nonces')`).Scan(&pol)
	}); err != nil {
		t.Fatal(err)
	}
	if rtPriv || pol != 0 {
		t.Fatalf("runtime privilege on key/nonce tables = %v, policies = %d (want false / 0)", rtPriv, pol)
	}

	// The verifier MINTS NOTHING: every actor_proof* function returns void or
	// boolean (never a token), and calling it without/with a forged proof fails.
	var bad int
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc p WHERE p.proname LIKE 'actor_proof%' AND p.prorettype NOT IN ('void'::regtype, 'boolean'::regtype, 'trigger'::regtype)`).Scan(&bad)
	}); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d actor_proof* function(s) return something other than void/boolean/trigger", bad)
	}
	call := func(proof string) error {
		return rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if proof != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
					return err
				}
			}
			_, e := tx.Exec(ctx, `SELECT actor_proof_require($1, 'tenant', $2, 'ledger_adjustment:approve', $3, $4)`, uuid.New(), uuid.New(), uuid.NewString(), strings.Repeat("0", 64))
			return e
		})
	}
	requireCode(t, call(""), "AP001")
	requireCode(t, call("a|b|c"), "AP001")
	requireCode(t, call(f.Valid(t, uuid.New(), "tenant", uuid.New(), "ledger_adjustment:approve", uuid.NewString(), strings.Repeat("0", 64))), "AP004")
	// Only the two provable scopes exist: a valid token for scope "platform" is
	// refused by the verifier itself (AP004), whatever the triggers do.
	tidv := uuid.New()
	actv := uuid.New()
	tgt := uuid.NewString()
	pl := strings.Repeat("0", 64)
	err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, f.Valid(t, actv, "platform", tidv, "ledger_adjustment:approve", tgt, pl)); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `SELECT actor_proof_require($1, 'platform', $2, 'ledger_adjustment:approve', $3, $4)`, actv, tidv, tgt, pl)
		return e
	})
	requireCode(t, err, "AP004")
	// A SECURITY DEFINER function with a pinned search_path, owned by the owner.
	var secdef, pinned int
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE prosecdef),
			count(*) FILTER (WHERE prosecdef AND proconfig::text LIKE '%search_path=pg_catalog, public, pg_temp%')
			FROM pg_proc WHERE proname IN ('actor_proof_require', 'actor_proof_key_active')`).Scan(&secdef, &pinned)
	}); err != nil {
		t.Fatal(err)
	}
	if secdef != 2 || pinned != 2 {
		t.Fatalf("verifier functions: security definer = %d, search_path pinned = %d (want 2/2)", secdef, pinned)
	}
}

// (1e) NULL-ARM-WRITE-1 chain, reassessed under the proof requirement (ADR 0110
// section "NULL-ARM-WRITE-1"). As the runtime role, mint (a) two new
// platform_admin principals through the NULL-tenant staff_users arm and (b) two
// new tenant finance staff through the tenant arm, each with a fresh Person.
// The minted principals ARE accepted as actors by the GUC-only validator
// (that is the finding), and we additionally give them the K2 grants (the K1
// grant path is out of this change's scope) - the point of the test is that,
// with all of that, a GUC-only submit/approve of a K2 adjustment is still
// refused and posts nothing.
func TestActorProof_K2_NullArmMintedPrincipals_CannotSubmitOrApprove(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	causation := w.fund(10_000)
	ctx := context.Background()

	mintTenantStaff := func() staffMember {
		s := staffMember{ID: uuid.New(), PersonID: uuid.New(), TenantID: w.Tenant, Role: "finance"}
		if err := rt.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, s.PersonID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
				VALUES ($1, $2, $3, 'attacker-chosen', 'finance', 'active', $4)`, s.ID, w.Tenant, "mint-"+s.ID.String()+"@x.invalid", s.PersonID)
			return err
		}); err != nil {
			t.Fatalf("the tenant-arm mint is expected to succeed (documented finding): %v", err)
		}
		return s
	}
	mintPlatformAdmin := func() staffMember {
		s := staffMember{ID: uuid.New(), PersonID: uuid.New(), Role: "platform_admin"}
		if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, s.PersonID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
				VALUES ($1, NULL, $2, 'attacker-chosen', 'platform_admin', 'active', $3)`, s.ID, "mint-"+s.ID.String()+"@x.invalid", s.PersonID)
			return err
		}); err != nil {
			t.Fatalf("the NULL-arm mint is expected to succeed (documented finding NULL-ARM-WRITE-1): %v", err)
		}
		return s
	}
	m1, m2 := mintTenantStaff(), mintTenantStaff()
	a1, a2 := mintPlatformAdmin(), mintPlatformAdmin()
	// Assumption made explicit: the attacker also holds K2 grants for the minted
	// tenant staff (the K1 grant path is not protected by this change).
	w.grantTenant(m1, "ledger_adjustment:initiate")
	w.grantTenant(m2, "ledger_adjustment:approve")

	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	id := uuid.New()
	err := attackSession(rt, w, m1, "", func(ctx context.Context, tx pgx.Tx) error { return attackSubmitSQL(ctx, tx, w, id, in) })
	requireCode(t, err, "AP001")
	// A minted platform admin in the plain platform shape is refused by K2 itself.
	err = rt.WithPlatformAdmin(ctx, a1.ID, func(ctx context.Context, tx pgx.Tx) error { return attackSubmitSQL(ctx, tx, w, id, in) })
	if err == nil {
		t.Fatal("a minted platform admin submitted a K2 request")
	}
	_ = a2

	// A genuine request by a real initiator; the minted approver cannot approve.
	r, err := NewService(rt).Submit(ctxFor(w.F1), w.target(), in, Meta{RequestID: "nullarm"})
	if err != nil {
		t.Fatalf("genuine submit: %v", err)
	}
	f := processForger(t)
	for _, proof := range []string{"", prooftest.Forger{KID: f.KID, Key: make([]byte, 32)}.Valid(t, m2.ID, "tenant", w.Tenant, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash)} {
		err = attackSession(rt, w, m2, proof, func(ctx context.Context, tx pgx.Tx) error {
			return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
		})
		if err == nil {
			t.Fatal("a minted approver was accepted without a server-issued proof")
		}
		if c := pgCode(err); c != "AP001" && c != "AP002" {
			t.Fatalf("want AP001/AP002, got %v", err)
		}
	}
	if got := w.approvalCount(r.ID); got != 0 {
		t.Fatalf("approval rows from minted principals: %d", got)
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("a ledger posting exists after the minted-principal attempts: %d", got)
	}
	w.assertInvariants()
}

// Residual pin (ADR 0110, NULL-ARM-WRITE-1 conclusion). The proof binds WHO the
// server authenticated; it does not decide whether the staff_users row itself
// is genuine. If an attacker also obtains a server-issued proof for a minted
// principal (by authenticating to the HTTP API as it), the database accepts
// that principal exactly as it would a real one. This test pins the boundary so
// it cannot be mistaken for covered: it is the documented residual, not a
// defect of the proof check.
func TestActorProof_Residual_ServerIssuedProofForMintedPrincipalIsAccepted(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	w.svc = NewService(rt)
	causation := w.fund(10_000)
	ctx := context.Background()
	m := staffMember{ID: uuid.New(), PersonID: uuid.New(), TenantID: w.Tenant, Role: "finance"}
	if err := rt.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, m.PersonID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'attacker-chosen', 'finance', 'active', $4)`, m.ID, w.Tenant, "mint-"+m.ID.String()+"@x.invalid", m.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.grantTenant(m, "ledger_adjustment:approve")
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	// The Service (the server-side issuer) signs for whoever the token subject
	// names - here the minted principal, as if it had logged in over HTTP.
	out, err := w.decide(m, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("residual pin: a server-issued proof for a principal the DB believes is genuine is accepted: %v %+v", err, out)
	}
}

// (2) POSITIVE through the real Service as the runtime role: genuine two-person
// approval, distinct-person rule, exactly one posting, SUM(D)=SUM(C), projection
// = rebuild, idempotent retries (a retry of the same logical approval issues a
// fresh proof and is refused by the approval's OWN guards - never by a replayed
// nonce - and posts nothing more).
func TestActorProof_Positive_TwoPerson_RealServicePath_IdempotentRetry(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	w.svc = NewService(rt)
	causation := w.fund(10_000)
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation

	n0 := w.proofNonceCount()
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := w.proofNonceCount(); got != n0+1 {
		t.Fatalf("submit must consume exactly one nonce, consumed %d", got-n0)
	}
	// Distinct-person rule holds with a perfectly valid proof for the initiator.
	if _, err := w.decide(w.F1, r, DecisionApprove); pgCode(err) != "MA031" {
		t.Fatalf("self-approval: want MA031, got %v", err)
	}
	if got := w.proofNonceCount(); got != n0+1 {
		t.Fatalf("a refused write must not burn a nonce (the nonce row rolls back with it): %d", got-n0)
	}
	// A failed first attempt of the approval (the approver pinned a payload hash
	// it did not see): the proof was issued but the write was refused.
	if _, err := w.svc.Decide(ctxFor(w.F2), w.target(), r.ID,
		DecisionInput{Decision: DecisionApprove, PayloadHash: strings.Repeat("0", 64), ReasonCode: "k2-test"}, Meta{}); pgCode(err) != "MA031" {
		t.Fatalf("stale payload hash: want MA031, got %v", err)
	}
	// The retry with the right payload carries a FRESH proof and succeeds.
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("approval by a second, distinct person: %v %+v", err, out)
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("want exactly one posting, got %d", got)
	}
	if got := w.playerCash(); got != 9_700 {
		t.Fatalf("player_cash: want 9700 got %d", got)
	}
	nAfter := w.proofNonceCount()
	// Replayed logical approval (client retry after success): fresh proof, refused
	// by the request-state guard, exactly one posting, no second execution.
	for i := 0; i < 2; i++ {
		if _, err := w.decide(w.F2, r, DecisionApprove); err == nil {
			t.Fatal("a retried approval of an executed request was accepted")
		}
	}
	if got := w.proofNonceCount(); got != nAfter {
		t.Fatalf("refused retries consumed nonces: %d", got-nAfter)
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("retry changed the posting count: %d", got)
	}
	w.assertPostingKeys(out.Request)
	w.assertInvariants() // SUM(D) = SUM(C) and projection == rebuild
}

// (2b) POSITIVE, acting-session path + beneficiary rule + cancel, all with
// server-issued proofs: an acting platform principal approves a tenant
// initiator's request; the initiator cancels a second one (cancel is actor-bound
// and proven); a staff member whose Person is the beneficiary is still refused
// (S-12) even with a valid proof.
func TestActorProof_Positive_ActingPath_Cancel_BeneficiaryRule(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	w.svc = NewService(rt)
	causation := w.fund(10_000)
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation

	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	// Acting platform principal (service dispatches db.WithPlatformActingInTenant).
	out, err := w.svc.Decide(ctxFor(w.Acting), w.target(), r.ID,
		DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "k2-test"}, Meta{RequestID: "acting"})
	if err != nil || !out.Executed {
		t.Fatalf("acting approval with a platform_acting proof: %v %+v", err, out)
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("want exactly one posting, got %d", got)
	}
	// Cancel path (initiator only).
	r2, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Cancel(ctxFor(w.F2), w.target(), r2.ID, Meta{}); err == nil {
		t.Fatal("a non-initiator cancelled the request")
	}
	if c, err := w.svc.Cancel(ctxFor(w.F1), w.target(), r2.ID, Meta{}); err != nil || c.State != StateCancelled {
		t.Fatalf("initiator cancel with a proof: %v %+v", err, c)
	}
	// Beneficiary rule (S-12): a staff row whose Person IS the player's Person.
	ben := staffMember{ID: uuid.New(), PersonID: w.PlayerPerson, TenantID: w.Tenant, Role: "finance"}
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1,$2,$3,'x','finance','active',$4)`,
			ben.ID, w.Tenant, "ben-"+ben.ID.String()+"@x.invalid", ben.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.grantTenant(ben, "ledger_adjustment:approve")
	r3, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.decide(ben, r3, DecisionApprove); pgCode(err) != "MA032" {
		t.Fatalf("beneficiary approval with a valid proof: want MA032, got %v", err)
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("posting count changed: %d", got)
	}
	w.assertInvariants()
}
