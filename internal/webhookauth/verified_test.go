package webhookauth

// ADR 0094 §4.1 VerifiedCallback (security co-sign C1-C3, C5; QA §11):
// single use, age bound, (domain, tenant, provider) binding, private copy
// of the verified bytes, redaction, and the re-check hand-off.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// fakeTx is a non-nil pgx.Tx for Redeem; no method is ever called by the
// test resolvers below.
type fakeTx struct{ pgx.Tx }

type verifiedFixture struct {
	tenant   uuid.UUID
	provider string
	key      []byte
	in       Inbound
	scheme   VerificationScheme
	creds    CredentialSet
	m        AuthMaterial
}

func newVerifiedFixture(t *testing.T) verifiedFixture {
	t.Helper()
	s := CasinoScheme()
	tenant, key := uuid.New(), testKey(t)
	in := signedInbound(s, key, tenant, "vendor-a", MockKeyID, []byte(`{"amount":1000}`))
	v := s.VerificationScheme()
	m, authErr := ExtractInbound(v, in)
	if authErr != nil {
		t.Fatal(authErr)
	}
	cred := credFor(tenant, "vendor-a", key)
	cred.HandleID = uuid.New()
	return verifiedFixture{tenant: tenant, provider: "vendor-a", key: key, in: in, scheme: v, creds: CredentialSet{Active: cred}, m: m}
}

func (f verifiedFixture) seal(t *testing.T, domain string) *VerifiedCallback {
	t.Helper()
	v, cred, authErr := VerifyAndSeal(domain, f.scheme, f.creds, CloneInbound(f.in), f.m, time.Now())
	if authErr != nil || v == nil {
		t.Fatalf("seal: %v", authErr)
	}
	if cred.HandleID != f.creds.Active.HandleID {
		t.Fatal("VerifyAndSeal must return the credential that verified, with its handle id")
	}
	return v
}

func wantUnavailable(t *testing.T, authErr *AuthError) {
	t.Helper()
	if authErr == nil || authErr.Reason != ReasonCredentialUnavailable {
		t.Fatalf("want credential_unavailable, got %v", authErr)
	}
}

func TestVerifyAndSeal_OnlyOnSuccessfulVerification(t *testing.T) {
	f := newVerifiedFixture(t)
	bad := CloneInbound(f.in)
	bad.Body = []byte(`{"amount":9999}`)
	if v, _, authErr := VerifyAndSeal("casino", f.scheme, f.creds, bad, f.m, time.Now()); v != nil || authErr == nil || authErr.Reason != ReasonSignatureInvalid {
		t.Fatalf("a failed verification must not seal: %v %v", v, authErr)
	}
}

