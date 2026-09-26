package webhookauthtest_test

// TEST-ONLY timestamped reference scheme (01-provider-trust-analysis.md
// §1.2: "The SC7 path is proven with a test-only timestamped reference
// scheme in the harness's own tests, never in production code"). It is NOT
// a vendor protocol and models no real vendor. Its bug switches build the
// deliberately broken schemes of the conformance self-test.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

const (
	refSigHeader     = "X-Ref-Signature"
	refTSHeader      = "X-Ref-Timestamp"
	refKeyIDHeader   = "X-Ref-Key-Id"
	refAccountHeader = "X-Ref-Account"
)

// refBugs are the self-test's deliberate defects - one per mandatory case.
type refBugs struct {
	ignoreTenant       bool // SC3
	ignoreProvider     bool // SC4
	ignoreTimestamp    bool // SC7
	timestampBeforeMAC bool // SC7 (C10: window checked before the MAC; security S-2)
	prefixCompare      bool // SC2 (accepts a truncated signature)
	ignoreAccount      bool // SC8
	ignoreNotAfter     bool // SC9
	acceptShortSecret  bool // SC10
	secretInError      bool // SC11
	panicOnLongHeader  bool // SC5
	trialOnAbsentKeyID bool // SC6 (C3)
	swappedOrderBoth   bool // KAV (Sign and Verify share the bug)
}

type refScheme struct {
	name    string
	binding webhookauth.TenantBinding
	keySel  webhookauth.KeySelection
	skew    time.Duration
	bugs    refBugs
}

type refMaterial struct {
	sig  string
	ts   int64
	acct string
}

