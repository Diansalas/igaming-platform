//go:build integration

// PRH-I2 (ADR 0095 §15.2/§15.3) introduced the KYC create/submit phase split;
// PRH-2 E1 (ADR 0095 §38, ADR 0106) moved the vendor call off every HTTP path
// into the outbox worker. This file keeps the properties the split introduced,
// now driven THROUGH THE WORKER: no transaction held across a provider call,
// the per-call CallContext's fields and redaction, the fail-closed branches
// (no outbound resolver, a mismatched resolved credential), IC condition 2 (an
// ambiguous/timeout/transport-error result leaves status unchanged), a
// crash-between-phases proxy, cross-tenant isolation, and the forward-only /
// staff-sticky status rules. Synchronous-behaviour assertions (an error
// returned to the caller) were rewritten to the worker's observable outcome
// (an outbox state and class) while IC condition 2 is still asserted.
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

// tryAcquireKYCConnection proves a pool connection is genuinely free (not
// merely idle by happenstance).
func tryAcquireKYCConnection(pool *db.Pool, tenantID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
}

// spyKYCProvider wraps a *MockKYCProvider and lets a test observe/override
// exactly the two methods the worker's phase B calls OUTSIDE any transaction -
// every other KYCProvider method is the embedded mock's own untouched behavior.
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

// ctxObliviousKYCResolver is a resolver that (unlike MockOutboundResolver) does
// NOT itself check txscope.Held(ctx) - it always succeeds. Used ONLY to isolate
// the worker's own phase-B call-site guard from the resolver's separate refusal.
type ctxObliviousKYCResolver struct{}

func (ctxObliviousKYCResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	return providercred.NewMockOutboundCredential(tenantID, "kyc", providerID), nil
}

// claimOne claims exactly the next due row (the worker's own claim statement).
func claimOne(t *testing.T, w *OutboxWorker) claimedRow {
	t.Helper()
	row, err := w.claimNext(context.Background(), nil)
	if err != nil {
		t.Fatalf("claimNext: %v", err)
	}
	if row == nil {
		t.Fatal("claimNext: no due row")
	}
	return *row
}

// IO-1B (INV-IO-1(b)): the worker's phase B refuses the adapter call when its
// ctx is marked as holding a pooled transaction - BEFORE the adapter is reached.
func TestWorker_CallVendor_RefusesUnderTxscopeHeld(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	var createCalls, submitCalls int
	provider := &spyKYCProvider{MockKYCProvider: NewMockKYCProvider()}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		createCalls++
		return provider.MockKYCProvider.CreateVerification(ctx, in)
	}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		submitCalls++
		return provider.MockKYCProvider.SubmitVerification(ctx, ref, docs, call)
	}
	v, _ := requestCreate(t, pool, f, "mock")
	w := workerFor(pool, ctxObliviousKYCResolver{}, provider)
	row := claimOne(t, w)
	prep := &prepared{verification: v}

	held := txscope.Mark(context.Background())
	if got := w.callVendor(held, row, prep); got.kind != outcomeNotSent {
		t.Fatalf("create under a held ctx: outcome kind = %v, want not sent", got.kind)
	}
	row.Operation = OpSubmit
	if got := w.callVendor(held, row, prep); got.kind != outcomeNotSent {
		t.Fatalf("submit under a held ctx: outcome kind = %v, want not sent", got.kind)
	}
	if createCalls != 0 || submitCalls != 0 {
		t.Fatalf("the adapter must be called ZERO times under a held ctx, got create=%d submit=%d", createCalls, submitCalls)
	}
}

// The MOCK resolver has its own, separate refusal (ADR 0095 §11).
func TestMockOutboundResolver_RefusesUnderTxscopeHeld(t *testing.T) {
	held := txscope.Mark(context.Background())
	_, err := MockOutboundResolver{}.Resolve(held, nil, uuid.New(), "mock")
	if !errors.Is(err, ErrProviderCallRefused) {
		t.Fatalf("expected ErrProviderCallRefused, got %v", err)
	}
}

// seedDocument uploads one document under verificationID in its own short
// transaction (UploadDocument is phase A: the document row, its audit and the
// submit outbox row for the CURRENT set).
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

// submitReady returns a verification that is created and sent (a bound
// reference, status pending) with its create row drained, plus a mock that
// knows the reference.
func submitReady(t *testing.T, pool *db.Pool, f fixture) (uuid.UUID, *MockKYCProvider) {
	t.Helper()
	verificationID := seedVerification(t, pool, f)
	base := NewMockKYCProvider()
	base.created[mustGetProviderReference(t, pool, f.tenantID, verificationID)] = true
	return verificationID, base
}

