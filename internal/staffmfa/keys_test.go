package staffmfa

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Keys in these tests are generated at runtime, never literals.

func randBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

type testFamilies struct {
	factor map[string][]byte
	mac    map[string][]byte
}

func newFamilies(t testing.TB) testFamilies {
	return testFamilies{
		factor: map[string][]byte{"f1": randBytes(t, 32)},
		mac:    map[string][]byte{"m1": randBytes(t, 32)},
	}
}

func mustKeys(t testing.TB, f testFamilies, factorKID, macKID string) *Keys {
	t.Helper()
	k, err := NewKeys(factorKID, f.factor, macKID, f.mac)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testKeys(t testing.TB) (*Keys, testFamilies) {
	f := newFamilies(t)
	return mustKeys(t, f, "f1", "m1"), f
}

func TestParseKeySet_Valid_AllBase64Forms(t *testing.T) {
	k1, k2, k3, k4 := randBytes(t, 32), randBytes(t, 33), randBytes(t, 40), randBytes(t, 64)
	raw := "a:" + base64.StdEncoding.EncodeToString(k1) +
		", b:" + base64.URLEncoding.EncodeToString(k2) +
		",c:" + base64.RawStdEncoding.EncodeToString(k3) +
		",d.1_x-y:" + base64.RawURLEncoding.EncodeToString(k4)
	got, err := ParseKeySet(EnvMACKeys, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || !bytes.Equal(got["a"], k1) || !bytes.Equal(got["b"], k2) || !bytes.Equal(got["c"], k3) || !bytes.Equal(got["d.1_x-y"], k4) {
		t.Fatal("parsed keys differ")
	}
}

func TestParseKeySet_FailsClosed_WithoutEchoingSecrets(t *testing.T) {
	good := b64(randBytes(t, 32))
	short := b64(randBytes(t, 31))
	huge := b64(randBytes(t, 1025))
	dupVal := b64(randBytes(t, 32))
	cases := map[string]string{
		"empty":             "",
		"blank":             "   ",
		"empty entry":       "a:" + good + ",,b:" + b64(randBytes(t, 32)),
		"trailing comma":    "a:" + good + ",",
		"no kid":            ":" + good,
		"no colon":          good,
		"bad kid char":      "a/b:" + good,
		"kid too long":      strings.Repeat("k", 33) + ":" + good,
		"not base64":        "a:not base64!",
		"bad padding":       "a:" + good + "=",
		"short":             "a:" + short,
		"too long":          "a:" + huge,
		"duplicate kid":     "a:" + good + ",a:" + b64(randBytes(t, 32)),
		"duplicate value":   "a:" + dupVal + ",b:" + dupVal,
		"whitespace inside": "a: " + good,
		"all zero":          "a:" + b64(make([]byte, 32)),
		"all one byte":      "a:" + b64(bytes.Repeat([]byte{0xA5}, 48)),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKeySet(EnvFactorKeys, raw)
			if !errors.Is(err, ErrKeyConfig) {
				t.Fatalf("want ErrKeyConfig, got %v", err)
			}
			for _, secret := range []string{good, short, dupVal, huge[:40]} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error echoes key material")
				}
			}
			if !strings.Contains(err.Error(), EnvFactorKeys) {
				t.Fatalf("error must name the family: %v", err)
			}
		})
	}
}

