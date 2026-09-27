// Security review RV-PRH-I2 C2: CallContext's redacting renderers
// (String/GoString/Format/LogValue/MarshalJSON) were untested - the
// reviewer's own spot-check mutation (appending the raw secret to
// callContextRedacted's output) survived the full test suite. These tests
// close that gap directly, with a known sentinel and a negative control,
// so a future regression (a new CallContext field, or an edit to
// callContextRedacted) is caught here rather than by CI staying green.
package casino

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// callContextRedactionSentinel is the exact secret NewMockOutboundCredential
// embeds - a known, greppable value this test can assert is never rendered
// by any of CallContext's or LaunchRequest's formatting/logging/marshaling
// paths.
const callContextRedactionSentinel = "mock-outbound-credential-not-a-real-secret"

// renderAllForms exercises every rendering path security review RV-PRH-I2
// C2 named: %v/%+v/%#v/%s/%q, slog's text AND JSON handlers, json.Marshal,
// and fmt.Errorf("%v", ...).
func renderAllForms(t *testing.T, v any) []string {
	t.Helper()
	outs := []string{
		fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v),
		fmt.Sprintf("%s", v), fmt.Sprintf("%q", v),
		fmt.Errorf("%v", v).Error(),
	}
	var textBuf, jsonLogBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("m", "v", v)
	slog.New(slog.NewJSONHandler(&jsonLogBuf, nil)).Info("m", "v", v)
	outs = append(outs, textBuf.String(), jsonLogBuf.String())
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	outs = append(outs, string(j))
	return outs
}

func assertNoSentinel(t *testing.T, outs []string, label string) {
	t.Helper()
	for _, out := range outs {
		if strings.Contains(out, callContextRedactionSentinel) {
			t.Fatalf("%s rendered the secret: %q", label, out)
		}
	}
}

// TestCallContext_NeverRendersSecret is security review RV-PRH-I2 C2's
// required test. It must kill the reviewer's own mutation A (appending
// `string(c.Credential.Secret())` to callContextRedacted's output).
func TestCallContext_NeverRendersSecret(t *testing.T) {
	cred := providercred.NewMockOutboundCredential(uuid.New(), "casino", "mock-casino")
	if !strings.Contains(string(cred.Secret()), callContextRedactionSentinel) {
		t.Fatalf("test setup: NewMockOutboundCredential's secret no longer contains the expected sentinel - update callContextRedactionSentinel")
	}

	call := CallContext{
		TenantID: uuid.New(), ProviderID: "mock-casino", Credential: cred,
		IdempotencyKey: "cas:test-session", Deadline: time.Now().Add(time.Minute),
	}
	req := LaunchRequest{
		ProviderGameID: "game-1", PlayerAccountID: uuid.New(), AssetCode: "EUR", Mode: ModeReal,
		LaunchToken: "tok", SessionID: uuid.New(), Call: call,
	}

	assertNoSentinel(t, renderAllForms(t, call), "CallContext")
	assertNoSentinel(t, renderAllForms(t, req), "LaunchRequest")
	assertNoSentinel(t, renderAllForms(t, &call), "*CallContext")
	assertNoSentinel(t, renderAllForms(t, &req), "*LaunchRequest")

	// Negative control (security review's explicit requirement): the
	// mechanism above must actually be capable of catching a leak, proven
	// against a plain type that does NOT redact (no embedded CallContext -
	// that would promote its redacting methods and defeat the control).
	type leaky struct{ Secret string }
	found := false
	for _, out := range renderAllForms(t, leaky{Secret: callContextRedactionSentinel}) {
		if strings.Contains(out, callContextRedactionSentinel) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("negative control failed: the sentinel must be detectable by this test's own rendering mechanism")
	}
}
