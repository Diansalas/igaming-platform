//go:build integration

// Closure security review F-1 (migration 0175): a DEPOSIT reversal tombstone is a signed, nonce-consumed system actor
// proof bound to the tenant, provider, original reference, ledger key, correlation and the stored reversal receipt, and
// verified in SQL (ADR 0110). A forged deposit tombstone by the runtime credential is refused; payment_ref_cleared (MA020)
// counts only proof-bound tombstones; the real verified-callback path still posts its tombstone.
package payments

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// f1RuntimePool connects as the REAL runtime role (igaming_runtime) to the database of TEST_DATABASE_URL.
func f1RuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	rt, owner := os.Getenv("TEST_RUNTIME_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if rt == "" || owner == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL / TEST_DATABASE_URL not set; skipping runtime-role test")
	}
	ru, err := url.Parse(rt)
	if err != nil {
		t.Fatal(err)
	}
	ou, err := url.Parse(owner)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = ou.Path
	pool, err := db.Connect(context.Background(), ru.String(), 8, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(pool.Close)
	var role string
	var super, bypass bool
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass); err != nil {
		t.Fatal(err)
	}
	if role != "igaming_runtime" || super || bypass {
		t.Fatalf("not the runtime role: %s super=%v bypassrls=%v", role, super, bypass)
	}
	return pool
}

func f1Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

type f1World struct {
	e     *depRefEnv
	rt    *db.Pool
	forge prooftest.Forger
}

func newF1World(t *testing.T, provider string) *f1World {
	t.Helper()
	pool := testPool(t)
	e := newDepRefEnv(t, pool, provider+"-"+uuid.NewString()[:8])
	return &f1World{e: e, rt: f1RuntimePool(t), forge: prooftest.ProcessForger(t, os.Getenv("TEST_DATABASE_URL"))}
}

// receipt stores an applied deposit_reversal receipt for (provider, ref) as the runtime role (any runtime session can).
func (w *f1World) receipt(t *testing.T, tenant uuid.UUID, provider, ref string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, _, err = insertReceiptDeduped(ctx, tx, tenant, provider, ReceiptEvidence{EventType: string(CallbackEventDepositReversal),
			ProviderReference: "f1-rev-" + uuid.NewString(), OriginalProviderReference: ref, Outcome: OutcomeSucceeded,
			Amount: 1, AssetCode: "EUR"}, DispositionApplied)
		return err
	}); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	return id
}

// f1Row is the tombstone a raw INSERT writes.
type f1Row struct {
	tenant        uuid.UUID
	provider, ref string
	key           string
	corr          uuid.UUID
	callerNonce   string
	extraGUCs     map[string]string
}

func (w *f1World) row(provider, ref string) f1Row {
	return f1Row{tenant: w.e.f.tenantID, provider: provider, ref: ref, key: "tombstone:" + provider + ":" + ref, corr: uuid.New()}
}

func (r f1Row) digest(receipt uuid.UUID) string {
	return depositTombstoneDigest(r.tenant, r.provider, r.ref, r.key, r.corr, receipt)
}