func TestNewKeys_Refusals(t *testing.T) {
	f := newFamilies(t)
	shared := randBytes(t, 32)
	cases := []struct {
		name           string
		fkid, mkid     string
		factor, mac    map[string][]byte
		wantSubstrings string
	}{
		{"no factor family", "f1", "m1", nil, f.mac, "required"},
		{"no mac family", "f1", "m1", f.factor, nil, "required"},
		{"empty factor active kid", "", "m1", f.factor, f.mac, EnvFactorActiveKID},
		{"empty mac active kid", "f1", "", f.factor, f.mac, EnvMACActiveKID},
		{"malformed active kid", "f 1", "m1", f.factor, f.mac, EnvFactorActiveKID},
		{"unknown factor active kid", "f9", "m1", f.factor, f.mac, EnvFactorActiveKID},
		{"unknown mac active kid", "f1", "m9", f.factor, f.mac, EnvMACActiveKID},
		{"short factor key", "f1", "m1", map[string][]byte{"f1": randBytes(t, 31)}, f.mac, EnvFactorKeys},
		{"short mac key", "f1", "m1", f.factor, map[string][]byte{"m1": randBytes(t, 16)}, EnvMACKeys},
		{"malformed kid in map", "f1", "m1", map[string][]byte{"f1": randBytes(t, 32), "bad kid": randBytes(t, 32)}, f.mac, "malformed"},
		{"same key across families", "f1", "m1", map[string][]byte{"f1": shared}, map[string][]byte{"m1": shared}, "same value"},
		{"same key across kids", "f1", "m1", map[string][]byte{"f1": shared, "f2": shared}, f.mac, "same value"},
		{"degenerate factor key", "f1", "m1", map[string][]byte{"f1": make([]byte, 32)}, f.mac, "degenerate"},
		{"degenerate mac key", "f1", "m1", f.factor, map[string][]byte{"m1": bytes.Repeat([]byte{7}, 32)}, "degenerate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, err := NewKeys(tc.fkid, tc.factor, tc.mkid, tc.mac)
			if k != nil || !errors.Is(err, ErrKeyConfig) || !strings.Contains(err.Error(), tc.wantSubstrings) {
				t.Fatalf("got %v, %v", k, err)
			}
		})
	}
}

func TestNewKeysFromConfig_AbsentPartialAndValid(t *testing.T) {
	if k, err := NewKeysFromConfig("", "", "", ""); k != nil || err != nil {
		t.Fatalf("all absent must be (nil, nil), got %v, %v", k, err)
	}
	fk, mk := "f1:"+b64(randBytes(t, 32)), "m1:"+b64(randBytes(t, 32))
	partial := [][4]string{
		{"f1", fk, "", ""},
		{"", "", "m1", mk},
		{"f1", "", "m1", mk},
		{"f1", fk, "m1", ""},
		{"", fk, "m1", mk},
		{"f1", fk, "", mk},
	}
	for i, p := range partial {
		if k, err := NewKeysFromConfig(p[0], p[1], p[2], p[3]); k != nil || err == nil {
			t.Fatalf("partial config %d must be refused", i)
		}
	}
	k, err := NewKeysFromConfig("f1", fk, "m1", mk)
	if err != nil || k == nil || k.FactorKID() != "f1" || k.MACKID() != "m1" {
		t.Fatalf("valid config: %v, %v", k, err)
	}
}

// The HKDF info is label || "|" || kid (ADR 0119 section 5.2): pinned against
// an independent computation.
func TestSubkeys_DerivationPinned(t *testing.T) {
	k, f := testKeys(t)
	want := func(master []byte, label, kid string) []byte {
		b, err := hkdf.Key(sha256.New, master, nil, label+"|"+kid, 32)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(k.m().factor["f1"], want(f.factor["f1"], "staff-mfa-factor-aead-v1", "f1")) {
		t.Fatal("factor subkey derivation changed")
	}
	s := k.m().mac["m1"]
	for label, got := range map[string][]byte{
		"staff-mfa-recovery-v1":      s.recovery,
		"staff-auth-challenge-v1":    s.challenge,
		"staff-enrolment-token-v1":   s.enrolment,
		"staff-session-assurance-v1": s.session,
	} {
		if !bytes.Equal(got, want(f.mac["m1"], label, "m1")) {
			t.Fatalf("subkey %s derivation changed", label)
		}
	}
}

func TestSubkeys_PairwiseDistinct_AcrossLabelsAndKids(t *testing.T) {
	master := randBytes(t, 32)
	var all [][]byte
	for _, kid := range []string{"a", "b"} {
		for _, label := range []string{labelFactorAEAD, labelRecovery, labelChallenge, labelEnrolmentToken, labelSessionAssurance} {
			sk, err := deriveSubkey(master, label, kid)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, sk)
		}
	}
	for i := range all {
		if bytes.Equal(all[i], master) {
			t.Fatal("a subkey equals the master")
		}
		for j := i + 1; j < len(all); j++ {
			if bytes.Equal(all[i], all[j]) {
				t.Fatalf("subkeys %d and %d are equal", i, j)
			}
		}
	}
}

