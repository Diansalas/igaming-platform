package staffmfa

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ADR 0119 section 7.2 (slice S3): the session assurance seal.

func sealKeys(t *testing.T) (*Keys, []byte) {
	t.Helper()
	m1 := make([]byte, 32)
	for i := range m1 {
		m1[i] = byte(200 - i)
	}
	f1 := make([]byte, 32)
	for i := range f1 {
		f1[i] = byte(i + 3)
	}
	m0 := make([]byte, 32)
	for i := range m0 {
		m0[i] = byte(i * 5)
	}
	k, err := NewKeys("f1", map[string][]byte{"f1": f1}, "m1", map[string][]byte{"m1": m1, "m0": m0})
	if err != nil {
		t.Fatal(err)
	}
	return k, m1
}

func sampleAssurance() SessionAssurance {
	return SessionAssurance{
		SessionID:     uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		PrincipalType: "staff",
		PrincipalID:   uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		TenantID:      uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		AAL:           2, AMR: []string{"pwd", "otp"},
		AuthTime:          time.UnixMicro(1_800_000_000_123_456).UTC(),
		MFAAt:             time.UnixMicro(1_800_000_060_000_001).UTC(),
		AbsoluteExpiresAt: time.UnixMicro(1_800_043_200_123_456).UTC(),
	}
}

// The canonical form and the subkey are pinned against an independent computation.
func TestSealSession_CanonicalFormPinned(t *testing.T) {
	k, master := sealKeys(t)
	a := sampleAssurance()
	got, err := k.SealSession(a)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := hkdf.Key(sha256.New, master, nil, "staff-session-assurance-v1|m1", 32)
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, sub)
	m.Write([]byte("ss-v1|11111111-1111-4111-8111-111111111111|staff|22222222-2222-4222-8222-222222222222|33333333-3333-4333-8333-333333333333|2|pwd,otp|1800000000123456|1800000060000001|1800043200123456"))
	if got.KID != "m1" || !hmac.Equal(got.MAC, m.Sum(nil)) {
		t.Fatal("the seal is not HMAC(staff-session-assurance-v1 subkey, the ss-v1 canonical form)")
	}
	if !k.VerifySessionSeal(a, got) {
		t.Fatal("round trip")
	}
	// Platform staff: the tenant renders as "platform".
	a.TenantID = uuid.Nil
	p, _ := k.SealSession(a)
	m = hmac.New(sha256.New, sub)
	m.Write([]byte("ss-v1|11111111-1111-4111-8111-111111111111|staff|22222222-2222-4222-8222-222222222222|platform|2|pwd,otp|1800000000123456|1800000060000001|1800043200123456"))
	if !hmac.Equal(p.MAC, m.Sum(nil)) {
		t.Fatal("platform staff seal")
	}
}

func TestSealSession_EveryFieldIsBound(t *testing.T) {
	k, _ := sealKeys(t)
	base := sampleAssurance()
	seal, err := k.SealSession(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*SessionAssurance){
		"session":   func(a *SessionAssurance) { a.SessionID = uuid.New() },
		"principal": func(a *SessionAssurance) { a.PrincipalID = uuid.New() },
		"tenant":    func(a *SessionAssurance) { a.TenantID = uuid.New() },
		"platform":  func(a *SessionAssurance) { a.TenantID = uuid.Nil },
		"aal":       func(a *SessionAssurance) { a.AAL = 3 },
		"amr":       func(a *SessionAssurance) { a.AMR = []string{"pwd"} },
		"amr order": func(a *SessionAssurance) { a.AMR = []string{"otp", "pwd"} },
		"auth_time": func(a *SessionAssurance) { a.AuthTime = a.AuthTime.Add(time.Microsecond) },
		"mfa_at":    func(a *SessionAssurance) { a.MFAAt = a.MFAAt.Add(time.Microsecond) },
		"absolute":  func(a *SessionAssurance) { a.AbsoluteExpiresAt = a.AbsoluteExpiresAt.Add(time.Microsecond) },
	} {
		a := base
		a.AMR = append([]string(nil), base.AMR...)
		mut(&a)
		if k.VerifySessionSeal(a, seal) {
			t.Errorf("%s: a changed value must not verify", name)
		}
	}
	// A flipped MAC bit, a short MAC, an unknown kid, a kid swap.
	bad := KeyedHash{KID: seal.KID, MAC: append([]byte(nil), seal.MAC...)}
	bad.MAC[31] ^= 1
	if k.VerifySessionSeal(base, bad) || k.VerifySessionSeal(base, KeyedHash{KID: "m1", MAC: seal.MAC[:31]}) ||
		k.VerifySessionSeal(base, KeyedHash{KID: "zz", MAC: seal.MAC}) || k.VerifySessionSeal(base, KeyedHash{KID: "m0", MAC: seal.MAC}) {
		t.Fatal("tampered seal verified")
	}
	// A retained verify-only kid still verifies its own seals (MAC-family rotation, section 5.3).
	old, err := k.macRotationSeal(base, "m0")
	if err != nil || !k.VerifySessionSeal(base, old) {
		t.Fatalf("a seal under a retained kid must verify: %v", err)
	}
}