// The split's own load-bearing property (ADR 0095 §15.2): P's and the claim's
// transactions committed - releasing their pooled connections - before the
// vendor's CreateVerification ever runs. Proven with a pool of exactly ONE
// connection: if any connection were still held, the hook's own concurrent
// acquire would time out.
func TestWorker_Create_NoConnectionHeldAcrossProviderCall(t *testing.T) {
	pool := rtPool(t, 1)
	f := seedFixture(t, pool)
	var sawHeld bool
	var acquireErr error
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		sawHeld = txscope.Held(ctx)
		acquireErr = tryAcquireKYCConnection(pool, f.tenantID)
		return base.CreateVerification(ctx, in)
	}
	v := createViaWorker(t, pool, NewMockOutboundResolver(), provider, f)
	if v.ProviderReference == "" {
		t.Fatal("expected the worker to have applied the create result")
	}
	if sawHeld {
		t.Fatal("txscope reported a transaction held during the vendor call")
	}
	if acquireErr != nil {
		t.Fatalf("could not acquire the pool's only connection during the vendor call (a connection was still held): %v", acquireErr)
	}
}

func TestWorker_Submit_NoConnectionHeldAcrossProviderCall(t *testing.T) {
	pool := rtPool(t, 1)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")

	var sawHeld bool
	var acquireErr error
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		sawHeld = txscope.Held(ctx)
		acquireErr = tryAcquireKYCConnection(pool, f.tenantID)
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit); r.State != OutboxSent {
		t.Fatalf("submit row = %s, want sent", r.State)
	}
	if sawHeld {
		t.Fatal("txscope reported a transaction held during the vendor call")
	}
	if acquireErr != nil {
		t.Fatalf("could not acquire the pool's only connection during the vendor call: %v", acquireErr)
	}
}

