//go:build integration

// PRH-2 R2 (migration 0116, ADR 0108, TRIGGER-SEARCH-PATH-1): the K2 four-eyes
// bypass reproduced by the security review (k3-delta-security.md finding 1) -
// the runtime role TEMP-shadowing staff_users / staff_capability_grants /
// staff_capability_grant_requests to forge a two-person approval of a real
// compensating debit - must now fail at the TEMP creation step; and a genuine
// two-person adjustment must still work, exactly once. Everything below talks
// to the database as the REAL runtime role (TEST_RUNTIME_DATABASE_URL), asserted
// neither superuser nor BYPASSRLS; fixtures use the owner pool.
//
// Non-vacuity is proven in-repo: TestTempRevoke_K2ShadowAttack_SucceedsWhenTempIsGrantedBack
// re-opens TEMP on a THROWAWAY scratch database and shows the very same forgery
// executes a compensating debit there.
package adjustment

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func tempRevokeRuntimePoolAt(t *testing.T, rtURL string) *db.Pool {
	t.Helper()
	rt, err := db.Connect(context.Background(), rtURL, 2, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("must run as a role that is neither superuser nor BYPASSRLS (super=%v bypassrls=%v)", super, bypass)
	}
	return rt
}

func tempRevokeRuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role test")
	}
	return tempRevokeRuntimePoolAt(t, u)
}

// adjustmentLedgerTxCount counts manual_adjustment ledger transactions that have
// a leg on the world's wallet (read through the owner pool).
func (w *world) adjustmentLedgerTxCount() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(DISTINCT lt.id) FROM ledger_transactions lt
			JOIN ledger_entries le ON le.ledger_transaction_id = lt.id
			JOIN ledger_accounts la ON la.id = le.ledger_account_id
			WHERE lt.transaction_type = 'manual_adjustment' AND la.wallet_id = $1`, w.Wallet).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// shadowK2Tables is the attack's setup: TEMP copies of the three tables the
// unpinned K2 guards read, then two invented staff X (initiate) and Y (approve)
// that have no row in public.staff_users. It returns the first error.
func shadowK2Tables(rt *db.Pool, w *world, x, y uuid.UUID) error {
	px, py := uuid.New(), uuid.New()
	return rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`CREATE TEMP TABLE staff_users AS SELECT * FROM public.staff_users WITH NO DATA`,
			`CREATE TEMP TABLE staff_capability_grants AS SELECT * FROM public.staff_capability_grants WITH NO DATA`,
			`CREATE TEMP TABLE staff_capability_grant_requests AS SELECT * FROM public.staff_capability_grant_requests WITH NO DATA`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		for _, s := range []struct {
			id, p uuid.UUID
			c     string
		}{{x, px, "ledger_adjustment:initiate"}, {y, py, "ledger_adjustment:approve"}} {
			req := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1,$2,$3,'x','finance','active',$4)`,
				s.id, w.Tenant, "forged-"+s.id.String()+"@x", s.p); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.staff_capability_grant_requests (id, grantee_person_id) VALUES ($1,$2)`, req, s.p); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.staff_capability_grants (id, tenant_id, grantee_staff_id, grantee_scope, capability, request_id, valid_from)
				VALUES (gen_random_uuid(),$1,$2,'tenant',$3,$4, now()-interval '1 day')`, w.Tenant, s.id, s.c, req); err != nil {
				return err
			}
		}
		return nil
	})
}