// Only derived subkeys are retained: no field of Keys holds a master key.
func TestKeys_DoNotRetainMasterKeys(t *testing.T) {
	k, f := testKeys(t)
	masters := [][]byte{f.factor["f1"], f.mac["m1"]}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		case reflect.Map:
			for _, key := range v.MapKeys() {
				walk(v.MapIndex(key))
			}
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				for _, m := range masters {
					if bytes.Equal(v.Bytes(), m) {
						t.Fatal("Keys retains a master key")
					}
				}
			}
		}
	}
	walk(reflect.ValueOf(k.m()).Elem())
	if len(k.m().factor) == 0 || len(k.m().mac) == 0 {
		t.Fatal("walk is vacuous")
	}
}

func TestMACKIDs_ActiveFirstThenSorted(t *testing.T) {
	f := newFamilies(t)
	f.mac["m0"], f.mac["m2"] = randBytes(t, 32), randBytes(t, 32)
	k := mustKeys(t, f, "f1", "m2")
	if got := strings.Join(k.MACKIDs(), ","); got != "m2,m0,m1" {
		t.Fatalf("MACKIDs = %s", got)
	}
}

// ---- AEAD ---------------------------------------------------------------------

func testAAD() FactorAAD {
	return FactorAAD{FactorID: uuid.New(), StaffUserID: uuid.New(), TenantID: uuid.New(), FactorType: FactorTOTP}
}

func mustSecret(t testing.TB) FactorSecret {
	s, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFactorAAD_FormatPinned(t *testing.T) {
	f, s, tn := uuid.MustParse("11111111-1111-4111-8111-111111111111"), uuid.MustParse("22222222-2222-4222-8222-222222222222"), uuid.MustParse("33333333-3333-4333-8333-333333333333")
	b, err := FactorAAD{FactorID: f, StaffUserID: s, TenantID: tn, FactorType: FactorTOTP}.bytes()
	if err != nil || string(b) != "sf-v1|11111111-1111-4111-8111-111111111111|22222222-2222-4222-8222-222222222222|33333333-3333-4333-8333-333333333333|totp" {
		t.Fatalf("tenant AAD = %q, %v", b, err)
	}
	b, err = FactorAAD{FactorID: f, StaffUserID: s, FactorType: FactorTOTP}.bytes()
	if err != nil || string(b) != "sf-v1|11111111-1111-4111-8111-111111111111|22222222-2222-4222-8222-222222222222|platform|totp" {
		t.Fatalf("platform AAD = %q, %v", b, err)
	}
	for _, bad := range []FactorAAD{
		{StaffUserID: s, FactorType: FactorTOTP},
		{FactorID: f, FactorType: FactorTOTP},
		{FactorID: f, StaffUserID: s},
		{FactorID: f, StaffUserID: s, FactorType: "webauthn"},
	} {
		if _, err := bad.bytes(); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("AAD %+v must be refused", bad)
		}
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	k, _ := testKeys(t)
	aad, secret := testAAD(), mustSecret(t)
	sealed, err := k.SealFactorSecret(aad, secret)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.KID != "f1" || len(sealed.Nonce) != NonceBytes || len(sealed.Ciphertext) != 20+16 {
		t.Fatalf("sealed shape: kid=%s nonce=%d ct=%d", sealed.KID, len(sealed.Nonce), len(sealed.Ciphertext))
	}
	if bytes.Contains(sealed.Ciphertext, secret.d().b) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := k.OpenFactorSecret(aad, sealed)
	if err != nil || !bytes.Equal(got.d().b, secret.d().b) {
		t.Fatalf("open: %v", err)
	}
	// Platform staff (nil tenant).
	aad.TenantID = uuid.Nil
	sealed, err = k.SealFactorSecret(aad, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.OpenFactorSecret(aad, sealed); err != nil {
		t.Fatal(err)
	}
}

func TestSeal_FreshNonceEachTime_AndDeterministicWithInjectedReader(t *testing.T) {
	k, _ := testKeys(t)
	aad, secret := testAAD(), mustSecret(t)
	a, _ := k.SealFactorSecret(aad, secret)
	b, _ := k.SealFactorSecret(aad, secret)
	if bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("nonce reuse")
	}
	k.m().rnd = bytes.NewReader(bytes.Repeat([]byte{7}, NonceBytes))
	c, err := k.SealFactorSecret(aad, secret)
	if err != nil || !bytes.Equal(c.Nonce, bytes.Repeat([]byte{7}, NonceBytes)) {
		t.Fatalf("injected reader not used: %v", err)
	}
	// An exhausted random source fails closed.
	if _, err := k.SealFactorSecret(aad, secret); err == nil {
		t.Fatal("seal with a failing random source must fail")
	}
}

func TestSeal_RefusesBadInput(t *testing.T) {
	k, _ := testKeys(t)
	if _, err := k.SealFactorSecret(FactorAAD{}, mustSecret(t)); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("empty AAD must be refused")
	}
	if _, err := k.SealFactorSecret(testAAD(), newFactorSecret(randBytes(t, 19))); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("a wrong-length secret must be refused")
	}
	if _, err := k.SealFactorSecret(testAAD(), FactorSecret{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("an empty secret must be refused")
	}
}

