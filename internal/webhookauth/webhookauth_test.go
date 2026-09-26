package webhookauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/uuid"
)

// allSchemes is every domain's platform-defined MOCK scheme.
var allSchemes = map[string]Scheme{
	"payments": PaymentsScheme(),
	"kyc":      KYCScheme(),
	"casino":   CasinoScheme(),
}

// testKey returns a fresh random key (never a literal).
func testKey(t *testing.T) []byte {
	t.Helper()
	return NewMockMaster()
}

func credFor(tenantID uuid.UUID, providerID string, key []byte) Credential {
	return Credential{TenantID: tenantID, ProviderID: providerID, KeyID: MockKeyID, Secret: key, Fingerprint: Fingerprint(key)}
}

func signedInbound(s Scheme, key []byte, tenantID uuid.UUID, providerID, keyID string, body []byte) Inbound {
	h := make(http.Header)
	s.SetHeaders(h, keyID, s.Sign(key, tenantID, providerID, keyID, body))
	return Inbound{TenantID: tenantID, ProviderID: providerID, Header: h, Body: body}
}

// TestPaymentsParameters_ByteIdentical pins the payments domain's
// parameters to Stage 10.1's literal values and proves the moved signing
// and key-derivation code is byte-identical to an independent
// reimplementation of the Stage 10.1 algorithm (payments regression
// guarantee, design §A).
func TestPaymentsParameters_ByteIdentical(t *testing.T) {
	if PaymentsSigningPrefix != "igaming.payments.webhook.v1" ||
		PaymentsSignatureHeader != "X-Payments-Signature" ||
		PaymentsKeyIDHeader != "X-Payments-Key-Id" ||
		PaymentsMockKeyLabel != "igaming/payments-mock-webhook/v1" {
		t.Fatal("payments webhook parameters changed - must stay byte-identical to Stage 10.1")
	}

	master := testKey(t)
	tenantID := uuid.New()
	body := []byte(`{"event_type":"deposit","amount":100}`)

	// Independent Stage 10.1 key derivation.
	kmac := hmac.New(sha256.New, master)
	kmac.Write([]byte("igaming/payments-mock-webhook/v1"))
	kmac.Write([]byte{0})
	kmac.Write([]byte(tenantID.String()))
	kmac.Write([]byte{0})
	kmac.Write([]byte("mock-psp"))
	wantKey := kmac.Sum(nil)
	gotKey := DeriveMockKey(master, PaymentsMockKeyLabel, tenantID, "mock-psp")
	if !bytes.Equal(gotKey, wantKey) {
		t.Fatal("DeriveMockKey is not byte-identical to the Stage 10.1 payments derivation")
	}

	// Independent Stage 10.1 signing input and signature.
	input := []byte("igaming.payments.webhook.v1\x00" + tenantID.String() + "\x00mock-psp\x00mock-v1\x00")
	input = append(input, body...)
	if !bytes.Equal(PaymentsScheme().SigningInput(tenantID, "mock-psp", "mock-v1", body), input) {
		t.Fatal("SigningInput is not byte-identical to the Stage 10.1 payments signing input")
	}
	smac := hmac.New(sha256.New, wantKey)
	smac.Write(input)
	if got, want := PaymentsScheme().Sign(wantKey, tenantID, "mock-psp", "mock-v1", body), hex.EncodeToString(smac.Sum(nil)); got != want {
		t.Fatal("Sign is not byte-identical to the Stage 10.1 payments signature")
	}
}

// TestDomainParameters_PairwiseDistinct: three independent separations per
// domain (prefix, headers, mock label) - ADR 0022 §3 point 8.
func TestDomainParameters_PairwiseDistinct(t *testing.T) {
	type params struct{ prefix, sig, kid, label string }
	ps := map[string]params{
		"payments": {PaymentsSigningPrefix, PaymentsSignatureHeader, PaymentsKeyIDHeader, PaymentsMockKeyLabel},
		"kyc":      {KYCSigningPrefix, KYCSignatureHeader, KYCKeyIDHeader, KYCMockKeyLabel},
		"casino":   {CasinoSigningPrefix, CasinoSignatureHeader, CasinoKeyIDHeader, CasinoMockKeyLabel},
	}
	for a, pa := range ps {
		for b, pb := range ps {
			if a == b {
				continue
			}
			if pa.prefix == pb.prefix || http.CanonicalHeaderKey(pa.sig) == http.CanonicalHeaderKey(pb.sig) ||
				http.CanonicalHeaderKey(pa.kid) == http.CanonicalHeaderKey(pb.kid) || pa.label == pb.label {
				t.Fatalf("domains %s and %s share a separation parameter", a, b)
			}
			if strings.HasPrefix(pa.prefix, pb.prefix) {
				t.Fatalf("prefix of %s is a prefix of %s's", b, a)
			}
		}
	}
}

