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
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
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
		v, _, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
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

	before := noeffect.Capture(t, pool, []uuid.UUID{fA.tenantID, fB.tenantID}, []noeffect.Verification{
		{TenantID: fA.tenantID, ID: verificationIDA}, {TenantID: fB.tenantID, ID: verificationIDB},
	})

	in := provider.CallbackPayload(fA.tenantID, refA, ProviderApproved, "auto_approved")
	var captured *recordingTx
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, _, err := orch.receiveCallbackInTx(ctx, captured, fB.tenantID, "mock", in)
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
	// Six-point checklist, BOTH tenants (design §H): A's own row/audit
	// state, and B's, are both untouched by an A-signed callback rejected
	// at B's slug.
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{fA.tenantID, fB.tenantID}, []noeffect.Verification{
		{TenantID: fA.tenantID, ID: verificationIDA}, {TenantID: fB.tenantID, ID: verificationIDB},
	}, before)
}

// equalSecretResolver returns the SAME credential secret regardless of
// tenantID - proving the tenant binding lives in the SIGNING INPUT (which
// includes tenant_id), not merely in "which secret got used" (K4).
type equalSecretResolver struct {
	secret     []byte
	providerID string
}

func (r equalSecretResolver) ResolveKey(_ context.Context, tenantID uuid.UUID, providerID, keyID string) (webhookauth.Credential, error) {
	if providerID != r.providerID || keyID != webhookauth.MockKeyID {
		return webhookauth.Credential{}, webhookauth.ErrCredentialUnavailable
	}
	return webhookauth.Credential{TenantID: tenantID, ProviderID: providerID, KeyID: keyID, Secret: r.secret, Fingerprint: webhookauth.Fingerprint(r.secret)}, nil
}

// Resolve adapts ResolveKey to the ADR 0093 §4 resolver signature (a
// single-key test double ignores tx; KeyImplicit fails closed).
func (r equalSecretResolver) Resolve(ctx context.Context, _ webhookauth.TenantReader, tenantID uuid.UUID, providerID, keyID string, sel webhookauth.KeySelection) (webhookauth.CredentialSet, error) {
	return webhookauth.ResolveSingleKey(ctx, r, tenantID, providerID, keyID, sel)
}

// Recheck implements webhookauth.Resolver for this test double (ADR 0094
// §4.1): it has no handle rows, so it accepts only a handle-less
// credential bound to tenantID.
func (r equalSecretResolver) Recheck(_ context.Context, _ pgx.Tx, tenantID uuid.UUID, c webhookauth.Credential) error {
	if c.HandleID != uuid.Nil || c.TenantID != tenantID {
		return webhookauth.ErrCredentialUnavailable
	}
	return nil
}