// A nil outbound resolver: the orphan stays exactly as phase A committed it
// (the harmless, documented failure mode), nothing is sent, the row retries
// (not_sent) and never writes the verification.
func TestWorker_NilOutboundResolverLeavesHarmlessOrphanRow(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	provider := NewMockKYCProvider()
	v, _ := requestCreate(t, pool, f, "mock")
	w := workerFor(pool, nil, provider)
	// A nil resolver passes the pass (ValidateForLoop would refuse the loop);
	// the item itself must fail closed, never panic.
	if st := w.RunPass(context.Background()); st.Results[resultRetry] != 1 {
		t.Fatalf("expected one retry, got %+v", st.Results)
	}
	r := onlyRow(t, pool, f.tenantID, v.ID, OpCreate)
	if r.State != OutboxPending || r.LastErrorClass != string(ClassNotSent) || r.FailedAttempts != 1 {
		t.Fatalf("create row = %+v, want pending / not_sent / 1 failed attempt", r)
	}

	var status VerificationStatus
	var ref *string
	var requested, submitted int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status, provider_reference FROM kyc_verifications WHERE id = $1`, v.ID).Scan(&status, &ref); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'kyc.verification_requested'`, f.tenantID).Scan(&requested); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'kyc.verification_submitted'`, f.tenantID).Scan(&submitted)
	}); err != nil {
		t.Fatalf("read orphan: %v", err)
	}
	if status != StatusUnverified || ref != nil {
		t.Fatalf("orphan must stay unverified with a NULL reference, got %q / %v", status, ref)
	}
	if requested != 1 || submitted != 0 {
		t.Fatalf("expected one verification_requested and NO verification_submitted, got %d / %d", requested, submitted)
	}
}

type mismatchedKYCOutboundResolver struct{}

func (mismatchedKYCOutboundResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, _ uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	// Always resolves for a DIFFERENT tenant than the one asked for.
	return providercred.NewMockOutboundCredential(uuid.New(), "kyc", providerID), nil
}

// A credential bound to the WRONG tenant never reaches the provider and is an
// integrity signal: failed_terminal at once, distinct class (security F5).
func TestWorker_Create_CredentialBindingMismatchTerminalNeverReachesProvider(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	var calls int
	provider := &spyKYCProvider{MockKYCProvider: NewMockKYCProvider()}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		calls++
		return provider.MockKYCProvider.CreateVerification(ctx, in)
	}
	v, _ := requestCreate(t, pool, f, "mock")
	passUntilQuiet(t, workerFor(pool, mismatchedKYCOutboundResolver{}, provider))
	if calls != 0 {
		t.Fatalf("a mismatched credential must never reach the provider, got %d calls", calls)
	}
	r := onlyRow(t, pool, f.tenantID, v.ID, OpCreate)
	if r.State != OutboxFailedTerminal || r.LastErrorClass != string(ClassCredentialBindingMismatch) {
		t.Fatalf("create row = %s / %q, want failed_terminal / credential_binding_mismatch", r.State, r.LastErrorClass)
	}
}

func TestWorker_Submit_CredentialBindingMismatchTerminalNeverReachesProvider(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	var calls int
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		calls++
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	submitViaWorker(t, pool, mismatchedKYCOutboundResolver{}, provider, f.tenantID, verificationID)
	if calls != 0 {
		t.Fatalf("a mismatched credential must never reach the provider, got %d calls", calls)
	}
	r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit)
	if r.State != OutboxFailedTerminal || r.LastErrorClass != string(ClassCredentialBindingMismatch) {
		t.Fatalf("submit row = %s / %q, want failed_terminal / credential_binding_mismatch", r.State, r.LastErrorClass)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusPending {
		t.Fatalf("the verification status must be untouched (pending), got %q", got)
	}
}

// ADR 0096 §20.5: an unrecognized outcome (ProviderError) with NO reference is
// never applied - the orphan stays exactly as phase A committed it - and is an
// ambiguous retry, never a `pending` row without a reference.
func TestWorker_Create_UnrecognizedOutcomeWithNoReferenceLeavesOrphanUntouched(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	provider := &spyKYCProvider{MockKYCProvider: NewMockKYCProvider()}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{Outcome: ProviderError, Reason: "vendor_ambiguous"}, nil
	}
	v, _ := requestCreate(t, pool, f, "mock")
	w := workerFor(pool, NewMockOutboundResolver(), provider)
	if st := w.RunPass(context.Background()); st.Results[resultRetry] != 1 {
		t.Fatalf("expected one ambiguous retry, got %+v", st.Results)
	}
	r := onlyRow(t, pool, f.tenantID, v.ID, OpCreate)
	if r.State != OutboxPending || r.LastErrorClass != string(ClassAmbiguous) {
		t.Fatalf("create row = %s / %q, want pending / ambiguous", r.State, r.LastErrorClass)
	}
	if got := mustGetStatus(t, pool, f.tenantID, v.ID); got != StatusUnverified {
		t.Fatalf("orphan must stay unverified, got %q", got)
	}
	if ref := mustGetProviderReference(t, pool, f.tenantID, v.ID); ref != "" {
		t.Fatalf("orphan must keep a NULL reference, got %q", ref)
	}
}

// Control: an unrecognized outcome WITH a genuine reference still becomes pending.
func TestWorker_Create_UnrecognizedOutcomeWithReferenceStillBecomesPending(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	provider := &spyKYCProvider{MockKYCProvider: NewMockKYCProvider()}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{Outcome: ProviderError, Reason: "vendor_ambiguous", ProviderReference: "mock-ref-still-issued"}, nil
	}
	v := createViaWorker(t, pool, NewMockOutboundResolver(), provider, f)
	if v.Status != StatusPending || v.ProviderReference != "mock-ref-still-issued" {
		t.Fatalf("want pending with the reference stored, got %q / %q", v.Status, v.ProviderReference)
	}
}

// Redaction: neither the worker's log lines nor any audit row nor the outbox
// carries a raw vendor outcome string (security re-verification 3 LOW, kept).
func TestWorker_UnrecognizedOutcome_LogsNeverLeakRawVendorText(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	const sentinelOutcome = ProviderOutcome("SUPER-SECRET-VENDOR-OUTCOME-CODE-xyz789")
	provider := &spyKYCProvider{MockKYCProvider: NewMockKYCProvider()}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return ProviderResult{Outcome: sentinelOutcome, Reason: "vendor_ambiguous"}, nil
	}
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	v, _ := requestCreate(t, pool, f, "mock")
	passUntilQuiet(t, workerFor(pool, NewMockOutboundResolver(), provider))
	if strings.Contains(logBuf.String(), string(sentinelOutcome)) {
		t.Fatalf("a worker log line leaked the raw vendor outcome string: %s", logBuf.String())
	}
	var meta string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(string_agg(metadata::text, ' '), '') FROM audit_log WHERE tenant_id = $1`, f.tenantID).Scan(&meta)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(meta, string(sentinelOutcome)) {
		t.Fatalf("an audit row leaked the raw vendor outcome string: %s", meta)
	}
	_ = v
}

