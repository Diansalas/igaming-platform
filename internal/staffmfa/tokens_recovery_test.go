package staffmfa

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ---- tokens ---------------------------------------------------------------------

func mustToken(t testing.TB, staff uuid.UUID) Token {
	t.Helper()
	tok, err := NewToken(staff)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestToken_WireRoundTrip(t *testing.T) {
	staff := uuid.New()
	tok := mustToken(t, staff)
	wire := tok.RevealForDelivery()
	if len(wire) != 64 || TokenLen != 64 || TokenLen != base64.RawURLEncoding.EncodedLen(16+tokenSecretBytes) {
		t.Fatalf("wire length %d", len(wire))
	}
	got, err := ParseToken(wire)
	if err != nil || got.StaffID() != staff || !bytes.Equal(got.d().secret, tok.d().secret) {
		t.Fatalf("round trip: %v", err)
	}
	if other := mustToken(t, staff); other.RevealForDelivery() == wire {
		t.Fatal("two tokens are equal")
	}
	if (Token{}).RevealForDelivery() != "" {
		t.Fatal("a zero token must not render")
	}
	if _, err := NewToken(uuid.Nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("a token for a nil staff id must be refused")
	}
}

func TestParseToken_FailsClosed(t *testing.T) {
	wire := mustToken(t, uuid.New()).RevealForDelivery()
	nilStaff := base64.RawURLEncoding.EncodeToString(append(make([]byte, 16), randBytes(t, 32)...))
	cases := map[string]string{
		"empty":          "",
		"short":          wire[:63],
		"long":           wire + "A",
		"padded":         wire[:62] + "==",
		"std alphabet":   strings.Replace(strings.Replace(wire, "-", "+", 1), "_", "/", 1) + strings.Repeat("+", 0),
		"space":          wire[:63] + " ",
		"non-ascii":      wire[:62] + "é",
		"nil staff":      nilStaff,
		"illegal char":   wire[:63] + "*",
		"newline suffix": wire[:63] + "\n",
	}
	for name, s := range cases {
		if name == "std alphabet" && !strings.ContainsAny(s, "+/") {
			continue // the random token happened to contain neither '-' nor '_'
		}
		t.Run(name, func(t *testing.T) {
			if _, err := ParseToken(s); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
			if s != "" {
				if _, err := ParseToken(s); err != nil && strings.Contains(err.Error(), s) {
					t.Fatal("error echoes the token")
				}
			}
		})
	}
}

func TestTokenHash_BindsKindStaffAndKey(t *testing.T) {
	k, _ := testKeys(t)
	staff := uuid.New()
	tok := mustToken(t, staff)
	h, err := k.HashToken(MACChallengeMFA, tok)
	if err != nil || h.KID != "m1" || len(h.MAC) != MACBytes {
		t.Fatalf("hash: %v", err)
	}
	if !k.VerifyTokenHash(MACChallengeMFA, tok, staff, h) {
		t.Fatal("own row must verify")
	}
	// An enrol challenge is not an mfa challenge, and the reverse.
	if k.VerifyTokenHash(MACChallengeEnrol, tok, staff, h) {
		t.Fatal("an mfa challenge MAC verified as enrol")
	}
	he, _ := k.HashToken(MACChallengeEnrol, tok)
	if bytes.Equal(he.MAC, h.MAC) || k.VerifyTokenHash(MACChallengeMFA, tok, staff, he) {
		t.Fatal("an enrol challenge MAC verified as mfa")
	}
	// A row moved to another staff member does not verify.
	if k.VerifyTokenHash(MACChallengeMFA, tok, uuid.New(), h) {
		t.Fatal("a row of another staff member verified")
	}
	// A token re-labelled with another staff id (same secret) hashes differently.
	moved := newTokenValue(tokenData{staff: uuid.New(), secret: tok.d().secret})
	if mh, _ := k.HashToken(MACChallengeMFA, moved); bytes.Equal(mh.MAC, h.MAC) {
		t.Fatal("the staff id is not bound into the MAC")
	}
	// Tampered MAC, short MAC, unknown kid.
	bad := KeyedHash{KID: h.KID, MAC: append([]byte(nil), h.MAC...)}
	bad.MAC[0] ^= 1
	if k.VerifyTokenHash(MACChallengeMFA, tok, staff, bad) {
		t.Fatal("tampered MAC verified")
	}
	if k.VerifyTokenHash(MACChallengeMFA, tok, staff, KeyedHash{KID: "m1", MAC: h.MAC[:31]}) {
		t.Fatal("short MAC verified")
	}
	if k.VerifyTokenHash(MACChallengeMFA, tok, staff, KeyedHash{KID: "m9", MAC: h.MAC}) {
		t.Fatal("unknown kid verified")
	}
	// A planted row: a MAC computed under a key the platform does not hold.
	other, _ := testKeys(t)
	planted, _ := other.HashToken(MACChallengeMFA, tok)
	if k.VerifyTokenHash(MACChallengeMFA, tok, staff, planted) {
		t.Fatal("a planted MAC verified")
	}
	// Invalid kinds and zero tokens fail closed.
	for _, kind := range []MACKind{0, MACRecoveryCode, 42} {
		if _, err := k.HashToken(kind, tok); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("kind %d must be refused", kind)
		}
		if _, err := k.TokenLookupHashes(kind, tok); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("lookup kind %d must be refused", kind)
		}
		if k.VerifyTokenHash(kind, tok, staff, h) {
			t.Fatalf("kind %d verified", kind)
		}
	}
	if _, err := k.HashToken(MACChallengeMFA, Token{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero token")
	}
	if k.VerifyTokenHash(MACChallengeMFA, Token{}, uuid.Nil, h) {
		t.Fatal("zero token verified")
	}
}