// insertRaw inserts r with raw SQL as the runtime role after setting the proof GUC to tok (empty = none).
func (w *f1World) insertRaw(ctx context.Context, tx pgx.Tx, tok string, r f1Row) error {
	if tok != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); err != nil {
			return err
		}
	}
	for k, v := range r.extraGUCs {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id, actor_proof_nonce)
		VALUES ($1, 'tombstone', $2, $3, $4, $5, NULLIF($6, ''))`, r.tenant, r.key, r.provider, r.ref, r.corr, r.callerNonce)
	return err
}

func (w *f1World) tryRaw(t *testing.T, tok string, r f1Row) error {
	t.Helper()
	return w.rt.WithTenant(context.Background(), r.tenant, func(ctx context.Context, tx pgx.Tx) error { return w.insertRaw(ctx, tx, tok, r) })
}

func (w *f1World) cleared(t *testing.T, tenant uuid.UUID, provider, ref string) bool {
	t.Helper()
	var ok bool
	if err := w.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payment_ref_cleared($1, $2, $3)`, tenant, provider, ref).Scan(&ok)
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func (w *f1World) tombstones(t *testing.T, tenant uuid.UUID, provider, ref string) (n int, nonces int) {
	t.Helper()
	if err := w.e.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), count(actor_proof_nonce) FROM ledger_transactions
			WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND provider_id = $2 AND provider_tx_id = $3`, tenant, provider, ref).Scan(&n, &nonces)
	}); err != nil {
		t.Fatal(err)
	}
	return n, nonces
}

// A forged deposit tombstone on a captured-unposted deposit reference is refused for the runtime role, in every key shape,
// with a caller-chosen nonce, and with a session context that would hide the payment rows; MA020 stays in force.
func TestF1Tomb_ForgedDepositTombstone_RuntimeRefused(t *testing.T) {
	w := newF1World(t, "mock-f1-forge")
	_, ref := w.e.ambiguousBound(t, "f1-forge")
	if w.cleared(t, w.e.f.tenantID, w.e.id, ref) {
		t.Fatal("setup: the reference must not be cleared")
	}
	base := w.row(w.e.id, ref)
	casinoShape := base
	casinoShape.key = "casino-rollback:" + ref
	nonce := base
	nonce.callerNonce = prooftest.Nonce22(t)
	foreignAdmin := base
	foreignAdmin.extraGUCs = map[string]string{"app.platform_admin_principal_id": uuid.NewString()}
	for _, c := range []struct {
		name string
		row  f1Row
	}{{"deposit key shape", base}, {"casino key shape", casinoShape}, {"caller-chosen nonce", nonce}, {"platform-admin GUC", foreignAdmin}} {
		err := w.tryRaw(t, "", c.row)
		if got := f1Code(err); got != "AP001" {
			t.Fatalf("%s: a forged deposit tombstone must be refused AP001, got %q (%v)", c.name, got, err)
		}
	}
	// A token that is not a proof at all.
	if got := f1Code(w.tryRaw(t, "v1|x|y", base)); got != "AP001" {
		t.Fatalf("malformed proof: want AP001, got %q", got)
	}
	if n, _ := w.tombstones(t, w.e.f.tenantID, w.e.id, ref); n != 0 || w.cleared(t, w.e.f.tenantID, w.e.id, ref) {
		t.Fatalf("nothing was written and MA020 stays in force: n=%d", n)
	}
	// A provider the tenant has never used for payments, in the plain tenant context, is a non-payment (casino) tombstone:
	// admitted WITHOUT a proof, but it can never clear MA020 (no nonce).
	other := "mock-f1-casino-" + uuid.NewString()[:8]
	cr := w.row(other, "c-"+uuid.NewString())
	if err := w.tryRaw(t, "", cr); err != nil {
		t.Fatalf("a non-payment provider tombstone is out of this guard's scope: %v", err)
	}
	if n, nn := w.tombstones(t, w.e.f.tenantID, other, cr.ref); n != 1 || nn != 0 || w.cleared(t, w.e.f.tenantID, other, cr.ref) {
		t.Fatalf("non-payment tombstone: n=%d nonces=%d, and it must not clear", n, nn)
	}
	// The same unknown provider under a context that could hide payment rows is treated as payment (fail closed).
	hidden := w.row("mock-f1-hidden-"+uuid.NewString()[:8], "h-"+uuid.NewString())
	hidden.extraGUCs = map[string]string{"app.platform_admin_principal_id": uuid.NewString()}
	if got := f1Code(w.tryRaw(t, "", hidden)); got != "AP001" {
		t.Fatalf("a context that hides payment rows must fail closed: want AP001, got %q", got)
	}
	// A provider known only by a stored receipt (no attempt) is a payment provider too.
	recOnly := "mock-f1-rec-" + uuid.NewString()[:8]
	rr := w.row(recOnly, "r-"+uuid.NewString())
	w.receipt(t, w.e.f.tenantID, recOnly, "unrelated-"+uuid.NewString())
	if got := f1Code(w.tryRaw(t, "", rr)); got != "AP001" {
		t.Fatalf("receipt-only payment provider: want AP001, got %q", got)
	}
}

// Every binding of the proof, exercised with an independent, correctly keyed signer (the key is the credential).
func TestF1Tomb_BindingMatrix(t *testing.T) {
	w := newF1World(t, "mock-f1-bind")
	_, ref := w.e.ambiguousBound(t, "f1-bind")
	tenant, prov := w.e.f.tenantID, w.e.id
	rec := w.receipt(t, tenant, prov, ref)
	otherRec := w.receipt(t, tenant, prov, "other-"+uuid.NewString())
	r := w.row(prov, ref)
	good := r.digest(rec)
	op, actor := actorproof.OpDepositReversalTombstone, actorproof.SystemActorDepositReversal()
	tenantB := uuid.New()
	wrongKey := prooftest.Forger{KID: w.forge.KID, Key: prooftest.NewKey(t)}
	now := time.Now().Unix()
	cases := []struct {
		name string
		tok  string
		want string
	}{
		{"wrong key", wrongKey.Valid(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(), good), "AP002"},
		{"another tenant", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenantB, op, rec.String(), good), "AP004"},
		{"another provider", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(),
			depositTombstoneDigest(tenant, prov+"x", ref, r.key, r.corr, rec)), "AP004"},
		{"another reference", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(),
			depositTombstoneDigest(tenant, prov, ref+"x", r.key, r.corr, rec)), "AP004"},
		{"another correlation", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(),
			depositTombstoneDigest(tenant, prov, ref, r.key, uuid.New(), rec)), "AP004"},
		{"another ledger key", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(),
			depositTombstoneDigest(tenant, prov, ref, r.key+"x", r.corr, rec)), "AP004"},
		{"target swapped, digest of the first receipt", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, otherRec.String(), good), "AP004"},
		{"receipt of another reference", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, otherRec.String(), r.digest(otherRec)), "DT001"},
		{"target not a receipt", w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, "new", r.digest(uuid.Nil)), "AP004"},
		{"dispatch-gate operation", w.forge.Valid(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeSystem, tenant, actorproof.OpDispatchGateAllow, rec.String(), good), "AP004"},
		{"dispatch-gate actor", w.forge.Valid(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeSystem, tenant, op, rec.String(), good), "AP004"},
		{"staff scope", w.forge.Valid(t, actor, actorproof.ScopeTenant, tenant, op, rec.String(), good), "AP004"},
		{"another actor", w.forge.Valid(t, uuid.New(), actorproof.ScopeSystem, tenant, op, rec.String(), good), "AP004"},
		{"expired", w.forge.Token(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(), good, now-120, now-60), "AP003"},
		{"lifetime over 60 s", w.forge.Token(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(), good, now, now+600), "AP003"},
	}
	for _, c := range cases {
		err := w.tryRaw(t, c.tok, r)
		if got := f1Code(err); got != c.want {
			t.Fatalf("%s: want %s, got %q (%v)", c.name, c.want, got, err)
		}
	}
	if n, _ := w.tombstones(t, tenant, prov, ref); n != 0 || w.cleared(t, tenant, prov, ref) {
		t.Fatalf("no refused case may write or clear: n=%d", n)
	}
	// The system scope of the new operation is not usable to forge a dispatch-gate allow either (verifier block).
	var code string
	if err := w.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`,
			w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, actorproof.OpDispatchGateAllow, rec.String(), good)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT actor_proof_require($1, 'system', $2, 'withdrawal_dispatch_gate:allow', $3, $4)`, actor, tenant, rec.String(), good)
		return err
	}); err != nil {
		code = f1Code(err)
	}
	if code != "AP004" {
		t.Fatalf("the deposit system actor must not prove a dispatch allow: want AP004, got %q", code)
	}

	// Valid proof: admitted, nonce recorded, MA020 cleared.
	tok := w.forge.Valid(t, actor, actorproof.ScopeSystem, tenant, op, rec.String(), good)
	// A use that rolls back does not burn the proof.
	errRB := errors.New("rollback")
	if err := w.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := w.insertRaw(ctx, tx, tok, r); err != nil {
			return err
		}
		return errRB
	}); !errors.Is(err, errRB) {
		t.Fatalf("rolled-back use: %v", err)
	}
	if err := w.tryRaw(t, tok, r); err != nil {
		t.Fatalf("a valid proof must be admitted (after a rolled-back use): %v", err)
	}
	n, nn := w.tombstones(t, tenant, prov, ref)
	if n != 1 || nn != 1 || !w.cleared(t, tenant, prov, ref) {
		t.Fatalf("proof-bound tombstone: n=%d nonces=%d cleared=%v", n, nn, w.cleared(t, tenant, prov, ref))
	}
	var stored string
	if err := w.e.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_proof_nonce FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3`,
			tenant, prov, ref).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if stored != strings.Split(tok, "|")[10] {
		t.Fatalf("the recorded nonce must be the proof's nonce: %q", stored)
	}
	// Replay of the committed proof: AP005 (the verifier runs before the row's unique keys).
	if got := f1Code(w.tryRaw(t, tok, r)); got != "AP005" {
		t.Fatalf("replay: want AP005, got %q", got)
	}
	// Other tenant: tenant B has no way to clear A's reference, and A's tombstone does not clear B.
	if w.cleared(t, tenantB, prov, ref) {
		t.Fatal("a tombstone of tenant A must not clear tenant B")
	}
}