// ADR 0095 §15.2/§9.1: TenantID/ProviderID match the verification, the
// credential domain is kyc, and the idempotency key is "kv:" + the row's id.
func TestWorker_Create_CallContextFieldsPassedToProvider(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	base := NewMockKYCProvider()
	var captured CreateVerificationInput
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		captured = in
		return base.CreateVerification(ctx, in)
	}
	v := createViaWorker(t, pool, NewMockOutboundResolver(), provider, f)
	if captured.Call.TenantID != f.tenantID || captured.Call.ProviderID != "mock" || captured.Call.Credential.Domain != "kyc" {
		t.Fatalf("unexpected call context %v", captured.Call)
	}
	if want := "kv:" + v.ID.String(); captured.Call.IdempotencyKey != want {
		t.Fatalf("Call.IdempotencyKey = %q, want %q", captured.Call.IdempotencyKey, want)
	}
	if captured.Call.Deadline.IsZero() {
		t.Fatal("expected a non-zero Call.Deadline")
	}
	if captured.PlayerAccountID != f.playerID || captured.PersonID != f.personID || captured.BrandID != f.brandID {
		t.Fatalf("the create input must carry the verification's own account ids, got %+v", captured)
	}
}

// IC condition 2 (ADR 0095 §15.3): an ambiguous/timeout/transport-error submit
// result leaves kyc_verifications.status COMPLETELY unchanged; the row retries.
func TestWorker_Submit_AmbiguousResultLeavesStatusUnchanged_ICCondition2(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	before := mustGetStatus(t, pool, f.tenantID, verificationID)
	if before != StatusPending {
		t.Fatalf("setup: expected pending, got %q", before)
	}
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		return ProviderResult{}, errors.New("simulated ambiguous/timeout transport failure")
	}
	submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if after := mustGetStatus(t, pool, f.tenantID, verificationID); after != before {
		t.Fatalf("IC condition 2 violated: status changed from %q to %q after an ambiguous result", before, after)
	}
	r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit)
	if r.State != OutboxPending || r.LastErrorClass != string(ClassAmbiguous) || r.FailedAttempts != 1 {
		t.Fatalf("submit row = %+v, want pending / ambiguous / 1", r)
	}
}

// IC condition 2's OTHER shape: a well-formed result carrying ProviderError.
func TestWorker_Submit_ProviderErrorOutcomeLeavesStatusUnchanged_ICCondition2(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	before := mustGetStatus(t, pool, f.tenantID, verificationID)
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		return ProviderResult{ProviderReference: ref, Outcome: ProviderError, Reason: "vendor_ambiguous"}, nil
	}
	v := submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if v.Status != before {
		t.Fatalf("IC condition 2 violated: status changed from %q to %q after a ProviderError outcome", before, v.Status)
	}
	if !auditActionExistsKYC(t, pool, f.tenantID, verificationID, "kyc.verification_submitted_to_provider") {
		t.Fatal("expected the existing kyc.verification_submitted_to_provider failure audit row to be kept")
	}
	if r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit); r.State != OutboxPending || r.LastErrorClass != string(ClassAmbiguous) {
		t.Fatalf("submit row = %s / %q, want pending / ambiguous", r.State, r.LastErrorClass)
	}
}

// A verification already terminal: the submit row is cancelled
// (verification_terminal) and NO provider call is attempted.
func TestWorker_Submit_TerminalVerificationCancelsWithoutACall(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	// The document is seeded while the verification is still non-terminal, so
	// `called == false` proves the terminal guard fired, not "nothing to send".
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "manual"})
		return err
	}); err != nil {
		t.Fatalf("seed terminal status: %v", err)
	}
	provider := &spyKYCProvider{MockKYCProvider: base}
	called := false
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		called = true
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	v := submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if called {
		t.Fatal("expected NO provider call for an already-terminal verification")
	}
	if v.Status != StatusApproved {
		t.Fatalf("expected the terminal status untouched, got %q", v.Status)
	}
	if r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit); r.State != OutboxCancelled || r.CancelReason != string(CancelVerificationTerminal) {
		t.Fatalf("submit row = %s / %q, want cancelled / verification_terminal", r.State, r.CancelReason)
	}
}

