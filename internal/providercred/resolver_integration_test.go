//go:build integration

// The real inbound resolver (ADR 0093 §4; security review §5; QA W2a plan):
// row counts, KeyImplicit, expiry, immediate revocation in every cache and
// breaker state, fail-closed store outage, fingerprint integrity, the
// pinned single handle read, and the pool-pinning bound.
package providercred

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/devfile"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// recordingTx wraps a pgx.Tx and records every statement text it runs.
type recordingTx struct {
	pgx.Tx
	mu    sync.Mutex
	stmts []string
}

func (r *recordingTx) record(sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, sql)
}

func (r *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.record(sql)
	return r.Tx.Exec(ctx, sql, args...)
}

func (r *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.record(sql)
	return r.Tx.Query(ctx, sql, args...)
}

func (r *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.record(sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

func (r *recordingTx) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}

func TestResolver_KeyFromHeader_ExactlyOneRow(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	s := f.spec(tenant, "acme", "k1")
	s.vendorAcct = ptr("merchant-7")
	h, secret := f.register(s)

	set, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !bytes.Equal(set.Active.Secret, secret) || set.Active.KeyID != "k1" || set.Active.TenantID != tenant ||
		set.Active.ProviderID != "acme" || set.Active.Fingerprint != h.Fingerprint || set.Active.BoundAccountID != "merchant-7" || set.Previous != nil {
		t.Fatalf("resolved %v", set)
	}
	for name, call := range map[string]func() error{
		"unknown key id": func() error {
			_, err := f.resolve("casino", tenant, "acme", "k2", webhookauth.KeyFromHeader)
			return err
		},
		"other provider": func() error {
			_, err := f.resolve("casino", tenant, "other", "k1", webhookauth.KeyFromHeader)
			return err
		},
		"other domain": func() error {
			_, err := f.resolve("kyc", tenant, "acme", "k1", webhookauth.KeyFromHeader)
			return err
		},
		"other tenant": func() error {
			_, err := f.resolve("casino", f.tenant(), "acme", "k1", webhookauth.KeyFromHeader)
			return err
		},
		"empty key id": func() error {
			_, err := f.resolve("casino", tenant, "acme", "", webhookauth.KeyFromHeader)
			return err
		},
	} {
		if err := call(); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
			t.Fatalf("%s: want credential_unavailable, got %v", name, err)
		}
	}
	// The outbound purpose never serves inbound.
	so := f.spec(tenant, "acme2", "k1")
	so.purpose = PurposeOutboundAPI
	f.register(so)
	if _, err := f.resolve("casino", tenant, "acme2", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("an outbound_api handle must never verify inbound, got %v", err)
	}
}

// rotateInbound registers a new active key for an inbound binding, with
// the predecessor demoted to verify_only until predNotAfter.
func (f *fx) rotateInbound(tenant uuid.UUID, provider, newKey string, pred Handle, predNotAfter time.Time) (Handle, []byte) {
	f.t.Helper()
	s := f.spec(tenant, provider, newKey)
	s.predecessor, s.disposition, s.predNotAfter, s.reason = &pred.ID, DispositionVerifyOnly, &predNotAfter, RequestReasonScheduledRotation
	return f.register(s)
}