// Four concurrent uses of one proof for one row: exactly one wins.
func TestF1Tomb_ConcurrentUseOfOneProof(t *testing.T) {
	w := newF1World(t, "mock-f1-conc")
	_, ref := w.e.ambiguousBound(t, "f1-conc")
	tenant, prov := w.e.f.tenantID, w.e.id
	rec := w.receipt(t, tenant, prov, ref)
	r := w.row(prov, ref)
	tok := w.forge.Valid(t, actorproof.SystemActorDepositReversal(), actorproof.ScopeSystem, tenant, actorproof.OpDepositReversalTombstone, rec.String(), r.digest(rec))
	var wg sync.WaitGroup
	errs := make([]error, 4)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = w.tryRaw(t, tok, r)
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case f1Code(err) == "AP005" || f1Code(err) == "23505":
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if n, nn := w.tombstones(t, tenant, prov, ref); ok != 1 || n != 1 || nn != 1 {
		t.Fatalf("exactly one winner: ok=%d n=%d nonces=%d", ok, n, nn)
	}
}

// The real verified-callback path posts a proof-bound tombstone; a duplicate delivery and a second reversal event for the
// same original are idempotent; concurrent identical deliveries write one tombstone.
func TestF1Tomb_RealReversalCallback_PostsSignedTombstone_Idempotent(t *testing.T) {
	w := newF1World(t, "mock-f1-real")
	tenant, prov := w.e.f.tenantID, w.e.id
	orig := "f1-real-orig-" + uuid.NewString()
	ev := ReceiptEvidence{EventType: string(CallbackEventDepositReversal), ProviderReference: "f1-real-rev-" + uuid.NewString(),
		OriginalProviderReference: orig, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}
	if d, err := iwApplyReceiptInTx(t, w.e, ev); err != nil || d != DispositionApplied {
		t.Fatalf("reversal callback: %v %v", d, err)
	}
	if n, nn := w.tombstones(t, tenant, prov, orig); n != 1 || nn != 1 || !w.cleared(t, tenant, prov, orig) {
		t.Fatalf("the real path posts one proof-bound tombstone: n=%d nonces=%d", n, nn)
	}
	if d, err := iwApplyReceiptInTx(t, w.e, ev); err != nil || d != DispositionDuplicateEffect {
		t.Fatalf("duplicate delivery: %v %v", d, err)
	}
	ev2 := ev
	ev2.ProviderReference = "f1-real-rev2-" + uuid.NewString()
	if d, err := iwApplyReceiptInTx(t, w.e, ev2); err != nil || d != DispositionApplied {
		t.Fatalf("second reversal event for the same original: %v %v", d, err)
	}
	if n, nn := w.tombstones(t, tenant, prov, orig); n != 1 || nn != 1 {
		t.Fatalf("still one tombstone: n=%d nonces=%d", n, nn)
	}
	// A late success of the original reference is still rejected by the tombstone (semantics unchanged).
	var exists bool
	if err := w.e.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exists, err = tombstoneExists(ctx, tx, tenant, prov, orig)
		return err
	}); err != nil || !exists {
		t.Fatalf("tombstoneExists: %v %v", exists, err)
	}

	// Concurrent identical deliveries of a fresh reversal.
	orig3 := "f1-real-conc-" + uuid.NewString()
	ev3 := ReceiptEvidence{EventType: string(CallbackEventDepositReversal), ProviderReference: "f1-real-rev3-" + uuid.NewString(),
		OriginalProviderReference: orig3, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}
	var wg sync.WaitGroup
	ds := make([]ReceiptDisposition, 4)
	errs := make([]error, 4)
	for i := range ds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ds[i], errs[i] = iwApplyReceiptInTx(t, w.e, ev3)
		}(i)
	}
	wg.Wait()
	applied := 0
	for i := range ds {
		if errs[i] == nil && ds[i] == DispositionApplied {
			applied++
		} else if errs[i] != nil && f1Code(errs[i]) != "23505" && f1Code(errs[i]) != "40001" {
			t.Fatalf("concurrent delivery %d: %v %v", i, ds[i], errs[i])
		}
	}
	if n, nn := w.tombstones(t, tenant, prov, orig3); applied != 1 || n != 1 || nn != 1 {
		t.Fatalf("concurrent deliveries: applied=%d n=%d nonces=%d (%v)", applied, n, nn, errs)
	}
}

