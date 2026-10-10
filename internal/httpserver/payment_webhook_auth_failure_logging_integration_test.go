//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// QA binding test plan (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md) T12: for each CallbackAuthReason, the
// single captured "payment_webhook_auth_failed" log line must carry ONLY
// allow-listed keys (request_id, reason, tenant_id, provider_id, key_id,
// credential_fingerprint, client_ip, body_len) - never the body, header
// values, signature, raw slug, or an `err` field - and the pre-verification
// rejection must write ZERO new audit_log rows.
//
// Reuses capturingHandler/newCapturingLogger (sportsbook_settlement_alert_
// audit_test.go) - same package, same technique, so this test cannot be
// fooled by string-formatting a real slog.Handler would apply.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// paymentWebhookAllowedLogKeys is PAY-WH-TENANT-1 design §3.3's exact
// allow-list, restated here (not imported from production code) so this
// test fails if a future change to logCallbackAuthFailure/
// callbackAuthFailureAllowlistFields silently adds a new field.
var paymentWebhookAllowedLogKeys = map[string]bool{
	"request_id":             true,
	"reason":                 true,
	"tenant_id":              true,
	"provider_id":            true,
	"key_id":                 true,
	"credential_fingerprint": true,
	"client_ip":              true,
	"body_len":               true,
}

func newFinancialTestServerWithLogger(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *payments.Orchestrator, logger *slog.Logger) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:              logger,
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         "platform-api-test",
		AccessTokenTTL:      5 * time.Minute,
		RefreshTokenTTL:     time.Hour,
		PaymentOrchestrator: orchestrator, PaymentsOutboundCredentials: payments.MockCredentialResolver{},
		PersonResolver: identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

// findAuthFailureLine returns the (exactly one expected) captured
// "payment_webhook_auth_failed" line, failing the test if there is not
// exactly one.
func findAuthFailureLine(t *testing.T, lines []capturedLogLine) capturedLogLine {
	t.Helper()
	var matches []capturedLogLine
	for _, l := range lines {
		if l.msg == "payment_webhook_auth_failed" {
			matches = append(matches, l)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 payment_webhook_auth_failed log line, got %d (%+v)", len(matches), matches)
	}
	return matches[0]
}

func assertOnlyAllowedKeys(t *testing.T, line capturedLogLine) {
	t.Helper()
	for k := range line.attrs {
		if !paymentWebhookAllowedLogKeys[k] {
			t.Errorf("payment_webhook_auth_failed carries a NON-allow-listed field %q (value %v)", k, line.attrs[k])
		}
	}
	for _, forbiddenKey := range []string{"body", "header", "signature", "slug", "err", "error", "raw"} {
		if _, present := line.attrs[forbiddenKey]; present {
			t.Errorf("payment_webhook_auth_failed must never carry a %q field", forbiddenKey)
		}
	}
}

func assertNoForbiddenLogValue(t *testing.T, line capturedLogLine, forbidden ...string) {
	t.Helper()
	for k, v := range line.attrs {
		s, ok := v.(string)
		if !ok {
			continue
		}
		for _, f := range forbidden {
			if f != "" && strings.Contains(s, f) {
				t.Errorf("log field %q=%q contains a forbidden substring %q", k, s, f)
			}
		}
	}
}

func auditCountForTenant(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count audit_log for tenant %s: %v", tenantID, err)
	}
	return count
}

