//go:build integration

// PRH-I2 (ADR 0095 §15.2/§15.3, identity-compliance part): the KYC
// CreateVerification/SubmitVerification two/three-phase split. These tests
// target properties that did not exist before the split - no transaction
// held across a provider call, the per-call CallContext's own fields and
// redaction, the new fail-closed branches (nil pool, nil outbound
// resolver, a mismatched resolved credential), IC condition 2 (an
// ambiguous/timeout/transport-error SubmitVerification result leaves
// status unchanged), a crash-between-phases proxy, cross-tenant isolation,
// and the retryable-5xx-never-200 callback-race disposition (IC-Q1) -
// mirrors internal/casino/launch_two_phase_integration_test.go's identical
// role and conventions for casino.LaunchGame.
package kyc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// testPoolSized mirrors casino's identical helper - a pool sized to an
// explicit, tiny connection bound, used only to PROVE a connection is
// genuinely free (tryAcquireKYCConnection), not merely idle by
// happenstance.
func testPoolSized(t *testing.T, maxConns int32) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, maxConns, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func tryAcquireKYCConnection(pool *db.Pool, tenantID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
}

// spyKYCProvider wraps a *MockKYCProvider and lets a test observe/override
// exactly the two methods CreateVerification/SubmitVerification call
// OUTSIDE any transaction - every other KYCProvider method is the embedded
// mock's own untouched behavior.
type spyKYCProvider struct {
	*MockKYCProvider
	onCreateVerification func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error)
	onSubmitVerification func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error)
}

func (s *spyKYCProvider) CreateVerification(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
	if s.onCreateVerification != nil {
		return s.onCreateVerification(ctx, in)
	}
	return s.MockKYCProvider.CreateVerification(ctx, in)
}

func (s *spyKYCProvider) SubmitVerification(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
	if s.onSubmitVerification != nil {
		return s.onSubmitVerification(ctx, ref, docs, call)
	}
	return s.MockKYCProvider.SubmitVerification(ctx, ref, docs, call)
}

func auditActionExistsKYC(t *testing.T, pool *db.Pool, tenantID, targetID uuid.UUID, action string) bool {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenantID, action, targetID.String()).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count %s audit rows: %v", action, err)
	}
	return n > 0
}

func mustGetProviderReference(t *testing.T, pool *db.Pool, tenantID, id uuid.UUID) string {
	t.Helper()
	var ref *string
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, id).Scan(&ref)
	})
	if err != nil {
		t.Fatalf("read provider_reference: %v", err)
	}
	if ref == nil {
		return ""
	}
	return *ref
}

// TestCreateVerification_NoConnectionHeldAcrossProviderCall is the split's
// own load-bearing property (ADR 0095 §15.2): phase A's transaction must
// have committed - releasing its pooled connection - before the provider's
// CreateVerification ever runs. Proven with a pool sized to exactly ONE
// connection: if phase A's connection were still held, this test's own
// concurrent WithTenant call from inside CreateVerification would time out
// waiting for a second connection that does not exist. Also asserts
// txscope.Held(ctx) is false - the same defence-in-depth signal
// providercred.OutboundResolver itself checks.
func TestCreateVerification_NoConnectionHeldAcrossProviderCall(t *testing.T) {
	singleConnPool := testPoolSized(t, 1)
	f := seedFixture(t, singleConnPool)

	var sawHeld bool
	var acquireErr error
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		sawHeld = txscope.Held(ctx)
		acquireErr = tryAcquireKYCConnection(singleConnPool, f.tenantID)
		return base.CreateVerification(ctx, in)
	}

	v, err := CreateVerification(context.Background(), singleConnPool, NewMockOutboundResolver(), provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification: %v", err)
	}
	if v.ID == uuid.Nil {
		t.Fatal("expected a verification id")
	}
	if sawHeld {
		t.Fatal("txscope reported a transaction held during CreateVerification - phase A must have committed first")
	}
	if acquireErr != nil {
		t.Fatalf("could not acquire the pool's only connection during CreateVerification (phase A's connection was still held): %v", acquireErr)
	}
}