// No issuer: the reversal callback fails closed (nothing stored, a retryable error the webhook maps to 503) and goes
// through on the retry once the key is back.
func TestF1Tomb_NoIssuer_FailsClosed_ThenRetrySucceeds(t *testing.T) {
	w := newF1World(t, "mock-f1-noiss")
	tenant, prov := w.e.f.tenantID, w.e.id
	orig := "f1-noiss-" + uuid.NewString()
	ev := ReceiptEvidence{EventType: string(CallbackEventDepositReversal), ProviderReference: "f1-noiss-rev-" + uuid.NewString(),
		OriginalProviderReference: orig, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}
	saved := actorproof.Default()
	actorproof.SetDefault(nil)
	_, err := iwApplyReceiptInTx(t, w.e, ev)
	actorproof.SetDefault(saved)
	if !errors.Is(err, ErrDepositTombstoneProofUnavailable) {
		t.Fatalf("no issuer: want ErrDepositTombstoneProofUnavailable, got %v", err)
	}
	var receipts int
	if err := w.e.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_id = $2 AND original_provider_reference = $3`,
			tenant, prov, orig).Scan(&receipts)
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.tombstones(t, tenant, prov, orig); n != 0 || receipts != 0 {
		t.Fatalf("fail closed means nothing stored (the PSP redelivers): tombstones=%d receipts=%d", n, receipts)
	}
	if d, err := iwApplyReceiptInTx(t, w.e, ev); err != nil || d != DispositionApplied {
		t.Fatalf("the redelivery with the issuer back goes through: %v %v", d, err)
	}
	if n, nn := w.tombstones(t, tenant, prov, orig); n != 1 || nn != 1 {
		t.Fatalf("after retry: n=%d nonces=%d", n, nn)
	}
}

// Catalog: the guard is the LAST BEFORE INSERT row trigger of ledger_transactions, not SECURITY DEFINER, pinned search_path;
// the verifier stays the only SECURITY DEFINER function of the chain.
func TestF1Tomb_DB_Catalog(t *testing.T) {
	pool := testPool(t)
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var last string
		if err := tx.QueryRow(ctx, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'ledger_transactions'::regclass AND NOT tgisinternal
			AND (tgtype & 2) = 2 AND (tgtype & 1) = 1 AND (tgtype & 4) = 4 ORDER BY tgname DESC LIMIT 1`).Scan(&last); err != nil {
			return err
		}
		if last != "ledger_transactions_zz_deposit_tombstone_proof_guard" {
			t.Fatalf("the proof guard must be the last BEFORE INSERT row trigger, got %s", last)
		}
		var secdef bool
		var cfg []string
		if err := tx.QueryRow(ctx, `SELECT prosecdef, COALESCE(proconfig, '{}') FROM pg_proc
			WHERE oid = 'ledger_transactions_deposit_tombstone_proof_guard()'::regprocedure`).Scan(&secdef, &cfg); err != nil {
			return err
		}
		if secdef || len(cfg) != 1 || cfg[0] != "search_path=pg_catalog, public, pg_temp" {
			t.Fatalf("guard: secdef=%v cfg=%v", secdef, cfg)
		}
		var src string
		if err := tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE oid = 'payment_ref_cleared(uuid, text, text)'::regprocedure`).Scan(&src); err != nil {
			return err
		}
		if !strings.Contains(src, "actor_proof_nonce IS NOT NULL") {
			t.Fatalf("payment_ref_cleared must count proof-bound tombstones only: %s", src)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