// runAuthFailureLogCase posts one webhook request expected to fail
// authentication, and asserts the resulting log line and audit-log delta
// together - the QA plan's own pairing ("no audit_log row for any 401
// case" alongside "only allow-listed keys").
func runAuthFailureLogCase(t *testing.T, pool *db.Pool, issuer *auth.Issuer, tenantID uuid.UUID, orchestrator *payments.Orchestrator,
	path string, header http.Header, body []byte, reason payments.CallbackAuthReason,
	wantTenantID, wantProviderID, wantKeyID, wantFingerprint bool) {
	t.Helper()
	logger, captured := newCapturingLogger()
	srv := newFinancialTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	auditBefore := auditCountForTenant(t, pool, tenantID)

	resp := rawPostCallback(t, srv, path, payments.InboundCallback{Header: header, Body: body})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	line := findAuthFailureLine(t, captured())
	assertOnlyAllowedKeys(t, line)
	assertNoForbiddenLogValue(t, line, "private_key", "deadbeef", string(body))

	if got, _ := line.attrs["reason"].(string); got != string(reason) {
		t.Errorf("expected reason=%q, got %q", reason, got)
	}
	if _, present := line.attrs["tenant_id"]; present != wantTenantID {
		t.Errorf("tenant_id presence = %v, want %v", present, wantTenantID)
	}
	if _, present := line.attrs["provider_id"]; present != wantProviderID {
		t.Errorf("provider_id presence = %v, want %v", present, wantProviderID)
	}
	if _, present := line.attrs["key_id"]; present != wantKeyID {
		t.Errorf("key_id presence = %v, want %v", present, wantKeyID)
	}
	if _, present := line.attrs["credential_fingerprint"]; present != wantFingerprint {
		t.Errorf("credential_fingerprint presence = %v, want %v", present, wantFingerprint)
	}
	if _, present := line.attrs["request_id"]; !present {
		t.Errorf("expected request_id to always be present")
	}
	if _, present := line.attrs["client_ip"]; !present {
		t.Errorf("expected client_ip to always be present")
	}
	if _, present := line.attrs["body_len"]; !present {
		t.Errorf("expected body_len to always be present")
	}

	auditAfter := auditCountForTenant(t, pool, tenantID)
	if auditAfter != auditBefore {
		t.Errorf("expected ZERO new audit_log rows for a 401, had %d before and %d after", auditBefore, auditAfter)
	}
}