// K4: even with a resolver that hands out an EQUAL secret for every
// tenant, an A-signed callback delivered to B is still rejected, because
// the tenant id is bound into the signing input itself.
func TestKYCWebhook_EqualSecretResolver_CrossTenantRejected(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)
	// K16 (Stage 10.2 final review, security §7): a fresh per-process
	// crypto/rand secret, never a hard-coded literal - even though this
	// fixture authenticates nothing outside this test's in-process
	// resolver, the codebase should never carry a literal of this shape.
	secret := webhookauth.NewMockMaster()
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

	before := noeffect.Capture(t, pool, []uuid.UUID{fA.tenantID, fB.tenantID}, []noeffect.Verification{
		{TenantID: fA.tenantID, ID: verificationIDA},
	})

	// Delivered to B: the header/body bytes are byte-identical to what
	// verified for A, but B's own tenant id gets substituted into the
	// signing input the orchestrator recomputes - it will not match.
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, fB.tenantID, "mock", inA)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("expected a signature_invalid auth error under an equal-secret resolver, got %v", err)
	}
	// Six-point checklist, BOTH tenants: neither A's row nor B's (empty)
	// state moved, and no audit row appeared for either tenant.
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{fA.tenantID, fB.tenantID}, []noeffect.Verification{
		{TenantID: fA.tenantID, ID: verificationIDA},
	}, before)
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

	// A second, unrelated verification in the SAME tenant, so the
	// cross-verification provider_reference substitution case below has a
	// real second reference to substitute in - J13 names this case
	// explicitly, distinct from K3/K4's cross-TENANT scenario.
	verificationID2 := seedVerification(t, pool, f)
	var ref2 string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID2).Scan(&ref2)
	})
	provider.created[ref2] = true

	base := func() webhookauth.Inbound { return provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "x") }

	// signBody signs body correctly for f.tenantID/provider - used by the
	// legacy-field case (K1, Stage 10.2 final review, M1): the guard it
	// exists to prove (mock_provider.go's post-verification rejection of a
	// legacy top-level "signature" field, ~line 202) is only reachable
	// through a GENUINELY VERIFIED body. Signing over the untampered
	// original body and then mutating in.Body afterward (as every other
	// case here deliberately does, to exercise Scheme.Verify itself) would
	// fail at Scheme.Verify BEFORE ever reaching that guard - exactly the
	// finding this fixes.
	signBody := func(body []byte) webhookauth.Inbound {
		key := provider.deriveKey(f.tenantID, provider.ID())
		sig := webhookauth.KYCScheme().Sign(key, f.tenantID, provider.ID(), webhookauth.MockKeyID, body)
		in := webhookauth.Inbound{TenantID: f.tenantID, ProviderID: provider.ID(), Body: body, Header: map[string][]string{}}
		webhookauth.KYCScheme().SetHeaders(in.Header, webhookauth.MockKeyID, sig)
		return in
	}

	// tamperCase pairs a body/header transform with the EXACT
	// webhookauth.Reason the orchestrator must report (K1, Stage 10.2
	// final review: this subtest previously asserted only
	// errors.As(*CallbackAuthError), never Reason).
	type tamperCase struct {
		build      func() webhookauth.Inbound
		wantReason webhookauth.Reason
	}

	cases := map[string]tamperCase{
		"flipped body byte": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			b := append([]byte{}, in.Body...)
			b[0] ^= 0xFF
			in.Body = b
			return in
		}},
		"whitespace appended to body (one byte)": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			in.Body = append(append([]byte{}, in.Body...), ' ')
			return in
		}},
		"provider_reference field value tampered": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			in.Body = []byte(`{"provider_reference":"tampered-` + ref + `","outcome":"approved","reason":"x"}`)
			return in
		}},
		"outcome field value tampered": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			in.Body = []byte(`{"provider_reference":"` + ref + `","outcome":"rejected","reason":"x"}`)
			return in
		}},
		"reason field value tampered": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			in.Body = []byte(`{"provider_reference":"` + ref + `","outcome":"approved","reason":"tampered"}`)
			return in
		}},
		"63 hex chars": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Header.Set(webhookauth.KYCSignatureHeader, sig[:len(sig)-1])
			return in
		}},
		"65 hex chars": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Header.Set(webhookauth.KYCSignatureHeader, sig+"0")
			return in
		}},
		"uppercase hex": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Header.Set(webhookauth.KYCSignatureHeader, strings.ToUpper(sig))
			return in
		}},
		"missing signature header": {wantReason: webhookauth.ReasonSignatureMissing, build: func() webhookauth.Inbound {
			in := base()
			in.Header.Del(webhookauth.KYCSignatureHeader)
			return in
		}},
		"missing key id header": {wantReason: webhookauth.ReasonSignatureMissing, build: func() webhookauth.Inbound {
			in := base()
			in.Header.Del(webhookauth.KYCKeyIDHeader)
			return in
		}},
		"unknown key id": {wantReason: webhookauth.ReasonCredentialUnavailable, build: func() webhookauth.Inbound {
			in := base()
			in.Header.Set(webhookauth.KYCKeyIDHeader, "mock-v2")
			return in
		}},
		"legacy signature field in body": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			// K1 fix: sign this legacy-shaped body with the REAL derived
			// key, so Scheme.Verify succeeds and the callback actually
			// reaches mock_provider.go's post-verification legacy-field
			// guard - without this, the guard is never exercised and
			// deleting it leaves this subtest green (M1).
			return signBody([]byte(`{"provider_reference":"` + ref + `","outcome":"approved","signature":"deadbeef"}`))
		}},
		"legacy trailing-newline signature format": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base()
			// Pre-Stage-10.2 wire shape (design §B1, since deleted:
			// sign/MockSignedCallbackBody/splitSignedPayload): the
			// signature trailed the RAW BODY itself after a newline,
			// rather than living exclusively in a header. The headers
			// here are still the CURRENT, validly-formatted ones - they
			// simply no longer match this longer, legacy-shaped body.
			sig := in.Header.Get(webhookauth.KYCSignatureHeader)
			in.Body = append(append([]byte{}, in.Body...), []byte("\n"+strings.TrimPrefix(sig, "v1="))...)
			return in
		}},
		"cross-verification provider_reference substitution, same tenant (J13)": {wantReason: webhookauth.ReasonSignatureInvalid, build: func() webhookauth.Inbound {
			in := base() // genuinely signed for verificationID's own reference
			in.Body = []byte(`{"provider_reference":"` + ref2 + `","outcome":"approved","reason":"x"}`)
			return in
		}},
	}
	before := noeffect.Capture(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{
		{TenantID: f.tenantID, ID: verificationID}, {TenantID: f.tenantID, ID: verificationID2},
	})
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := tc.build()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
				return err
			})
			var authErr *CallbackAuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("%s: expected a CallbackAuthError, got %v", name, err)
			}
			if authErr.Reason != tc.wantReason {
				t.Fatalf("%s: expected Reason %q, got %q", name, tc.wantReason, authErr.Reason)
			}
		})
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusPending {
		t.Fatalf("expected the verification untouched by every tampered attempt, got %s", got)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID2); got != StatusPending {
		t.Fatalf("expected the SECOND verification (substitution target) untouched too, got %s", got)
	}
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{
		{TenantID: f.tenantID, ID: verificationID}, {TenantID: f.tenantID, ID: verificationID2},
	}, before)
}

