package webhookauth_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// TestLogVerifiedKey_KeyImplicitOnlyAndAllowListedFields pins the shared
// W2A-SEC-2 line: a KeyImplicit scheme logs exactly request_id,
// tenant_id, provider_id and key_id; a KeyFromHeader scheme (whose key id
// VerifyInbound already pinned to the active key) and a nil scheme log
// nothing.
func TestLogVerifiedKey_KeyImplicitOnlyAndAllowListedFields(t *testing.T) {
	tenant := uuid.New()
	f := webhookauthtest.NewTwoKeyImplicit(tenant, "vendor-implicit")
	in := webhookauth.Inbound{TenantID: tenant, ProviderID: "vendor-implicit"}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	webhookauth.LogVerifiedKey(context.Background(), logger, "req-1", f.Scheme, in, f.Previous)
	webhookauthtest.AssertVerifiedKeyLog(t, buf.Bytes(), "req-1", tenant, "vendor-implicit", "k-previous", f.Active, f.Previous)

	buf.Reset()
	webhookauth.LogVerifiedKey(context.Background(), logger, "req-2", webhookauth.PaymentsScheme().VerificationScheme(), in, f.Active)
	webhookauth.LogVerifiedKey(context.Background(), logger, "req-3", nil, in, f.Active)
	if buf.Len() != 0 {
		t.Fatalf("a KeyFromHeader or nil scheme must not log: %s", buf.Bytes())
	}
}
