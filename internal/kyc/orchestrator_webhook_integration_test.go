//go:build integration

// Stage 10.2 KYC-WH-1 (ADR 0091, design §H): K2-K9, K12, K13 package-level
// integration coverage for kyc.Orchestrator.ReceiveCallback - HTTP-layer
// coverage (K1, K10, K11, K14, K15, K16) lives in internal/httpserver and
// cmd/platform-api per the binding QA test-name map
// (docs/plans/stage-10.2-planning/03-review-qa-test-plan.md).
package kyc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func newWebhookFixture(t *testing.T, pool *db.Pool) (fixture, *MockKYCProvider, *Orchestrator, uuid.UUID) {
	t.Helper()
	f := seedFixture(t, pool)
	provider := NewMockKYCProvider()
	orch := NewOrchestrator(map[string]KYCProvider{"mock": provider}, NewMockWebhookCredentials(provider))
	verificationID := seedVerification(t, pool, f)
	var ref string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	if err != nil {
		t.Fatalf("read seeded provider_reference: %v", err)
	}
	provider.created[ref] = true // the seeded row's reference was minted by CreateVerification, so this mirrors the provider's own bookkeeping.
	return f, provider, orch, verificationID
}

func mustGetStatus(t *testing.T, pool *db.Pool, tenantID, id uuid.UUID) VerificationStatus {
	t.Helper()
	var status VerificationStatus
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_verifications WHERE id = $1`, id).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read verification status: %v", err)
	}
	return status
}

func mustCountAudit(t *testing.T, pool *db.Pool, tenantID, targetID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'kyc.provider_callback' AND target_id = $1`, targetID.String()).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// K2: valid in-process signature, A->A approves.
func TestKYCWebhook_ValidSameTenant_Approves(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")
	var v Verification
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Status != StatusApproved {
		t.Fatalf("expected approved, got %s", v.Status)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", n)
	}
}