// TestSubmitVerification_NoConnectionHeldAcrossProviderCall is
// CreateVerification's identical property for SubmitVerification's own
// phase B (ADR 0095 §15.3).
func TestSubmitVerification_NoConnectionHeldAcrossProviderCall(t *testing.T) {
	singleConnPool := testPoolSized(t, 1)
	f := seedFixture(t, singleConnPool)
	verificationID := seedVerification(t, singleConnPool, f)

	var sawHeld bool
	var acquireErr error
	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, singleConnPool, f.tenantID, verificationID)] = true
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		sawHeld = txscope.Held(ctx)
		acquireErr = tryAcquireKYCConnection(singleConnPool, f.tenantID)
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	if _, err := SubmitVerification(context.Background(), singleConnPool, NewMockOutboundResolver(), provider, f.tenantID, verificationID); err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if sawHeld {
		t.Fatal("txscope reported a transaction held during SubmitVerification - phase A must have committed first")
	}
	if acquireErr != nil {
		t.Fatalf("could not acquire the pool's only connection during SubmitVerification (phase A's connection was still held): %v", acquireErr)
	}
}

// TestCreateVerification_NilPoolFailsClosedWithoutPanic mirrors casino's
// identical fail-closed convention.
func TestCreateVerification_NilPoolFailsClosedWithoutPanic(t *testing.T) {
	_, err := CreateVerification(context.Background(), nil, NewMockOutboundResolver(), NewMockKYCProvider(), CreateVerificationParams{
		TenantID: uuid.New(), BrandID: uuid.New(), PlayerAccountID: uuid.New(), PersonID: uuid.New(),
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a nil pool, got %v", err)
	}
}

// TestCreateVerification_NilProviderFailsClosedWithoutPanic: a nil
// KYCProvider (e.g. selection returned none) must never reach phase A at
// all - no orphan row is created for a call that could never have named a
// provider.
func TestCreateVerification_NilProviderFailsClosedWithoutPanic(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	_, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), nil, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a nil provider, got %v", err)
	}
}

// TestCreateVerification_NilOutboundResolverLeavesHarmlessOrphanRow: phase A
// still commits the orphan row (status='unverified', provider_reference
// NULL), but phase B refuses to call the provider with no credential
// resolver configured. ADR 0095 §15.2: this orphan has NO enforcement
// effect (ADR 0096's EvaluateEnforcement only allows on 'passed') - it is
// the harmless, documented failure mode, never rolled back.
func TestCreateVerification_NilOutboundResolverLeavesHarmlessOrphanRow(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	provider := NewMockKYCProvider()

	_, err := CreateVerification(context.Background(), pool, nil, provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a nil outbound resolver, got %v", err)
	}

	var count int
	var status VerificationStatus
	var ref *string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM kyc_verifications WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerID).Scan(&count); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status, provider_reference FROM kyc_verifications WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerID).Scan(&status, &ref)
	}); err != nil {
		t.Fatalf("read orphan verification row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one orphan verification row, got %d", count)
	}
	if status != StatusUnverified {
		t.Fatalf("expected the orphan row to stay 'unverified', got %q", status)
	}
	if ref != nil {
		t.Fatalf("expected provider_reference to stay NULL on the orphan row, got %q", *ref)
	}
	var requestedCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'kyc.verification_requested'`, f.tenantID).Scan(&requestedCount)
	}); err != nil {
		t.Fatalf("count kyc.verification_requested audit rows: %v", err)
	}
	if requestedCount != 1 {
		t.Fatalf("expected exactly one kyc.verification_requested audit row from phase A, got %d", requestedCount)
	}
	var submittedCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'kyc.verification_submitted'`, f.tenantID).Scan(&submittedCount)
	}); err != nil {
		t.Fatalf("count kyc.verification_submitted audit rows: %v", err)
	}
	if submittedCount != 0 {
		t.Fatalf("expected NO kyc.verification_submitted audit row - phase C never ran for a phase-B failure, got %d", submittedCount)
	}
}

