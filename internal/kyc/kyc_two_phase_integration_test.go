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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
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

// ctxObliviousKYCResolver is a resolver that (unlike MockOutboundResolver)
// does NOT itself check txscope.Held(ctx) - it always succeeds, exactly
// like a hypothetical resolver bug or a resolver that predates IO-1B's own
// fix. Used ONLY to isolate CreateVerification's/SubmitVerification's own
// phase-B call-site guard (verification_service.go/document_service.go,
// immediately before the adapter call) from MockOutboundResolver's OWN,
// separate refusal, so the call-site tests below prove the call-site guard
// specifically, not merely "some refusal happened somewhere upstream".
type ctxObliviousKYCResolver struct{}

func (ctxObliviousKYCResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	return providercred.NewMockOutboundCredential(tenantID, "kyc", providerID), nil
}

// TestCreateVerification_RefusesUnderTxscopeHeld is IO-1B's own required
// test (architect review `rv-prh-architect.md`, INV-IO-1(b)): calling
// CreateVerification with a ctx that already carries txscope's held marker
// (as it would if, despite the API shape, some future caller invoked it
// from inside its own WithTenant closure) must refuse BEFORE ever reaching
// the adapter's own CreateVerification method - phase A itself still runs
// (its own nested WithTenant call is legal; txscope.Mark is idempotent on
// an already-marked ctx), but phase B's own txscope.Held(ctx) check fires
// on the SAME outer ctx the test itself marked, refusing with
// ErrProviderCallRefused and never invoking the adapter at all. Uses
// ctxObliviousKYCResolver (not the real MockOutboundResolver) so this test
// isolates the call-site guard itself, not the resolver's own separate
// refusal (which TestMockOutboundResolver_RefusesUnderTxscopeHeld, below,
// covers directly).
func TestCreateVerification_RefusesUnderTxscopeHeld(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var createCalls int
	provider := &spyKYCProvider{MockKYCProvider: NewMockKYCProvider()}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		createCalls++
		return provider.MockKYCProvider.CreateVerification(ctx, in)
	}

	var createErr error
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, createErr = CreateVerification(ctx, pool, ctxObliviousKYCResolver{}, provider, CreateVerificationParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
		})
		return nil
	}); err != nil {
		t.Fatalf("outer WithTenant (test scaffolding only): %v", err)
	}

	if !errors.Is(createErr, ErrProviderCallRefused) {
		t.Fatalf("expected ErrProviderCallRefused, got %v", createErr)
	}
	if createCalls != 0 {
		t.Fatalf("expected the adapter's own CreateVerification method to be called ZERO times, got %d", createCalls)
	}
}

// TestSubmitVerification_RefusesUnderTxscopeHeld is IO-1B's own required
// test for SubmitVerification's own phase B - same shape as
// TestCreateVerification_RefusesUnderTxscopeHeld above.
func TestSubmitVerification_RefusesUnderTxscopeHeld(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

	base := NewMockKYCProvider()
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.created[ref] = true
	var submitCalls int
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		submitCalls++
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	var submitErr error
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, submitErr = SubmitVerification(ctx, pool, ctxObliviousKYCResolver{}, provider, f.tenantID, verificationID)
		return nil
	}); err != nil {
		t.Fatalf("outer WithTenant (test scaffolding only): %v", err)
	}

	if !errors.Is(submitErr, ErrProviderCallRefused) {
		t.Fatalf("expected ErrProviderCallRefused, got %v", submitErr)
	}
	if submitCalls != 0 {
		t.Fatalf("expected the adapter's own SubmitVerification method to be called ZERO times, got %d", submitCalls)
	}
}

// TestMockOutboundResolver_RefusesUnderTxscopeHeld is IO-1B's own required
// coverage of the MOCK resolver's own, separate refusal (ADR 0095 §11's
// "a MOCK uses a Synthetic credential source with the SAME refusal" claim,
// which was previously false for KYC too): MockOutboundResolver.Resolve
// itself must refuse a txscope-held ctx, independent of CreateVerification/
// SubmitVerification's own call-site guard.
func TestMockOutboundResolver_RefusesUnderTxscopeHeld(t *testing.T) {
	held := txscope.Mark(context.Background())
	_, err := MockOutboundResolver{}.Resolve(held, nil, uuid.New(), "mock")
	if !errors.Is(err, ErrProviderCallRefused) {
		t.Fatalf("expected ErrProviderCallRefused, got %v", err)
	}
}