// Tamper tests for the AAD binding (ADR 0119 sections 5.3 and 17.6): moved or
// rewritten ciphertext never opens.
func TestOpen_AADTamper_EachFieldBinds(t *testing.T) {
	k, _ := testKeys(t)
	aad, secret := testAAD(), mustSecret(t)
	sealed, err := k.SealFactorSecret(aad, secret)
	if err != nil {
		t.Fatal(err)
	}
	platform := aad
	platform.TenantID = uuid.Nil
	mutations := map[string]FactorAAD{
		"other factor id":         {FactorID: uuid.New(), StaffUserID: aad.StaffUserID, TenantID: aad.TenantID, FactorType: aad.FactorType},
		"other staff (moved row)": {FactorID: aad.FactorID, StaffUserID: uuid.New(), TenantID: aad.TenantID, FactorType: aad.FactorType},
		"other tenant":            {FactorID: aad.FactorID, StaffUserID: aad.StaffUserID, TenantID: uuid.New(), FactorType: aad.FactorType},
		"tenant row as platform":  platform,
	}
	for name, m := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := k.OpenFactorSecret(m, sealed); !errors.Is(err, ErrFactorIntegrity) {
				t.Fatalf("want ErrFactorIntegrity, got %v", err)
			}
		})
	}
	// And the reverse: a platform row presented as a tenant row.
	ps, _ := k.SealFactorSecret(platform, secret)
	if _, err := k.OpenFactorSecret(aad, ps); !errors.Is(err, ErrFactorIntegrity) {
		t.Fatal("platform ciphertext opened under a tenant AAD")
	}
	// An invalid factor type is an input error, not an open.
	bad := aad
	bad.FactorType = "webauthn"
	if _, err := k.OpenFactorSecret(bad, sealed); !errors.Is(err, ErrInvalidInput) || !errors.Is(err, ErrFactorIntegrity) {
		t.Fatal("invalid factor type must be refused as an integrity failure")
	}
	// A nil-id AAD is also an integrity failure, never "factor absent".
	for _, nilAAD := range []FactorAAD{{}, {FactorID: aad.FactorID, FactorType: FactorTOTP}, {StaffUserID: aad.StaffUserID, FactorType: FactorTOTP}} {
		if _, err := k.OpenFactorSecret(nilAAD, sealed); !errors.Is(err, ErrFactorIntegrity) {
			t.Fatalf("nil AAD %+v must be an integrity failure, got %v", nilAAD, err)
		}
	}
}