type mismatchedKYCOutboundResolver struct{}

func (mismatchedKYCOutboundResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, _ uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	// Always resolves for a DIFFERENT tenant than the one asked for.
	return providercred.NewMockOutboundCredential(uuid.New(), "kyc", providerID), nil
}

// TestCreateVerification_CredentialBindingMismatchFailsClosed is the
// defence-in-depth binding check (ADR 0095 §9.1/S95-C8(b), mirroring
// casino's identical test): a resolver that hands back a credential for the
// WRONG tenant must never reach the provider.
func TestCreateVerification_CredentialBindingMismatchFailsClosed(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	provider := NewMockKYCProvider()

	_, err := CreateVerification(context.Background(), pool, mismatchedKYCOutboundResolver{}, provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a mismatched credential, got %v", err)
	}
}

// TestCreateVerification_CallContextFieldsPassedToProvider pins the exact
// shape ADR 0095 §15.2/§9.1 specifies: TenantID/ProviderID match the
// verification, Credential.Domain is "kyc", and IdempotencyKey is "kv:" +
// the new row's own id.
func TestCreateVerification_CallContextFieldsPassedToProvider(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	base := NewMockKYCProvider()

	var captured CreateVerificationInput
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		captured = in
		return base.CreateVerification(ctx, in)
	}

	v, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification: %v", err)
	}
	if captured.Call.TenantID != f.tenantID {
		t.Fatalf("expected Call.TenantID = %s, got %s", f.tenantID, captured.Call.TenantID)
	}
	if captured.Call.ProviderID != "mock" {
		t.Fatalf("expected Call.ProviderID = mock, got %s", captured.Call.ProviderID)
	}
	if captured.Call.Credential.Domain != "kyc" {
		t.Fatalf("expected Call.Credential.Domain = kyc, got %s", captured.Call.Credential.Domain)
	}
	want := "kv:" + v.ID.String()
	if captured.Call.IdempotencyKey != want {
		t.Fatalf("expected Call.IdempotencyKey = %q, got %q", want, captured.Call.IdempotencyKey)
	}
	if captured.Call.Deadline.IsZero() {
		t.Fatal("expected a non-zero Call.Deadline")
	}
}

// TestSubmitVerification_AmbiguousResultLeavesStatusUnchanged is IC
// condition 2's own required test (ADR 0095 §15.3, not inherited from
// §4.4's matrix): an ambiguous/timeout/transport-error SubmitVerification
// result must leave kyc_verifications.status COMPLETELY unchanged - never
// read by statusForOutcome, never applied via updateVerificationStatus.
func TestSubmitVerification_AmbiguousResultLeavesStatusUnchanged(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	before := mustGetStatus(t, pool, f.tenantID, verificationID)
	if before != StatusPending {
		t.Fatalf("test setup: expected the seeded verification to start 'pending' (MockKYCProvider.CreateVerification's own outcome), got %q", before)
	}

	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
	provider := &spyKYCProvider{MockKYCProvider: base}
	ambiguousErr := errors.New("simulated ambiguous/timeout transport failure")
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		return ProviderResult{}, ambiguousErr
	}

	_, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err == nil {
		t.Fatal("expected an error for an ambiguous/timeout SubmitVerification result")
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
	after := mustGetStatus(t, pool, f.tenantID, verificationID)
	if after != before {
		t.Fatalf("IC condition 2 violated: status changed from %q to %q after an ambiguous SubmitVerification result", before, after)
	}
}