func TestResolver_KeyImplicit_Selection(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h1, s1 := f.register(f.spec(tenant, "acme", "k1"))

	set, err := f.resolve("casino", tenant, "acme", "", webhookauth.KeyImplicit)
	if err != nil || set.Active.KeyID != "k1" || set.Previous != nil || !bytes.Equal(set.Active.Secret, s1) {
		t.Fatalf("single active key: %v %v", set, err)
	}

	predNotAfter := time.Now().Add(3 * time.Second).Truncate(time.Microsecond)
	h2, s2 := f.rotateInbound(tenant, "acme", "k2", h1, predNotAfter)
	set, err = f.resolve("casino", tenant, "acme", "", webhookauth.KeyImplicit)
	if err != nil || set.Active.KeyID != "k2" || !bytes.Equal(set.Active.Secret, s2) || set.Active.NotAfter != (time.Time{}) {
		t.Fatalf("active after rotation: %v %v", set, err)
	}
	if set.Previous == nil || set.Previous.KeyID != "k1" || !bytes.Equal(set.Previous.Secret, s1) || !set.Previous.NotAfter.Equal(predNotAfter) {
		t.Fatalf("the verify_only predecessor must be offered within its window: %v", set)
	}
	// KeyFromHeader for the old key id still resolves within the window
	// (exactly one row, the named one).
	if set, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); err != nil || set.Previous != nil {
		t.Fatalf("header selection of the predecessor: %v %v", set, err)
	}

	// After not_after the predecessor is no longer offered (DB clock).
	time.Sleep(time.Until(predNotAfter) + 300*time.Millisecond)
	set, err = f.resolve("casino", tenant, "acme", "", webhookauth.KeyImplicit)
	if err != nil || set.Previous != nil || set.Active.KeyID != "k2" {
		t.Fatalf("after not_after only the active key remains: %v %v", set, err)
	}

	// No active key at all: fail closed (a lone verify_only is never
	// promoted).
	f.revoke(tenant, h2.ID)
	if _, err := f.resolve("casino", tenant, "acme", "", webhookauth.KeyImplicit); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("no active key must fail closed, got %v", err)
	}
	// A key id passed with KeyImplicit is refused (selection never from
	// the request).
	if _, err := f.resolve("casino", tenant, "acme", "k2", webhookauth.KeyImplicit); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("KeyImplicit with a key id must fail closed, got %v", err)
	}
}

// TestResolver_KeyImplicit_VerifiesThroughPlatform runs a KeyImplicit
// reference scheme end to end through ResolveCredentials + VerifyInbound
// with the real resolver: a callback signed with the predecessor verifies
// inside the overlap, and the log-able matched key id is the predecessor's.
func TestResolver_KeyImplicit_VerifiesThroughPlatform(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h1, s1 := f.register(f.spec(tenant, "acme", "k1"))
	f.rotateInbound(tenant, "acme", "k2", h1, time.Now().Add(time.Hour))
	scheme := implicitTestScheme{}
	in := webhookauth.Inbound{TenantID: tenant, ProviderID: "acme", Body: []byte(`{"e":1}`)}
	in.Header = map[string][]string{"X-Sig": {signFor(s1, in.Body)}}
	err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		m, authErr := webhookauth.ExtractInbound(scheme, in)
		if authErr != nil {
			return authErr
		}
		set, authErr := webhookauth.ResolveCredentials(ctx, tx, scheme, f.sub.Resolver("casino"), in, m)
		if authErr != nil {
			return authErr
		}
		cred, authErr := webhookauth.VerifyInbound(scheme, set, in, m, time.Now())
		if authErr != nil {
			return authErr
		}
		if cred.KeyID != "k1" {
			return fmt.Errorf("verified key id %q, want the predecessor k1", cred.KeyID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestResolver_Expiry: not_before in the future and not_after passed are
// both excluded by the handle read (DB clock).
func TestResolver_Expiry(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	future := f.spec(tenant, "future", "k1")
	future.notBefore = time.Now().Add(time.Hour)
	f.register(future)
	if _, err := f.resolve("casino", tenant, "future", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("not_before in the future must not resolve, got %v", err)
	}
	expiring := f.spec(tenant, "expiring", "k1")
	na := time.Now().Add(3 * time.Second)
	expiring.notAfter = &na
	f.register(expiring)
	if _, err := f.resolve("casino", tenant, "expiring", "k1", webhookauth.KeyFromHeader); err != nil {
		t.Fatalf("inside the window it resolves: %v", err)
	}
	time.Sleep(time.Until(na) + 300*time.Millisecond)
	if _, err := f.resolve("casino", tenant, "expiring", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("after not_after it must not resolve, got %v", err)
	}
}

func TestResolver_RevocationImmediate(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, _ := f.register(f.spec(tenant, "acme", "k1"))
	if _, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); err != nil {
		t.Fatal(err) // warms the cache
	}
	f.revoke(tenant, h.ID)
	calls := f.mem.Calls()
	if _, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("a revoked handle must stop resolving on the very next call, got %v", err)
	}
	if f.mem.Calls() != calls {
		t.Fatal("a revoked handle must never reach the store")
	}
}

// tripStoreBreaker opens the memory backend's breaker by failing three
// fresh refs of an unrelated binding.
func (f *fx) tripStoreBreaker(t *testing.T) {
	t.Helper()
	other := f.tenant()
	var handles []Handle
	for i := 0; i < secretstore.BreakerTripThreshold; i++ {
		h, _ := f.register(f.spec(other, fmt.Sprintf("trip%d", i), "k1"))
		handles = append(handles, h)
	}
	for _, h := range handles {
		f.mem.FailRef(h.SecretRef, secretstore.ClassUnavailable)
		if _, err := f.resolve("casino", other, h.ProviderID, "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialStoreUnavailable) {
			t.Fatalf("expected a store failure, got %v", err)
		}
	}
	if s := f.sub.Fetcher().BreakerState("memory"); s != "open" {
		t.Fatalf("breaker = %s, want open", s)
	}
}

func TestResolver_RevokeImmediateWhileBreakerOpen(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, secret := f.register(f.spec(tenant, "acme", "k1"))
	if _, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); err != nil {
		t.Fatal(err)
	}
	f.tripStoreBreaker(t)
	// Cached, breaker open: still served (fingerprint compared)...
	set, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader)
	if err != nil || !bytes.Equal(set.Active.Secret, secret) {
		t.Fatalf("a cached credential is served while the breaker is open: %v", err)
	}
	// ...until it is revoked: the handle read runs in every breaker state.
	f.revoke(tenant, h.ID)
	calls := f.mem.Calls()
	if _, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("revocation must be immediate while the breaker is open, got %v", err)
	}
	if f.mem.Calls() != calls {
		t.Fatal("no store call may happen for a revoked handle")
	}
}

