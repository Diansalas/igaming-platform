//go:build integration

// Shared fixtures for the Stage 10.3 W2a provider-credential integration
// tests. Every test runs as the NOBYPASSRLS runtime role
// (TEST_RUNTIME_DATABASE_URL) unless its name or comment says "owner".
// Secrets are generated at runtime from crypto/rand; nothing here holds a
// literal secret.
package providercred

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func runtimePool(t testing.TB) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping provider credential runtime-role test")
	}
	return connect(t, url, 20)
}

func ownerPool(t testing.TB) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return connect(t, url, 5)
}

func connect(t testing.TB, url string, conns int32) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), url, conns, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func confirmationFor(secret []byte) string {
	sum := sha256.Sum256(secret)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// logBuffer captures every log line the subsystem writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// fx is one test's world: a runtime-role pool, a memory store, a
// subsystem with a runtime fingerprint key, and staff principals.
type fx struct {
	t          testing.TB
	rt         *db.Pool
	mem        *memstore.Store
	sub        *Subsystem
	logs       *logBuffer
	clock      *fakeClock
	requester  uuid.UUID // platform admin, Person A
	approver   uuid.UUID // platform admin, Person B
	approver2  uuid.UUID // platform admin, Person C
	samePerson uuid.UUID // platform admin, Person A (second account)
	unlinked   uuid.UUID // platform admin without a Person
	suspended  uuid.UUID // platform admin, Person D, suspended
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	on  bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.on {
		return time.Now()
	}
	return c.now
}

func (c *fakeClock) Freeze(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on, c.now = true, at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testConfig(t testing.TB) config.Config {
	return config.Config{
		Environment: "development", EnvironmentExplicit: true,
		JWTSigningSecret:                 hex.EncodeToString(randBytes(t, 32)),
		ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(randBytes(t, 32))),
	}
}

func newFx(t testing.TB) *fx {
	t.Helper()
	f := &fx{t: t, rt: runtimePool(t), mem: memstore.New(), logs: &logBuffer{}, clock: &fakeClock{}}
	router, err := memstore.NewRouter(f.mem)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.sub, err = New(testConfig(t), router, WithLogger(logger), WithFetcherOptions(
		secretstore.WithClock(f.clock.Now), secretstore.WithSleep(func(context.Context, time.Duration) {})))
	if err != nil || f.sub == nil {
		t.Fatalf("subsystem: %v", err)
	}
	pA, pB, pC, pD := f.person(), f.person(), f.person(), f.person()
	f.requester = f.platformStaff(&pA, "active")
	f.approver = f.platformStaff(&pB, "active")
	f.approver2 = f.platformStaff(&pC, "active")
	f.samePerson = f.platformStaff(&pA, "active")
	f.unlinked = f.platformStaff(nil, "active")
	f.suspended = f.platformStaff(&pD, "suspended")
	return f
}

func (f *fx) person() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, id)
		return err
	}); err != nil {
		f.t.Fatalf("seed person: %v", err)
	}
	return id
}

func (f *fx) platformStaff(person *uuid.UUID, status string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, $4)`, id, "pc-"+id.String()+"@test.example", status, person)
		return err
	}); err != nil {
		f.t.Fatalf("seed platform staff: %v", err)
	}
	return id
}

func (f *fx) tenantStaff(tenantID uuid.UUID, role string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if err := f.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', $4)`,
			id, tenantID, "pc-"+id.String()+"@test.example", role)
		return err
	}); err != nil {
		f.t.Fatalf("seed tenant staff: %v", err)
	}
	return id
}

