package providercred

// Security review C1 of PRH-I4 (HIGH). This is the surgical, deterministic
// unit-level pin for the exact defect security found: Resolver.Resolve's
// single handle-read call is gated by ADR 0097's admission layer (via a
// TenantReader implementation, e.g. internal/httpserver's gatedReader) -
// a capacity rejection from that gate must propagate as
// webhookauth.ErrTenantReaderUnavailable, NEVER folded into
// webhookauth.ErrCredentialUnavailable (which every uniform-401 caller
// treats identically to "no credential exists"). This is payments' SECOND
// read (credential resolution, called from resolveAndVerify after the
// first ProviderAcceptsWebhook read already succeeds) and casino/KYC's
// ONLY read - the same Resolver.Resolve code path serves all three
// domains, so one test here pins the fix for all of them; the HTTP-level
// regression tests (internal/httpserver's TestAdmission_C1_*) additionally
// prove the full response-mapping end to end per domain.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// gateUnavailableReader is a fake webhookauth.TenantReader that always
// refuses, exactly mirroring what a saturated ADR 0097 A4b gate does -
// this deliberately does NOT touch a real database, so this test is
// unit-speed and needs no DB fixture.
type gateUnavailableReader struct{}

func (gateUnavailableReader) WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error {
	return webhookauth.ErrTenantReaderUnavailable
}

func newTestResolver(t *testing.T, domain string) *Resolver {
	t.Helper()
	router, err := memstore.NewRouter(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sub, err := New(config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(key))}, router)
	if err != nil || sub == nil {
		t.Fatalf("New: %v (nil=%v)", err, sub == nil)
	}
	r, ok := sub.Resolver(domain).(*Resolver)
	if !ok {
		t.Fatalf("Resolver(%q) did not return *Resolver", domain)
	}
	return r
}

// TestResolve_GateUnavailable_PropagatesDistinctSentinel is the C1 pin:
// a gate-unavailable TenantReader must make Resolve return
// webhookauth.ErrTenantReaderUnavailable, and specifically must NOT also
// satisfy errors.Is against webhookauth.ErrCredentialUnavailable (the
// "folded into a real auth failure" defect security found).
func TestResolve_GateUnavailable_PropagatesDistinctSentinel(t *testing.T) {
	for _, sel := range []webhookauth.KeySelection{webhookauth.KeyFromHeader, webhookauth.KeyImplicit} {
		keyID := "k1"
		if sel == webhookauth.KeyImplicit {
			keyID = ""
		}
		r := newTestResolver(t, "payments")
		_, err := r.Resolve(context.Background(), gateUnavailableReader{}, uuid.New(), "some-provider", keyID, sel)
		if !errors.Is(err, webhookauth.ErrTenantReaderUnavailable) {
			t.Fatalf("sel=%v: want errors.Is(err, ErrTenantReaderUnavailable), got %v", sel, err)
		}
		if errors.Is(err, webhookauth.ErrCredentialUnavailable) {
			t.Fatalf("sel=%v: a gate rejection must NEVER also satisfy errors.Is(err, ErrCredentialUnavailable) - that is exactly the security review C1 defect (folded into the uniform 401)", sel)
		}
	}
}

// TestResolve_RealDBError_StillFoldsIntoCredentialUnavailable is the
// negative control: a GENUINE database error (not a capacity rejection)
// must still fold into ErrCredentialUnavailable exactly as before this
// fix (security C5's "any other DB error fails closed as
// credential_unavailable" rule is unchanged for real errors) - the C1 fix
// must not accidentally widen what counts as "admission unavailable".
type genuineDBErrorReader struct{}

func (genuineDBErrorReader) WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error {
	return pgx.ErrTxClosed
}

func TestResolve_RealDBError_StillFoldsIntoCredentialUnavailable(t *testing.T) {
	r := newTestResolver(t, "payments")
	_, err := r.Resolve(context.Background(), genuineDBErrorReader{}, uuid.New(), "some-provider", "k1", webhookauth.KeyFromHeader)
	if !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("a genuine DB error must still fold into ErrCredentialUnavailable, got %v", err)
	}
	if errors.Is(err, webhookauth.ErrTenantReaderUnavailable) {
		t.Fatal("a genuine DB error must never be mistaken for a capacity rejection")
	}
}
