//go:build integration

// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091). C7 (design §H; QA plan
// "I1 pre-verification proof"; architect ruling J8/§2): strict I1 for
// casino is that ReceiveCallback runs ZERO tenant-scoped statements before
// (c) HMAC verification succeeds - stricter than payments (which has
// exactly one pre-verification read, ProviderAcceptsWebhook) because the
// casino capability check ((d) in ReceiveCallback's own doc comment) is
// DELIBERATELY post-verification (design §C3). This file proves that
// invariant directly, the same way internal/kyc/recording_tx_integration_
// test.go proves K7 for KYC, by wrapping the real pgx.Tx ReceiveCallback
// runs on with a statement-recording observer and asserting the recorded
// log.
//
// It covers three pre-verification failure shapes named by the task:
//   - a bad signature (TestCasinoWebhook_BadSignature_NoStatementBeforeVerification);
//   - a missing credential, i.e. the resolver has nothing bound to the
//     requested key id (TestCasinoWebhook_MissingCredential_NoStatementBeforeVerification);
//   - a foreign credential, i.e. a (deliberately broken, for this test
//     only) resolver that returns a credential bound to the WRONG tenant/
//     provider - exercising ReceiveCallback's own defense-in-depth
//     cred.TenantID/cred.ProviderID re-check
//     (TestCasinoWebhook_ForeignCredential_NoStatementBeforeVerification).
//
// and the positive half: on a verified callback, the first statement is
// the L0.1 bet-delivery advisory lock - dispatch happens IMMEDIATELY
// after verification, with no intervening tenant-scoped statement
// (TestCasinoWebhook_VerifiedCallback_AdvisoryLockIsFirstStatement).
// Stage 10.3 CAS-CAP-ROLLBACK-1 deleted ReceiveCallback's own pre-dispatch
// LoadCapability read entirely (capability/status now gate NEW BETS ONLY,
// resolved inside postBet for the session's own brand, after L0.1/
// idempotency/tombstone) - so this test's assertion moved from "the first
// statement is LoadCapability" to "the first statement is L0.1", which is
// the new, stricter thing that is actually invariant across bet/win/
// rollback dispatch (win and rollback take the SAME advisory lock as
// their own very first statement too - see orchestrator.go's
// acquireProviderTxDeliveryLock).
//
// Mutation-kill demonstration (recorded in docs/plans/stage-10.2-planning/
// 08-webhook-test-traceability.md, not committed as code): moving
// LoadCapability (or any other tenant-scoped read) to run BEFORE
// provider.HandleCallback in ReceiveCallback makes
// TestCasinoWebhook_BadSignature_NoStatementBeforeVerification fail (a
// non-empty statement list is captured for a callback that never
// verifies); the missing/foreign-credential and nil-resolver cases reject
// earlier, before the moved read, so only the bad-signature case can
// catch that mutation - see the traceability doc's transcript. The
// cross-tenant shape is covered in
// cross_tenant_statement_capture_integration_test.go. The mutation was reverted immediately after capturing
// that failure.
package casino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// foreignCredentialResolver is a deliberately misbehaving webhookauth.
// Resolver used ONLY to exercise ReceiveCallback's own defense-in-depth
// check (orchestrator.go's "no conforming resolver should ever return a
// credential bound to a different tenant/provider" comment) - it always
// returns a credential bound to a DIFFERENT tenant than the one it was
// asked to resolve for, no real resolver implementation behaves this way.
type foreignCredentialResolver struct {
	foreignTenantID uuid.UUID
	providerID      string
	secret          []byte
}

func (r foreignCredentialResolver) ResolveKey(_ context.Context, _ uuid.UUID, providerID, keyID string) (webhookauth.Credential, error) {
	return webhookauth.Credential{
		TenantID:    r.foreignTenantID, // wrong on purpose: never the tenantID the caller asked for.
		ProviderID:  r.providerID,
		KeyID:       keyID,
		Secret:      r.secret,
		Fingerprint: webhookauth.Fingerprint(r.secret),
	}, nil
}

// Resolve adapts ResolveKey to the ADR 0093 §4 resolver signature (a
// single-key test double ignores tx; KeyImplicit fails closed).
func (r foreignCredentialResolver) Resolve(ctx context.Context, _ webhookauth.TenantReader, tenantID uuid.UUID, providerID, keyID string, sel webhookauth.KeySelection) (webhookauth.CredentialSet, error) {
	return webhookauth.ResolveSingleKey(ctx, r, tenantID, providerID, keyID, sel)
}

// Recheck implements webhookauth.Resolver for this test double (ADR 0094
// §4.1): it has no handle rows, so it accepts only a handle-less
// credential bound to tenantID.
func (r foreignCredentialResolver) Recheck(_ context.Context, _ pgx.Tx, tenantID uuid.UUID, c webhookauth.Credential) error {
	if c.HandleID != uuid.Nil || c.TenantID != tenantID {
		return webhookauth.ErrCredentialUnavailable
	}
	return nil
}