// (3) The attack. The three TEMP shadows cannot even be created by the runtime
// role (42501). Defence in depth, WITHOUT any shadow in place: the forged
// identities are refused by the real guards with the real codes, an ungranted
// real staff member is refused with MA003, and no compensating entry is posted.
// (assertInvariants - debits == credits - guards ledger integrity; it cannot
// detect a forged approval, so the posting count and the codes are the evidence.)
func TestTempRevoke_K2ShadowAttack_FailsAtTempCreation_NoCompensatingEntry(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	causation := w.fund(10_000)
	before := w.playerCash()

	X, Y := uuid.New(), uuid.New()
	err := shadowK2Tables(rt, w, X, Y)
	if err == nil {
		t.Fatal("the RUNTIME role created the TEMP shadows: the K2 four-eyes bypass is open")
	}
	requireCode(t, err, "42501")

	// Defence in depth (no shadow exists): real codes, not "any error".
	svc := NewService(rt)
	in := SubmitInput{WalletID: w.Wallet, AssetCode: w.Asset, Direction: DirectionDebitPlayer, Amount: 300,
		ReasonCode: ReasonCompensatingEntry, CausationTransactionID: &causation, EvidenceRefHash: evidenceFor(ReasonCompensatingEntry), Note: "forged"}
	forged := staffMember{ID: X, PersonID: uuid.New(), TenantID: w.Tenant, Role: "finance"}
	if _, err := svc.Submit(ctxFor(forged), w.target(), in, Meta{RequestID: "temp-revoke"}); err == nil {
		t.Fatal("a forged initiator (no public.staff_users row) was accepted by Submit")
	} else {
		requireCode(t, err, "CG001")
	}
	// A real staff member without the initiate grant: MA003.
	if _, err := svc.Submit(ctxFor(w.FinanceNoGrant), w.target(), in, Meta{RequestID: "temp-revoke"}); err == nil {
		t.Fatal("a staff member without the initiate grant was accepted by Submit")
	} else {
		requireCode(t, err, "MA003")
	}
	// A real request, then a forged approver: refused with CG001 as well.
	real, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("setup: genuine submit: %v", err)
	}
	forgedApprover := staffMember{ID: Y, PersonID: uuid.New(), TenantID: w.Tenant, Role: "finance"}
	if _, err := svc.Decide(ctxFor(forgedApprover), w.target(), real.ID,
		DecisionInput{Decision: DecisionApprove, PayloadHash: real.PayloadHash, ReasonCode: "temp-revoke"}, Meta{RequestID: "temp-revoke"}); err == nil {
		t.Fatal("a forged approver was accepted by Decide")
	} else {
		requireCode(t, err, "CG001")
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("a compensating ledger transaction exists after the attack: %d", got)
	}
	if got := w.playerCash(); got != before {
		t.Fatalf("player_cash changed %d -> %d", before, got)
	}
	w.assertInvariants()
}

// Non-vacuity of the attack test, in the repo: on a THROWAWAY scratch database
// (never the shared one) PUBLIC's TEMP is granted back by the owner, and the very
// same forgery - two invented staff, three TEMP shadows - executes a real
// compensating debit with no real approver. With 0116's end state it cannot.
func TestTempRevoke_K2ShadowAttack_SucceedsWhenTempIsGrantedBack(t *testing.T) {
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set")
	}
	scratchURL := scratchdb.New(t, "temprevk2_")
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	dbName := strings.TrimPrefix(su.Path, "/")
	ru, err := url.Parse(rtBase)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = su.Path

	ctx := context.Background()
	owner, err := db.Connect(ctx, scratchURL, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	// The runtime role's grants on the scratch database (the priv_db.sh pattern;
	// database-level GRANTs only, no role is created or altered).
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := owner.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// 0116 is in force on the scratch DB: the attack is refused here too ...
	rt := tempRevokeRuntimePoolAt(t, ru.String())
	w := newWorldOn(t, owner, worldOpts{base: 1})
	causation := w.fund(10_000)
	X, Y := uuid.New(), uuid.New()
	if err := shadowK2Tables(rt, w, X, Y); err == nil {
		t.Fatal("the shadows were created on a database with 0116 applied")
	} else {
		requireCode(t, err, "42501")
	}
	// ... and re-opening TEMP (the owner side) makes the forgery work.
	if _, err := owner.Raw().Exec(ctx, `GRANT TEMPORARY ON DATABASE `+pgx.Identifier{dbName}.Sanitize()+` TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	// The shadows must live in the SAME session as the forged Submit/Decide, so the
	// attack runs on a single-connection runtime pool (session-scoped TEMP tables).
	rt1, err := db.Connect(ctx, ru.String(), 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt1.Close)
	if err := forgeOnSessionPool(t, rt1, w, X, Y, causation); err != nil {
		t.Fatal(err)
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("non-vacuity: the forged approval must have posted exactly one compensating debit, got %d", got)
	}
	if got := w.playerCash(); got != 9_700 {
		t.Fatalf("non-vacuity: the forged debit must have reduced player_cash to 9700, got %d", got)
	}
}

// forgeOnSessionPool creates the shadows with session scope (not
// transaction scope) on the pool's single connection and then runs the forged
// Submit + Decide through adjustment.Service on that same pool, so the shadows
// are visible to every statement. It reports an error unless the forgery
// executed.
func forgeOnSessionPool(t *testing.T, rt1 *db.Pool, w *world, x, y, causation uuid.UUID) error {
	t.Helper()
	ctx := context.Background()
	px, py := uuid.New(), uuid.New()
	conn, err := rt1.Raw().Acquire(ctx)
	if err != nil {
		return err
	}
	for _, q := range []string{
		`CREATE TEMP TABLE staff_users AS SELECT * FROM public.staff_users WITH NO DATA`,
		`CREATE TEMP TABLE staff_capability_grants AS SELECT * FROM public.staff_capability_grants WITH NO DATA`,
		`CREATE TEMP TABLE staff_capability_grant_requests AS SELECT * FROM public.staff_capability_grant_requests WITH NO DATA`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			conn.Release()
			return err
		}
	}
	for _, s := range []struct {
		id, p uuid.UUID
		c     string
	}{{x, px, "ledger_adjustment:initiate"}, {y, py, "ledger_adjustment:approve"}} {
		req := uuid.New()
		if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1,$2,$3,'x','finance','active',$4)`,
			s.id, w.Tenant, "forged-"+s.id.String()+"@x", s.p); err != nil {
			conn.Release()
			return err
		}
		if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.staff_capability_grant_requests (id, grantee_person_id) VALUES ($1,$2)`, req, s.p); err != nil {
			conn.Release()
			return err
		}
		if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.staff_capability_grants (id, tenant_id, grantee_staff_id, grantee_scope, capability, request_id, valid_from)
			VALUES (gen_random_uuid(),$1,$2,'tenant',$3,$4, now()-interval '1 day')`, w.Tenant, s.id, s.c, req); err != nil {
			conn.Release()
			return err
		}
	}
	conn.Release() // the single pooled connection keeps its session (and the temp tables)

	svc := NewService(rt1)
	in := SubmitInput{WalletID: w.Wallet, AssetCode: w.Asset, Direction: DirectionDebitPlayer, Amount: 300,
		ReasonCode: ReasonCompensatingEntry, CausationTransactionID: &causation, EvidenceRefHash: evidenceFor(ReasonCompensatingEntry), Note: "forged"}
	fx := staffMember{ID: x, PersonID: px, TenantID: w.Tenant, Role: "finance"}
	fy := staffMember{ID: y, PersonID: py, TenantID: w.Tenant, Role: "finance"}
	req, err := svc.Submit(ctxFor(fx), w.target(), in, Meta{RequestID: "temp-revoke-vacuity"})
	if err != nil {
		t.Fatalf("non-vacuity: with TEMP granted back the forged Submit must be accepted (as the security review reproduced): %v", err)
	}
	out, err := svc.Decide(ctxFor(fy), w.target(), req.ID,
		DecisionInput{Decision: DecisionApprove, PayloadHash: req.PayloadHash, ReasonCode: "temp-revoke-vacuity"}, Meta{RequestID: "temp-revoke-vacuity"})
	if err != nil {
		t.Fatalf("non-vacuity: forged Decide: %v", err)
	}
	if !out.Executed {
		t.Fatalf("non-vacuity: the forged approval must execute, got %+v", out)
	}
	return nil
}

