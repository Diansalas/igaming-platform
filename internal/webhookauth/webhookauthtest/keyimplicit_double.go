package webhookauthtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TwoKeyImplicit is a TEST DOUBLE for the matched-key_id log (security
// C4; gate W2 condition W2A-SEC-2): a KeyImplicit scheme plus a resolver
// that returns one active key and one in-window verify_only predecessor
// for a single (tenant, provider). Each domain's log-capture test signs
// with one key or the other and asserts which key_id the domain logged.
//
// It is never registered (it is not in the conformance manifest and is
// not a vendor protocol), so it is handed straight to each domain's
// unexported resolve-and-verify step. Both secrets are fresh
// crypto/rand bytes; nothing here is a literal credential.
type TwoKeyImplicit struct {
	Scheme   webhookauth.VerificationScheme
	Resolver webhookauth.Resolver
	Active   webhookauth.Credential
	Previous webhookauth.Credential
}

// TwoKeyImplicitSignatureHeader carries hex(HMAC-SHA256(secret, body)).
const TwoKeyImplicitSignatureHeader = "X-Test-Implicit-Signature"

// NewTwoKeyImplicit builds the double for (tenant, providerID). The active
// key id is "k-active" and the predecessor's is "k-previous"; the
// predecessor's not_after is one hour from now.
func NewTwoKeyImplicit(tenant uuid.UUID, providerID string) TwoKeyImplicit {
	mk := func(keyID string, notAfter time.Time) webhookauth.Credential {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			panic(err)
		}
		return webhookauth.Credential{
			TenantID: tenant, ProviderID: providerID, KeyID: keyID,
			Secret: secret, Fingerprint: webhookauth.Fingerprint(secret), NotAfter: notAfter,
		}
	}
	active := mk("k-active", time.Time{})
	previous := mk("k-previous", time.Now().Add(time.Hour))
	return TwoKeyImplicit{
		Scheme:   twoKeyImplicitScheme{},
		Resolver: twoKeyImplicitResolver{active: active, previous: previous},
		Active:   active,
		Previous: previous,
	}
}

// Inbound returns a callback for body signed with signer's secret.
func (f TwoKeyImplicit) Inbound(signer webhookauth.Credential, body []byte) webhookauth.Inbound {
	h := http.Header{}
	h.Set(TwoKeyImplicitSignatureHeader, hex.EncodeToString(mac(signer.Secret, body)))
	return webhookauth.Inbound{TenantID: signer.TenantID, ProviderID: signer.ProviderID, Header: h, Body: body}
}

func mac(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return m.Sum(nil)
}

type twoKeyImplicitScheme struct{}

func (twoKeyImplicitScheme) Name() string { return "test-two-key-implicit-v1" }

func (twoKeyImplicitScheme) Properties() webhookauth.SchemeProperties {
	return webhookauth.SchemeProperties{
		Binding:         webhookauth.BindingPerMerchantKey,
		KeySelection:    webhookauth.KeyImplicit,
		SignedTimestamp: true,
		MaxSkew:         time.Minute,
		Replay:          webhookauth.ReplayTimestampWindow,
	}
}

func (twoKeyImplicitScheme) Extract(in webhookauth.Inbound) (webhookauth.AuthMaterial, webhookauth.Reason, bool) {
	v := in.Header.Get(TwoKeyImplicitSignatureHeader)
	if v == "" {
		return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureMissing, false
	}
	sig, err := hex.DecodeString(v)
	if err != nil || len(sig) != sha256.Size {
		return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureInvalid, false
	}
	return webhookauth.NewAuthMaterial("", sig), "", true
}

func (twoKeyImplicitScheme) Verify(creds webhookauth.CredentialSet, in webhookauth.Inbound, m webhookauth.AuthMaterial, _ time.Time) (string, error) {
	sig, _ := m.Private().([]byte)
	candidates := []webhookauth.Credential{creds.Active}
	if creds.Previous != nil {
		candidates = append(candidates, *creds.Previous)
	}
	for _, c := range candidates {
		if c.TenantID == in.TenantID && c.ProviderID == in.ProviderID && hmac.Equal(sig, mac(c.Secret, in.Body)) {
			return c.KeyID, nil
		}
	}
	return "", webhookauth.ErrSignatureInvalid
}