func TestResolver_StoreOutageFailsClosed(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.register(f.spec(tenant, "acme", "k1"))
	f.mem.FailAll(secretstore.ClassUnavailable)
	before := auditCount(t, f, tenant)
	if _, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialStoreUnavailable) {
		t.Fatalf("a store outage with no cached value must be credential_store_unavailable, got %v", err)
	}
	if auditCount(t, f, tenant) != before {
		t.Fatal("a resolver failure must write nothing")
	}
}

func auditCount(t *testing.T, f *fx, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolver_FingerprintMismatchFailsClosed(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, _ := f.register(f.spec(tenant, "acme", "k1"))
	// The bytes behind the pinned ref change (store tampering, wrong-ref
	// wiring): the fetched value no longer matches the row.
	f.mem.Put(h.SecretRef, randBytes(t, 32))
	if _, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialIntegrity) {
		t.Fatalf("a fingerprint mismatch must be credential_integrity, got %v", err)
	}
	if !strings.Contains(f.logs.String(), `"alert":"P1"`) || !strings.Contains(f.logs.String(), "credential_integrity") {
		t.Fatalf("a P1 integrity alert must be logged: %s", f.logs.String())
	}
}

func TestResolver_RefOutsideNamespaceIsIntegrity(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	// Such a row cannot exist (migration 0096 CHECK); the resolver refuses
	// it independently (C1, resolver side).
	_, err := f.sub.secretFor(context.Background(), tenant, "casino", "acme", handleRow{
		id: uuid.New(), keyID: "k1", secretRef: memRef(f.tenant(), "casino", "acme", "n"), fingerprint: randomFingerprint(t),
	})
	if !errors.Is(err, webhookauth.ErrCredentialIntegrity) {
		t.Fatalf("got %v", err)
	}
}