// TestWebhook_AuthFailureLogging_AllowListOnly is QA plan T12, covering
// every CallbackAuthReason the public webhook route can produce.
func TestWebhook_AuthFailureLogging_AllowListOnly(t *testing.T) {
	pool, issuer := testEnv(t)

	activeTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, activeTenant)

	suspendedTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, suspendedTenant)
	if err := launchfix.TrySetTenantStatus(context.Background(), suspendedTenant.ID, "suspended"); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	unconfiguredTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, unconfiguredTenant)

	genuineBody := []byte(`{"event_type":"deposit","provider_reference":"t12-ref","outcome":"succeeded","amount":1000,"asset_code":"EUR"}`)
	keyMaterialBody := []byte(`{"event_type":"deposit","provider_reference":"t12-ref","outcome":"succeeded","amount":1000,"asset_code":"EUR","private_key":"deadbeef-should-never-be-logged"}`)

	t.Run("tenant_unknown", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		genuine := mock.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/does-not-exist-slug/mock", genuine.Header, genuineBody,
			payments.ReasonTenantUnknown, false, true, false, false)
	})

	t.Run("tenant_inactive", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, suspendedTenant.ID, mock)
		genuine := mock.CallbackPayload(suspendedTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		runAuthFailureLogCase(t, pool, issuer, suspendedTenant.ID, orch,
			"/v1/webhooks/payments/"+suspendedTenant.Slug+"/mock", genuine.Header, genuineBody,
			payments.ReasonTenantInactive, true, true, false, false)
	})

	t.Run("provider_invalid", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		genuine := mock.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/BAD_ID!", genuine.Header, genuineBody,
			payments.ReasonProviderInvalid, false, false, false, false)
	})

	t.Run("provider_unregistered", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		genuine := mock.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		// Stage 10.3 W1a (WH-VENDOR-SCHEME-1, 01-provider-trust-analysis.md
		// §1.1): the shared preamble selects the scheme by provider id, so
		// an unregistered provider is rejected BEFORE the tenant lookup and
		// before any header is parsed - tenant_id and key_id are therefore
		// now correctly ABSENT (previously present, since the lookup ran
		// after the tenant lookup inside ReceiveCallback).
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/not-a-real-provider", genuine.Header, genuineBody,
			payments.ReasonProviderUnregistered, false, true, false, false)
	})

	t.Run("provider_not_configured", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		// unconfiguredTenant deliberately has NO capability row for "mock".
		genuine := mock.CallbackPayload(unconfiguredTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		runAuthFailureLogCase(t, pool, issuer, unconfiguredTenant.ID, orch,
			"/v1/webhooks/payments/"+unconfiguredTenant.Slug+"/mock", genuine.Header, genuineBody,
			payments.ReasonProviderNotConfigured, true, true, true, false)
	})

	t.Run("no_resolver", func(t *testing.T) {
		mock := payments.NewMockProvider("mock", "EUR", "USD")
		orch := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock}, nil)
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		genuine := mock.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/mock", genuine.Header, genuineBody,
			payments.ReasonNoResolver, true, true, true, false)
	})

	t.Run("credential_unavailable", func(t *testing.T) {
		mock := payments.NewMockProvider("mock", "EUR", "USD")
		orch := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock}, payments.MultiWebhookCredentialResolver{})
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		genuine := mock.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/mock", genuine.Header, genuineBody,
			payments.ReasonCredentialUnavailable, true, true, true, false)
	})

	t.Run("signature_missing", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		// Stage 10.1 security review P2-1/code review F1/architect PW-1
		// (ruling 5): header format validation now runs BEFORE the tenant
		// lookup in deposit_handlers.go, so a missing signature header is
		// rejected with NO tenant yet resolved - tenant_id is correctly
		// absent here, unlike every reason below that is only reachable
		// after the tenant lookup succeeds.
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/mock", http.Header{}, genuineBody,
			payments.ReasonSignatureMissing, false, true, false, false)
	})

	t.Run("signature_invalid", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		genuine := mock.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "t12-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
		badHeader := genuine.Header.Clone()
		sig := badHeader.Get(payments.HeaderSignature)
		// Flip the last hex digit only (the header is ^v1=[0-9a-f]{64}$): the old
		// "first 0, else first 1" rewrite hit the "v1=" prefix when the hex had no
		// '0' (~1.6% of runs), testing a malformed header instead (TEST-T11A-FLIP-1).
		flipped := sig[:len(sig)-1] + "0"
		if sig[len(sig)-1] == '0' {
			flipped = sig[:len(sig)-1] + "1"
		}
		badHeader.Set(payments.HeaderSignature, flipped)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/mock", badHeader, genuineBody,
			payments.ReasonSignatureInvalid, true, true, true, true)
	})

	t.Run("body_too_large", func(t *testing.T) {
		// N5 (Stage 10.1 code-review re-verification, 2026-09-26): the
		// only reason previously untested here. deposit_handlers.go's
		// step 2 (body size limit) runs BEFORE any tenant lookup (ruling
		// 5), so tenant_id must be ABSENT here exactly like
		// signature_missing above, even against activeTenant's own valid
		// slug - unlike every other subtest in this table, which all
		// require the tenant lookup to have already succeeded.
		// oversizedBody's SIGNATURE never needs to verify (or even be
		// present) - size alone triggers the rejection, before headers are
		// even parsed - so an entirely unsigned, headerless request is
		// enough, mirroring TestWebhook_EnumerationOracle_
		// IndistinguishableResponses's own oversized-body probe
		// (payment_webhook_tenant_binding_test.go).
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		oversizedBody := make([]byte, maxWebhookBodyBytes+1)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/mock", http.Header{}, oversizedBody,
			payments.ReasonBodyTooLarge, false, true, false, false)
	})

	t.Run("key_material", func(t *testing.T) {
		orch, mock := newMockOrchestrator()
		mustRegisterCapability(t, pool, activeTenant.ID, mock)
		// Stage 10.1 security review P2-1/code review F1/architect PW-1:
		// HandleCallback now verifies the signature BEFORE parsing the body
		// at all, so the key-material scan is only ever reached for a body
		// that genuinely matches its own signature - SignRawBody (not
		// CallbackPayload, whose fixed mockCallbackBody shape has no room
		// for a bogus "private_key" field) signs keyMaterialBody's own
		// exact bytes.
		genuine := mock.SignRawBody(activeTenant.ID, keyMaterialBody)
		runAuthFailureLogCase(t, pool, issuer, activeTenant.ID, orch,
			"/v1/webhooks/payments/"+activeTenant.Slug+"/mock", genuine.Header, keyMaterialBody,
			payments.ReasonKeyMaterial, true, true, true, false)
	})
}