var (
	refSig64      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	refSigLenient = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
	refTS         = regexp.MustCompile(`^[0-9]{1,12}$`)
	refKeyID      = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	refAccount    = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

func (s refScheme) Name() string { return s.name }

func (s refScheme) Properties() webhookauth.SchemeProperties {
	return webhookauth.SchemeProperties{
		Binding: s.binding, KeySelection: s.keySel,
		SignedTimestamp: true, MaxSkew: s.skew, Replay: webhookauth.ReplayTimestampWindow,
	}
}

func (s refScheme) signedAccount() bool {
	return s.binding == webhookauth.BindingPerMerchantKeySignedAccount
}

func (s refScheme) macInput(ts int64, acct string, body []byte) []byte {
	tsStr := strconv.FormatInt(ts, 10)
	if s.bugs.swappedOrderBoth {
		return []byte(string(body) + "\n" + tsStr + "\n" + acct + "\nref.v1")
	}
	return append([]byte("ref.v1\n"+tsStr+"\n"+acct+"\n"), body...)
}

func (s refScheme) mac(secret []byte, ts int64, acct string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write(s.macInput(ts, acct, body))
	return hex.EncodeToString(m.Sum(nil))
}

func (s refScheme) Extract(in webhookauth.Inbound) (webhookauth.AuthMaterial, webhookauth.Reason, bool) {
	if s.bugs.panicOnLongHeader {
		for _, h := range []string{refSigHeader, refTSHeader, refKeyIDHeader, refAccountHeader} {
			if len(in.Header.Get(h)) > 1024 {
				panic("reference scheme: header too long")
			}
		}
	}
	sig, ts := in.Header.Get(refSigHeader), in.Header.Get(refTSHeader)
	if sig == "" || ts == "" {
		return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureMissing, false
	}
	keyID := ""
	if s.keySel == webhookauth.KeyFromHeader {
		keyID = in.Header.Get(refKeyIDHeader)
		if keyID == "" && !s.bugs.trialOnAbsentKeyID {
			return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureMissing, false
		}
		if keyID != "" && !refKeyID.MatchString(keyID) {
			return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureInvalid, false
		}
	}
	acct := ""
	if s.signedAccount() {
		acct = in.Header.Get(refAccountHeader)
		if acct == "" {
			return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureMissing, false
		}
		if !refAccount.MatchString(acct) {
			return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureInvalid, false
		}
	}
	sigOK := refSig64.MatchString(sig)
	if s.bugs.prefixCompare {
		sigOK = refSigLenient.MatchString(sig)
	}
	if !sigOK || !refTS.MatchString(ts) {
		return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureInvalid, false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return webhookauth.AuthMaterial{}, webhookauth.ReasonSignatureInvalid, false
	}
	return webhookauth.NewAuthMaterial(keyID, refMaterial{sig: sig, ts: n, acct: acct}), "", true
}

func (s refScheme) fail(creds webhookauth.CredentialSet) error {
	if s.bugs.secretInError {
		return fmt.Errorf("%w: key=%x", webhookauth.ErrSignatureInvalid, creds.Active.Secret)
	}
	return webhookauth.ErrSignatureInvalid
}

func (s refScheme) Verify(creds webhookauth.CredentialSet, in webhookauth.Inbound, m webhookauth.AuthMaterial, now time.Time) (string, error) {
	mat, ok := m.Private().(refMaterial)
	if !ok {
		return "", s.fail(creds)
	}
	if s.bugs.timestampBeforeMAC {
		// The C10 defect: the window is checked BEFORE the MAC, so an
		// unauthenticated stale-and-tampered request yields
		// ErrTimestampOutOfWindow instead of ErrSignatureInvalid. Only SC7's
		// stale-and-tampered assertion can see this (security S-2).
		d := now.Sub(time.Unix(mat.ts, 0))
		if d > s.skew || d < -s.skew {
			return "", webhookauth.ErrTimestampOutOfWindow
		}
	}
	var candidates []webhookauth.Credential
	switch {
	case s.keySel == webhookauth.KeyFromHeader && m.KeyID == "" && s.bugs.trialOnAbsentKeyID:
		candidates = append(candidates, creds.Active)
		if creds.Previous != nil {
			candidates = append(candidates, *creds.Previous)
		}
	case s.keySel == webhookauth.KeyFromHeader:
		if m.KeyID != creds.Active.KeyID {
			return "", s.fail(creds)
		}
		candidates = append(candidates, creds.Active)
	default:
		candidates = append(candidates, creds.Active)
		if p := creds.Previous; p != nil && (s.bugs.ignoreNotAfter || (!p.NotAfter.IsZero() && now.Before(p.NotAfter))) {
			candidates = append(candidates, *p)
		}
	}
	var matched *webhookauth.Credential
	for i := range candidates {
		c := candidates[i]
		usable := (s.bugs.acceptShortSecret || len(c.Secret) >= webhookauth.MinSecretBytes) &&
			(s.bugs.ignoreTenant || c.TenantID == in.TenantID) &&
			(s.bugs.ignoreProvider || c.ProviderID == in.ProviderID)
		expected := s.mac(c.Secret, mat.ts, mat.acct, in.Body)
		var eq bool
		if s.bugs.prefixCompare {
			eq = strings.HasPrefix(expected, mat.sig)
		} else {
			eq = hmac.Equal([]byte(expected), []byte(mat.sig))
		}
		// Every candidate is evaluated (no short-circuit, security C4).
		if usable && eq && matched == nil {
			matched = &candidates[i]
		}
	}
	if matched == nil {
		return "", s.fail(creds)
	}
	if s.signedAccount() && !s.bugs.ignoreAccount && mat.acct != matched.BoundAccountID {
		return "", s.fail(creds)
	}
	if !s.bugs.ignoreTimestamp {
		d := now.Sub(time.Unix(mat.ts, 0))
		if d > s.skew || d < -s.skew {
			return "", webhookauth.ErrTimestampOutOfWindow
		}
	}
	return matched.KeyID, nil
}

// refKAV constants: HMAC-SHA256 computed independently of this file with
//
//	printf 'ref.v1\n1767225600\n\n{"kav":1}' | openssl dgst -sha256 -hmac 'conformance-kav-test-key-0123456789'
//	printf 'ref.v1\n1767225600\nmerchant-kav\n{"kav":1}' | openssl dgst -sha256 -hmac 'conformance-kav-test-key-0123456789'
//
// (2026-09-26). The key is a published, test-only string - not a credential.
const (
	refKAVKey        = "conformance-kav-test-key-0123456789"
	refKAVTimestamp  = 1767225600 // 2026-01-01T00:00:00Z
	refKAVBody       = `{"kav":1}`
	refKAVSig        = "85b25dc8ca247c1f9cb5edf06109715691e81e5e921de30e73c0ae5fba0e13d4"
	refKAVAccount    = "merchant-kav"
	refKAVSigAccount = "9a93c71b7f6ae2be339f942a319e02795d50e40a90f4a1f559848ebf58a31c69"
	refKAVProvenance = "TEST-ONLY reference scheme (no vendor); HMAC-SHA256 computed independently with openssl dgst on 2026-09-26"
)

var refKAVTenant = uuid.MustParse("11111111-2222-3333-4444-555555555555")

// newRefFixture returns the conformance fixture for s, including known-answer
// vectors matching its binding and key selection.
func newRefFixture(s refScheme) webhookauthtest.Fixture {
	headers := []string{refSigHeader, refTSHeader}
	keyHeader := ""
	if s.keySel == webhookauth.KeyFromHeader {
		headers = append(headers, refKeyIDHeader)
		keyHeader = refKeyIDHeader
	}
	if s.signedAccount() {
		headers = append(headers, refAccountHeader)
	}
	sign := func(cred webhookauth.Credential, _ uuid.UUID, _ string, body []byte, at time.Time) http.Header {
		h := http.Header{}
		h.Set(refSigHeader, s.mac(cred.Secret, at.Unix(), cred.BoundAccountID, body))
		h.Set(refTSHeader, strconv.FormatInt(at.Unix(), 10))
		if s.keySel == webhookauth.KeyFromHeader {
			h.Set(refKeyIDHeader, cred.KeyID)
		}
		if s.signedAccount() {
			h.Set(refAccountHeader, cred.BoundAccountID)
		}
		return h
	}
	kavCred := webhookauth.Credential{
		TenantID: refKAVTenant, ProviderID: "conformance-provider-a", KeyID: "kav-key",
		Secret: []byte(refKAVKey), Fingerprint: webhookauth.Fingerprint([]byte(refKAVKey)),
	}
	kavHeader := http.Header{}
	kavHeader.Set(refTSHeader, strconv.Itoa(refKAVTimestamp))
	kavHeader.Set(refSigHeader, refKAVSig)
	if s.keySel == webhookauth.KeyFromHeader {
		kavHeader.Set(refKeyIDHeader, "kav-key")
	}
	if s.signedAccount() {
		kavCred.BoundAccountID = refKAVAccount
		kavHeader.Set(refAccountHeader, refKAVAccount)
		kavHeader.Set(refSigHeader, refKAVSigAccount)
	}
	kavNow := time.Unix(refKAVTimestamp, 0).UTC()
	valid := webhookauth.Inbound{TenantID: refKAVTenant, ProviderID: "conformance-provider-a", Header: kavHeader, Body: []byte(refKAVBody)}
	invalid := webhookauth.Inbound{TenantID: refKAVTenant, ProviderID: "conformance-provider-a", Header: kavHeader.Clone(), Body: []byte(refKAVBody)}
	sig := invalid.Header.Get(refSigHeader)
	last := "0"
	if sig[63] == '0' {
		last = "1"
	}
	invalid.Header.Set(refSigHeader, sig[:63]+last)
	return webhookauthtest.Fixture{
		Scheme:      s,
		AuthHeaders: headers,
		KeyIDHeader: keyHeader,
		Sign:        sign,
		NewCredential: func(tenantID uuid.UUID, providerID, keyID string) webhookauth.Credential {
			secret := make([]byte, 32)
			_, _ = rand.Read(secret)
			return webhookauth.Credential{TenantID: tenantID, ProviderID: providerID, KeyID: keyID, Secret: secret, Fingerprint: webhookauth.Fingerprint(secret)}
		},
		KnownAnswers: []webhookauthtest.KnownAnswer{
			{Provenance: refKAVProvenance, Credential: kavCred, Inbound: valid, Now: kavNow, WantValid: true},
			{Provenance: refKAVProvenance + " (last hex digit altered)", Credential: kavCred, Inbound: invalid, Now: kavNow, WantValid: false},
		},
	}
}

func baseRef() refScheme {
	return refScheme{
		name: "test-reference:per-merchant-key", binding: webhookauth.BindingPerMerchantKey,
		keySel: webhookauth.KeyFromHeader, skew: 5 * time.Minute,
	}
}

func accountRef() refScheme {
	s := baseRef()
	s.name, s.binding = "test-reference:signed-account", webhookauth.BindingPerMerchantKeySignedAccount
	return s
}

func implicitRef() refScheme {
	s := baseRef()
	s.name, s.keySel = "test-reference:key-implicit", webhookauth.KeyImplicit
	return s
}