// K3: A-signed delivered to B, where B has a row with the SAME reference
// string (direct insert - not something CreateVerification would ever
// produce on its own, but nothing stops a coincidence/adversary from
// trying it). Statement capture proves no read of B's row.
func TestKYCWebhook_CrossTenant_Rejected(t *testing.T) {
	pool := testPool(t)
	fA, provider, orch, verificationIDA := newWebhookFixture(t, pool)
	fB := seedFixture(t, pool)

	var refA string
	_ = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationIDA).Scan(&refA)
	})

	var verificationIDB uuid.UUID
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		verificationIDB = uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
			 VALUES ($1, $2, $3, $4, $5, 'pending', 'mock', $6)`,
			verificationIDB, fB.tenantID, fB.brandID, fB.playerID, fB.personID, refA,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant B row with A's reference: %v", err)
	}

	in := provider.CallbackPayload(fA.tenantID, refA, ProviderApproved, "auto_approved")
	var captured *recordingTx
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.ReceiveCallback(ctx, captured, fB.tenantID, "mock", in)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("expected a signature_invalid auth error, got %v", err)
	}
	for _, sql := range captured.Statements() {
		if strings.Contains(sql, "kyc_verifications") {
			t.Fatalf("K3: expected no read of any kyc_verifications row (tenant B's included), got statement: %q", sql)
		}
	}
	if got := mustGetStatus(t, pool, fB.tenantID, verificationIDB); got != StatusPending {
		t.Fatalf("expected tenant B's row unchanged, got %s", got)
	}
}

// equalSecretResolver returns the SAME credential secret regardless of
// tenantID - proving the tenant binding lives in the SIGNING INPUT (which
// includes tenant_id), not merely in "which secret got used" (K4).
type equalSecretResolver struct {
	secret     []byte
	providerID string
}

func (r equalSecretResolver) Resolve(_ context.Context, tenantID uuid.UUID, providerID, keyID string) (webhookauth.Credential, error) {
	if providerID != r.providerID || keyID != webhookauth.MockKeyID {
		return webhookauth.Credential{}, webhookauth.ErrCredentialUnavailable
	}
	return webhookauth.Credential{TenantID: tenantID, ProviderID: providerID, KeyID: keyID, Secret: r.secret, Fingerprint: webhookauth.Fingerprint(r.secret)}, nil
}

// K4: even with a resolver that hands out an EQUAL secret for every
// tenant, an A-signed callback delivered to B is still rejected, because
// the tenant id is bound into the signing input itself.
func TestKYCWebhook_EqualSecretResolver_CrossTenantRejected(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)
	secret := []byte("equal-secret-shared-by-every-tenant-32bytes!!")
	resolver := equalSecretResolver{secret: secret, providerID: "mock"}

	provider := NewMockKYCProvider()
	orch := NewOrchestrator(map[string]KYCProvider{"mock": provider}, resolver)

	verificationIDA := seedVerification(t, pool, fA)
	var refA string
	_ = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationIDA).Scan(&refA)
	})

	scheme := webhookauth.KYCScheme()
	body := []byte(`{"provider_reference":"` + refA + `","outcome":"approved","reason":"x"}`)
	sigForA := scheme.Sign(secret, fA.tenantID, "mock", webhookauth.MockKeyID, body)
	inA := webhookauth.Inbound{TenantID: fA.tenantID, ProviderID: "mock", Body: body, Header: map[string][]string{}}
	scheme.SetHeaders(headerOf(inA), webhookauth.MockKeyID, sigForA)

	// Delivered to B: the header/body bytes are byte-identical to what
	// verified for A, but B's own tenant id gets substituted into the
	// signing input the orchestrator recomputes - it will not match.
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fB.tenantID, "mock", inA)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("expected a signature_invalid auth error under an equal-secret resolver, got %v", err)
	}
}

func headerOf(in webhookauth.Inbound) map[string][]string { return in.Header }

// K5: field tampering - each named field/byte/header variant is rejected.
func TestKYCWebhook_TamperMatrix_Rejected(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	base := func() webhookauth.Inbound { return provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "x") }

	cases := map[string]func() webhookauth.Inbound{
		"flipped body byte": func() webhookauth.Inbound {
			in := base()
			b := append([]byte{}, in.Body...)
			b[0] ^= 0xFF
			in.Body = b
			return in
		},
		"whitespace appended to body": func() webhookauth.Inbound {
			in := base()
			in.Body = append(append([]byte{}, in.Body...), ' ')
			return in
		},
		"63 hex chars": func() webhookauth.Inbound {
			in := base()
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Header.Set(webhookauth.KYCSignatureHeader, sig[:len(sig)-1])
			return in
		},
		"65 hex chars": func() webhookauth.Inbound {
			in := base()
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Header.Set(webhookauth.KYCSignatureHeader, sig+"0")
			return in
		},
		"uppercase hex": func() webhookauth.Inbound {
			in := base()
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Header.Set(webhookauth.KYCSignatureHeader, strings.ToUpper(sig))
			return in
		},
		"missing key id header": func() webhookauth.Inbound {
			in := base()
			in.Header.Del(webhookauth.KYCKeyIDHeader)
			return in
		},
		"unknown key id": func() webhookauth.Inbound {
			in := base()
			in.Header.Set(webhookauth.KYCKeyIDHeader, "mock-v2")
			return in
		},
		"legacy signature field in body": func() webhookauth.Inbound {
			in := base()
			in.Body = []byte(`{"provider_reference":"` + ref + `","outcome":"approved","signature":"deadbeef"}`)
			return in
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			in := build()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
				return err
			})
			var authErr *CallbackAuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("%s: expected a CallbackAuthError, got %v", name, err)
			}
		})
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusPending {
		t.Fatalf("expected the verification untouched by every tampered attempt, got %s", got)
	}
}

// K7: a bad signature runs NO tenant-scoped statement at all (strict I1 -
// KYC has no ProviderAcceptsWebhook-equivalent pre-verification read).
func TestKYCWebhook_BadSignature_NoStatementBeforeVerification(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "x")
	sig := in.Header.Get(webhookauth.KYCSignatureHeader)
	flipped := strings.Replace(sig, "0", "f", 1)
	if flipped == sig {
		flipped = strings.Replace(sig, "1", "e", 1)
	}
	in.Header.Set(webhookauth.KYCSignatureHeader, flipped)

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.ReceiveCallback(ctx, captured, f.tenantID, "mock", in)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("expected signature_invalid, got %v", err)
	}
	if statements := captured.Statements(); len(statements) != 0 {
		t.Fatalf("strict I1: expected ZERO statements before verification succeeds, got %q", statements)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 0 {
		t.Fatalf("expected no audit row for a rejected bad-signature callback, got %d", n)
	}
}

// K8: replay is a no-op; pending-after-review_required is a no-op;
// anything after a staff decision is a no-op; 8 concurrent
// approved/rejected callbacks converge to exactly one terminal state with
// exactly one audit row.
func TestKYCWebhook_Replay_NoOp(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	apply := func(outcome ProviderOutcome) Verification {
		in := provider.CallbackPayload(f.tenantID, ref, outcome, "x")
		var v Verification
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			v, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
			return err
		})
		if err != nil {
			t.Fatalf("unexpected error applying %s: %v", outcome, err)
		}
		return v
	}

	v := apply(ProviderApproved)
	if v.Status != StatusApproved {
		t.Fatalf("expected approved, got %s", v.Status)
	}
	// Replay of the original approval: no-op, no second audit row.
	v = apply(ProviderApproved)
	if v.Status != StatusApproved {
		t.Fatalf("expected approved to remain after replay, got %s", v.Status)
	}
	// A backward transition after terminal: no-op, never resurrected (J11).
	v = apply(ProviderRejected)
	if v.Status != StatusApproved {
		t.Fatalf("expected approved to remain after a post-terminal rejected callback, got %s", v.Status)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 1 {
		t.Fatalf("expected exactly 1 audit row across the original approval + 2 no-ops, got %d", n)
	}
}

func TestKYCWebhook_PendingAfterReviewRequired_NoOp(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	apply := func(outcome ProviderOutcome) Verification {
		in := provider.CallbackPayload(f.tenantID, ref, outcome, "x")
		var v Verification
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			v, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
			return err
		})
		if err != nil {
			t.Fatalf("unexpected error applying %s: %v", outcome, err)
		}
		return v
	}
	v := apply(ProviderReviewRequired)
	if v.Status != StatusReviewRequired {
		t.Fatalf("expected review_required, got %s", v.Status)
	}
	v = apply(ProviderPending)
	if v.Status != StatusReviewRequired {
		t.Fatalf("expected review_required to survive a backward 'pending' callback, got %s", v.Status)
	}
}

func TestKYCWebhook_8ConcurrentTerminalCallbacks_ExactlyOneWins(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		outcome := ProviderApproved
		if i%2 == 0 {
			outcome = ProviderRejected
		}
		wg.Add(1)
		go func(i int, outcome ProviderOutcome) {
			defer wg.Done()
			in := provider.CallbackPayload(f.tenantID, ref, outcome, "concurrent")
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
				return err
			})
		}(i, outcome)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	final := mustGetStatus(t, pool, f.tenantID, verificationID)
	if final != StatusApproved && final != StatusRejected {
		t.Fatalf("expected exactly one terminal state, got %s", final)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 1 {
		t.Fatalf("expected exactly 1 audit row across 8 concurrent terminal callbacks, got %d", n)
	}
}

// K9: verified caller, unknown reference -> ErrNotFound (404 at the HTTP
// layer); bad outcome/non-JSON -> ErrCallbackMalformedBody, no audit row;
// outcome "error" -> no state change, one failure audit row.
func TestKYCWebhook_VerifiedUnknownReference_NotFound(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, _ := newWebhookFixture(t, pool)
	in := provider.CallbackPayload(f.tenantID, "no-such-reference", ProviderApproved, "x")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestKYCWebhook_VerifiedBadOutcome_MalformedBody_NoAudit(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	scheme := webhookauth.KYCScheme()
	body := []byte(`{"provider_reference":"` + ref + `","outcome":"not-a-real-outcome"}`)
	master := provider.master
	key := webhookauth.DeriveMockKey(master, webhookauth.KYCMockKeyLabel, f.tenantID, "mock")
	sig := scheme.Sign(key, f.tenantID, "mock", webhookauth.MockKeyID, body)
	in := webhookauth.Inbound{TenantID: f.tenantID, ProviderID: "mock", Body: body, Header: map[string][]string{}}
	scheme.SetHeaders(headerOf(in), webhookauth.MockKeyID, sig)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if !errors.Is(err, ErrCallbackMalformedBody) {
		t.Fatalf("expected ErrCallbackMalformedBody, got %v", err)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 0 {
		t.Fatalf("expected no audit row for a malformed (but verified) callback, got %d", n)
	}
}

func TestKYCWebhook_OutcomeError_NoStateChange_OneFailureAudit(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	before := mustGetStatus(t, pool, f.tenantID, verificationID)

	in := provider.CallbackPayload(f.tenantID, ref, ProviderError, "vendor_outage")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := mustGetStatus(t, pool, f.tenantID, verificationID)
	if before != after {
		t.Fatalf("expected outcome=error to never change status: before=%s after=%s", before, after)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 1 {
		t.Fatalf("expected exactly 1 failure audit row for outcome=error, got %d", n)
	}
}

// K13-adjacent (package-level): keys differ per tenant, per instance, and
// per domain label; stable within one instance.
func TestMockKYCProvider_KeyDerivation_PerTenantAndInstance(t *testing.T) {
	p1 := NewMockKYCProvider()
	p2 := NewMockKYCProvider()
	tenantA, tenantB := uuid.New(), uuid.New()

	kA1 := p1.deriveKey(tenantA, "mock")
	kA1Again := p1.deriveKey(tenantA, "mock")
	kB1 := p1.deriveKey(tenantB, "mock")
	kA2 := p2.deriveKey(tenantA, "mock")

	if string(kA1) != string(kA1Again) {
		t.Fatal("expected the derived key to be stable within one instance for the same tenant")
	}
	if string(kA1) == string(kB1) {
		t.Fatal("expected different tenants to derive different keys within one instance")
	}
	if string(kA1) == string(kA2) {
		t.Fatal("expected different instances (different per-process masters) to derive different keys for the same tenant")
	}
}
