//go:build integration

// N3/N-2 (RV-PRH-I2 KYC code re-review / security re-verification), rewritten
// for PRH-2 E1 (ADR 0106): no HTTP path calls a KYC vendor any more, so the
// create handler returns 201 without touching the adapter; the vendor call and
// its failure happen in the OUTBOX WORKER, whose log lines carry only ids and
// closed classes. This test proves end to end that raw adapter error text
// (here a credential-bearing URL) never reaches a log line, an audit row or
// the outbox, and that the handler itself makes no vendor call.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

const createVerificationLogRedactionSentinel = "VENDOR-TRANSPORT-SECRET-xyz789"

// failingCreateVerificationAdapter wraps the MOCK but always fails
// CreateVerification with an error carrying a sentinel string - standing
// in for a real adapter's own transport error, which can embed a vendor
// response body, header, or a credential-bearing URL. calls counts the
// vendor calls actually made.
type failingCreateVerificationAdapter struct {
	*kyc.MockKYCProvider
	calls *atomic.Int32
}

func (a failingCreateVerificationAdapter) CreateVerification(ctx context.Context, in kyc.CreateVerificationInput) (kyc.ProviderResult, error) {
	a.calls.Add(1)
	return kyc.ProviderResult{}, errors.New("POST https://vendor.example/verify?token=" + createVerificationLogRedactionSentinel)
}

func newCreateVerificationLogRedactionServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, logger *slog.Logger) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	adapter := failingCreateVerificationAdapter{MockKYCProvider: kyc.NewMockKYCProvider(), calls: new(atomic.Int32)}
	purgeKYCOutbox(t)
	t.Cleanup(func() { purgeKYCOutbox(t) })
	orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": adapter}, kyc.NewMockWebhookCredentials(adapter.MockKYCProvider))
	w := registerKYCOutboxWorker(t, pool, orch, kyc.NewMockOutboundResolver())
	w.Logger = logger
	srv := httptest.NewServer(New(Deps{
		Logger:                 logger,
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        orch,
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv, adapter.calls
}

// TestKYC_CreateVerificationProviderFailureLog_NeverLeaksRawErrorText: the
// create handler returns 201 with NO vendor call; the worker's vendor call then
// fails with raw adapter error text, and that text reaches no log line, no audit
// row and no outbox column.
func TestKYC_CreateVerificationProviderFailureLog_NeverLeaksRawErrorText(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	srv, calls := newCreateVerificationLogRedactionServer(t, pool, issuer, logger)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 (asynchronous vendor submission), got %d", resp.StatusCode)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("the HTTP create path must make NO vendor call, got %d", got)
	}

	drainKYCOutbox(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("the worker must make exactly one vendor call, got %d", got)
	}

	var lines []capturedLogLine
	for _, l := range captured() {
		if l.msg == "kyc_outbox_provider_call_failed" {
			lines = append(lines, l)
		}
		for k, v := range l.attrs {
			if s, ok := v.(string); ok && strings.Contains(s, createVerificationLogRedactionSentinel) {
				t.Fatalf("log line %q field %q leaked raw provider error text: %q", l.msg, k, s)
			}
		}
		if l.msg == "create_verification_provider_unavailable" {
			t.Fatal("the handler must not log a vendor failure: it makes no vendor call")
		}
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 kyc_outbox_provider_call_failed line, got %d: %+v", len(lines), lines)
	}
	if class, _ := lines[0].attrs["class"].(string); class != string(kyc.ClassAmbiguous) {
		t.Fatalf(`expected class="ambiguous", got %q`, class)
	}

	var leaked int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND metadata::text LIKE '%' || $2 || '%'`,
			tenant.ID, createVerificationLogRedactionSentinel).Scan(&leaked)
	}); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("%d audit row(s) leaked the raw provider error text", leaked)
	}
}
