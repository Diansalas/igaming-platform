package staffmfa

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRedaction_NoSecretTypeEverRenders: every secret-bearing type redacts
// under every fmt verb, slog (text and JSON handlers, as a value and as a
// struct field) and encoding/json, as a value, as a pointer and nested in an
// exported field (ADR 0119 sections 4.2, 5.4 and 17.7).
func TestRedaction_NoSecretTypeEverRenders(t *testing.T) {
	keys, fam := testKeys(t)
	secret := mustSecret(t)
	staff := uuid.New()
	tok := mustToken(t, staff)
	batch := mustBatch(t)
	code := batch.codes[0]
	material, err := NewTOTPEnrolmentMaterial(DefaultIssuer, "someone@example.test", secret)
	if err != nil {
		t.Fatal(err)
	}
	totpAssertion := NewTOTPAssertion("492039")
	recAssertion := NewRecoveryCodeAssertion(code.RevealForDisplay())

	// Every raw form a leak could take.
	var needles []string
	addBytes := func(b []byte) {
		needles = append(needles, string(b), hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b),
			base64.RawURLEncoding.EncodeToString(b), base32NoPad.EncodeToString(b), fmt.Sprintf("%d %d %d %d %d", b[0], b[1], b[2], b[3], b[4]))
	}
	addBytes(secret.b)
	addBytes(tok.secret)
	addBytes(fam.factor["f1"])
	addBytes(fam.mac["m1"])
	addBytes(keys.factor["f1"])
	addBytes(keys.mac["m1"].recovery)
	needles = append(needles, tok.RevealForDelivery(), material.otpauthURI(), material.secretBase32(), "492039")
	for _, c := range batch.codes {
		needles = append(needles, c.v, c.RevealForDisplay())
	}

	values := map[string]any{
		"FactorSecret":      secret,
		"Assertion(totp)":   totpAssertion,
		"Assertion(rec)":    recAssertion,
		"EnrolmentMaterial": material,
		"Token":             tok,
		"RecoveryCode":      code,
		"RecoveryCodeBatch": batch,
		"Keys":              *keys,
	}
	pointers := map[string]any{
		"*FactorSecret":      &secret,
		"*Assertion":         &totpAssertion,
		"*EnrolmentMaterial": &material,
		"*Token":             &tok,
		"*RecoveryCode":      &code,
		"*RecoveryCodeBatch": &batch,
		"*Keys":              keys,
	}
	type wrapper struct {
		Secret   FactorSecret
		Assert   Assertion
		Material EnrolmentMaterial
		Token    Token
		Code     RecoveryCode
		Batch    RecoveryCodeBatch
		Codes    []RecoveryCode
		Keys     *Keys
		Map      map[string]Token
	}
	w := wrapper{secret, totpAssertion, material, tok, code, batch, batch.Codes(), keys, map[string]Token{"t": tok}}

	check := func(name, out string) {
		t.Helper()
		for _, n := range needles {
			if n != "" && strings.Contains(out, n) {
				t.Fatalf("%s leaked secret material: %q", name, out)
			}
		}
	}
	render := func(name string, v any) {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T %v"} {
			check(name+" "+verb, fmt.Sprintf(verb, v))
		}
		check(name+" Sprint", fmt.Sprint(v))
		check(name+" Sprintln", fmt.Sprintln(v))
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s json: %v", name, err)
		}
		check(name+" json", string(j))
		for _, h := range []func(*bytes.Buffer) slog.Handler{
			func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var buf bytes.Buffer
			slog.New(h(&buf)).Info("msg", "v", v, slog.Any("any", v), slog.Group("g", "inner", v))
			check(name+" slog", buf.String())
		}
	}
	for name, v := range values {
		render(name, v)
	}
	for name, v := range pointers {
		render(name, v)
	}
	render("wrapper", w)
	render("*wrapper", &w)
	render("slice", []any{secret, tok, code, material})

	// Every redacting String is exactly the marker.
	for name, s := range map[string]fmt.Stringer{"FactorSecret": secret, "Assertion": totpAssertion, "EnrolmentMaterial": material,
		"Token": tok, "RecoveryCode": code, "RecoveryCodeBatch": batch} {
		if s.String() != Redacted {
			t.Fatalf("%s.String() = %q", name, s.String())
		}
	}
}

// Every error this package can return on a secret-bearing input path is a
// fixed message: it never contains the input.
func TestErrors_NeverCarryInputs(t *testing.T) {
	keys, _ := testKeys(t)
	secretish := []string{"ABCDE-FGHJU", "x" + strings.Repeat("Z", 70), "otp-492039"}
	for _, s := range secretish {
		if _, err := NormaliseRecoveryCode(s); err == nil || strings.Contains(err.Error(), s) {
			t.Fatalf("NormaliseRecoveryCode error: %v", err)
		}
		if _, err := ParseToken(s); err == nil || strings.Contains(err.Error(), s) {
			t.Fatalf("ParseToken error: %v", err)
		}
	}
	key := b64(randBytes(t, 31))
	if _, err := NewKeysFromConfig("f1", "f1:"+key, "m1", "m1:"+b64(randBytes(t, 32))); err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("key error: %v", err)
	}
	aad := testAAD()
	s, _ := keys.SealFactorSecret(aad, mustSecret(t))
	s.Ciphertext[0] ^= 1
	if _, err := keys.OpenFactorSecret(aad, s); err == nil || strings.Contains(err.Error(), hex.EncodeToString(s.Ciphertext)) {
		t.Fatalf("open error: %v", err)
	}
	if err := VerifyStartup(nil); err == nil || strings.Contains(err.Error(), "=") {
		t.Fatalf("startup error: %v", err)
	}
}