// seedDocument uploads one document under verificationID, using its own
// short transaction (UploadDocument's own phase-A-only shape) - a helper so
// SubmitVerification's own tests can exercise a non-empty document set (C4:
// an empty set is now a documented, provider-call-free no-op, so a test
// that means to observe the provider call must seed at least one document
// first).
func seedDocument(t *testing.T, pool *db.Pool, f fixture, verificationID uuid.UUID, docType DocumentType, filename string) uuid.UUID {
	t.Helper()
	var docID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := UploadDocument(ctx, tx, NewMockDocumentStorageProvider(), NewMockMalwareScanner(), UploadDocumentParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			VerificationID: verificationID, DocumentType: docType, Filename: filename, Content: tinyPNGBytes,
		})
		docID = d.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return docID
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
	seedDocument(t, singleConnPool, f, verificationID, DocumentPassport, "p.png")

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

// TestCreateVerification_UnrecognizedOutcomeWithNoReferenceLeavesOrphanUntouched
// is the code re-review 2 "pre-existing, low" note's own required test
// (RV-PRH-I2 KYC code re-review 2, 2026-09-27; ADR 0096 §20.5): a PROVIDER
// DEPENDENT phase-B outcome - an unrecognized ProviderOutcome (e.g.
// ProviderError) returned WITHOUT a Go error AND with no reference - must
// leave the phase-A orphan EXACTLY as committed ('unverified', NULL
// reference), never write a `pending` row with no reference (which would
// both supersede an existing approval under the primary read and become a
// permanent upload/submit dead end under N5).
func TestCreateVerification_UnrecognizedOutcomeWithNoReferenceLeavesOrphanUntouched(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{Outcome: ProviderError, Reason: "vendor_ambiguous"}, nil
	}

	_, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for an unrecognized outcome with no reference, got %v", err)
	}

	var status string
	var ref *string
	if dbErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, provider_reference FROM kyc_verifications WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerID).Scan(&status, &ref)
	}); dbErr != nil {
		t.Fatalf("read verification: %v", dbErr)
	}
	if status != string(StatusUnverified) || ref != nil {
		t.Fatalf("expected the orphan to be left untouched (unverified, NULL reference), got status=%q reference=%v", status, ref)
	}
}

// TestCreateVerification_UnrecognizedOutcomeWithReferenceStillBecomesPending
// is the control case: an unrecognized outcome WITH a genuine reference
// (the vendor accepted the request and gave back an id, but its own
// status code isn't one this platform recognizes yet) is UNAFFECTED by the
// fix above - it still becomes `pending`, since a real reference exists
// for a future callback/re-submission to resolve, unlike the no-reference
// case.
func TestCreateVerification_UnrecognizedOutcomeWithReferenceStillBecomesPending(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{Outcome: ProviderError, Reason: "vendor_ambiguous", ProviderReference: "mock-ref-still-issued"}, nil
	}

	v, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification: %v", err)
	}
	if v.Status != StatusPending {
		t.Fatalf("expected an unrecognized outcome WITH a reference to still become pending, got %q", v.Status)
	}
	if v.ProviderReference != "mock-ref-still-issued" {
		t.Fatalf("expected the reference to be stored, got %q", v.ProviderReference)
	}
}

