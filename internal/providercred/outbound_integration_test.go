//go:build integration

// PROV-OUTBOUND-CRED-1 (ADR 0093 §5; security review §2): outbound calls
// carry the tenant's credential, resolved per call from the outbound_api
// handle in a transaction that has closed before the HTTP call, applied
// through a per-call httpclient.Authenticator.
package providercred

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providers/httpclient"
)

// fakeVendor records the credential header of every request by path.
type fakeVendor struct {
	srv      *httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	seen     map[string][]string // path -> X-Api-Key values
	onCall   func()
}

func newFakeVendor(t *testing.T) *fakeVendor {
	v := &fakeVendor{seen: map[string][]string{}}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.requests.Add(1)
		if v.onCall != nil {
			v.onCall()
		}
		v.mu.Lock()
		v.seen[r.URL.Path] = append(v.seen[r.URL.Path], r.Header.Get("X-Api-Key"))
		v.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// outboundCall is the per-call pattern an orchestrator follows: resolve
// (own short tenant tx, committed), then build a per-call Authenticator,
// then call. Nothing is kept afterwards.
func outboundCall(ctx context.Context, o *OutboundResolver, pool TenantTxRunner, client *httpclient.Client, tenant uuid.UUID, provider, path string) error {
	cred, err := o.Resolve(ctx, pool, tenant, provider)
	if err != nil {
		return err
	}
	_, err = client.Do(ctx, httpclient.Request{
		Method: http.MethodGet, Path: path, Operation: "test",
		Auth: httpclient.NewHeaderAuthenticator("X-Api-Key", hex.EncodeToString(cred.Secret())),
	})
	return err
}

func (f *fx) registerOutbound(tenant uuid.UUID, provider, keyID string) (Handle, []byte) {
	s := f.spec(tenant, provider, keyID)
	s.domain, s.purpose = "payments", PurposeOutboundAPI
	return f.register(s)
}

func TestOutbound_RevokeThenNextCallFailsClosed(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, secret := f.registerOutbound(tenant, "psp", "k1")
	vendor := newFakeVendor(t)
	client := httpclient.New(httpclient.ClientConfig{ProviderName: "psp", BaseURL: vendor.srv.URL, Timeout: 2 * time.Second})
	o := f.sub.Outbound("payments")

	if err := outboundCall(context.Background(), o, f.rt, client, tenant, "psp", "/a"); err != nil {
		t.Fatal(err)
	}
	if got := vendor.seen["/a"]; len(got) != 1 || got[0] != hex.EncodeToString(secret) {
		t.Fatalf("the call must carry the tenant's resolved credential, got %v", got)
	}
	f.revoke(tenant, h.ID)
	before := vendor.requests.Load()
	err := outboundCall(context.Background(), o, f.rt, client, tenant, "psp", "/b")
	if !errors.Is(err, ErrOutboundCredentialUnavailable) {
		t.Fatalf("after revocation the next call must fail closed, got %v", err)
	}
	if vendor.requests.Load() != before {
		t.Fatal("no request may reach the vendor after revocation")
	}
}

func TestOutbound_TenantACallNeverCarriesBCredential(t *testing.T) {
	f := newFx(t)
	a, b := f.tenant(), f.tenant()
	_, secretA := f.registerOutbound(a, "psp", "k1")
	_, secretB := f.registerOutbound(b, "psp", "k1")
	vendor := newFakeVendor(t)
	// ONE shared client and ONE resolver for both tenants (the shape a real
	// shared adapter has).
	client := httpclient.New(httpclient.ClientConfig{ProviderName: "psp", BaseURL: vendor.srv.URL, Timeout: 2 * time.Second})
	o := f.sub.Outbound("payments")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for tenant, path := range map[uuid.UUID]string{a: "/tenant-a", b: "/tenant-b"} {
			wg.Add(1)
			go func(tenant uuid.UUID, path string) {
				defer wg.Done()
				if err := outboundCall(context.Background(), o, f.rt, client, tenant, "psp", path); err != nil {
					t.Error(err)
				}
			}(tenant, path)
		}
	}
	wg.Wait()
	for path, want := range map[string][]byte{"/tenant-a": secretA, "/tenant-b": secretB} {
		got := vendor.seen[path]
		if len(got) != 20 {
			t.Fatalf("%s: %d calls", path, len(got))
		}
		for _, v := range got {
			if v != hex.EncodeToString(want) {
				t.Fatalf("%s carried another tenant's credential", path)
			}
		}
	}
}