// submissionIdempotencyKey's OWN direct unit test (no database): two
// deliberately differently-ordered slices of the same ids derive the SAME key.
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
	if want := independentSubmissionIdempotencyKey(verificationID, []uuid.UUID{a, b, c}); key1 != want {
		t.Fatalf("unexpected idempotency key shape: got %q, want %q", key1, want)
	}
}

// independentSubmissionIdempotencyKey computes the documented key shape from
// FIRST PRINCIPLES (never through submissionIdempotencyKey itself), so a test
// asserting against it is not tautological.
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

// The pinned set's key reaches the vendor, byte-identical to the independent
// computation, with all three documents; a re-upload of the same set is a
// duplicate no-op (one live/sent row).
func TestWorker_Submit_IdempotencyKeyForMultiDocumentSet(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	id1 := seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	id2 := seedDocument(t, pool, f, verificationID, DocumentSelfie, "s.png")
	id3 := seedDocument(t, pool, f, verificationID, DocumentProofOfAddress, "a.png")

	var keys []string
	var docCounts []int
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		keys = append(keys, call.IdempotencyKey)
		docCounts = append(docCounts, len(docs))
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if len(keys) != 1 || docCounts[0] != 3 {
		t.Fatalf("expected exactly one send of 3 documents (the two earlier rows are superseded), got keys=%d counts=%v", len(keys), docCounts)
	}
	want := independentSubmissionIdempotencyKey(verificationID, []uuid.UUID{id3, id1, id2})
	if keys[0] != want {
		t.Fatalf("unexpected idempotency key: got %q, want %q", keys[0], want)
	}
	// Superseded earlier rows (sets of 1 and 2 documents), one sent row of 3.
	var sent, superseded int
	for _, r := range readOutbox(t, pool, f.tenantID, verificationID) {
		if r.Operation != OpSubmit {
			continue
		}
		switch {
		case r.State == OutboxSent:
			sent++
		case r.State == OutboxCancelled && r.CancelReason == string(CancelSuperseded):
			superseded++
		}
	}
	if sent != 1 || superseded != 2 {
		t.Fatalf("want 1 sent + 2 superseded submit rows, got %d + %d", sent, superseded)
	}
}

// Two tenants each create their own verification; each is visible only under
// its own tenant scope (RLS).
func TestWorker_Create_CrossTenantNeverLeaksAcrossTenants(t *testing.T) {
	pool := rtPool(t, 5)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)
	vA := createViaWorker(t, pool, NewMockOutboundResolver(), NewMockKYCProvider(), fA)
	vB := createViaWorker(t, pool, NewMockOutboundResolver(), NewMockKYCProvider(), fB)
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetVerificationByID(ctx, tx, vA.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound reading tenant A's verification under tenant B, got %v", err)
	}
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetVerificationByID(ctx, tx, vB.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound reading tenant B's verification under tenant A, got %v", err)
	}
	if len(readOutbox(t, pool, fB.tenantID, vA.ID)) != 0 {
		t.Fatal("tenant B must not see tenant A's outbox rows")
	}
}

// Crash proxy (a genuine process crash cannot be simulated in-process): the
// worker's ctx is cancelled the INSTANT the vendor call returns. Phase C runs
// on a detached, bounded context, so the vendor's accepted result is applied.
func TestWorker_Create_ContextCancelledBeforePhaseC_StillAppliesResult(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	base := NewMockKYCProvider()
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onCreateVerification = func(c context.Context, in CreateVerificationInput) (ProviderResult, error) {
		result, err := base.CreateVerification(c, in)
		cancel() // the vendor accepted; only the platform's phase C remains
		return result, err
	}
	v, _ := requestCreate(t, pool, f, "mock")
	w := workerFor(pool, NewMockOutboundResolver(), provider)
	row := claimOne(t, w)
	if got := w.processItem(ctx, row); got != resultSent {
		t.Fatalf("processItem result = %q, want sent despite the cancelled ctx", got)
	}
	if ctx.Err() == nil {
		t.Fatal("setup: the ctx should be cancelled")
	}
	if mustGetProviderReference(t, pool, f.tenantID, v.ID) == "" || mustGetStatus(t, pool, f.tenantID, v.ID) != StatusPending {
		t.Fatal("expected phase C to have applied the provider's reference and pending status")
	}
	if !auditActionExistsKYC(t, pool, f.tenantID, v.ID, "kyc.verification_submitted") {
		t.Fatal("expected a kyc.verification_submitted audit record from phase C")
	}
}