// (4) Positive: a genuine two-person adjustment, driven through the RUNTIME role,
// still submits, is approved by the second Person and executes exactly once;
// self-approval and single-person attempts remain refused.
func TestTempRevoke_GenuineTwoPersonAdjustment_StillWorks_ExactlyOnce(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	w.svc = NewService(rt) // the service speaks as the runtime role; fixtures stay on the owner pool
	causation := w.fund(10_000)

	in := w.credit(300, ReasonCompensatingEntry)
	in.Direction = DirectionDebitPlayer
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("genuine submit as the runtime role: %v", err)
	}
	if r.State != StatePending || r.RequiredAtSubmission != 1 {
		t.Fatalf("unexpected pinned request: %+v", r)
	}
	// Single person / self-approval: refused, nothing executed.
	if _, err := w.decide(w.F1, r, DecisionApprove); pgCode(err) != "MA031" {
		t.Fatalf("self-approval: want MA031, got %v", err)
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("self-approval posted %d transactions", got)
	}
	// A staff member with no grant cannot approve either.
	if _, err := w.decide(w.FinanceNoGrant, r, DecisionApprove); pgCode(err) != "MA003" {
		t.Fatalf("a staff member without the approve grant: want MA003, got %v", err)
	}
	// The second, distinct Person approves: executed in the approval's own tx.
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil {
		t.Fatalf("genuine approval as the runtime role: %v", err)
	}
	if !out.Executed || out.Request.State != StateExecuted || out.Request.LedgerTransactionID == nil {
		t.Fatalf("expected executed, got %+v", out)
	}
	if got := w.playerCash(); got != 9_700 {
		t.Fatalf("player_cash after the debit: want 9700 got %d", got)
	}
	// Exactly once: a replayed decision does not execute again.
	if _, err := w.decide(w.F3, r, DecisionApprove); err == nil {
		t.Fatal("a decision on an already-executed request was accepted")
	}
	if got := w.adjustmentLedgerTxCount(); got != 1 {
		t.Fatalf("want exactly 1 manual_adjustment transaction, got %d", got)
	}
	if got := w.playerCash(); got != 9_700 {
		t.Fatalf("player_cash after the replay: want 9700 got %d", got)
	}
	w.assertPostingKeys(out.Request)
	w.assertInvariants()
}