func TestOutbound_RotationRevokesOldAtomically(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h1, _ := f.registerOutbound(tenant, "psp", "k1")
	s := f.spec(tenant, "psp", "k2")
	s.domain, s.purpose = "payments", PurposeOutboundAPI
	s.predecessor, s.disposition, s.reason = &h1.ID, DispositionRevoked, RequestReasonScheduledRotation
	h2, secret2 := f.register(s)

	handles := listHandles(t, f, tenant)
	var active int
	for _, h := range handles {
		if h.Status == "active" {
			active++
		}
		if h.ID == h1.ID && (h.Status != "revoked" || h.RevokeReason == nil || *h.RevokeReason != RevokeReasonRotationComplete) {
			t.Fatalf("the old outbound key must be revoked (rotation_complete) by the same apply: %+v", h)
		}
	}
	if active != 1 {
		t.Fatalf("exactly one active outbound key, got %d", active)
	}
	cred, err := f.sub.Outbound("payments").Resolve(context.Background(), f.rt, tenant, "psp")
	if err != nil || cred.HandleID != h2.ID || hex.EncodeToString(cred.Secret()) != hex.EncodeToString(secret2) {
		t.Fatalf("resolve after rotation: %v %v", cred, err)
	}
	// The predecessor transition and the activation each wrote an audit row.
	if n := auditActions(t, f, tenant, AuditPredecessorTransitioned); n != 1 {
		t.Fatalf("predecessor_transitioned audit rows = %d", n)
	}
}

// TestOutbound_NoTxHeldAcrossHTTPCall: while the vendor handler runs, the
// pool the resolver used holds no connection (its tenant transaction has
// already committed).
func TestOutbound_NoTxHeldAcrossHTTPCall(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.registerOutbound(tenant, "psp", "k1")
	dedicated := connect(t, runtimeURL(t), 2)
	vendor := newFakeVendor(t)
	var acquiredDuringCall atomic.Int32
	acquiredDuringCall.Store(-1)
	vendor.onCall = func() { acquiredDuringCall.Store(dedicated.Raw().Stat().AcquiredConns()) }
	client := httpclient.New(httpclient.ClientConfig{ProviderName: "psp", BaseURL: vendor.srv.URL, Timeout: 2 * time.Second})
	if err := outboundCall(context.Background(), f.sub.Outbound("payments"), dedicated, client, tenant, "psp", "/x"); err != nil {
		t.Fatal(err)
	}
	if got := acquiredDuringCall.Load(); got != 0 {
		t.Fatalf("the pool held %d connections during the HTTP call, want 0", got)
	}
}

func TestOutbound_NilResolverFailsClosed(t *testing.T) {
	var sub *Subsystem
	if o := sub.Outbound("payments"); o != nil {
		t.Fatal("a nil subsystem has no outbound resolver")
	}
	var o *OutboundResolver
	if _, err := o.Resolve(context.Background(), nil, uuid.New(), "psp"); !errors.Is(err, ErrOutboundCredentialUnavailable) {
		t.Fatalf("got %v", err)
	}
	f := newFx(t)
	tenant := f.tenant()
	// Only an inbound handle: no outbound credential.
	f.register(f.spec(tenant, "psp", "k1"))
	if _, err := f.sub.Outbound("casino").Resolve(context.Background(), f.rt, tenant, "psp"); !errors.Is(err, ErrOutboundCredentialUnavailable) {
		t.Fatalf("an inbound handle must never serve outbound, got %v", err)
	}
}

// TestOutbound_DerivedTokenCacheBoundToFreshRead (security review §2
// precision 3): a derived token is keyed on (tenant, handle, fingerprint),
// is only reachable through a credential THIS call's read returned, never
// outlives the vendor expiry, and a rotated handle misses.
func TestOutbound_DerivedTokenCacheBoundToFreshRead(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h1, _ := f.registerOutbound(tenant, "psp", "k1")
	o := f.sub.Outbound("payments")
	now := time.Now()
	cache := NewDerivedTokenCache(func() time.Time { return now })
	cred, err := o.Resolve(context.Background(), f.rt, tenant, "psp")
	if err != nil {
		t.Fatal(err)
	}
	token := randBytes(t, 24)
	cache.Put(cred, token, now.Add(time.Minute))
	if got, ok := cache.Get(cred); !ok || hex.EncodeToString(got) != hex.EncodeToString(token) {
		t.Fatal("the token is served for the same fresh credential")
	}
	// Another tenant's credential never hits.
	other := cred
	other.TenantID = uuid.New()
	if _, ok := cache.Get(other); ok {
		t.Fatal("cross-tenant derived-token hit")
	}
	// Revocation: the next read fails, so the token is unreachable.
	f.revoke(tenant, h1.ID)
	if _, err := o.Resolve(context.Background(), f.rt, tenant, "psp"); !errors.Is(err, ErrOutboundCredentialUnavailable) {
		t.Fatal("the read after revocation must fail before any cached token is consulted")
	}
	// Rotation to a new handle: a different key, so a miss.
	h2, _ := f.registerOutbound(tenant, "psp", "k2")
	cred2, err := o.Resolve(context.Background(), f.rt, tenant, "psp")
	if err != nil || cred2.HandleID != h2.ID {
		t.Fatal(err)
	}
	if _, ok := cache.Get(cred2); ok {
		t.Fatal("a rotated handle must not reach the old derived token")
	}
	// Expiry.
	cache.Put(cred2, token, now.Add(time.Second))
	now = now.Add(2 * time.Second)
	if _, ok := cache.Get(cred2); ok {
		t.Fatal("a derived token must not outlive the vendor expiry")
	}
}

func listHandles(t *testing.T, f *fx, tenant uuid.UUID) []Handle {
	t.Helper()
	var out []Handle
	if err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgxTx) error {
		var err error
		out, err = ListHandles(ctx, tx, tenant)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func auditActions(t *testing.T, f *fx, tenant uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenant, action).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
