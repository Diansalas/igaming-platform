//go:build integration

// Gate 10.3-W1 fix round, security S-5 / code review #4 / identity-
// compliance condition 1: the PLATFORM normalizes an adapter's reason at
// every write site (CreateVerification, the document-submission path,
// applyCallbackOutcome) and records `reason_truncated: true` in the audit
// metadata. These tests use an adapter that normalizes NOTHING and returns
// a ~4 KB, control/bidi-laden reason from each method.
package kyc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// rawReason is the unnormalized adapter reason: C0 controls, an ANSI
// escape, a bidi override, and ~4 KB of payload.
var rawReason = "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text" + strings.Repeat("R", 4096) + "\u2066tail\x00"

// rawReasonAdapter wraps the MOCK (real verification, parsing and
// references) but returns rawReason, unnormalized and with
// ReasonTruncated=false, from CreateVerification, SubmitVerification and
// HandleCallback - the "adapter normalizes only somewhere else, or not at
// all" failure scenario of security F5. It stays a synthetic component
// (the embedded MOCK's SyntheticComponent is promoted).
type rawReasonAdapter struct{ *MockKYCProvider }

func (a rawReasonAdapter) CreateVerification(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
	r, err := a.MockKYCProvider.CreateVerification(ctx, in)
	r.Reason, r.ReasonTruncated = rawReason, false
	return r, err
}

func (a rawReasonAdapter) SubmitVerification(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
	r, err := a.MockKYCProvider.SubmitVerification(ctx, ref, docs, call)
	r.Reason, r.ReasonTruncated = rawReason, false
	return r, err
}

func (a rawReasonAdapter) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (ProviderResult, error) {
	r, err := a.MockKYCProvider.HandleCallback(ctx, in, cred)
	if err != nil {
		return r, err
	}
	r.Reason, r.ReasonTruncated = rawReason, false
	return r, nil
}

func assertBoundedClean(t *testing.T, where, got string) {
	t.Helper()
	if len(got) == 0 || len(got) > MaxReasonBytes {
		t.Fatalf("%s: want a non-empty reason of at most %d bytes, got %d bytes", where, MaxReasonBytes, len(got))
	}
	if err := reasonConformanceViolation(got); err != nil {
		t.Fatalf("%s: %v (%q)", where, err, got)
	}
	want, _ := NormalizeReason(rawReason)
	if got != want {
		t.Fatalf("%s: want exactly NormalizeReason(raw), got %q", where, got)
	}
}

func storedReason(t *testing.T, pool *db.Pool, tenantID, id uuid.UUID) string {
	t.Helper()
	var reason *string
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM kyc_verifications WHERE id = $1`, id).Scan(&reason)
	}); err != nil {
		t.Fatalf("read stored reason: %v", err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

// latestAuditMetadata returns the metadata of the newest audit row with
// action for target id.
func latestAuditMetadata(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string, id uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE action = $1 AND target_id = $2 ORDER BY created_at DESC LIMIT 1`,
			action, id.String()).Scan(&raw)
	}); err != nil {
		t.Fatalf("read %s audit row: %v", action, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s audit metadata: %v", action, err)
	}
	return m
}

func assertTruncatedFlag(t *testing.T, where string, m map[string]any) {
	t.Helper()
	if v, ok := m["reason_truncated"]; !ok || v != true {
		t.Fatalf("%s: want reason_truncated=true in audit metadata, got %v", where, m)
	}
}

// createWithRawAdapter runs CreateVerification with a rawReasonAdapter and
// returns the adapter, the verification and its provider reference
// (registered with the MOCK so its callbacks and submissions resolve).
func createWithRawAdapter(t *testing.T, pool *db.Pool, f fixture) (rawReasonAdapter, Verification) {
	t.Helper()
	adapter := rawReasonAdapter{NewMockKYCProvider()}
	v, err := CreateVerification(context.Background(), pool, NewMockOutboundResolver(), adapter, CreateVerificationParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
	})
	if err != nil {
		t.Fatalf("CreateVerification with an unnormalized adapter reason must succeed (never a CHECK-constraint 500), got %v", err)
	}
	return adapter, v
}