func TestScheme_SignVerifyRoundtrip(t *testing.T) {
	for name, s := range allSchemes {
		t.Run(name, func(t *testing.T) {
			key := testKey(t)
			tenantID := uuid.New()
			in := signedInbound(s, key, tenantID, "mock-x", MockKeyID, []byte(`{"a":1}`))
			if err := s.Verify(credFor(tenantID, "mock-x", key), in); err != nil {
				t.Fatalf("expected a correctly signed callback to verify, got %v", err)
			}
			// Empty body is a legitimate signed input too.
			in = signedInbound(s, key, tenantID, "mock-x", MockKeyID, nil)
			if err := s.Verify(credFor(tenantID, "mock-x", key), in); err != nil {
				t.Fatalf("expected an empty-body callback to verify, got %v", err)
			}
		})
	}
}

func TestScheme_Verify_TamperRejected(t *testing.T) {
	for name, s := range allSchemes {
		t.Run(name, func(t *testing.T) {
			key := testKey(t)
			tenantA, tenantB := uuid.New(), uuid.New()
			body := []byte(`{"amount":1000,"ref":"r-1"}`)
			cred := credFor(tenantA, "mock-x", key)
			fresh := func() Inbound { return signedInbound(s, key, tenantA, "mock-x", MockKeyID, body) }
			sig := func(in Inbound) string { return strings.TrimPrefix(in.Header.Get(s.SignatureHeader), "v1=") }

			cases := map[string]func() (Credential, Inbound){
				"body byte flipped": func() (Credential, Inbound) {
					in := fresh()
					b := append([]byte(nil), in.Body...)
					b[10] ^= 0x01
					in.Body = b
					return cred, in
				},
				"whitespace byte appended": func() (Credential, Inbound) {
					in := fresh()
					in.Body = append(append([]byte(nil), in.Body...), ' ')
					return cred, in
				},
				"tenant swapped (A-signed delivered as B, B's own cred with EQUAL key)": func() (Credential, Inbound) {
					in := fresh()
					in.TenantID = tenantB
					return credFor(tenantB, "mock-x", key), in
				},
				"provider swapped (equal key)": func() (Credential, Inbound) {
					in := fresh()
					in.ProviderID = "mock-y"
					return credFor(tenantA, "mock-y", key), in
				},
				"key id header changed": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Set(s.KeyIDHeader, "mock-v2")
					return cred, in
				},
				"key id signed differs from credential key id": func() (Credential, Inbound) {
					in := signedInbound(s, key, tenantA, "mock-x", "mock-v2", body)
					c := cred
					c.KeyID = "mock-v2"
					in.Header.Set(s.KeyIDHeader, MockKeyID)
					return c, in
				},
				"credential bound to another tenant": func() (Credential, Inbound) {
					return credFor(tenantB, "mock-x", key), fresh()
				},
				"credential bound to another provider": func() (Credential, Inbound) {
					return credFor(tenantA, "mock-y", key), fresh()
				},
				"63 hex characters": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Set(s.SignatureHeader, "v1="+sig(in)[:63])
					return cred, in
				},
				"65 hex characters": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Set(s.SignatureHeader, "v1="+sig(in)+"0")
					return cred, in
				},
				"uppercase hex": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Set(s.SignatureHeader, "v1="+strings.ToUpper(sig(in)))
					return cred, in
				},
				"wrong version tag": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Set(s.SignatureHeader, "v2="+sig(in))
					return cred, in
				},
				"missing signature header": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Del(s.SignatureHeader)
					return cred, in
				},
				"missing key id header": func() (Credential, Inbound) {
					in := fresh()
					in.Header.Del(s.KeyIDHeader)
					return cred, in
				},
				"wrong key": func() (Credential, Inbound) {
					return credFor(tenantA, "mock-x", testKey(t)), fresh()
				},
				"empty secret": func() (Credential, Inbound) {
					return credFor(tenantA, "mock-x", nil), signedInbound(s, nil, tenantA, "mock-x", MockKeyID, body)
				},
			}
			// Sanity: the untampered fixture verifies, so each rejection
			// below is about its specific tamper.
			if err := s.Verify(cred, fresh()); err != nil {
				t.Fatalf("untampered fixture must verify, got %v", err)
			}
			for cname, build := range cases {
				t.Run(cname, func(t *testing.T) {
					c, in := build()
					if err := s.Verify(c, in); err != ErrSignatureInvalid { //nolint:errorlint // exact sentinel identity is the contract
						t.Fatalf("expected exactly ErrSignatureInvalid, got %v", err)
					}
				})
			}
		})
	}
}