// macRotationSeal seals under a named (non-active) kid, as a session sealed before a rotation would be.
func (k *Keys) macRotationSeal(a SessionAssurance, kid string) (KeyedHash, error) {
	mac, err := k.sessionMAC(kid, a)
	return KeyedHash{KID: kid, MAC: mac}, err
}

func TestSealSession_RefusesMalformedInput(t *testing.T) {
	k, _ := sealKeys(t)
	for name, mut := range map[string]func(*SessionAssurance){
		"no session":     func(a *SessionAssurance) { a.SessionID = uuid.Nil },
		"no principal":   func(a *SessionAssurance) { a.PrincipalID = uuid.Nil },
		"player":         func(a *SessionAssurance) { a.PrincipalType = "player" },
		"aal 0":          func(a *SessionAssurance) { a.AAL = 0 },
		"no amr":         func(a *SessionAssurance) { a.AMR = nil },
		"amr with comma": func(a *SessionAssurance) { a.AMR = []string{"pwd,otp"} },
		"amr with pipe":  func(a *SessionAssurance) { a.AMR = []string{"pwd|x"} },
		"amr upper":      func(a *SessionAssurance) { a.AMR = []string{"PWD"} },
		"amr empty":      func(a *SessionAssurance) { a.AMR = []string{""} },
		"no auth_time":   func(a *SessionAssurance) { a.AuthTime = time.Time{} },
		"no mfa_at":      func(a *SessionAssurance) { a.MFAAt = time.Time{} },
		"no absolute":    func(a *SessionAssurance) { a.AbsoluteExpiresAt = time.Time{} },
	} {
		a := sampleAssurance()
		mut(&a)
		if _, err := k.SealSession(a); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: want ErrInvalidInput, got %v", name, err)
		}
		if k.VerifySessionSeal(a, KeyedHash{KID: "m1", MAC: make([]byte, 32)}) {
			t.Errorf("%s: malformed input must never verify", name)
		}
	}
}

func TestSessionTime_TruncatesToMicroseconds(t *testing.T) {
	in := time.Date(2026, 10, 13, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	got := SessionTime(in)
	if got.Location() != time.UTC || got.Nanosecond() != 123456000 || !got.Equal(in.Truncate(time.Microsecond)) {
		t.Fatalf("SessionTime: %v", got)
	}
}

// LoadKeys is the one constructor: it refuses an absent configuration in every environment, wraps a malformed one
// in ErrStartupGate, and returns keys that passed VerifyStartup. CheckKeyConfig returns no keys.
func TestLoadKeys_IsTheStartupGate(t *testing.T) {
	if _, err := LoadKeys("", "", "", ""); !errors.Is(err, ErrStartupGate) {
		t.Fatalf("absent keys: want ErrStartupGate, got %v", err)
	}
	if _, err := LoadKeys("f1", "f1:short", "m1", "m1:short"); !errors.Is(err, ErrStartupGate) || !errors.Is(err, ErrKeyConfig) {
		t.Fatalf("malformed keys: want ErrStartupGate wrapping ErrKeyConfig, got %v", err)
	}
	fk, mk := make([]byte, 32), make([]byte, 32)
	for i := range fk {
		fk[i], mk[i] = byte(i+7), byte(250-i)
	}
	f := "f1:" + b64(fk)
	m := "m1:" + b64(mk)
	k, err := LoadKeys("f1", f, "m1", m)
	if err != nil || k == nil || VerifyStartup(k) != nil {
		t.Fatalf("valid keys: %v", err)
	}
	if ok, err := CheckKeyConfig("", "", "", ""); ok || err != nil {
		t.Fatalf("CheckKeyConfig absent: %v %v", ok, err)
	}
	if ok, err := CheckKeyConfig("f1", f, "m1", m); !ok || err != nil {
		t.Fatalf("CheckKeyConfig valid: %v %v", ok, err)
	}
	if _, err := CheckKeyConfig("f1", f, "", ""); err == nil {
		t.Fatal("CheckKeyConfig must refuse a partial configuration")
	}
}