// TestCreateVerification_PhaseCFailureLog_NeverLeaksRawVendorOutcomeText is
// the security re-verification 3 LOW finding's own required test: phase
// C's own "kyc_create_verification_phase_c_failed" log line used to log
// err.Error() directly, which embeds the RAW, vendor-controlled outcome
// string via applyCreateVerificationResult's own "%q" formatting (the
// unrecognized-outcome-with-no-reference error, §21.8) - unbounded
// adapter-supplied text in operator logs. Now bounded via
// RedactedProviderErrorDetail.
func TestCreateVerification_PhaseCFailureLog_NeverLeaksRawVendorOutcomeText(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	const sentinelOutcome = ProviderOutcome("SUPER-SECRET-VENDOR-OUTCOME-CODE-xyz789")
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{Outcome: sentinelOutcome, Reason: "vendor_ambiguous"}, nil
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	_, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), provider, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "kyc_create_verification_phase_c_failed") {
		t.Fatalf("expected the phase_c_failed log line, got: %s", logged)
	}
	if strings.Contains(logged, string(sentinelOutcome)) {
		t.Fatalf("kyc_create_verification_phase_c_failed leaked the raw vendor outcome string: %s", logged)
	}
	if !strings.Contains(logged, `detail="provider unavailable"`) {
		t.Fatalf(`expected detail="provider unavailable" in the log line, got: %s`, logged)
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
// read by statusForOutcome, never applied via applyForwardOnlyStatus.
func TestSubmitVerification_AmbiguousResultLeavesStatusUnchanged(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
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
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
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
	// N1 (RV-PRH-I2 KYC code re-review): a document is REQUIRED here, seeded
	// while the verification is still non-terminal. Without it, C4's own
	// "empty document set is a no-op" skip (§ above) would ALSO produce
	// `called == false` even if gatherSubmissionDocuments' terminal guard
	// were removed entirely - the two no-op paths would be indistinguishable,
	// making this test pass regardless of whether the terminal guard exists
	// (confirmed: removing the terminal guard alone left this test green
	// until this document was added). Seeding a document here is what makes
	// `called == false` actually PROVE the terminal guard fired, not merely
	// that there was nothing to submit.
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
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

// TestSubmissionIdempotencyKey_SortsDocumentIDs is submissionIdempotencyKey's
// OWN direct unit test (no database), added because
// TestSubmitVerification_IdempotencyKeyStableForMultiDocumentSet's own
// documents arrive from gatherSubmissionDocuments' `ORDER BY id` SQL
// query, which for standard UUIDs already happens to equal
// sort.Strings' own string-sorted order regardless of upload sequence -
// so that integration test alone cannot distinguish "submissionIdempotencyKey
// sorts its own input" from "its caller already handed it pre-sorted
// input" (removing sort.Strings there is a SURVIVING, not merely
// equivalent, mutant against that test alone - confirmed: code review's own
// re-verification reproduced the survival). This test calls
// submissionIdempotencyKey directly with two DELIBERATELY differently-
// ordered slices of the identical three document ids and requires the
// SAME key from both - a removed sort.Strings makes them diverge.
func TestSubmissionIdempotencyKey_SortsDocumentIDs(t *testing.T) {
	verificationID := uuid.New()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	order1 := []SubmittedDocument{{DocumentID: a}, {DocumentID: b}, {DocumentID: c}}
	order2 := []SubmittedDocument{{DocumentID: c}, {DocumentID: a}, {DocumentID: b}}

	key1 := submissionIdempotencyKey(verificationID, order1)
	key2 := submissionIdempotencyKey(verificationID, order2)
	if key1 != key2 {
		t.Fatalf("expected the SAME idempotency key regardless of input order, got %q vs %q", key1, key2)
	}
	want := independentSubmissionIdempotencyKey(verificationID, []uuid.UUID{a, b, c})
	if key1 != want {
		t.Fatalf("unexpected idempotency key shape: got %q, want %q", key1, want)
	}
}

// independentSubmissionIdempotencyKey computes ADR 0095 §15.3's documented
// key shape ("ks:" + verification id + ":" + sha256(sorted document ids,
// NUL-separated)) from FIRST PRINCIPLES - crypto/sha256 and sort.Strings
// called directly here, never through submissionIdempotencyKey itself - so
// a test asserting against this is not tautological (RV-PRH-I2 KYC code
// review F3/MD: the original test called submissionIdempotencyKey to
// compute its own "expected" value, which cannot detect a bug IN
// submissionIdempotencyKey, e.g. a dropped sort.Strings).
func independentSubmissionIdempotencyKey(verificationID uuid.UUID, docIDs []uuid.UUID) string {
	ids := make([]string, len(docIDs))
	for i, id := range docIDs {
		ids[i] = id.String()
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return "ks:" + verificationID.String() + ":" + hex.EncodeToString(h.Sum(nil))
}

// TestSubmitVerification_IdempotencyKeyStableForMultiDocumentSet is ADR
// 0095 §15.3's content-derived key, exercised over the full SubmitVerification
// pipeline with a THREE-document set (RV-PRH-I2 KYC code review F3/MD: the
// original test only ever submitted an EMPTY set, which never reached the
// provider with any documents at all - a real, non-trivial document_count is
// required to prove the key covers the actual submitted set). Its own
// expected value is computed independently
// (independentSubmissionIdempotencyKey above) rather than by calling
// submissionIdempotencyKey itself, for the same reason
// TestSubmissionIdempotencyKey_SortsDocumentIDs's own doc comment gives.
// NOTE (code review N2): this test alone does NOT prove sort.Strings is
// exercised - gatherSubmissionDocuments' own `ORDER BY id` SQL already
// yields sorted input regardless of upload order (confirmed: dropping
// sort.Strings survives this test on its own). This test instead proves
// end-to-end stability (the SAME key across two real calls) and that
// document_count=3 actually reaches the provider;
// TestSubmissionIdempotencyKey_SortsDocumentIDs above is the one that pins
// the sort itself.
func TestSubmitVerification_IdempotencyKeyStableForMultiDocumentSet(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	id1 := seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	id2 := seedDocument(t, pool, f, verificationID, DocumentSelfie, "s.png")
	id3 := seedDocument(t, pool, f, verificationID, DocumentProofOfAddress, "a.png")

	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
	var keys []string
	var docCounts []int
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		keys = append(keys, call.IdempotencyKey)
		docCounts = append(docCounts, len(docs))
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
	for i, n := range docCounts {
		if n != 3 {
			t.Fatalf("call %d: expected 3 documents submitted, got %d", i, n)
		}
	}
	if keys[0] != keys[1] {
		t.Fatalf("expected the SAME idempotency key for the same 3-document set, got %q and %q", keys[0], keys[1])
	}
	// Built with the three ids in a DIFFERENT order than they were uploaded
	// (id3, id1, id2) purely so this assertion cannot be satisfied by
	// accidentally matching upload order - independentSubmissionIdempotencyKey
	// sorts its own argument regardless, so this pins the documented key
	// shape either way. (This ordering choice does NOT by itself prove
	// submissionIdempotencyKey sorts its own input - see the NOTE above and
	// TestSubmissionIdempotencyKey_SortsDocumentIDs.)
	want := independentSubmissionIdempotencyKey(verificationID, []uuid.UUID{id3, id1, id2})
	if keys[0] != want {
		t.Fatalf("unexpected idempotency key shape: got %q, want %q", keys[0], want)
	}
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
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

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

// TestSubmitVerification_EmptyDocumentSetIsANoOp is C4's own required test
// (RV-PRH-I2 KYC code review): a verification with no current non-rejected
// documents at all is a documented no-op - no provider call is made, and
// the verification is returned unchanged.
func TestSubmitVerification_EmptyDocumentSetIsANoOp(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	called := false
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		called = true
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	before := mustGetStatus(t, pool, f.tenantID, verificationID)
	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if called {
		t.Fatal("C4: expected NO provider call for a verification with no non-rejected documents to submit")
	}
	if v.Status != before {
		t.Fatalf("expected the status untouched, got %q (was %q)", v.Status, before)
	}
}

// TestSubmitVerification_CredentialBindingMismatchFailsClosed is MB's own
// required test (RV-PRH-I2 KYC code review F3): SubmitVerification carries
// its OWN copy of the tenant/provider/domain binding check
// (CreateVerification's is already covered) - a resolver that hands back a
// credential for the WRONG tenant must never reach the provider here
// either.
func TestSubmitVerification_CredentialBindingMismatchFailsClosed(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

	// The SAME provider instance the verification was created against (its
	// provider_reference is registered in THIS instance's own `created`
	// map) - so that, if the binding check were ever removed, the call
	// would actually reach and SUCCEED against the provider, proving the
	// check itself is what stops it, rather than an unrelated "unknown
	// reference" failure from a differently-instantiated mock masking the
	// real assertion (a fresh, unregistered *MockKYCProvider would fail
	// closed for that unrelated reason regardless of this check).
	provider := NewMockKYCProvider()
	provider.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true

	_, err := SubmitVerification(context.Background(), pool, mismatchedKYCOutboundResolver{}, provider, f.tenantID, verificationID)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a mismatched credential, got %v", err)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusPending {
		t.Fatalf("expected the status untouched (still 'pending'), got %q", got)
	}
}

// TestSubmitVerification_NilOutboundResolverFailsClosedWithoutPanic is ME's
// own required test (RV-PRH-I2 KYC code review F3): a nil outbound
// resolver must fail closed with ErrProviderUnavailable, never panic.
func TestSubmitVerification_NilOutboundResolverFailsClosedWithoutPanic(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	provider := NewMockKYCProvider()

	_, err := SubmitVerification(context.Background(), pool, nil, provider, f.tenantID, verificationID)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for a nil outbound resolver, got %v", err)
	}
}

// TestCreateVerification_PhaseCCASRejectsAlreadyAppliedRow is MC's own
// required test (RV-PRH-I2 KYC code review F3): §15.2's own phase-C CAS
// predicate (`id AND tenant_id AND provider_reference IS NULL AND
// status='unverified'`) must refuse to re-apply onto a row that has
// already left that pre-reference state - exercised here by calling
// applyCreateVerificationResult a SECOND time with the same stale
// (pre-phase-C) Verification value, after the first (real)
// CreateVerification call has already applied its own result.
func TestCreateVerification_PhaseCCASRejectsAlreadyAppliedRow(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)

	v, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), NewMockKYCProvider(), CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification: %v", err)
	}
	if v.ProviderReference == "" {
		t.Fatal("test setup: expected phase C to have already applied a provider_reference")
	}

	// staleOrphan mirrors exactly what phase A's own insertOrphanVerification
	// return value looked like BEFORE phase C ran - status='unverified', no
	// provider_reference - the same value applyCreateVerificationResult
	// would be called with on a (hypothetical) second invocation.
	staleOrphan := Verification{
		ID: v.ID, TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
		Status: StatusUnverified, ProviderID: v.ProviderID,
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := applyCreateVerificationResult(ctx, tx, f.tenantID, staleOrphan, ProviderResult{
			ProviderReference: "second-application-attempt", Outcome: ProviderApproved, Reason: "should_never_apply",
		})
		return err
	})
	if err == nil {
		t.Fatal("MC: expected the phase-C CAS to refuse re-applying onto a row that already left the pre-reference state")
	}

	// The row's REAL, first-applied result must be completely untouched.
	got := mustGetStatus(t, pool, f.tenantID, v.ID)
	if got != v.Status {
		t.Fatalf("expected the original phase-C result untouched (%q), got %q", v.Status, got)
	}
	if gotRef := mustGetProviderReference(t, pool, f.tenantID, v.ID); gotRef != v.ProviderReference {
		t.Fatalf("expected the original provider_reference untouched (%q), got %q", v.ProviderReference, gotRef)
	}
}