func TestScheme_Verify_EmptyPrefixFailsClosed(t *testing.T) {
	key := testKey(t)
	tenantID := uuid.New()
	s := Scheme{SignatureHeader: "X-S", KeyIDHeader: "X-K"}
	in := signedInbound(s, key, tenantID, "mock-x", MockKeyID, []byte("{}"))
	if err := s.Verify(credFor(tenantID, "mock-x", key), in); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("a scheme with no domain prefix must never verify, got %v", err)
	}
}

// TestScheme_CrossDomain_NeverVerifiesUnderEqualKey is the MANDATORY
// cross-domain test (architect review §1, QA P2; ADR 0022 §3 point 8): a
// signature valid in one domain never verifies in another, even under an
// EQUAL key, the same tenant, provider, key id and body - including when an
// attacker relabels the headers to the target domain's names.
func TestScheme_CrossDomain_NeverVerifiesUnderEqualKey(t *testing.T) {
	key := testKey(t)
	tenantID := uuid.New()
	body := []byte(`{"provider_reference":"ref-1","outcome":"approved"}`)
	for from, sFrom := range allSchemes {
		for to, sTo := range allSchemes {
			if from == to {
				continue
			}
			t.Run(from+"->"+to, func(t *testing.T) {
				cred := credFor(tenantID, "mock-x", key)
				src := signedInbound(sFrom, key, tenantID, "mock-x", MockKeyID, body)
				// Sanity: valid in its own domain.
				if err := sFrom.Verify(cred, src); err != nil {
					t.Fatalf("source signature must verify in its own domain, got %v", err)
				}
				// As delivered (source header names).
				if err := sTo.Verify(cred, src); !errors.Is(err, ErrSignatureInvalid) {
					t.Fatalf("a %s signature verified as %s (as delivered)", from, to)
				}
				// Relabelled to the target domain's header names.
				relabelled := make(http.Header)
				sTo.SetHeaders(relabelled, MockKeyID, strings.TrimPrefix(src.Header.Get(sFrom.SignatureHeader), "v1="))
				in := Inbound{TenantID: tenantID, ProviderID: "mock-x", Header: relabelled, Body: body}
				if err := sTo.Verify(cred, in); !errors.Is(err, ErrSignatureInvalid) {
					t.Fatalf("a %s signature verified as %s under an equal key", from, to)
				}
			})
		}
	}
	// And the mock key derivations differ per domain under an equal master.
	labels := []string{PaymentsMockKeyLabel, KYCMockKeyLabel, CasinoMockKeyLabel}
	for i := range labels {
		for j := range labels {
			if i != j && bytes.Equal(DeriveMockKey(key, labels[i], tenantID, "mock-x"), DeriveMockKey(key, labels[j], tenantID, "mock-x")) {
				t.Fatalf("mock keys for labels %q and %q collide under an equal master", labels[i], labels[j])
			}
		}
	}
}