// TestVerifiedCallback_ZeroOrMismatchRejected is ADR 0094 §9.3 test 10 at
// the unit level (plus the C5 variants that need no database): nil/zero,
// wrong domain, tenant, provider, reuse (C1) and age > 30 s (C2) all fail
// closed as credential_unavailable, and none of them reaches Recheck.
func TestVerifiedCallback_ZeroOrMismatchRejected(t *testing.T) {
	f := newVerifiedFixture(t)
	r := &countingResolver{}
	ctx := context.Background()
	tx := fakeTx{}

	var nilV *VerifiedCallback
	_, _, authErr := nilV.Redeem(ctx, tx, "casino", f.tenant, f.provider, r)
	wantUnavailable(t, authErr)
	_, _, authErr = (&VerifiedCallback{}).Redeem(ctx, tx, "casino", f.tenant, f.provider, r)
	wantUnavailable(t, authErr)

	for name, call := range map[string]func(v *VerifiedCallback) *AuthError{
		"other domain": func(v *VerifiedCallback) *AuthError {
			_, _, e := v.Redeem(ctx, tx, "payments", f.tenant, f.provider, r)
			return e
		},
		"other tenant": func(v *VerifiedCallback) *AuthError {
			_, _, e := v.Redeem(ctx, tx, "casino", uuid.New(), f.provider, r)
			return e
		},
		"other provider": func(v *VerifiedCallback) *AuthError {
			_, _, e := v.Redeem(ctx, tx, "casino", f.tenant, "vendor-b", r)
			return e
		},
		"nil resolver": func(v *VerifiedCallback) *AuthError {
			_, _, e := v.Redeem(ctx, tx, "casino", f.tenant, f.provider, nil)
			return e
		},
		"nil tx": func(v *VerifiedCallback) *AuthError {
			_, _, e := v.Redeem(ctx, nil, "casino", f.tenant, f.provider, r)
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := f.seal(t, "casino")
			wantUnavailable(t, call(v))
			// A failed redemption still consumed the token (C1).
			_, _, e := v.Redeem(ctx, tx, "casino", f.tenant, f.provider, r)
			wantUnavailable(t, e)
		})
	}
	if len(r.rechecks) != 0 {
		t.Fatalf("a rejected token must never reach Recheck, got %d calls", len(r.rechecks))
	}

	t.Run("age over 30s (C2)", func(t *testing.T) {
		old := sinceVerified
		defer func() { sinceVerified = old }()
		v := f.seal(t, "casino")
		sinceVerified = func(time.Time) time.Duration { return MaxVerifiedCallbackAge + time.Millisecond }
		_, _, e := v.Redeem(ctx, tx, "casino", f.tenant, f.provider, r)
		wantUnavailable(t, e)
		v2 := f.seal(t, "casino")
		sinceVerified = func(time.Time) time.Duration { return MaxVerifiedCallbackAge }
		if _, _, e := v2.Redeem(ctx, tx, "casino", f.tenant, f.provider, r); e != nil {
			t.Fatalf("a token exactly MaxVerifiedCallbackAge old is still valid: %v", e)
		}
	})
	if MaxVerifiedCallbackAge != 30*time.Second || MaxVerifiedCallbackAge >= MaxSkewCap {
		t.Fatalf("MaxVerifiedCallbackAge = %s, security C2 says 30 s (inside MaxSkewCap %s)", MaxVerifiedCallbackAge, MaxSkewCap)
	}
}

// TestVerifiedCallback_SingleUse is security condition C1: the first
// Redeem consumes the token, whatever the outcome, also across copies and
// under concurrency.
func TestVerifiedCallback_SingleUse(t *testing.T) {
	f := newVerifiedFixture(t)
	r := &countingResolver{}
	v := f.seal(t, "casino")
	if _, _, e := v.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, r); e != nil {
		t.Fatalf("first redemption: %v", e)
	}
	_, _, e := v.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, r)
	wantUnavailable(t, e)
	// A dereferenced copy shares the consumed flag.
	v2 := f.seal(t, "casino")
	cp := *v2
	if _, _, e := v2.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, r); e != nil {
		t.Fatal(e)
	}
	_, _, e = cp.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, r)
	wantUnavailable(t, e)
	// Concurrent redemptions: exactly one wins.
	v3 := f.seal(t, "casino")
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, e := v3.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, &countingResolver{}); e == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d concurrent redemptions succeeded, want exactly 1", ok.Load())
	}
}

// TestVerifiedCallback_RecheckFailureFailsClosed: the re-check of the
// verified handle decides; any error is credential_unavailable, and the
// handle re-checked is the one of the credential that VERIFIED (C4).
func TestVerifiedCallback_RecheckFailureFailsClosed(t *testing.T) {
	f := newVerifiedFixture(t)
	v := f.seal(t, "casino")
	r := &countingResolver{recheckErr: errors.New("revoked")}
	_, _, e := v.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, r)
	wantUnavailable(t, e)
	if len(r.rechecks) != 1 || r.rechecks[0].HandleID != f.creds.Active.HandleID {
		t.Fatalf("Recheck must be asked about the verified handle, got %v", r.rechecks)
	}
}

