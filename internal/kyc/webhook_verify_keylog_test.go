package kyc

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// TestResolveAndVerify_LogsMatchedKeyIDForKeyImplicit (security gate W2
// condition W2A-SEC-2; C4; ADR 0022 §3 point 2 "The log records which
// key_id verified"): with a KeyImplicit scheme and two live keys (active
// plus an in-window verify_only predecessor), a successful verification
// logs exactly one Info line naming the key that MATCHED - the active key
// when the active key signed, the predecessor when the predecessor signed -
// with only request_id, tenant_id, provider_id and key_id, and never either
// secret or fingerprint. A failed verification logs nothing.
func TestResolveAndVerify_LogsMatchedKeyIDForKeyImplicit(t *testing.T) {
	const providerID = "vendor-implicit"
	tenant := uuid.New()
	f := webhookauthtest.NewTwoKeyImplicit(tenant, providerID)
	body := []byte(`{"event":"w2a-sec-2"}`)

	run := func(t *testing.T, in webhookauth.Inbound) (webhookauth.Credential, *webhookauth.AuthError, []byte) {
		t.Helper()
		var buf bytes.Buffer
		o := &Orchestrator{webhookCredentialResolver: f.Resolver}
		o.SetWebhookLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
		ctx := observability.WithRequestState(context.Background(),
			&observability.RequestState{RequestID: "req-w2a-sec-2", TenantID: tenant.String()})
		m, reason, ok := f.Scheme.Extract(in)
		if !ok {
			t.Fatalf("extract: %s", reason)
		}
		cred, authErr := o.resolveAndVerify(ctx, nil, f.Scheme, in, m)
		return cred, authErr, buf.Bytes()
	}

	for _, tc := range []struct {
		name   string
		signer webhookauth.Credential
	}{
		{"active key signed", f.Active},
		{"predecessor key signed", f.Previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cred, authErr, out := run(t, f.Inbound(tc.signer, body))
			if authErr != nil || cred.KeyID != tc.signer.KeyID {
				t.Fatalf("verify: cred=%v err=%v, want key %q", cred, authErr, tc.signer.KeyID)
			}
			webhookauthtest.AssertVerifiedKeyLog(t, out, "req-w2a-sec-2", tenant, providerID, tc.signer.KeyID, f.Active, f.Previous)
		})
	}

	t.Run("failed verification logs nothing", func(t *testing.T) {
		stranger := f.Active
		stranger.Secret = bytes.Repeat([]byte{0x5a}, 32)
		_, authErr, out := run(t, f.Inbound(stranger, body))
		if authErr == nil || authErr.Reason != webhookauth.ReasonSignatureInvalid {
			t.Fatalf("want signature_invalid, got %v", authErr)
		}
		if len(out) != 0 {
			t.Fatalf("a failed verification must not log a verified key: %s", out)
		}
	})
}