func TestOpen_CiphertextTamper(t *testing.T) {
	k, _ := testKeys(t)
	aad := testAAD()
	sealed, err := k.SealFactorSecret(aad, mustSecret(t))
	if err != nil {
		t.Fatal(err)
	}
	flip := func(b []byte, i int) []byte { c := append([]byte(nil), b...); c[i] ^= 1; return c }
	cases := map[string]SealedSecret{
		"ct first byte":   {KID: sealed.KID, Nonce: sealed.Nonce, Ciphertext: flip(sealed.Ciphertext, 0)},
		"ct tag byte":     {KID: sealed.KID, Nonce: sealed.Nonce, Ciphertext: flip(sealed.Ciphertext, len(sealed.Ciphertext)-1)},
		"ct truncated":    {KID: sealed.KID, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext[:len(sealed.Ciphertext)-1]},
		"ct empty":        {KID: sealed.KID, Nonce: sealed.Nonce},
		"nonce flipped":   {KID: sealed.KID, Nonce: flip(sealed.Nonce, 0), Ciphertext: sealed.Ciphertext},
		"nonce short":     {KID: sealed.KID, Nonce: sealed.Nonce[:11], Ciphertext: sealed.Ciphertext},
		"nonce long":      {KID: sealed.KID, Nonce: append(append([]byte(nil), sealed.Nonce...), 0), Ciphertext: sealed.Ciphertext},
		"nonce nil":       {KID: sealed.KID, Ciphertext: sealed.Ciphertext},
		"kid empty":       {Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext},
		"kid unknown":     {KID: "f9", Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext},
		"attacker sealed": attackerSeal(t, aad),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := k.OpenFactorSecret(aad, s); !errors.Is(err, ErrFactorIntegrity) {
				t.Fatalf("want ErrFactorIntegrity, got %v", err)
			}
		})
	}
	if _, err := k.OpenFactorSecret(aad, cases["kid unknown"]); !errors.Is(err, ErrUnknownKID) {
		t.Fatal("an unknown kid must also match ErrUnknownKID")
	}
}