// TestSubmitVerification_ProviderErrorOutcomeLeavesStatusUnchanged is IC
// condition 2's OTHER shape: a well-formed ProviderResult carrying
// Outcome==ProviderError (a definitive "the provider itself reported
// failure", not a transport error) must ALSO leave status unchanged - this
// is applySubmissionResult's own phase-C branch, distinct from the
// transport-error case above which never reaches phase C at all.
func TestSubmitVerification_ProviderErrorOutcomeLeavesStatusUnchanged(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	before := mustGetStatus(t, pool, f.tenantID, verificationID)

	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		return ProviderResult{ProviderReference: ref, Outcome: ProviderError, Reason: "vendor_ambiguous"}, nil
	}

	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if v.Status != before {
		t.Fatalf("IC condition 2 violated: status changed from %q to %q after a ProviderError outcome", before, v.Status)
	}
	after := mustGetStatus(t, pool, f.tenantID, verificationID)
	if after != before {
		t.Fatalf("IC condition 2 violated: persisted status changed from %q to %q after a ProviderError outcome", before, after)
	}
	if !auditActionExistsKYC(t, pool, f.tenantID, verificationID, "kyc.verification_submitted_to_provider") {
		t.Fatal("expected a kyc.verification_submitted_to_provider audit row even for a ProviderError outcome")
	}
}

// TestSubmitVerification_TerminalVerificationIsANoOp: a verification
// already in a terminal status is left untouched by SubmitVerification -
// no provider call is even attempted.
func TestSubmitVerification_TerminalVerificationIsANoOp(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "manual",
		})
		return err
	}); err != nil {
		t.Fatalf("seed terminal status: %v", err)
	}

	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	called := false
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		called = true
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if called {
		t.Fatal("expected NO provider call for an already-terminal verification")
	}
	if v.Status != StatusApproved {
		t.Fatalf("expected the terminal status untouched, got %q", v.Status)
	}
}

// TestSubmitVerification_IdempotencyKeyStableForSameDocumentSet is ADR
// 0095 §15.3's content-derived key: submitting the SAME (empty) document
// set twice derives the exact same key both times.
func TestSubmitVerification_IdempotencyKeyStableForSameDocumentSet(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
	var keys []string
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		keys = append(keys, call.IdempotencyKey)
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	for i := 0; i < 2; i++ {
		if _, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID); err != nil {
			t.Fatalf("SubmitVerification call %d: %v", i, err)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 captured idempotency keys, got %d", len(keys))
	}
	if keys[0] != keys[1] {
		t.Fatalf("expected the SAME idempotency key for the same (empty) document set, got %q and %q", keys[0], keys[1])
	}
	if keys[0] != "ks:"+verificationID.String()+":"+emptyDocSetSHA256() {
		t.Fatalf("unexpected idempotency key shape: %q", keys[0])
	}
}

// emptyDocSetSHA256 is the SHA-256 of zero document ids (submissionIdempotencyKey's
// own hash over an empty, sorted id list) - computed independently here so
// this test does not merely assert "stable", but pins the actual documented
// shape ("ks:" + verification id + ":" + sha256(sorted document ids)).
func emptyDocSetSHA256() string {
	return submissionIdempotencyKey(uuid.Nil, nil)[len("ks:"+uuid.Nil.String()+":"):]
}

// TestCreateVerification_CrossTenantNeverLeaksAcrossTenants: two tenants
// each create their own verification; each is visible only under its own
// tenant scope (RLS), never the other's.
func TestCreateVerification_CrossTenantNeverLeaksAcrossTenants(t *testing.T) {
	pool := testPoolSized(t, 1)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	vA, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), NewMockKYCProvider(), CreateVerificationParams{
		TenantID: fA.tenantID, BrandID: fA.brandID, PlayerAccountID: fA.playerID, PersonID: fA.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification tenant A: %v", err)
	}
	vB, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), NewMockKYCProvider(), CreateVerificationParams{
		TenantID: fB.tenantID, BrandID: fB.brandID, PlayerAccountID: fB.playerID, PersonID: fB.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification tenant B: %v", err)
	}

	// Tenant B's scope must never resolve tenant A's row, and vice versa.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetVerificationByID(ctx, tx, vA.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound reading tenant A's verification under tenant B's scope, got %v", err)
	}
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetVerificationByID(ctx, tx, vB.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound reading tenant B's verification under tenant A's scope, got %v", err)
	}
}