func TestWorker_Submit_ContextCancelledBeforePhaseC_StillAppliesResult(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto"})
	ctx, cancel := context.WithCancel(context.Background())
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(c context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		result, err := base.SubmitVerification(c, ref, docs, call)
		cancel()
		return result, err
	}
	w := workerFor(pool, NewMockOutboundResolver(), provider)
	row := claimOne(t, w)
	if got := w.processItem(ctx, row); got != resultSent {
		t.Fatalf("processItem result = %q, want sent despite the cancelled ctx", got)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusApproved {
		t.Fatalf("expected the approved outcome applied despite the cancelled ctx, got %q", got)
	}
}

// kycCallContextRedactionSentinel is the exact secret NewMockOutboundCredential
// embeds - asserted never rendered by any of CallContext's formatting paths.
const kycCallContextRedactionSentinel = "mock-outbound-credential-not-a-real-secret"

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

// ADR 0095 §9.1's redaction contract, applied to KYC's own CallContext copy
// (including its negative control).
func TestCallContext_NeverRendersSecret(t *testing.T) {
	cred := providercred.NewMockOutboundCredential(uuid.New(), "kyc", "mock")
	if !strings.Contains(string(cred.Secret()), kycCallContextRedactionSentinel) {
		t.Fatalf("setup: the mock credential's secret no longer contains the expected sentinel")
	}
	call := CallContext{TenantID: uuid.New(), ProviderID: "mock", Credential: cred, IdempotencyKey: "kv:test-verification", Deadline: time.Now().Add(time.Minute)}
	in := CreateVerificationInput{TenantID: call.TenantID, BrandID: uuid.New(), PlayerAccountID: uuid.New(), PersonID: uuid.New(), Call: call}
	assertNoSentinelKYC(t, renderAllFormsKYC(t, call), "CallContext")
	assertNoSentinelKYC(t, renderAllFormsKYC(t, in), "CreateVerificationInput")
	assertNoSentinelKYC(t, renderAllFormsKYC(t, &call), "*CallContext")
	assertNoSentinelKYC(t, renderAllFormsKYC(t, &in), "*CreateVerificationInput")

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

// C4: a pinned document rejected before the send leaves NO current document:
// the row is cancelled (no_documents) and NO provider call is made.
func TestWorker_Submit_NoDocumentsLeftIsANoCallCancel(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	docID := seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewDocument(ctx, tx, ReviewDocumentParams{DocumentID: docID, StaffID: staffID, NewStatus: DocumentRejected, Reason: "blurry"})
		return err
	}); err != nil {
		t.Fatalf("reject document: %v", err)
	}
	provider := &spyKYCProvider{MockKYCProvider: base}
	called := false
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		called = true
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	before := mustGetStatus(t, pool, f.tenantID, verificationID)
	submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if called {
		t.Fatal("C4: expected NO provider call when no non-rejected documents remain")
	}
	if r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit); r.State != OutboxCancelled || r.CancelReason != string(CancelNoDocuments) {
		t.Fatalf("submit row = %s / %q, want cancelled / no_documents", r.State, r.CancelReason)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != before {
		t.Fatalf("status must be untouched, got %q (was %q)", got, before)
	}
}

// §15.2's phase-C CAS predicate must refuse to re-apply onto a row that has
// already left the pre-reference state: the typed CAS miss, status untouched.
func TestApplyCreateVerificationResult_CASRefusesAlreadyAppliedRow(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	v := createViaWorker(t, pool, NewMockOutboundResolver(), NewMockKYCProvider(), f)
	if v.ProviderReference == "" {
		t.Fatal("setup: expected a bound provider_reference")
	}
	staleOrphan := Verification{
		ID: v.ID, TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
		Status: StatusUnverified, ProviderID: v.ProviderID,
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := applyCreateVerificationResult(ctx, tx, f.tenantID, staleOrphan, ProviderResult{
			ProviderReference: "second-application-attempt", Outcome: ProviderApproved, Reason: "should_never_apply",
		}, workerAudit{})
		return err
	})
	if !errors.Is(err, errCreateDecidedConcurrently) {
		t.Fatalf("MC: expected the typed CAS miss, got %v", err)
	}
	if got := mustGetStatus(t, pool, f.tenantID, v.ID); got != v.Status {
		t.Fatalf("expected the original result untouched (%q), got %q", v.Status, got)
	}
	if gotRef := mustGetProviderReference(t, pool, f.tenantID, v.ID); gotRef != v.ProviderReference {
		t.Fatalf("expected the original reference untouched (%q), got %q", v.ProviderReference, gotRef)
	}
}