func TestEnrolmentTokenKinds_AreSeparated(t *testing.T) {
	k, _ := testKeys(t)
	staff := uuid.New()
	tok := mustToken(t, staff)
	kinds := []MACKind{MACEnrolInvite, MACEnrolMFAReset, MACEnrolPasswordReset, MACEnrolBootstrap, MACChallengeMFA, MACChallengeEnrol}
	for _, a := range kinds {
		h, err := k.HashToken(a, tok)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range kinds {
			if got := k.VerifyTokenHash(b, tok, staff, h); got != (a == b) {
				t.Fatalf("kind %d hash verified as kind %d: %v", a, b, got)
			}
		}
	}
}

// Rotation of the MAC family (ADR 0119 section 5.3): rows written under the
// old kid keep verifying while it is retained (verify-only) and are found by a
// lookup; once the kid is retired they are never found.
func TestMACRotation_LookupAndRetirement(t *testing.T) {
	f := newFamilies(t)
	old := mustKeys(t, f, "f1", "m1")
	staff := uuid.New()
	tok := mustToken(t, staff)
	code := mustBatch(t).codes[0]
	oldTok, _ := old.HashToken(MACEnrolInvite, tok)
	oldCode, _ := old.HashRecoveryCode(staff, code)

	f.mac["m2"] = randBytes(t, 32)
	rotated := mustKeys(t, f, "f1", "m2")
	newTok, _ := rotated.HashToken(MACEnrolInvite, tok)
	if newTok.KID != "m2" || bytes.Equal(newTok.MAC, oldTok.MAC) {
		t.Fatal("new hashes must use the new active kid")
	}
	if !rotated.VerifyTokenHash(MACEnrolInvite, tok, staff, oldTok) || !rotated.VerifyRecoveryCodeHash(staff, code, oldCode) {
		t.Fatal("old rows must verify under a retained kid")
	}
	lh, err := rotated.TokenLookupHashes(MACEnrolInvite, tok)
	if err != nil || len(lh) != 2 || lh[0].KID != "m2" || lh[1].KID != "m1" || !bytes.Equal(lh[1].MAC, oldTok.MAC) || !bytes.Equal(lh[0].MAC, newTok.MAC) {
		t.Fatalf("lookup hashes: %v", err)
	}
	ch, err := rotated.RecoveryCodeLookupHashes(staff, code)
	if err != nil || len(ch) != 2 || !bytes.Equal(ch[1].MAC, oldCode.MAC) {
		t.Fatalf("recovery lookup hashes: %v", err)
	}

	delete(f.mac, "m1")
	retired := mustKeys(t, f, "f1", "m2")
	if retired.VerifyTokenHash(MACEnrolInvite, tok, staff, oldTok) || retired.VerifyRecoveryCodeHash(staff, code, oldCode) {
		t.Fatal("a retired kid must be refused")
	}
	lh, _ = retired.TokenLookupHashes(MACEnrolInvite, tok)
	for _, h := range lh {
		if bytes.Equal(h.MAC, oldTok.MAC) {
			t.Fatal("a lookup still finds the retired-kid row")
		}
	}
}