func TestResolver_UnknownKeyIDNoStoreCall(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.register(f.spec(tenant, "acme", "k1"))
	calls := f.mem.Calls()
	for i := 0; i < 20; i++ {
		if _, err := f.resolve("casino", tenant, "acme", fmt.Sprintf("probe-%d", i), webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
			t.Fatal(err)
		}
	}
	if f.mem.Calls() != calls {
		t.Fatalf("cycling unknown key ids reached the store %d times", f.mem.Calls()-calls)
	}
}

// TestResolver_MemoryRefRowFailsClosedOutsideTests: a process whose router
// is built by NewRouter (the environment allow-list) never resolves a
// memory:// row - credential_unavailable, security review §4.1 point 3.
func TestResolver_MemoryRefRowFailsClosedOutsideTests(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.register(f.spec(tenant, "acme", "k1"))
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dev := config.Config{Environment: "development", EnvironmentExplicit: true,
		ProviderCredentialFingerprintKey: config.NewSecretValue(hexOf(randBytes(t, 32)))}
	store, err := devfile.New(dev, root)
	if err != nil {
		t.Fatal(err)
	}
	router, err := secretstore.NewRouter(dev, store)
	if err != nil {
		t.Fatal(err)
	}
	real, err := New(dev, router)
	if err != nil || real == nil {
		t.Fatalf("subsystem: %v", err)
	}
	err = f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := real.Resolver("casino").Resolve(ctx, tx, tenant, "acme", "k1", webhookauth.KeyFromHeader)
		return err
	})
	if !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("a memory:// row must never resolve outside tests, got %v", err)
	}
}

// TestResolver_HandleReadIsTheOnlyStatement pins the one pre-verification
// statement (ADR 0022 §3 point 9 as amended): exactly HandleReadSQL, with
// its explicit tenant predicate, no lock, no write - also when resolution
// fails.
func TestResolver_HandleReadIsTheOnlyStatement(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.register(f.spec(tenant, "acme", "k1"))
	for name, keyID := range map[string]string{"found": "k1", "not found": "nope"} {
		t.Run(name, func(t *testing.T) {
			var rec *recordingTx
			_ = f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
				rec = &recordingTx{Tx: tx}
				_, err := f.sub.Resolver("casino").Resolve(ctx, rec, tenant, "acme", keyID, webhookauth.KeyFromHeader)
				return err
			})
			got := rec.Statements()
			if len(got) != 1 || got[0] != HandleReadSQL {
				t.Fatalf("statements = %q, want exactly [HandleReadSQL]", got)
			}
		})
	}
	for _, must := range []string{"tenant_id = $1", "not_after > now()", "not_before <= now()", "status IN ('active', 'verify_only')"} {
		if !strings.Contains(HandleReadSQL, must) {
			t.Fatalf("HandleReadSQL lost %q", must)
		}
	}
	for _, mustNot := range []string{"FOR UPDATE", "FOR SHARE", "pg_advisory", "INSERT", "UPDATE ", "DELETE"} {
		if strings.Contains(strings.ToUpper(HandleReadSQL), mustNot) {
			t.Fatalf("HandleReadSQL must not contain %q", mustNot)
		}
	}
}

// TestResolver_SingleflightOneStoreCall: N concurrent cold resolves of one
// handle make exactly one store call.
func TestResolver_SingleflightOneStoreCall(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.register(f.spec(tenant, "acme", "k1"))
	calls := f.mem.Calls()
	f.mem.Block()
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader)
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	f.mem.Unblock()
	wg.Wait()
	if got := f.mem.Calls() - calls; got != 1 {
		t.Fatalf("concurrent identical lookups made %d store calls, want 1", got)
	}
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok == 0 {
		t.Fatal("the shared fetch must serve callers")
	}
}