// TestSubmitVerification_CrossTenantMismatchFailsClosed: calling
// SubmitVerification for tenant A's verification while asserting tenant
// B's tenantID must never resolve or submit tenant A's row (RLS makes the
// phase-A read return "not found" under the wrong tenant scope).
func TestSubmitVerification_CrossTenantMismatchFailsClosed(t *testing.T) {
	pool := testPoolSized(t, 1)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, fA)

	_, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), NewMockKYCProvider(), fB.tenantID, verificationID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound submitting tenant A's verification under tenant B's scope, got %v", err)
	}
}

// TestCreateVerification_ContextCancelledBeforePhaseC_StillAppliesResult is
// this codebase's practical proxy for "crash between phase B and phase C"
// (a genuine process crash cannot be simulated in-process - see casino's
// identical convention, ADR 0095 §16.2 item 18): the request ctx is
// cancelled the INSTANT the provider call returns, before phase C's own
// CAS transaction would otherwise begin. Phase C runs on
// context.WithoutCancel(ctx) plus its own bounded timeout specifically so
// this can never silently lose the provider's already-accepted result.
func TestCreateVerification_ContextCancelledBeforePhaseC_StillAppliesResult(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)

	ctx, cancel := context.WithCancel(context.Background())
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		result, err := base.CreateVerification(ctx, in)
		// The vendor accepted BEFORE the client's own connection drops -
		// "crash after vendor accept": only the platform's own phase-C
		// follow-up (the CAS apply + audit) still has to run.
		cancel()
		return result, err
	}

	v, err := CreateVerification(ctx, pool, NewMockOutboundResolver(), provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if err != nil {
		t.Fatalf("expected CreateVerification to succeed despite the ctx being cancelled right after the provider call returned, got %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test setup: expected the request ctx to actually be cancelled by the time CreateVerification returned")
	}
	if v.ProviderReference == "" {
		t.Fatal("expected phase C to have applied the provider's reference despite the cancelled ctx")
	}
	if v.Status != StatusPending {
		t.Fatalf("expected status 'pending' (MockKYCProvider's own CreateVerification outcome), got %q", v.Status)
	}
	if !auditActionExistsKYC(t, pool, f.tenantID, v.ID, "kyc.verification_submitted") {
		t.Fatal("expected a kyc.verification_submitted audit record from phase C even though the request ctx was cancelled")
	}
	// Persisted state must match the returned value (phase C really
	// committed, this is not merely an in-memory return value).
	if got := mustGetStatus(t, pool, f.tenantID, v.ID); got != StatusPending {
		t.Fatalf("expected persisted status 'pending', got %q", got)
	}
	if got := mustGetProviderReference(t, pool, f.tenantID, v.ID); got != v.ProviderReference {
		t.Fatalf("expected persisted provider_reference %q, got %q", v.ProviderReference, got)
	}
}

// TestSubmitVerification_ContextCancelledBeforePhaseC_StillAppliesResult is
// CreateVerification's identical ctx-cancellation proxy for SubmitVerification's
// own phase C.
func TestSubmitVerification_ContextCancelledBeforePhaseC_StillAppliesResult(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	ctx, cancel := context.WithCancel(context.Background())
	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
	base.SetOutcome(mustGetProviderReference(t, pool, f.tenantID, verificationID), ProviderResult{Outcome: ProviderApproved, Reason: "auto"})
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		result, err := base.SubmitVerification(ctx, ref, docs, call)
		cancel()
		return result, err
	}

	v, err := SubmitVerification(ctx, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("expected SubmitVerification to succeed despite the ctx being cancelled right after the provider call returned, got %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test setup: expected the request ctx to actually be cancelled by the time SubmitVerification returned")
	}
	if v.Status != StatusApproved {
		t.Fatalf("expected phase C to have applied the approved outcome despite the cancelled ctx, got %q", v.Status)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusApproved {
		t.Fatalf("expected persisted status 'approved', got %q", got)
	}
}