func TestPlatformNormalizesReason_CreateVerification(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	_, v := createWithRawAdapter(t, pool, f)

	assertBoundedClean(t, "returned verification", v.Reason)
	assertBoundedClean(t, "kyc_verifications.reason", storedReason(t, pool, f.tenantID, v.ID))
	assertTruncatedFlag(t, "kyc.verification_submitted", latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted", v.ID))
}

func TestPlatformNormalizesReason_SubmitVerification(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	adapter, v := createWithRawAdapter(t, pool, f)
	seedDocument(t, pool, f, v.ID, DocumentPassport, "p.png") // C4: an empty document set is now a no-op, never reaching the provider

	_, err := SubmitVerification(context.Background(), pool, NewMockOutboundResolver(), adapter, f.tenantID, v.ID)
	if err != nil {
		t.Fatalf("submission with an unnormalized adapter reason must succeed, got %v", err)
	}
	assertBoundedClean(t, "kyc_verifications.reason after submission", storedReason(t, pool, f.tenantID, v.ID))
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_submitted_to_provider", v.ID)
	assertTruncatedFlag(t, "kyc.verification_submitted_to_provider", m)
	reason, _ := m["reason"].(string)
	assertBoundedClean(t, "submission audit reason", reason)
}

func TestPlatformNormalizesReason_CallbackStatusUpdate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	adapter, v := createWithRawAdapter(t, pool, f)
	orch := NewOrchestrator(map[string]KYCProvider{"mock": adapter}, NewMockWebhookCredentials(adapter.MockKYCProvider))

	in := adapter.CallbackPayload(f.tenantID, v.ProviderReference, ProviderRejected, "short")
	var applied bool
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, applied, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if err != nil || !applied {
		t.Fatalf("a verified callback with an unnormalized adapter reason must apply (never a CHECK-constraint 500), got applied=%v err=%v", applied, err)
	}
	if got := mustGetStatus(t, pool, f.tenantID, v.ID); got != StatusRejected {
		t.Fatalf("want status rejected, got %s", got)
	}
	assertBoundedClean(t, "kyc_verifications.reason after callback", storedReason(t, pool, f.tenantID, v.ID))
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.provider_callback", v.ID)
	assertTruncatedFlag(t, "kyc.provider_callback (success)", m)
	reason, _ := m["reason"].(string)
	assertBoundedClean(t, "callback success audit reason", reason)
}

func TestPlatformNormalizesReason_CallbackProviderError(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	adapter, v := createWithRawAdapter(t, pool, f)
	orch := NewOrchestrator(map[string]KYCProvider{"mock": adapter}, NewMockWebhookCredentials(adapter.MockKYCProvider))
	before := storedReason(t, pool, f.tenantID, v.ID)

	in := adapter.CallbackPayload(f.tenantID, v.ProviderReference, ProviderError, "short")
	var applied bool
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, applied, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if err != nil || !applied {
		t.Fatalf("an outcome=error callback with an unnormalized reason must write its failure audit row, got applied=%v err=%v", applied, err)
	}
	if after := storedReason(t, pool, f.tenantID, v.ID); after != before {
		t.Fatalf("outcome=error must not change the stored reason, got %q -> %q", before, after)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.provider_callback", v.ID)
	assertTruncatedFlag(t, "kyc.provider_callback (failure)", m)
	reason, _ := m["reason"].(string)
	assertBoundedClean(t, "callback error audit reason", reason)
}

// TestPlatformNormalizesReason_MockTruncationFlagSurvives: the MOCK already
// normalizes (and so truncates) in HandleCallback; the platform's second,
// idempotent pass cannot see that, so the flag must travel on
// ProviderResult.ReasonTruncated into the audit row (identity-compliance
// condition 1, the mock call site).
func TestPlatformNormalizesReason_MockTruncationFlagSurvives(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	}); err != nil {
		t.Fatal(err)
	}
	in := provider.CallbackPayload(f.tenantID, ref, ProviderRejected, rawReason)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertTruncatedFlag(t, "mock callback", latestAuditMetadata(t, pool, f.tenantID, "kyc.provider_callback", verificationID))
}

// TestPlatformNormalizesReason_ShortCleanReasonNotFlagged is the
// no-false-positive control: a short, clean reason is stored unchanged and
// its audit row carries no reason_truncated key.
func TestPlatformNormalizesReason_ShortCleanReasonNotFlagged(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	}); err != nil {
		t.Fatal(err)
	}
	in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "document_ok")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := storedReason(t, pool, f.tenantID, verificationID); got != "document_ok" {
		t.Fatalf("a clean short reason must be stored unchanged, got %q", got)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.provider_callback", verificationID)
	if _, ok := m["reason_truncated"]; ok {
		t.Fatalf("a non-truncated reason must not carry reason_truncated, got %v", m)
	}
	if m["reason"] != "document_ok" {
		t.Fatalf("audit reason = %v, want document_ok", m["reason"])
	}
}

// TestPlatformNormalizesReason_StaffReviewFlagged: the staff review path
// (ReviewVerification) records the same reason_truncated flag on its
// kyc.verification_status_changed audit row (identity-compliance
// condition 1, the staff-reason write).
func TestPlatformNormalizesReason_StaffReviewFlagged(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: rawReason,
		})
		return err
	}); err != nil {
		t.Fatalf("staff review with an oversized reason must succeed, got %v", err)
	}
	m := latestAuditMetadata(t, pool, f.tenantID, "kyc.verification_status_changed", verificationID)
	assertTruncatedFlag(t, "kyc.verification_status_changed", m)
	reason, _ := m["reason"].(string)
	assertBoundedClean(t, "staff review audit reason", reason)
}