type twoKeyImplicitResolver struct {
	active, previous webhookauth.Credential
}

func (r twoKeyImplicitResolver) Resolve(_ context.Context, _ webhookauth.TenantReader, tenantID uuid.UUID, providerID, keyID string, sel webhookauth.KeySelection) (webhookauth.CredentialSet, error) {
	if sel != webhookauth.KeyImplicit || keyID != "" || tenantID != r.active.TenantID || providerID != r.active.ProviderID {
		return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
	}
	p := r.previous
	return webhookauth.CredentialSet{Active: r.active, Previous: &p}, nil
}

// Recheck implements webhookauth.Resolver: a test double has no handle
// rows, so it accepts exactly its own two credentials (by key id and
// fingerprint) for its tenant and provider.
func (r twoKeyImplicitResolver) Recheck(_ context.Context, _ pgx.Tx, tenantID uuid.UUID, c webhookauth.Credential) error {
	for _, k := range []webhookauth.Credential{r.active, r.previous} {
		if tenantID == k.TenantID && c.TenantID == k.TenantID && c.ProviderID == k.ProviderID &&
			c.KeyID == k.KeyID && c.Fingerprint == k.Fingerprint {
			return nil
		}
	}
	return webhookauth.ErrCredentialUnavailable
}

// AssertVerifiedKeyLog checks out (JSON slog output) holds exactly one
// line, the webhookauth.VerifiedKeyLogEvent Info line, with exactly the
// attributes request_id, tenant_id, provider_id and key_id (plus slog's
// own time/level/msg) and the wanted values. It also checks that no
// secret of any credential in never (raw, hex, HEX, base64 or base64url)
// and no fingerprint appears anywhere in out.
func AssertVerifiedKeyLog(t testing.TB, out []byte, requestID string, tenant uuid.UUID, providerID, keyID string, never ...webhookauth.Credential) {
	t.Helper()
	AssertNoSecretInLog(t, out, never...)
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(bytes.TrimSpace(out)) == 0 || len(lines) != 1 {
		t.Fatalf("want exactly one log line, got %d: %s", len(lines), out)
	}
	var rec map[string]any
	if err := json.Unmarshal(lines[0], &rec); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, lines[0])
	}
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "key_id,level,msg,provider_id,request_id,tenant_id,time"; got != want {
		t.Fatalf("log attributes = %s, want exactly %s", got, want)
	}
	want := map[string]string{
		"msg": webhookauth.VerifiedKeyLogEvent, "level": "INFO", "request_id": requestID,
		"tenant_id": tenant.String(), "provider_id": providerID, "key_id": keyID,
	}
	for k, v := range want {
		if rec[k] != v {
			t.Fatalf("log %s = %v, want %q (line %s)", k, rec[k], v, lines[0])
		}
	}
}

// AssertNoSecretInLog fails if any credential's secret (raw, hex, HEX,
// base64, base64url) or fingerprint appears in out.
func AssertNoSecretInLog(t testing.TB, out []byte, creds ...webhookauth.Credential) {
	t.Helper()
	s := string(out)
	for _, c := range creds {
		forms := map[string]string{
			"raw secret": string(c.Secret), "hex secret": hex.EncodeToString(c.Secret),
			"HEX secret":       strings.ToUpper(hex.EncodeToString(c.Secret)),
			"base64 secret":    base64.StdEncoding.EncodeToString(c.Secret),
			"base64url secret": base64.RawURLEncoding.EncodeToString(c.Secret),
			"fingerprint":      c.Fingerprint,
		}
		for name, v := range forms {
			if v != "" && strings.Contains(s, v) {
				t.Fatalf("log output contains the %s of key %q", name, c.KeyID)
			}
		}
	}
}