// TestResolver_CacheRevokeRace (QA W2a concurrency): resolves racing a
// revocation either see the pre-revocation credential (with the right
// secret) or credential_unavailable - never a torn read - and every resolve
// starting after the revoke commits fails.
func TestResolver_CacheRevokeRace(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, secret := f.register(f.spec(tenant, "acme", "k1"))
	var revoked atomic.Bool
	var wg sync.WaitGroup
	errc := make(chan error, 200)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			after := revoked.Load()
			set, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader)
			switch {
			case err == nil && !bytes.Equal(set.Active.Secret, secret):
				errc <- errors.New("torn read: wrong secret served")
			case err == nil && after:
				errc <- errors.New("a resolve starting after the revoke committed succeeded")
			case err != nil && !errors.Is(err, webhookauth.ErrCredentialUnavailable):
				errc <- fmt.Errorf("unexpected error %v", err)
			}
		}(i)
	}
	time.Sleep(30 * time.Millisecond)
	f.revoke(tenant, h.ID)
	revoked.Store(true)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
}

// TestStoreOutage_DoesNotPinPool (security review §5): a blocking store,
// 50 concurrent callbacks over 8 refs. At most 4 store calls are in
// flight, at most 4 transactions are held for more than 250 ms, and an
// unrelated tenant query still completes in under 500 ms.
func TestStoreOutage_DoesNotPinPool(t *testing.T) {
	f := newFx(t)
	var tenants []uuid.UUID
	for i := 0; i < 8; i++ {
		tenant := f.tenant()
		f.register(f.spec(tenant, "acme", "k1"))
		tenants = append(tenants, tenant)
	}
	unrelated := f.tenant()
	f.mem.Block()
	defer f.mem.Unblock()

	var wg sync.WaitGroup
	durations := make([]time.Duration, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := tenants[i%len(tenants)]
			_ = f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
				start := time.Now()
				_, _ = f.sub.Resolver("casino").Resolve(ctx, tx, tenant, "acme", "k1", webhookauth.KeyFromHeader)
				durations[i] = time.Since(start)
				return nil
			})
		}(i)
	}
	time.Sleep(400 * time.Millisecond)
	qStart := time.Now()
	if err := f.rt.WithTenant(context.Background(), unrelated, func(ctx context.Context, tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
	}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(qStart); d > 500*time.Millisecond {
		t.Fatalf("an unrelated tenant query took %s during the store outage", d)
	}
	wg.Wait()
	if m := f.mem.MaxConcurrent(); m > secretstore.MaxConcurrentStoreCalls {
		t.Fatalf("%d concurrent store calls, want <= %d", m, secretstore.MaxConcurrentStoreCalls)
	}
	long := 0
	for _, d := range durations {
		if d > 400*time.Millisecond { // 250 ms slot wait + scheduling slack
			long++
		}
	}
	if long > secretstore.MaxConcurrentStoreCalls {
		t.Fatalf("%d transactions were held on the store for > 250 ms, want <= %d", long, secretstore.MaxConcurrentStoreCalls)
	}
}

// --- a KeyImplicit reference scheme (test-only; HMAC over the body) ------

type implicitTestScheme struct{}

func (implicitTestScheme) Name() string { return "test-implicit" }

func (implicitTestScheme) Extract(in webhookauth.Inbound) (webhookauth.AuthMaterial, webhookauth.Reason, bool) {
	sig := in.Header.Get("X-Sig")
	if sig == "" {
		return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureMissing, false
	}
	return webhookauth.NewAuthMaterial("", sig), "", true
}

func (implicitTestScheme) Verify(creds webhookauth.CredentialSet, in webhookauth.Inbound, m webhookauth.AuthMaterial, _ time.Time) (string, error) {
	sig, _ := m.Private().(string)
	matched := ""
	for _, c := range []*webhookauth.Credential{&creds.Active, creds.Previous} {
		if c != nil && hmacEqualString(signFor(c.Secret, in.Body), sig) && matched == "" {
			matched = c.KeyID
		}
	}
	if matched == "" {
		return "", webhookauth.ErrSignatureInvalid
	}
	return matched, nil
}

func (implicitTestScheme) Properties() webhookauth.SchemeProperties {
	return webhookauth.SchemeProperties{Binding: webhookauth.BindingPerMerchantKey, KeySelection: webhookauth.KeyImplicit,
		SignedTimestamp: true, MaxSkew: time.Minute, Replay: webhookauth.ReplayTimestampWindow}
}