// R1 P1 (RV-PRH-I2 security C1, HIGH): a compliance officer's REJECT committed
// WHILE the vendor round-trip is in flight survives the vendor's later approval.
func TestWorker_Submit_StaffRejectDuringProviderCall_Survives(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
	provider := &spyKYCProvider{MockKYCProvider: base}
	provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: "suspected_fraud"})
			return err
		}); err != nil {
			t.Fatalf("simulate staff reject during phase B: %v", err)
		}
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	v := submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if v.Status != StatusRejected || v.ReviewedBy != staffID {
		t.Fatalf("R1 (P1): the staff REJECT must survive; got %q reviewed_by %s", v.Status, v.ReviewedBy)
	}
	if r := onlyRow(t, pool, f.tenantID, verificationID, OpSubmit); r.State != OutboxSent {
		t.Fatalf("the submit row ends sent (the vendor call happened), got %s", r.State)
	}
}

// R1 P2: a verified callback's approval committed mid-call is never demoted.
func TestWorker_Submit_CallbackApprovalDuringProviderCall_NotDemoted(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
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
	v := submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
	if v.Status != StatusApproved {
		t.Fatalf("R1 (P2): the concurrent callback's approval must survive, got %q", v.Status)
	}
}

// N4: `status_applied` is the ONLY audit signal distinguishing a submission
// whose own status write applied from one superseded by a concurrent decision.
func TestWorker_Submit_AuditRecordsStatusAppliedFlag(t *testing.T) {
	t.Run("true when the submission's own status write applies", func(t *testing.T) {
		pool := rtPool(t, 5)
		f := seedFixture(t, pool)
		verificationID, base := submitReady(t, pool, f)
		seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
		v := submitViaWorker(t, pool, NewMockOutboundResolver(), base, f.tenantID, verificationID)
		if v.Status == StatusPending {
			t.Fatalf("setup: expected the submission to move the status forward, got %q", v.Status)
		}
		m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
		if applied, ok := m["status_applied"].(bool); !ok || !applied {
			t.Fatalf("N4: expected status_applied=true, got %v (present=%v)", m["status_applied"], ok)
		}
		if m["platform_service"] != workerPlatformService {
			t.Fatalf("the worker-written row must carry metadata.platform_service, got %v", m["platform_service"])
		}
	})
	t.Run("false when a concurrent staff decision supersedes the submission", func(t *testing.T) {
		pool := rtPool(t, 5)
		f := seedFixture(t, pool)
		verificationID, base := submitReady(t, pool, f)
		seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
		staffID := seedComplianceStaff(t, pool, f)
		ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
		base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
		provider := &spyKYCProvider{MockKYCProvider: base}
		provider.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: "suspected_fraud"})
				return err
			}); err != nil {
				t.Fatalf("simulate staff reject during phase B: %v", err)
			}
			return base.SubmitVerification(ctx, ref, docs, call)
		}
		submitViaWorker(t, pool, NewMockOutboundResolver(), provider, f.tenantID, verificationID)
		m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
		if applied, ok := m["status_applied"].(bool); !ok || applied {
			t.Fatalf("N4: expected status_applied=false, got %v (present=%v)", m["status_applied"], ok)
		}
	})
}

// KYC-REVIEWREQ-FORWARD-1: a staff-set review_required is never advanced by a
// later provider approval; the provider-set one still advances.
func TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: verificationID, StaffID: staffID, NewStatus: StatusReviewRequired, Reason: "needs additional evidence"})
		return err
	}); err != nil {
		t.Fatalf("seed staff review_required: %v", err)
	}
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
	v := submitViaWorker(t, pool, NewMockOutboundResolver(), base, f.tenantID, verificationID)
	if v.Status != StatusReviewRequired || v.ReviewedBy != staffID {
		t.Fatalf("KYC-REVIEWREQ-FORWARD-1: staff review_required must stay, got %q reviewed_by %s", v.Status, v.ReviewedBy)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
	if applied, ok := m["status_applied"].(bool); !ok || applied {
		t.Fatalf("expected status_applied=false on the sticky-blocked submission, got %v", m["status_applied"])
	}
}