func TestScheme_ParseHeaders(t *testing.T) {
	good := "v1=" + strings.Repeat("a", 64)
	for name, s := range allSchemes {
		t.Run(name, func(t *testing.T) {
			cases := []struct {
				name       string
				sig, kid   string
				wantOK     bool
				wantReason Reason
			}{
				{"valid", good, "mock-v1", true, ""},
				{"missing both", "", "", false, ReasonSignatureMissing},
				{"missing signature", "", "mock-v1", false, ReasonSignatureMissing},
				{"missing key id", good, "", false, ReasonSignatureMissing},
				{"key id bad charset", good, "Mock_V1", false, ReasonSignatureInvalid},
				{"key id too long", good, strings.Repeat("a", 33), false, ReasonSignatureInvalid},
				{"63 hex", "v1=" + strings.Repeat("a", 63), "mock-v1", false, ReasonSignatureInvalid},
				{"65 hex", "v1=" + strings.Repeat("a", 65), "mock-v1", false, ReasonSignatureInvalid},
				{"uppercase", "v1=" + strings.Repeat("A", 64), "mock-v1", false, ReasonSignatureInvalid},
				{"no version", strings.Repeat("a", 64), "mock-v1", false, ReasonSignatureInvalid},
			}
			for _, c := range cases {
				h := make(http.Header)
				if c.sig != "" {
					h.Set(s.SignatureHeader, c.sig)
				}
				if c.kid != "" {
					h.Set(s.KeyIDHeader, c.kid)
				}
				kid, sig, reason, ok := s.ParseHeaders(h)
				if ok != c.wantOK || reason != c.wantReason {
					t.Fatalf("%s: got ok=%v reason=%q, want ok=%v reason=%q", c.name, ok, reason, c.wantOK, c.wantReason)
				}
				if ok && (kid != c.kid || sig != strings.Repeat("a", 64)) {
					t.Fatalf("%s: wrong parsed values", c.name)
				}
			}
			// Another domain's headers are "missing" for this scheme.
			for other, so := range allSchemes {
				if other == name {
					continue
				}
				h := make(http.Header)
				so.SetHeaders(h, "mock-v1", strings.Repeat("a", 64))
				if _, _, reason, ok := s.ParseHeaders(h); ok || reason != ReasonSignatureMissing {
					t.Fatalf("%s headers accepted by %s scheme", other, name)
				}
			}
		})
	}
}

func TestScheme_CheckPreamble(t *testing.T) {
	s := PaymentsScheme()
	const maxBody = 16
	good := make(http.Header)
	s.SetHeaders(good, "mock-v1", strings.Repeat("a", 64))

	t.Run("provider id charset first, body never read", func(t *testing.T) {
		r := &countingReader{r: strings.NewReader("{}")}
		res, ok := s.CheckPreamble("Bad_Provider", good, r, maxBody)
		if ok || res.Reason != ReasonProviderInvalid || res.ProviderIDValid || res.BodyLen != 0 || r.n != 0 {
			t.Fatalf("unexpected result %+v (bytes read %d)", res, r.n)
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		res, ok := s.CheckPreamble("mock-x", good, strings.NewReader(strings.Repeat("x", maxBody+10)), maxBody)
		if ok || res.Reason != ReasonBodyTooLarge || !res.ProviderIDValid || res.BodyLen != maxBody+1 || res.Body != nil {
			t.Fatalf("unexpected result %+v", res)
		}
	})
	t.Run("exactly max body is accepted", func(t *testing.T) {
		res, ok := s.CheckPreamble("mock-x", good, strings.NewReader(strings.Repeat("x", maxBody)), maxBody)
		if !ok || len(res.Body) != maxBody || res.BodyLen != maxBody {
			t.Fatalf("unexpected result %+v", res)
		}
	})
	t.Run("unreadable body", func(t *testing.T) {
		res, ok := s.CheckPreamble("mock-x", good, iotest.ErrReader(errors.New("boom")), maxBody)
		if ok || res.Reason != ReasonBodyTooLarge || !res.ProviderIDValid || res.BodyLen != 0 {
			t.Fatalf("unexpected result %+v", res)
		}
	})
	t.Run("missing headers", func(t *testing.T) {
		res, ok := s.CheckPreamble("mock-x", http.Header{}, strings.NewReader("{}"), maxBody)
		if ok || res.Reason != ReasonSignatureMissing || !res.ProviderIDValid || res.BodyLen != 2 {
			t.Fatalf("unexpected result %+v", res)
		}
	})
	t.Run("non-JSON body with valid headers is not parsed", func(t *testing.T) {
		res, ok := s.CheckPreamble("mock-x", good, strings.NewReader("not json"), maxBody)
		if !ok || string(res.Body) != "not json" {
			t.Fatalf("unexpected result %+v", res)
		}
	})
}

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestMockResolver(t *testing.T) {
	ctx := context.Background()
	master, otherMaster := NewMockMaster(), NewMockMaster()
	tenantA, tenantB := uuid.New(), uuid.New()
	r := MockResolver{Master: master, Label: PaymentsMockKeyLabel, ProviderID: "mock-x"}

	a1, err := r.Resolve(ctx, tenantA, "mock-x", MockKeyID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	a2, _ := r.Resolve(ctx, tenantA, "mock-x", MockKeyID)
	b, _ := r.Resolve(ctx, tenantB, "mock-x", MockKeyID)
	other, _ := MockResolver{Master: otherMaster, Label: PaymentsMockKeyLabel, ProviderID: "mock-x"}.Resolve(ctx, tenantA, "mock-x", MockKeyID)
	kyc, _ := MockResolver{Master: master, Label: KYCMockKeyLabel, ProviderID: "mock-x"}.Resolve(ctx, tenantA, "mock-x", MockKeyID)

	if !bytes.Equal(a1.Secret, a2.Secret) {
		t.Fatal("key must be stable within one master")
	}
	if bytes.Equal(a1.Secret, b.Secret) || bytes.Equal(a1.Secret, other.Secret) || bytes.Equal(a1.Secret, kyc.Secret) {
		t.Fatal("keys must differ per tenant, per master (instance) and per domain label")
	}
	if a1.TenantID != tenantA || a1.ProviderID != "mock-x" || a1.KeyID != MockKeyID || a1.Fingerprint != Fingerprint(a1.Secret) {
		t.Fatalf("credential binding wrong: %v", a1)
	}

	failClosed := map[string]func() (Credential, error){
		"foreign provider": func() (Credential, error) { return r.Resolve(ctx, tenantA, "mock-y", MockKeyID) },
		"foreign key id":   func() (Credential, error) { return r.Resolve(ctx, tenantA, "mock-x", "mock-v2") },
		"nil master": func() (Credential, error) {
			return MockResolver{Label: "l", ProviderID: "mock-x"}.Resolve(ctx, tenantA, "mock-x", MockKeyID)
		},
		"short master": func() (Credential, error) {
			return MockResolver{Master: master[:16], Label: "l", ProviderID: "mock-x"}.Resolve(ctx, tenantA, "mock-x", MockKeyID)
		},
		"empty label": func() (Credential, error) {
			return MockResolver{Master: master, ProviderID: "mock-x"}.Resolve(ctx, tenantA, "mock-x", MockKeyID)
		},
		"empty provider id": func() (Credential, error) {
			return MockResolver{Master: master, Label: "l"}.Resolve(ctx, tenantA, "", MockKeyID)
		},
		"multi: absent": func() (Credential, error) { return MultiResolver{}.Resolve(ctx, tenantA, "mock-x", MockKeyID) },
		"multi: nil entry": func() (Credential, error) {
			return MultiResolver{"mock-x": nil}.Resolve(ctx, tenantA, "mock-x", MockKeyID)
		},
		"multi: other entry": func() (Credential, error) {
			return MultiResolver{"mock-y": r}.Resolve(ctx, tenantA, "mock-x", MockKeyID)
		},
	}
	for name, f := range failClosed {
		t.Run(name, func(t *testing.T) {
			c, err := f()
			if !errors.Is(err, ErrCredentialUnavailable) || len(c.Secret) != 0 {
				t.Fatalf("expected ErrCredentialUnavailable and no secret, got %v", err)
			}
		})
	}
	if c, err := (MultiResolver{"mock-x": r}).Resolve(ctx, tenantA, "mock-x", MockKeyID); err != nil || !bytes.Equal(c.Secret, a1.Secret) {
		t.Fatalf("multi resolver must dispatch to the registered entry, got %v", err)
	}
}

func TestDeriveMockKey_RefusesShortMaster(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("DeriveMockKey must panic on a short master rather than derive a predictable key")
		}
	}()
	DeriveMockKey(nil, PaymentsMockKeyLabel, uuid.New(), "mock-x")
}