// TestSubmitVerification_StaffRejectDuringProviderCall_Survives is R1's own
// required regression test, P1 (RV-PRH-I2 KYC code review/security review
// C1, HIGH): a compliance officer's REJECT decision, committed WHILE
// SubmitVerification's own provider round-trip is in flight, must survive
// - the provider's own (later, lower-priority) approval must never
// overwrite it. Before the fix this test reproduced the security review's
// exact finding: final status "approved" with the staff reviewer's
// reviewed_by still stamped on the row.
func TestSubmitVerification_StaffRejectDuringProviderCall_Survives(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)

	base := NewMockKYCProvider()
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.created[ref] = true
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		// Simulate a compliance officer's REJECT decision landing WHILE the
		// vendor round-trip is in flight - the exact race window §15.3's
		// provider call opens between phase A's read and phase C's write.
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
				VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: "suspected_fraud",
			})
			return err
		}); err != nil {
			t.Fatalf("simulate staff reject during phase B: %v", err)
		}
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if v.Status != StatusRejected {
		t.Fatalf("R1 (P1): expected the staff REJECT decision to survive a concurrent provider approval, got %q", v.Status)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusRejected {
		t.Fatalf("R1 (P1): expected the PERSISTED status to still be 'rejected', got %q", got)
	}
	if v.ReviewedBy != staffID {
		t.Fatalf("R1 (P1): expected the staff reviewer's own reviewed_by to survive, got %s (want %s)", v.ReviewedBy, staffID)
	}
}