// attackerSeal is a planted factor (ADR 0119 section 17.6): a well-formed row
// sealed under a key the platform does not hold, with the platform's kid.
func attackerSeal(t *testing.T, aad FactorAAD) SealedSecret {
	other, _ := testKeys(t)
	s, err := other.SealFactorSecret(aad, mustSecret(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpen_WrongPlaintextLengthIsIntegrity(t *testing.T) {
	k, _ := testKeys(t)
	aad := testAAD()
	ad, _ := aad.bytes()
	gcm, err := newGCM(k.m().factor["f1"])
	if err != nil {
		t.Fatal(err)
	}
	nonce := randBytes(t, NonceBytes)
	for _, n := range []int{0, 19, 21, 32} {
		ct := gcm.Seal(nil, nonce, randBytes(t, n), ad)
		if _, err := k.OpenFactorSecret(aad, SealedSecret{KID: "f1", Nonce: nonce, Ciphertext: ct}); !errors.Is(err, ErrFactorIntegrity) {
			t.Fatalf("plaintext of %d bytes must fail integrity, got %v", n, err)
		}
	}
}

// Rotation of the FACTOR family (ADR 0119 section 5.3): a new active kid seals
// new factors; old rows keep opening while the old kid is retained; once the
// old kid is removed (retired) they fail closed.
func TestFactorRotation_ByKID(t *testing.T) {
	f := newFamilies(t)
	old := mustKeys(t, f, "f1", "m1")
	aad, secret := testAAD(), mustSecret(t)
	oldSealed, err := old.SealFactorSecret(aad, secret)
	if err != nil {
		t.Fatal(err)
	}
	f.factor["f2"] = randBytes(t, 32)
	rotated := mustKeys(t, f, "f2", "m1")
	if got, err := rotated.OpenFactorSecret(aad, oldSealed); err != nil || !bytes.Equal(got.d().b, secret.d().b) {
		t.Fatalf("old row under a retained kid must open: %v", err)
	}
	newSealed, err := rotated.SealFactorSecret(aad, secret)
	if err != nil || newSealed.KID != "f2" {
		t.Fatalf("new seals must use the new active kid: %s, %v", newSealed.KID, err)
	}
	// Ciphertext relabelled with the other kid does not open.
	relabelled := oldSealed
	relabelled.KID = "f2"
	if _, err := rotated.OpenFactorSecret(aad, relabelled); !errors.Is(err, ErrFactorIntegrity) {
		t.Fatal("a relabelled kid must not open")
	}
	delete(f.factor, "f1")
	retired := mustKeys(t, f, "f2", "m1")
	if _, err := retired.OpenFactorSecret(aad, oldSealed); !errors.Is(err, ErrFactorIntegrity) || !errors.Is(err, ErrUnknownKID) {
		t.Fatalf("a retired kid must be refused, got %v", err)
	}
	if _, err := retired.OpenFactorSecret(aad, newSealed); err != nil {
		t.Fatal(err)
	}
}

// The same master under two different kids yields different subkeys, so a
// kid relabel of a ciphertext fails even when the master is reused across
// environments by mistake.
func TestFactorKID_BoundIntoSubkey(t *testing.T) {
	master := randBytes(t, 32)
	a := mustKeys(t, testFamilies{factor: map[string][]byte{"a": master}, mac: map[string][]byte{"m": randBytes(t, 32)}}, "a", "m")
	b := mustKeys(t, testFamilies{factor: map[string][]byte{"b": master}, mac: map[string][]byte{"m": randBytes(t, 32)}}, "b", "m")
	aad := testAAD()
	s, _ := a.SealFactorSecret(aad, mustSecret(t))
	s.KID = "b"
	if _, err := b.OpenFactorSecret(aad, s); !errors.Is(err, ErrFactorIntegrity) {
		t.Fatal("the kid must be bound into the subkey")
	}
}

// ---- keyed hashes ---------------------------------------------------------------

func TestMAC_FormatPinned(t *testing.T) {
	k, f := testKeys(t)
	staff := uuid.New()
	value := []byte("value-bytes")
	cases := []struct {
		kind    MACKind
		label   string
		purpose string
	}{
		{MACRecoveryCode, labelRecovery, "recovery"},
		{MACChallengeMFA, labelChallenge, "mfa"},
		{MACChallengeEnrol, labelChallenge, "enrol"},
		{MACEnrolInvite, labelEnrolmentToken, "invite"},
		{MACEnrolMFAReset, labelEnrolmentToken, "mfa_reset"},
		{MACEnrolPasswordReset, labelEnrolmentToken, "password_reset"},
		{MACEnrolBootstrap, labelEnrolmentToken, "bootstrap"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		sub, _ := hkdf.Key(sha256.New, f.mac["m1"], nil, tc.label+"|m1", 32)
		m := hmac.New(sha256.New, sub)
		m.Write([]byte(tc.purpose + "|" + staff.String() + "|"))
		m.Write(value)
		want := m.Sum(nil)
		got, err := k.macUnder("m1", tc.kind, staff, value)
		if err != nil || !bytes.Equal(got, want) || len(got) != MACBytes {
			t.Fatalf("kind %s: MAC format changed", tc.purpose)
		}
		if seen[string(got)] {
			t.Fatalf("kind %s: MAC collides with another kind", tc.purpose)
		}
		seen[string(got)] = true
	}
}

func TestMAC_RefusesInvalidInput(t *testing.T) {
	k, _ := testKeys(t)
	if _, err := k.macUnder("m1", MACKind(0), uuid.New(), []byte("v")); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero kind")
	}
	if _, err := k.macUnder("m1", MACKind(99), uuid.New(), []byte("v")); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("unknown kind")
	}
	if _, err := k.macUnder("m1", MACRecoveryCode, uuid.Nil, []byte("v")); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("nil staff")
	}
	if _, err := k.macUnder("m1", MACRecoveryCode, uuid.New(), nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("empty value")
	}
	if _, err := k.macUnder("m9", MACRecoveryCode, uuid.New(), []byte("v")); !errors.Is(err, ErrUnknownKID) {
		t.Fatal("unknown kid")
	}
	if k.verifyUnder("m1", MACRecoveryCode, uuid.New(), []byte("v"), make([]byte, 31)) {
		t.Fatal("short stored MAC must not verify")
	}
}