func (f *fx) setStaffStatus(id uuid.UUID, status string) {
	f.t.Helper()
	if err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = $2 WHERE id = $1`, id, status)
		return err
	}); err != nil {
		f.t.Fatalf("set staff status: %v", err)
	}
}

func (f *fx) tenant() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'PC Test', $2, 'under_platform_licence')`,
			id, "pc-"+strings.ReplaceAll(id.String(), "-", "")[:16])
		return err
	}); err != nil {
		f.t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func memRef(tenantID uuid.UUID, domain, provider, name string) string {
	return fmt.Sprintf("memory://vault/provider-creds/%s/%s/%s/%s?version=v1", tenantID, domain, provider, name)
}

// spec describes one registration.
type spec struct {
	tenant       uuid.UUID
	domain       string
	provider     string
	purpose      string
	keyID        string
	notBefore    time.Time
	notAfter     *time.Time
	vendorAcct   *string
	predecessor  *uuid.UUID
	disposition  string
	predNotAfter *time.Time
	reason       string
}

func (f *fx) spec(tenantID uuid.UUID, provider, keyID string) spec {
	return spec{
		tenant: tenantID, domain: "casino", provider: provider, purpose: PurposeWebhookVerify, keyID: keyID,
		notBefore: time.Now().Add(-time.Minute), disposition: DispositionNone, reason: RequestReasonInitialRegistration,
	}
}

func ac(actor uuid.UUID) AuditContext {
	return AuditContext{ActorID: actor, IPAddress: "192.0.2.10", UserAgent: "pc-test", RequestID: "req-" + uuid.NewString()[:8]}
}

// put stores a fresh random secret under a fresh ref for s.
func (f *fx) put(s spec) (ref string, secret []byte) {
	secret = randBytes(f.t, 32)
	ref = memRef(s.tenant, s.domain, s.provider, "k-"+uuid.NewString()[:8])
	f.mem.Put(ref, secret)
	return ref, secret
}

func (f *fx) params(s spec, ref string, secret []byte) FileRequestParams {
	return FileRequestParams{
		TargetTenantID: s.tenant, Domain: s.domain, ProviderID: s.provider, Purpose: s.purpose, KeyID: s.keyID,
		SecretRef: ref, Confirmation: confirmationFor(secret), VendorAccountID: s.vendorAcct,
		NotBefore: s.notBefore, NotAfter: s.notAfter, PredecessorHandleID: s.predecessor,
		PredecessorDisposition: s.disposition, PredecessorNotAfter: s.predNotAfter, ReasonCode: s.reason,
		RequestedBy: f.requester,
	}
}

func (f *fx) file(s spec) (Request, []byte) {
	f.t.Helper()
	ref, secret := f.put(s)
	r, err := f.sub.FileRequest(context.Background(), f.rt, f.params(s, ref, secret), ac(f.requester))
	if err != nil {
		f.t.Fatalf("file request: %v", err)
	}
	return r, secret
}

func (f *fx) approve(r Request, approver uuid.UUID) {
	f.t.Helper()
	if _, err := f.decide(r, approver, "approve", r.ContentHash, ""); err != nil {
		f.t.Fatalf("approve: %v", err)
	}
}

func (f *fx) decide(r Request, approver uuid.UUID, decision, hash, reason string) (Approval, error) {
	return f.sub.DecideRequest(context.Background(), f.rt, DecideParams{
		TargetTenantID: r.TargetTenantID, RequestID: r.ID, Approver: approver,
		Decision: decision, ContentHash: hash, ReasonCode: reason,
	}, ac(approver))
}

func (f *fx) apply(r Request) (Handle, error) {
	return f.sub.ApplyRequest(context.Background(), f.rt, ApplyParams{
		TargetTenantID: r.TargetTenantID, RequestID: r.ID, Applier: f.requester,
	}, ac(f.requester))
}

// register runs the full four-eyes path and returns the active handle and
// its secret.
func (f *fx) register(s spec) (Handle, []byte) {
	f.t.Helper()
	r, secret := f.file(s)
	f.approve(r, f.approver)
	h, err := f.apply(r)
	if err != nil {
		f.t.Fatalf("apply: %v (%s)", err, ClassOf(err))
	}
	return h, secret
}

func (f *fx) transition(tenantID, handleID uuid.UUID, action string, notAfter *time.Time, reason string) (Handle, error) {
	var h Handle
	err := f.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		h, err = TransitionHandle(ctx, tx, handleID, action, notAfter, reason, f.requester, ac(f.requester))
		return err
	})
	return h, err
}

func (f *fx) revoke(tenantID, handleID uuid.UUID) {
	f.t.Helper()
	if _, err := f.transition(tenantID, handleID, ActionRevoke, nil, RevokeReasonSuspectedCompromise); err != nil {
		f.t.Fatalf("revoke: %v", err)
	}
}

// resolve runs the real resolver in a tenant-scoped transaction, the way
// a domain orchestrator does.
func (f *fx) resolve(domain string, tenantID uuid.UUID, provider, keyID string, sel keySel) (credSet, error) {
	var set credSet
	err := f.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		set, err = f.sub.Resolver(domain).Resolve(ctx, tx, tenantID, provider, keyID, sel)
		return err
	})
	return set, err
}

func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func ptr[T any](v T) *T { return &v }

type (
	keySel  = webhookauth.KeySelection
	credSet = webhookauth.CredentialSet
)

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// signFor is the test-only KeyImplicit reference scheme's MAC.
func signFor(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func hmacEqualString(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

type pgxTx = pgx.Tx

func runtimeURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping provider credential runtime-role test")
	}
	return url
}
