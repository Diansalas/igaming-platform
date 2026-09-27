package httpserver

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAdmission_OverflowLogged is security review's Low finding: "emit
// the overflow error log" - admission.GCRALimiter.OverflowOccurred() was
// computed but never checked/logged anywhere before this fix. This drives
// the A3 pre-auth unknown-bucket limiter (maxKeys effectively 1, since the
// unknown bucket is a single shared key per domain) past overflow via a
// tiny VerifiedMaxKeys/DirectoryCap-independent path: three distinct
// UNREGISTERED provider ids collapse to the SAME "_unknown" providerKey
// anyway (ADR 0097 §4.2), so this test instead drives B1's verified
// limiter (keyed per (tenant_id, provider_id), genuinely high-cardinality)
// past its tiny VerifiedMaxKeys cap directly.
func TestAdmission_OverflowLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	rb := func(rate float64, burst int) WebhookRateBurst { return WebhookRateBurst{Rate: rate, Burst: burst} }
	settings := WebhookAdmissionSettings{
		Enabled:            true,
		PreAuthRate:        map[string]WebhookRateBurst{"payments": rb(1000, 1000), "casino": rb(1000, 1000), "kyc": rb(1000, 1000)},
		PreAuthUnknownRate: map[string]WebhookRateBurst{"payments": rb(1000, 1000), "casino": rb(1000, 1000), "kyc": rb(1000, 1000)},
		VerifiedRate:       map[string]WebhookRateBurst{"payments": rb(1000, 1000), "casino": rb(1000, 1000), "kyc": rb(1000, 1000)},
		InFlightGlobal:     1000, InFlightPerKey: 1000, InFlightUnknown: 1000,
		DBGateGlobal: 1000, DBGatePerKey: 1000, DBGateUnknown: 1000, DBGateWait: time.Second,
		DomainTxPerTenant: 1000, DomainWait: time.Second,
		DirectoryRefresh: time.Hour, DirectoryCap: 100,
		IdleEvict: time.Hour,
		// The overflow trigger: only 2 verified keys tracked before folding.
		VerifiedMaxKeys: 2, PerIPMaxKeys: 100,
	}
	rt := newWebhookAdmission(settings, nil, logger)
	if rt == nil {
		t.Fatal("expected a non-nil runtime")
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/payments/t/mock", nil)
	for i := 0; i < 5; i++ {
		_ = i
		release, ok := rt.admitVerified(w, r, domainPayments, uuid.New(), "provider", nil)
		if ok && release != nil {
			release()
		}
	}

	if !strings.Contains(buf.String(), "webhook_admission_limiter_overflow") {
		t.Fatalf("expected an overflow log line, got: %s", buf.String())
	}
}