// TestSubmitVerification_CallbackApprovalDuringProviderCall_NotDemoted is
// R1's own required regression test, P2: a verified callback's OWN
// forward-only CAS transition, committed WHILE SubmitVerification's
// provider round-trip is in flight, must never be DEMOTED by that
// submission's own (lower-rank) result - review_required must never
// overwrite an already-committed approved.
func TestSubmitVerification_CallbackApprovalDuringProviderCall_NotDemoted(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

	base := NewMockKYCProvider()
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.created[ref] = true
	// The submission's own eventual result - review_required, rank(2) -
	// strictly LOWER than the approved(rank 3) a callback commits mid-call.
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderReviewRequired, Reason: "manual_review"})
	orch := NewOrchestrator(map[string]KYCProvider{"mock": base}, NewMockWebhookCredentials(base))

	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		in := base.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto")
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
			return err
		}); err != nil {
			t.Fatalf("simulate callback approval during phase B: %v", err)
		}
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if v.Status != StatusApproved {
		t.Fatalf("R1 (P2): expected the concurrent callback's approval to survive a lower-rank submission result, got %q", v.Status)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusApproved {
		t.Fatalf("R1 (P2): expected the PERSISTED status to still be 'approved', got %q", got)
	}
}

// TestSubmitVerification_AuditRecordsStatusAppliedFlag is N4's own required
// test (RV-PRH-I2 KYC code re-review): `status_applied` is the ONLY audit
// signal distinguishing a submission whose own status write actually
// applied from one superseded by a concurrent, higher-or-equal-rank
// decision (applyForwardOnlyStatus's own `applied` return value, R1 fix) -
// hard-wiring it to `true` in applySubmissionResult would survive every
// other test, since none of them read this specific field. This asserts
// both values directly: `true` for an ordinary submission that changes the
// row, and `false` for one superseded by a concurrent staff decision
// (reusing the exact P1 race shape).
func TestSubmitVerification_AuditRecordsStatusAppliedFlag(t *testing.T) {
	t.Run("true when the submission's own status write applies", func(t *testing.T) {
		pool := testPoolSized(t, 1)
		f := seedFixture(t, pool)
		verificationID := seedVerification(t, pool, f)
		seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

		provider := NewMockKYCProvider()
		provider.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
		v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
		if err != nil {
			t.Fatalf("SubmitVerification: %v", err)
		}
		if v.Status == StatusPending {
			t.Fatalf("test setup: expected the submission to move the status forward, got %q", v.Status)
		}
		m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
		applied, ok := m["status_applied"].(bool)
		if !ok || !applied {
			t.Fatalf("N4: expected status_applied=true on an ordinary submission's audit row, got %v (present=%v)", m["status_applied"], ok)
		}
	})

	t.Run("false when a concurrent staff decision supersedes the submission", func(t *testing.T) {
		pool := testPoolSized(t, 1)
		f := seedFixture(t, pool)
		verificationID := seedVerification(t, pool, f)
		seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
		staffID := seedComplianceStaff(t, pool, f)

		base := NewMockKYCProvider()
		ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
		base.created[ref] = true
		base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
		provider := &spyKYCProvider{MockKYCProvider: base}
		provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
					VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: "suspected_fraud",
				})
				return err
			}); err != nil {
				t.Fatalf("simulate staff reject during phase B: %v", err)
			}
			return base.SubmitVerification(ctx, ref, docs, call)
		}

		if _, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID); err != nil {
			t.Fatalf("SubmitVerification: %v", err)
		}
		m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
		applied, ok := m["status_applied"].(bool)
		if !ok || applied {
			t.Fatalf("N4: expected status_applied=false on a superseded submission's audit row, got %v (present=%v)", m["status_applied"], ok)
		}
	})
}

// TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval
// is KYC-REVIEWREQ-FORWARD-1's own required test (identity-compliance
// domain ruling, `rv-prh-i2-kyc-identity-compliance.md` Ruling 2,
// 2026-09-27): a review_required row that a STAFF member set (reviewed_by
// IS NOT NULL) must NEVER be advanced to approved by a later provider
// result arriving on its own - only another explicit staff
// ReviewVerification call may move it forward. A provider result landing
// on such a row is a documented no-op (status_applied=false), never an
// error.
func TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)

	// A compliance officer escalates to review_required - a deliberate
	// human judgment call, stamping reviewed_by.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusReviewRequired, Reason: "needs additional evidence",
		})
		return err
	}); err != nil {
		t.Fatalf("seed staff review_required: %v", err)
	}

	// A later provider result, with NO staff actor involved, tries to move
	// the row forward to approved on its own (statusRank alone would allow
	// this: review_required(2) < approved's terminal rank(3)) - it must be
	// refused.
	provider := NewMockKYCProvider()
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	provider.created[ref] = true
	provider.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})

	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification: %v", err)
	}
	if v.Status != StatusReviewRequired {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: expected a staff-set review_required to stay review_required against a provider approval, got %q", v.Status)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusReviewRequired {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: expected the PERSISTED status to still be review_required, got %q", got)
	}
	if v.ReviewedBy != staffID {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: expected the staff reviewer's own reviewed_by to survive untouched, got %s (want %s)", v.ReviewedBy, staffID)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
	if applied, ok := m["status_applied"].(bool); !ok || applied {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: expected status_applied=false (a documented no-op, not an error) on the sticky-blocked submission, got %v (present=%v)", m["status_applied"], ok)
	}
}