// ---- recovery codes ---------------------------------------------------------------

func mustBatch(t testing.TB) RecoveryCodeBatch {
	t.Helper()
	b, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecoveryCodes_Generation(t *testing.T) {
	b := mustBatch(t)
	codes := b.Codes()
	if len(codes) != 10 || RecoveryCodesPerBatch != 10 || RecoveryCodeChars != 10 {
		t.Fatalf("batch size %d", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c.d().v) != 10 || seen[c.d().v] {
			t.Fatalf("bad or duplicate code")
		}
		seen[c.d().v] = true
		for i := 0; i < len(c.d().v); i++ {
			if !strings.ContainsRune(crockford, rune(c.d().v[i])) {
				t.Fatal("code outside the Crockford alphabet")
			}
		}
		d := c.RevealForDisplay()
		if len(d) != 11 || d[5] != '-' || d[:5]+d[6:] != c.d().v {
			t.Fatalf("display form %q", "redacted-in-test")
		}
		n, err := NormaliseRecoveryCode(d)
		if err != nil || n.d().v != c.d().v {
			t.Fatal("display form must normalise back")
		}
	}
	// Codes() returns a copy.
	codes[0] = RecoveryCode{}
	if b.Codes()[0].d().v == "" {
		t.Fatal("Codes must return a copy")
	}
	if (RecoveryCode{}).RevealForDisplay() != "" {
		t.Fatal("zero code must not render")
	}
}

func TestRecoveryCodes_DeterministicWithInjectedReader(t *testing.T) {
	src := make([]byte, 100)
	for i := range src {
		src[i] = byte(i)
	}
	b, err := generateRecoveryCodes(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	// Byte i maps to crockford[i & 31].
	if b.codes[0].d().v != "0123456789" || b.codes[1].d().v != "ABCDEFGHJK" || b.codes[2].d().v != "MNPQRSTVWX" || b.codes[3].d().v != "YZ01234567" {
		t.Fatal("generation mapping changed")
	}
	// A short random source fails closed.
	if _, err := generateRecoveryCodes(bytes.NewReader(src[:50])); err == nil {
		t.Fatal("an exhausted source must fail")
	}
	// A repeating source cannot loop forever.
	if _, err := generateRecoveryCodes(bytes.NewReader(make([]byte, 10_000))); err == nil {
		t.Fatal("a constant source must fail (duplicates)")
	}
	// A duplicate inside the batch is skipped, not returned.
	dup := append(append([]byte{}, src[:10]...), src[:100]...)
	b, err = generateRecoveryCodes(bytes.NewReader(dup))
	if err != nil {
		t.Fatal(err)
	}
	if b.codes[0].d().v == b.codes[1].d().v {
		t.Fatal("duplicate code in a batch")
	}
}

func TestNormaliseRecoveryCode(t *testing.T) {
	ok := map[string]string{
		"ABCDE-FGHJK":          "ABCDEFGHJK",
		"abcde-fghjk":          "ABCDEFGHJK",
		"ABCDEFGHJK":           "ABCDEFGHJK",
		" abcde fghjk ":        "ABCDEFGHJK",
		"a-b-c-d-e-f-g-h-j-k":  "ABCDEFGHJK",
		"IiLl0-OoQRS":          "1111000QRS",
		"01234-56789":          "0123456789",
		"zzzzz-zzzzz":          "ZZZZZZZZZZ",
		"--ABCDE--FGHJK--":     "ABCDEFGHJK",
		"ab cd e-fg hj k     ": "ABCDEFGHJK",
		"VWXYZ-TSRQP":          "VWXYZTSRQP",
		"mnpqr-stvwx":          "MNPQRSTVWX",
		"MNPQR STVWX":          "MNPQRSTVWX",
		"1111111111":           "1111111111",
		"iiiiilllll":           "1111111111",
		"ooooo00000":           "0000000000",
		"ABCDE-FGHJK-":         "ABCDEFGHJK",
		"-ABCDE-FGHJK":         "ABCDEFGHJK",
		"ab-cd-ef-gh-jk":       "ABCDEFGHJK",
		"a b c d e f g h j k ": "ABCDEFGHJK",
	}
	for in, want := range ok {
		got, err := NormaliseRecoveryCode(in)
		if err != nil || got.d().v != want {
			t.Fatalf("normalise(%q) = %q, %v; want %q", in, got.d().v, err, want)
		}
	}
	bad := []string{
		"", "ABCDE-FGHJ", "ABCDE-FGHJKM", "ABCDE-FGHJU", "ABCDE_FGHJK", "ABCDE.FGHJK",
		"ABCDE\tFGHJK", "ABCDE-FGHJË", "ABCDE-FGHJı", "ABCDE-FGHJK", "ABCDE+FGHJK",
		strings.Repeat("-", 65), strings.Repeat("A", 11), "ABCDE-FGHJK\x00",
	}
	for _, in := range bad {
		got, err := NormaliseRecoveryCode(in)
		if !errors.Is(err, ErrInvalidInput) || got.d().v != "" {
			t.Fatalf("normalise(%q) must fail closed, got %q, %v", in, got.d().v, err)
		}
		if in != "" && strings.Contains(err.Error(), in) {
			t.Fatal("error echoes the code")
		}
	}
	// 64 characters of separators around a valid code are accepted; 65 are not.
	pad := strings.Repeat(" ", 54)
	if _, err := NormaliseRecoveryCode("ABCDEFGHJK" + pad); err != nil {
		t.Fatalf("64-byte input: %v", err)
	}
	if _, err := NormaliseRecoveryCode("ABCDEFGHJK" + pad + " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("65-byte input must be refused")
	}
}

func TestRecoveryCodeHash_BindsStaffAndKey(t *testing.T) {
	k, _ := testKeys(t)
	staff := uuid.New()
	codes := mustBatch(t).codes
	h, err := k.HashRecoveryCode(staff, codes[0])
	if err != nil || h.KID != "m1" || len(h.MAC) != MACBytes {
		t.Fatal(err)
	}
	if !k.VerifyRecoveryCodeHash(staff, codes[0], h) {
		t.Fatal("own code must verify")
	}
	if k.VerifyRecoveryCodeHash(staff, codes[1], h) {
		t.Fatal("another code verified")
	}
	if k.VerifyRecoveryCodeHash(uuid.New(), codes[0], h) {
		t.Fatal("another staff member's code verified")
	}
	// A recovery-code MAC is not a token MAC (different subkey).
	tokLike := newTokenValue(tokenData{staff: staff, secret: append([]byte(codes[0].d().v), make([]byte, 22)...)})
	if th, _ := k.HashToken(MACEnrolInvite, tokLike); bytes.Equal(th.MAC, h.MAC) {
		t.Fatal("subkeys are not separated")
	}
	other, _ := testKeys(t)
	planted, _ := other.HashRecoveryCode(staff, codes[0])
	if k.VerifyRecoveryCodeHash(staff, codes[0], planted) {
		t.Fatal("a planted (wrong-key) MAC verified")
	}
	// Normalised presentations hash identically.
	n, _ := NormaliseRecoveryCode(strings.ToLower(codes[0].RevealForDisplay()))
	if !k.VerifyRecoveryCodeHash(staff, n, h) {
		t.Fatal("normalised input must verify")
	}
	// Zero codes fail closed.
	if _, err := k.HashRecoveryCode(staff, RecoveryCode{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero code")
	}
	if _, err := k.RecoveryCodeLookupHashes(staff, RecoveryCode{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero code lookup")
	}
	if k.VerifyRecoveryCodeHash(staff, RecoveryCode{}, h) {
		t.Fatal("zero code verified")
	}
	if _, err := k.HashRecoveryCode(uuid.Nil, codes[0]); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("nil staff")
	}
}

func TestRecoveryCodeFromAssertion(t *testing.T) {
	c := mustBatch(t).codes[0]
	got, err := RecoveryCodeFromAssertion(NewRecoveryCodeAssertion(c.RevealForDisplay()))
	if err != nil || got.d().v != c.d().v {
		t.Fatal(err)
	}
	if _, err := RecoveryCodeFromAssertion(NewTOTPAssertion(c.RevealForDisplay())); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("a TOTP assertion is not a recovery code")
	}
	if _, err := RecoveryCodeFromAssertion(Assertion{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero assertion")
	}
}