// kycCallContextRedactionSentinel is the exact secret NewMockOutboundCredential
// embeds - a known, greppable value asserted never rendered by any of
// CallContext's formatting/logging/marshaling paths (mirrors
// internal/casino/callcontext_redaction_test.go's identical convention
// exactly).
const kycCallContextRedactionSentinel = "mock-outbound-credential-not-a-real-secret"

// renderAllFormsKYC exercises every rendering path security review
// RV-PRH-I2 C2 named for casino, applied here to KYC's own CallContext:
// %v/%+v/%#v/%s/%q, slog's text AND JSON handlers, json.Marshal, and
// fmt.Errorf("%v", ...).
func renderAllFormsKYC(t *testing.T, v any) []string {
	t.Helper()
	outs := []string{
		fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v),
		fmt.Sprintf("%s", v), fmt.Sprintf("%q", v),
		fmt.Errorf("%v", v).Error(),
	}
	var textBuf, jsonLogBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("m", "v", v)
	slog.New(slog.NewJSONHandler(&jsonLogBuf, nil)).Info("m", "v", v)
	outs = append(outs, textBuf.String(), jsonLogBuf.String())
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	outs = append(outs, string(j))
	return outs
}

func assertNoSentinelKYC(t *testing.T, outs []string, label string) {
	t.Helper()
	for _, out := range outs {
		if strings.Contains(out, kycCallContextRedactionSentinel) {
			t.Fatalf("%s rendered the secret: %q", label, out)
		}
	}
}

// TestCallContext_NeverRendersSecret is ADR 0095 §9.1's redaction contract,
// applied to KYC's own CallContext copy (mirrors
// internal/casino/callcontext_redaction_test.go exactly, including its
// negative control).
func TestCallContext_NeverRendersSecret(t *testing.T) {
	cred := providercred.NewMockOutboundCredential(uuid.New(), "kyc", "mock")
	if !strings.Contains(string(cred.Secret()), kycCallContextRedactionSentinel) {
		t.Fatalf("test setup: NewMockOutboundCredential's secret no longer contains the expected sentinel - update kycCallContextRedactionSentinel")
	}

	call := CallContext{
		TenantID: uuid.New(), ProviderID: "mock", Credential: cred,
		IdempotencyKey: "kv:test-verification", Deadline: time.Now().Add(time.Minute),
	}
	in := CreateVerificationInput{
		TenantID: call.TenantID, BrandID: uuid.New(), PlayerAccountID: uuid.New(), PersonID: uuid.New(), Call: call,
	}

	assertNoSentinelKYC(t, renderAllFormsKYC(t, call), "CallContext")
	assertNoSentinelKYC(t, renderAllFormsKYC(t, in), "CreateVerificationInput")
	assertNoSentinelKYC(t, renderAllFormsKYC(t, &call), "*CallContext")
	assertNoSentinelKYC(t, renderAllFormsKYC(t, &in), "*CreateVerificationInput")

	// Negative control (security review's explicit requirement, mirrored
	// here): the mechanism above must actually be capable of catching a
	// leak, proven against a plain type that does NOT redact.
	type leaky struct{ Secret string }
	found := false
	for _, out := range renderAllFormsKYC(t, leaky{Secret: kycCallContextRedactionSentinel}) {
		if strings.Contains(out, kycCallContextRedactionSentinel) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("negative control failed: the sentinel must be detectable by this test's own rendering mechanism")
	}
}