// TestApplyForwardOnlyStatus_ProviderSetReviewRequiredStillAdvancesToApproved
// is KYC-REVIEWREQ-FORWARD-1's own required control case: a
// PROVIDER-set review_required (no staff actor - reviewed_by is still
// uuid.Nil, the vendor's own "needs manual review" signal) is UNAFFECTED
// by the sticky rule above - the ordinary forward-only behaviour (a later
// vendor `approved` proceeding automatically) still applies, since no
// human judgment is being silently overridden.
func TestApplyForwardOnlyStatus_ProviderSetReviewRequiredStillAdvancesToApproved(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

	// First submission: the PROVIDER's own outcome sets review_required -
	// no staff actor involved, reviewed_by stays uuid.Nil.
	provider := NewMockKYCProvider()
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	provider.created[ref] = true
	provider.SetOutcome(ref, ProviderResult{Outcome: ProviderReviewRequired, Reason: "needs_more_evidence"})
	v, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification (first, provider review_required): %v", err)
	}
	if v.Status != StatusReviewRequired {
		t.Fatalf("test setup: expected the first submission to reach review_required, got %q", v.Status)
	}
	if v.ReviewedBy != uuid.Nil {
		t.Fatalf("test setup: expected a PROVIDER-set review_required to leave reviewed_by uuid.Nil, got %s", v.ReviewedBy)
	}

	// Second submission: a later provider result reaches a genuine
	// approved outcome. With no staff actor ever having touched this row,
	// the ordinary forward-only rule applies and it proceeds.
	seedDocument(t, pool, f, verificationID, DocumentProofOfAddress, "addr.png")
	provider.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
	v, err = SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if err != nil {
		t.Fatalf("SubmitVerification (second, provider approved): %v", err)
	}
	if v.Status != StatusApproved {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: expected a PROVIDER-set review_required to still advance to approved on a later provider decision, got %q", v.Status)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
	if applied, ok := m["status_applied"].(bool); !ok || !applied {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: expected status_applied=true for the provider-to-provider forward move, got %v (present=%v)", m["status_applied"], ok)
	}
}

// seedOrphanVerificationID creates a genuine phase-A-only orphan row (no
// live provider reference) using CreateVerification's own documented
// nil-outbound-resolver failure mode (ADR 0095 §15.2) - the same mechanism
// TestCreateVerification_NilOutboundResolverLeavesHarmlessOrphanRow uses -
// rather than a raw SQL insert, so this is exactly the shape a real
// vendor outage produces.
func seedOrphanVerificationID(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	_, err := CreateVerification(context.Background(), pool, nil, NewMockKYCProvider(), CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("seed orphan verification: expected ErrProviderUnavailable, got %v", err)
	}
	var id uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM kyc_verifications WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerID).Scan(&id)
	}); err != nil {
		t.Fatalf("seed orphan verification: read id: %v", err)
	}
	return id
}

