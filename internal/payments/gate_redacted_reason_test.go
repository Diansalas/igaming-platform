package payments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// REDACTEDREASON-PASSTHROUGH-1: redactedReason must never echo the text of
// an error this package did not itself construct. Only its own sentinels
// (fixed text), the gate's own panic error (type name only), transport and
// context classifications survive; everything else is a generic label.

const redactFakeSecret = "SENTINEL-sk_live_9f2cQ-player-jane.doe@example.invalid"

type vendorBodyError struct{ body string }

func (e vendorBodyError) Error() string { return "vendor responded: " + e.body }

func TestRedactedReason_NeverEchoesForeignErrorText(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"errors_new", errors.New("declined: api_key=" + redactFakeSecret)},
		{"fmt_wrapped", fmt.Errorf("sdk: %w", errors.New(redactFakeSecret))},
		{"custom_type", vendorBodyError{body: `{"card_holder":"` + redactFakeSecret + `"}`}},
		{"custom_type_ptr", &vendorBodyError{body: redactFakeSecret}},
		{"joined", errors.Join(errors.New("a"), errors.New(redactFakeSecret))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactedReason(tc.err)
			if got != redactedAdapterErrorLabel {
				t.Fatalf("redactedReason(%s) = %q, want the generic label %q", tc.name, got, redactedAdapterErrorLabel)
			}
			if strings.Contains(got, redactFakeSecret) || strings.Contains(got, "api_key") {
				t.Fatalf("foreign error text leaked: %q", got)
			}
		})
	}
}

func TestRedactedReason_OwnSentinelsYieldFixedTextOnly(t *testing.T) {
	for _, sentinel := range redactedReasonSentinels {
		t.Run(sentinel.Error(), func(t *testing.T) {
			if got := redactedReason(sentinel); got != sentinel.Error() {
				t.Fatalf("bare sentinel: got %q, want %q", got, sentinel.Error())
			}
			// Context wrapped AROUND an allow-listed sentinel is dropped.
			wrapped := fmt.Errorf("%w: upstream said token=%s", sentinel, redactFakeSecret)
			got := redactedReason(wrapped)
			if got != sentinel.Error() || strings.Contains(got, redactFakeSecret) {
				t.Fatalf("wrapped sentinel: got %q, want exactly %q", got, sentinel.Error())
			}
		})
	}
	// The allow-list is exact: pin its membership so a widening is a
	// deliberate, reviewed change.
	want := []error{ErrProviderCallRefused, ErrStatementFetchUnderTx, providercred.ErrOutboundCredentialUnavailable}
	if len(redactedReasonSentinels) != len(want) {
		t.Fatalf("redactedReasonSentinels has %d entries, want %d (review any change)", len(redactedReasonSentinels), len(want))
	}
	for i := range want {
		if redactedReasonSentinels[i] != want[i] {
			t.Fatalf("redactedReasonSentinels[%d] = %v, want %v", i, redactedReasonSentinels[i], want[i])
		}
	}
}

func TestRedactedReason_TransportContextAndPanicUnchanged(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&url.Error{Op: "Post", URL: "https://psp.invalid/c?api_key=" + redactFakeSecret, Err: errors.New(redactFakeSecret)}, "Post: transport error"},
		{&url.Error{Op: "Get", URL: "https://psp.invalid/", Err: context.Canceled}, "Get: canceled"},
		{fmt.Errorf("x %s: %w", redactFakeSecret, context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("x %s: %w", redactFakeSecret, context.Canceled), "canceled"},
		{&adapterPanicError{typ: "string"}, "payments: adapter panic recovered (string)"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := redactedReason(tc.err); got != tc.want {
			t.Fatalf("redactedReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// End to end through the gate: an adapter's own (non-transport) error
// text never reaches GateResult.Err, which feeds logs, audit and receipts.
func TestCallProvider_AdapterErrorTextNeverReachesErr(t *testing.T) {
	tok := uuid.New()
	in := callProviderInput{
		TenantID: uuid.New(), ProviderID: "mock-psp-err",
		AttemptState: AttemptSubmitting, ClaimToken: tok, ExpectedClaim: tok,
		Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
	}
	gr := callProvider(context.Background(), nil, MockCredentialResolver{}, in,
		func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
			return DepositResult{}, ErrorClassAmbiguous, vendorBodyError{body: redactFakeSecret}
		})
	if gr.Class != ErrorClassAmbiguous || !gr.Attempted || gr.Err == nil {
		t.Fatalf("class=%s attempted=%v err=%v, want Ambiguous/attempted/non-nil", gr.Class, gr.Attempted, gr.Err)
	}
	msg := gr.Err.Error()
	if strings.Contains(msg, redactFakeSecret) || strings.Contains(msg, "vendor responded") {
		t.Fatalf("adapter error text leaked into GateResult.Err: %q", msg)
	}
	if msg != "provider call error: "+redactedAdapterErrorLabel {
		t.Fatalf("GateResult.Err = %q", msg)
	}
}

type secretLeakingResolver struct{}

func (secretLeakingResolver) Resolve(context.Context, providercred.TenantTxRunner, uuid.UUID, string) (providercred.OutboundCredential, error) {
	return providercred.OutboundCredential{}, fmt.Errorf("%w: vault path secret/psp token=%s", providercred.ErrOutboundCredentialUnavailable, redactFakeSecret)
}

type foreignErrorResolver struct{}

func (foreignErrorResolver) Resolve(context.Context, providercred.TenantTxRunner, uuid.UUID, string) (providercred.OutboundCredential, error) {
	return providercred.OutboundCredential{}, errors.New("store: dsn=postgres://u:" + redactFakeSecret + "@db")
}

func TestCallProvider_CredentialResolutionErrorRedacted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver OutboundCredentialResolver
		wantTail string
	}{
		{"sentinel_with_context", secretLeakingResolver{}, providercred.ErrOutboundCredentialUnavailable.Error()},
		{"foreign_error", foreignErrorResolver{}, redactedAdapterErrorLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := uuid.New()
			in := callProviderInput{
				TenantID: uuid.New(), ProviderID: "mock-psp-cred",
				AttemptState: AttemptSubmitting, ClaimToken: tok, ExpectedClaim: tok,
				Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
			}
			called := false
			gr := callProvider(context.Background(), nil, tc.resolver, in,
				func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
					called = true
					return DepositResult{}, ErrorClassSucceeded, nil
				})
			if called || gr.Attempted || gr.Class != ErrorClassNotSent {
				t.Fatalf("called=%v attempted=%v class=%s, want NotSent with no call", called, gr.Attempted, gr.Class)
			}
			if !errors.Is(gr.Err, ErrProviderCallRefused) {
				t.Fatalf("err %v must still wrap ErrProviderCallRefused", gr.Err)
			}
			msg := gr.Err.Error()
			if strings.Contains(msg, redactFakeSecret) || strings.Contains(msg, "dsn=") || strings.Contains(msg, "vault path") {
				t.Fatalf("credential-resolution error text leaked: %q", msg)
			}
			if !strings.HasSuffix(msg, "credential resolution failed: "+tc.wantTail) {
				t.Fatalf("err = %q, want suffix %q", msg, tc.wantTail)
			}
		})
	}
}