// K5 "provider id" tamper case: a genuinely-signed callback for one
// REGISTERED provider is replayed against a DIFFERENT registered provider
// (same tenant). Both providers are registered and both resolve a
// credential (so this is not merely ReasonProviderUnregistered/
// ReasonCredentialUnavailable) - the substitution is only caught because
// provider_id is bound into the signing input itself (webhookauth.Scheme.
// SigningInput), exactly like tenant_id is for K3/K4.
func TestKYCWebhook_ProviderIDSubstitution_Rejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	providerA := NewMockKYCProvider() // registered as "mock"
	providerB := NewMockKYCProvider() // registered as "mock2" - a DIFFERENT adapter instance/master, standing in for a second real vendor sharing this tenant.
	orch := NewOrchestrator(
		map[string]KYCProvider{"mock": providerA, "mock2": providerB},
		webhookauth.MultiResolver{
			"mock": NewMockWebhookCredentials(providerA),
			// NewMockWebhookCredentials(providerB) would resolve under
			// providerB.ID() (always "mock" - MockKYCProvider.ID() is not
			// configurable), which would make "mock2" itself fail closed
			// with credential_unavailable rather than exercising the
			// substitution this test targets. Build the "mock2" credential
			// directly, bound to its OWN provider id, from providerB's own
			// master - exactly what a second, distinctly-configured real
			// adapter's resolver entry would look like.
			"mock2": webhookauth.MockResolver{Master: providerB.master, Label: webhookauth.KYCMockKeyLabel, ProviderID: "mock2"},
		},
	)

	verificationID := seedVerification(t, pool, f)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	providerA.created[ref] = true

	// Genuinely signed for tenant f, provider "mock".
	in := providerA.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")

	before := noeffect.Capture(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}})

	// Delivered to provider "mock2" under the SAME tenant: registered,
	// resolvable, but signed for the WRONG provider id.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock2", in)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("expected a signature_invalid auth error for a provider-id-substituted callback, got %v", err)
	}
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}}, before)
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
	// Flip the last hex digit only (the header is ^v1=[0-9a-f]{64}$): the old
	// "first 0, else first 1" rewrite hit the "v1=" prefix when the hex had no
	// '0' (~1.6% of runs), testing a malformed header instead (TEST-T11A-FLIP-1).
	flipped := sig[:len(sig)-1] + "0"
	if sig[len(sig)-1] == '0' {
		flipped = sig[:len(sig)-1] + "1"
	}
	in.Header.Set(webhookauth.KYCSignatureHeader, flipped)

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, _, err := orch.receiveCallbackInTx(ctx, captured, f.tenantID, "mock", in)
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
			v, _, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
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
			v, _, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
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
				_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
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