// TestUploadDocument_OrphanVerificationFailsClosed is N5's own required
// test for the upload side (RV-PRH-I2 KYC code review): a player must not
// be able to upload a document against an orphan verification (no live
// provider reference).
func TestUploadDocument_OrphanVerificationFailsClosed(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedOrphanVerificationID(t, pool, f)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UploadDocument(ctx, tx, NewMockDocumentStorageProvider(), NewMockMalwareScanner(), UploadDocumentParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			VerificationID: verificationID, DocumentType: DocumentPassport, Filename: "p.png", Content: tinyPNGBytes,
		})
		return err
	})
	if !errors.Is(err, ErrVerificationNotSubmitted) {
		t.Fatalf("N5: expected ErrVerificationNotSubmitted uploading against an orphan verification, got %v", err)
	}
	var docCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_documents WHERE verification_id = $1`, verificationID).Scan(&docCount)
	}); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if docCount != 0 {
		t.Fatalf("N5: expected NO document row for a rejected orphan upload, got %d", docCount)
	}
}

// TestSubmitVerification_OrphanVerificationFailsClosed is N5's own required
// test for the submit side: SubmitVerification must never call the
// provider with an empty reference against an orphan verification.
func TestSubmitVerification_OrphanVerificationFailsClosed(t *testing.T) {
	pool := testPoolSized(t, 1)
	f := seedFixture(t, pool)
	verificationID := seedOrphanVerificationID(t, pool, f)

	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	called := false
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		called = true
		return base.SubmitVerification(ctx, ref, docs, call)
	}

	_, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if !errors.Is(err, ErrVerificationNotSubmitted) {
		t.Fatalf("N5: expected ErrVerificationNotSubmitted submitting an orphan verification, got %v", err)
	}
	if called {
		t.Fatal("N5: expected NO provider call for an orphan verification (never send an empty reference to the vendor)")
	}
}

// TestReviewVerification_ConcurrentSubmissionDuringReview_ReturnsConflict is
// the "mirror race" required regression test (RV-PRH-I2 KYC code review,
// surfaced-to-other-owners item 1): ReviewVerification's own write was,
// until this fix, a blind `UPDATE ... WHERE id = $4` with no status
// predicate - so a SubmitVerification phase C (or a verified callback)
// committing between ReviewVerification's own read and its write could be
// silently overwritten, including moving a terminal `approved` BACKWARD to
// a staff `review_required`. Uses reviewVerificationTestRaceHook (the
// P1/P2 tests' own style, applied to ReviewVerification's read/write
// window instead of SubmitVerification's provider-call window) to commit a
// concurrent SubmitVerification approval exactly between
// ReviewVerification's read and its CAS write.
func TestReviewVerification_ConcurrentSubmissionDuringReview_ReturnsConflict(t *testing.T) {
	// A pool of >1 connections is REQUIRED here (unlike this file's other
	// tests): the hook below runs a genuinely separate transaction (its own
	// SubmitVerification call) WHILE the outer ReviewVerification
	// transaction is still open on its own connection - a single-connection
	// pool would deadlock (the outer tx holds the only connection and
	// never releases it until the hook's own call - which needs a second
	// connection from the SAME pool - returns).
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)

	base := NewMockKYCProvider()
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.created[ref] = true
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})

	t.Cleanup(func() { reviewVerificationTestRaceHook = nil })
	reviewVerificationTestRaceHook = func(id uuid.UUID) {
		if id != verificationID {
			return
		}
		if _, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), base, f.tenantID, verificationID); err != nil {
			t.Fatalf("simulate concurrent submission during review: %v", err)
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusReviewRequired, Reason: "need more documents",
		})
		return err
	})
	if !errors.Is(err, ErrVerificationStatusConflict) {
		t.Fatalf("mirror race: expected ErrVerificationStatusConflict, got %v", err)
	}
	// The concurrent submission's own approval must survive completely
	// untouched - the staff review must never have silently overwritten it
	// (moving a terminal approved BACKWARD to review_required).
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusApproved {
		t.Fatalf("mirror race: expected the concurrent approval to survive untouched, got %q", got)
	}
}