// assertZeroStatementsBeforeVerification is this file's shared negative
// assertion: captured must be completely empty (strict I1 - stricter than
// payments' "at most the one read-only EXISTS" bar, since casino has no
// pre-verification tenant-scoped read at all).
func assertZeroStatementsBeforeVerification(t *testing.T, captured *recordingTx, err error, wantReason webhookauth.Reason) {
	t.Helper()
	var authErr *webhookauth.AuthError
	if !errors.As(err, &authErr) || authErr.Reason != wantReason {
		t.Fatalf("expected *webhookauth.AuthError{Reason: %s}, got %v", wantReason, err)
	}
	if statements := captured.Statements(); len(statements) != 0 {
		t.Fatalf("strict I1: expected ZERO statements before verification succeeds, got %q", statements)
	}
}

// TestCasinoWebhook_BadSignature_NoStatementBeforeVerification is C7's bad-
// signature case: a genuinely registered provider and a genuinely
// resolvable credential, but a tampered signature header - the ONLY thing
// wrong is (c) itself.
func TestCasinoWebhook_BadSignature_NoStatementBeforeVerification(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-c7-bad-sig", "", "round-c7", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())
	sig := payload.Header.Get(webhookauth.CasinoSignatureHeader)
	flipped := strings.Replace(sig, "0", "f", 1)
	if flipped == sig {
		flipped = strings.Replace(sig, "1", "e", 1)
	}
	payload.Header = payload.Header.Clone()
	payload.Header.Set(webhookauth.CasinoSignatureHeader, flipped)

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock-casino", payload)
		return err
	})
	assertZeroStatementsBeforeVerification(t, captured, err, webhookauth.ReasonSignatureInvalid)
}

// TestCasinoWebhook_MissingCredential_NoStatementBeforeVerification is
// C7's missing-credential case: the provider is registered, but the
// resolver has nothing bound to the key id the callback presents (an
// unknown key id - the resolver's own ErrCredentialUnavailable path, the
// same "missing credential" shape a real vendor's key rotation or a
// misconfigured tenant would produce).
func TestCasinoWebhook_MissingCredential_NoStatementBeforeVerification(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-c7-missing-cred", "", "round-c7", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())
	payload.Header = payload.Header.Clone()
	payload.Header.Set(webhookauth.CasinoKeyIDHeader, "mock-v2") // never resolvable: the mock resolver only ever knows "mock-v1".

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock-casino", payload)
		return err
	})
	assertZeroStatementsBeforeVerification(t, captured, err, webhookauth.ReasonCredentialUnavailable)
}

// TestCasinoWebhook_ForeignCredential_NoStatementBeforeVerification is
// C7's foreign-credential case: the resolver itself is broken and returns
// a credential bound to a DIFFERENT tenant than requested - ReceiveCallback's
// own defense-in-depth re-check (cred.TenantID != tenantID) must reject it
// before ANY tenant-scoped statement runs, exactly like every other
// pre-verification failure.
func TestCasinoWebhook_ForeignCredential_NoStatementBeforeVerification(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)

	foreignTenantID := uuid.New()
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, foreignCredentialResolver{
		foreignTenantID: foreignTenantID, providerID: "mock-casino", secret: webhookauth.NewMockMaster(),
	})

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-c7-foreign-cred", "", "round-c7", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock-casino", payload)
		return err
	})
	assertZeroStatementsBeforeVerification(t, captured, err, webhookauth.ReasonCredentialUnavailable)
}

// TestCasinoWebhook_NilResolver_NoStatementBeforeVerification is C7/C10's
// nil-resolver case: production wiring (mockProviderWiring with test
// support off) leaves the Orchestrator's resolver nil, and every callback
// must fail closed as ReasonNoResolver before any tenant-scoped statement -
// never a fallback to unauthenticated verification.
func TestCasinoWebhook_NilResolver_NoStatementBeforeVerification(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, nil)

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-c7-nil-resolver", "", "round-c7", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock-casino", payload)
		return err
	})
	assertZeroStatementsBeforeVerification(t, captured, err, webhookauth.ReasonNoResolver)
}

// TestCasinoWebhook_VerifiedCallback_AdvisoryLockIsFirstStatement is C7's
// positive half, updated for Stage 10.3 CAS-CAP-ROLLBACK-1: once (c)
// verification succeeds, the very FIRST tenant-scoped statement
// ReceiveCallback issues is postBet's own L0.1 bet-delivery advisory
// lock - proving dispatch runs immediately after verification, with no
// earlier session/ledger/round/capability read.
func TestCasinoWebhook_VerifiedCallback_AdvisoryLockIsFirstStatement(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-c7-verified", "", "round-c7-verified", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock-casino", payload)
		return err
	})
	// This callback's session_id is a fresh uuid.New(), never a real launch
	// session, so postBet itself fails past the capability check (
	// ErrLaunchSessionRequired) - irrelevant to what this test proves,
	// which is only about statement ORDER up to and including the first
	// one. Any error other than the pre-verification AuthError family is
	// acceptable here.
	var authErr *webhookauth.AuthError
	if errors.As(err, &authErr) {
		t.Fatalf("expected verification to SUCCEED (a post-verification error is fine), got a pre-verification AuthError: %v", err)
	}

	statements := captured.Statements()
	if len(statements) == 0 {
		t.Fatal("expected at least one statement (the L0.1 advisory lock), got none")
	}
	first := statements[0]
	if !strings.Contains(first, "pg_advisory_xact_lock") || !strings.Contains(first, "casino_bet_delivery") {
		t.Fatalf("expected the FIRST statement to be the L0.1 casino_bet_delivery advisory lock, got %q\nall statements: %q", first, statements)
	}
}