// TestVerifiedCallback_PredecessorHandleRechecked (C4): under KeyImplicit
// the credential that verified may be the verify_only predecessor; the
// re-check is about ITS handle, not the active one.
func TestVerifiedCallback_PredecessorHandleRechecked(t *testing.T) {
	tenant := uuid.New()
	now := time.Now()
	active := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-new", Secret: make([]byte, 32), HandleID: uuid.New()}
	prev := Credential{TenantID: tenant, ProviderID: "vendor-a", KeyID: "k-old", Secret: make([]byte, 32), NotAfter: now.Add(time.Hour), HandleID: uuid.New()}
	runs := new(int)
	scheme := spyScheme{name: "x", props: SchemeProperties{Binding: BindingPerMerchantKey, KeySelection: KeyImplicit, SignedTimestamp: true, MaxSkew: time.Minute, Replay: ReplayTimestampWindow},
		verify: func(CredentialSet, Inbound, AuthMaterial, time.Time) (string, error) { return "k-old", nil }, verifyRuns: runs, lastCreds: new(CredentialSet)}
	in := Inbound{TenantID: tenant, ProviderID: "vendor-a", Header: nil, Body: []byte("x")}
	v, cred, authErr := VerifyAndSeal("casino", scheme, CredentialSet{Active: active, Previous: &prev}, CloneInbound(in), NewAuthMaterial("", nil), now)
	if authErr != nil || cred.HandleID != prev.HandleID {
		t.Fatalf("the predecessor verified: %v %v", cred, authErr)
	}
	r := &countingResolver{}
	if _, _, e := v.Redeem(context.Background(), fakeTx{}, "casino", tenant, "vendor-a", r); e != nil {
		t.Fatal(e)
	}
	if len(r.rechecks) != 1 || r.rechecks[0].HandleID != prev.HandleID {
		t.Fatalf("Recheck must be about the predecessor's handle %s, got %v", prev.HandleID, r.rechecks)
	}
}

// TestVerifiedCallback_BytesArePrivateCopy is security condition C3 at the
// unit level: mutating the caller's inbound after sealing never changes
// the bytes Redeem hands to HandleCallback.
func TestVerifiedCallback_BytesArePrivateCopy(t *testing.T) {
	f := newVerifiedFixture(t)
	orig := CloneInbound(f.in)
	callerIn := CloneInbound(f.in)
	v, _, authErr := VerifyAndSeal("casino", f.scheme, f.creds, CloneInbound(callerIn), f.m, time.Now())
	if authErr != nil {
		t.Fatal(authErr)
	}
	callerIn.Body[1] = 'X'
	callerIn.Header.Set("X-Evil", "1")
	in, _, e := v.Redeem(context.Background(), fakeTx{}, "casino", f.tenant, f.provider, &countingResolver{})
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(in.Body, orig.Body) || in.Header.Get("X-Evil") != "" {
		t.Fatal("the redeemed inbound must be exactly the verified copy")
	}
	// CloneInbound itself never aliases.
	c := CloneInbound(orig)
	c.Body[0] = 'Z'
	c.Header.Set("X-Other", "1")
	if orig.Body[0] == 'Z' || orig.Header.Get("X-Other") != "" {
		t.Fatal("CloneInbound aliased its input")
	}
}

// TestVerifiedCallback_Redacted is security condition C3/C15: no rendering
// path of a VerifiedCallback carries the secret, the body or a header.
func TestVerifiedCallback_Redacted(t *testing.T) {
	f := newVerifiedFixture(t)
	v := f.seal(t, "casino")
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "v", v, "vv", *v)
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	outs := []string{fmt.Sprintf("%v %+v %#v %s %x %q", v, v, v, v, v, v), fmt.Sprintf("%v %#v", *v, *v), string(j), logs.String()}
	for _, out := range outs {
		for _, bad := range []string{string(f.key), fmt.Sprintf("%x", f.key), string(f.in.Body), "amount", CasinoSignatureHeader} {
			if strings.Contains(out, bad) {
				t.Fatalf("a VerifiedCallback rendering leaked %q: %s", bad, out)
			}
		}
	}
	if !strings.Contains(outs[0], f.tenant.String()) {
		t.Fatalf("the redacted form must still identify the tenant: %s", outs[0])
	}
}