// K9/ADR 0095 §15.2/IC-Q1: verified caller, unknown reference ->
// ErrVerificationReferenceUnknown (a retryable 5xx at the HTTP layer, never
// ErrNotFound's 404 and never a 200 - see provider.go's own comment on
// ReceiveVerifiedCallback step (d) for why KYC's lack of a receipt table
// makes this indistinguishable from a callback racing CreateVerification's
// own phase C); bad outcome/non-JSON -> ErrCallbackMalformedBody, no audit
// row; outcome "error" -> no state change, one failure audit row.
func TestKYCWebhook_VerifiedUnknownReference_NotFound(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, _ := newWebhookFixture(t, pool)
	in := provider.CallbackPayload(f.tenantID, "no-such-reference", ProviderApproved, "x")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if !errors.Is(err, ErrVerificationReferenceUnknown) {
		t.Fatalf("expected ErrVerificationReferenceUnknown, got %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("ErrVerificationReferenceUnknown must NOT also satisfy errors.Is(err, ErrNotFound) - the HTTP layer must route these to different response classes (5xx vs 404)")
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

	before := noeffect.Capture(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}})
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if !errors.Is(err, ErrCallbackMalformedBody) {
		t.Fatalf("expected ErrCallbackMalformedBody, got %v", err)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 0 {
		t.Fatalf("expected no audit row for a malformed (but verified) callback, got %d", n)
	}
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}}, before)
}

// K8: anything arriving after a STAFF decision (ReviewVerification, not a
// provider callback) is also a no-op - J11's "never resurrects a terminal
// verification" applies identically whether the prior terminal transition
// came from a callback (already covered by TestKYCWebhook_Replay_NoOp) or
// from a human reviewer.
func TestKYCWebhook_AfterStaffDecision_NoOp(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	staffID := seedComplianceStaff(t, pool, f)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: "staff_decision",
		})
		return err
	})
	if err != nil {
		t.Fatalf("staff review: %v", err)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusRejected {
		t.Fatalf("expected staff decision to apply, got %s", got)
	}

	before := noeffect.Capture(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}})

	// A later, genuinely-signed provider callback (even "approved", a
	// HIGHER rank than the staff's "rejected") must never overturn the
	// staff's terminal decision.
	in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")
	v, err := func() (Verification, error) {
		var v Verification
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			v, _, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
			return err
		})
		return v, err
	}()
	if err != nil {
		t.Fatalf("unexpected error applying a post-staff-decision callback: %v", err)
	}
	if v.Status != StatusRejected {
		t.Fatalf("expected the staff's rejected decision to survive a later provider callback, got %s", v.Status)
	}
	// No new audit row: the callback is a no-op (rank(approved)==rank
	// (rejected)==terminal==3, so it never re-enters the compare-and-set).
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}}, before)
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
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
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