func TestCredential_Redaction(t *testing.T) {
	secret := NewMockMaster()
	c := credFor(uuid.New(), "mock-x", secret)
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "cred", c)
	renderings := map[string]string{
		"%v":   fmt.Sprintf("%v", c),
		"%+v":  fmt.Sprintf("%+v", c),
		"%#v":  fmt.Sprintf("%#v", c),
		"Str":  c.String(),
		"nest": fmt.Sprintf("%+v", struct{ C Credential }{c}),
		"slog": buf.String(),
	}
	for name, out := range renderings {
		if strings.Contains(out, hex.EncodeToString(secret)) || strings.Contains(out, string(secret)) ||
			strings.Contains(out, fmt.Sprintf("%d", secret)) {
			t.Fatalf("%s rendering leaks the secret", name)
		}
		if !strings.Contains(out, c.Fingerprint) {
			t.Fatalf("%s rendering should carry the fingerprint", name)
		}
	}
}

func TestAuthError_IsAuthFailed(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &AuthError{Reason: ReasonSignatureInvalid})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatal("any *AuthError must match ErrAuthFailed")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Reason != ReasonSignatureInvalid {
		t.Fatal("errors.As must recover the reason")
	}
	if errors.Is(err, ErrSignatureInvalid) || errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("an AuthError must not match the other sentinels")
	}
}