func TestApplyForwardOnlyStatus_ProviderSetReviewRequiredStillAdvancesToApproved(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderReviewRequired, Reason: "needs_more_evidence"})
	v := submitViaWorker(t, pool, NewMockOutboundResolver(), base, f.tenantID, verificationID)
	if v.Status != StatusReviewRequired || v.ReviewedBy != uuid.Nil {
		t.Fatalf("setup: expected provider review_required with no reviewer, got %q / %s", v.Status, v.ReviewedBy)
	}
	seedDocument(t, pool, f, verificationID, DocumentProofOfAddress, "addr.png")
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})
	v = submitViaWorker(t, pool, NewMockOutboundResolver(), base, f.tenantID, verificationID)
	if v.Status != StatusApproved {
		t.Fatalf("expected a provider-set review_required to advance to approved, got %q", v.Status)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", verificationID)
	if applied, ok := m["status_applied"].(bool); !ok || !applied {
		t.Fatalf("expected status_applied=true for the provider-to-provider forward move, got %v", m["status_applied"])
	}
}

// N5 (relaxed by ADR 0106): an upload against an orphan whose create row is
// STILL LIVE is accepted (its submit row waits for the create); against an
// orphan with no live create it fails closed exactly as before.
func TestUploadDocument_OrphanWithLiveCreateAcceptedAndEnqueued(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID := orphanViaPhaseA(t, pool, f)
	docID := seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	var submit *obRow
	for _, r := range readOutbox(t, pool, f.tenantID, verificationID) {
		if r.Operation == OpSubmit {
			rr := r
			submit = &rr
		}
	}
	if submit == nil || submit.State != OutboxPending || len(submit.DocumentIDs) != 1 || submit.DocumentIDs[0] != docID {
		t.Fatalf("expected a pending submit row pinning the uploaded document, got %+v", submit)
	}
}

func TestUploadDocument_OrphanWithoutLiveCreateFailsClosed(t *testing.T) {
	pool := rtPool(t, 5)
	f := seedFixture(t, pool)
	verificationID := orphanViaPhaseA(t, pool, f)
	// The create ends failed_terminal (a deterministic binding mismatch).
	passUntilQuiet(t, workerFor(pool, mismatchedKYCOutboundResolver{}, NewMockKYCProvider()))
	if r := onlyRow(t, pool, f.tenantID, verificationID, OpCreate); r.State != OutboxFailedTerminal {
		t.Fatalf("setup: create row = %s, want failed_terminal", r.State)
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UploadDocument(ctx, tx, NewMockDocumentStorageProvider(), NewMockMalwareScanner(), UploadDocumentParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			VerificationID: verificationID, DocumentType: DocumentPassport, Filename: "p.png", Content: tinyPNGBytes,
		})
		return err
	})
	if !errors.Is(err, ErrVerificationNotSubmitted) {
		t.Fatalf("N5: expected ErrVerificationNotSubmitted, got %v", err)
	}
	var docCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_documents WHERE verification_id = $1`, verificationID).Scan(&docCount)
	}); err != nil {
		t.Fatal(err)
	}
	if docCount != 0 {
		t.Fatalf("N5: expected NO document row for a rejected orphan upload, got %d", docCount)
	}
}

// "Mirror race": ReviewVerification's own CAS fails closed when a worker's
// submit result commits between its read and its write.
func TestReviewVerification_ConcurrentSubmissionDuringReview_ReturnsConflict(t *testing.T) {
	// A pool of >1 connection is REQUIRED: the hook below runs a genuinely
	// separate transaction (a worker pass) while the outer review tx is open.
	pool := rtPool(t, 8)
	f := seedFixture(t, pool)
	verificationID, base := submitReady(t, pool, f)
	seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")
	staffID := seedComplianceStaff(t, pool, f)
	ref := mustGetProviderReference(t, pool, f.tenantID, verificationID)
	base.SetOutcome(ref, ProviderResult{Outcome: ProviderApproved, Reason: "auto_approved"})

	t.Cleanup(func() { reviewVerificationTestRaceHook = nil })
	reviewVerificationTestRaceHook = func(id uuid.UUID) {
		if id != verificationID {
			return
		}
		passUntilQuiet(t, workerFor(pool, NewMockOutboundResolver(), base))
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: verificationID, StaffID: staffID, NewStatus: StatusReviewRequired, Reason: "need more documents"})
		return err
	})
	if !errors.Is(err, ErrVerificationStatusConflict) {
		t.Fatalf("mirror race: expected ErrVerificationStatusConflict, got %v", err)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusApproved {
		t.Fatalf("mirror race: the concurrent approval must survive untouched, got %q", got)
	}
}