// K5 (Stage 10.2 final review, L2/F-5): a NON-terminal verification still
// writes one failure audit row PER DELIVERY of outcome=error - an exact
// replay (identical body/headers, redelivered) is not deduplicated the way
// every other outcome's replay is, because it never advances rank and so
// never reaches the compare-and-set that would otherwise make it
// idempotent. This is the disclosed, narrower exception the terminal case
// below closes off.
func TestKYCWebhook_OutcomeErrorNonTerminal_RedeliveredWritesOneAuditRowPerDelivery(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	in := provider.CallbackPayload(f.tenantID, ref, ProviderError, "vendor_outage")
	for i := 0; i < 2; i++ {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
			return err
		})
		if err != nil {
			t.Fatalf("delivery %d: unexpected error: %v", i, err)
		}
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusPending {
		t.Fatalf("expected outcome=error to never change status, got %s", got)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 2 {
		t.Fatalf("expected exactly 2 failure audit rows for 2 redelivered outcome=error callbacks against a non-terminal verification, got %d", n)
	}
}

// K5 (Stage 10.2 final review, L2/F-5): an outcome=error callback against
// an ALREADY-TERMINAL verification writes NO failure audit row - closing
// the one gap where "a replay writes no audit row" (§E) didn't otherwise
// hold: without this, a captured error callback replayed after the
// verification reached a terminal status could grow audit_log without
// bound.
func TestKYCWebhook_OutcomeErrorAgainstTerminal_NoAuditRow(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	// Reach a terminal status first (a normal approval).
	approve := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", approve)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error approving: %v", err)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 1 {
		t.Fatalf("expected exactly 1 audit row after the approval, got %d", n)
	}

	before := noeffect.Capture(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}})

	// A LATER outcome=error callback against the now-terminal verification
	// (e.g. a delayed/duplicate vendor delivery) must write NO additional
	// audit row, and must never change the (already terminal) status.
	in := provider.CallbackPayload(f.tenantID, ref, ProviderError, "vendor_outage")
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusApproved {
		t.Fatalf("expected the terminal approved status to survive a later outcome=error callback, got %s", got)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != 1 {
		t.Fatalf("expected STILL exactly 1 audit row (no new one for outcome=error against a terminal verification), got %d", n)
	}
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}}, before)
}

// TestKYCWebhook_OversizedControlCharacterReason_NormalizedInDBAndAudit is
// KYC-REASON-BOUND-1's binding integration case (QA W1d plan): a verified
// callback carrying an oversized, control/bidi-character-laden reason (the
// same shape as the E6 pre-fix evidence) is normalized BEFORE it reaches
// the DB row and the audit trail - the STORED value is asserted, not just
// the in-memory ProviderResult.
func TestKYCWebhook_OversizedControlCharacterReason_NormalizedInDBAndAudit(t *testing.T) {
	pool := testPool(t)
	f, provider, orch, verificationID := newWebhookFixture(t, pool)
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})

	controlLaden := "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text"
	hugeReason := controlLaden + strings.Repeat("A", 4096) + controlLaden

	in := provider.CallbackPayload(f.tenantID, ref, ProviderRejected, hugeReason)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock", in)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var storedReason string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&storedReason)
	})
	if err != nil {
		t.Fatalf("read stored reason: %v", err)
	}
	if storedReason == hugeReason {
		t.Fatal("expected the stored reason to be normalized (bounded/cleaned), got the raw value verbatim")
	}
	if len(storedReason) > MaxReasonBytes {
		t.Fatalf("expected the stored reason to be at most %d bytes, got %d", MaxReasonBytes, len(storedReason))
	}
	for _, bad := range []string{"\r", "\x1b", "\u202E"} {
		if strings.Contains(storedReason, bad) {
			t.Fatalf("expected control/bidi character %q stripped from the stored reason, got %q", bad, storedReason)
		}
	}

	var auditReason string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'reason' FROM audit_log WHERE action = 'kyc.provider_callback' AND target_id = $1 ORDER BY created_at DESC LIMIT 1`,
			verificationID.String()).Scan(&auditReason)
	})
	if err != nil {
		t.Fatalf("read audit metadata reason: %v", err)
	}
	if auditReason != storedReason {
		t.Fatalf("expected audit_log.metadata to carry the SAME normalized reason as the DB row, got %q vs %q", auditReason, storedReason)
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
