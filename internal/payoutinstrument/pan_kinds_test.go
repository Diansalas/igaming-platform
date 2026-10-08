package payoutinstrument

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestContainsPAN(t *testing.T) {
	yes := []string{
		"4111111111111111", "4111 1111 1111 1111", "4111-1111-1111-1111", "card 4012888888881881 end",
		"5555555555554444", "378282246310005" /* 15 digits amex */, "6011111111111117", "3530111333300000",
		"4222222222222" /* 13 */, "4000056655665556",
		"100000000008" /* 12: the lower bound */, "6000000000000000004", /* 19: the upper bound */
	}
	for _, s := range yes {
		if !ContainsPAN(s) {
			t.Errorf("expected a PAN to be detected in %q", s)
		}
	}
	no := []string{
		"", "1234", "4111111111111112" /* Luhn-invalid */, "12345678901", /* 11 digits */
		"DE89370400440532013000" /* IBAN: its digit run is 20 long */, "tok_abcdef", "20250101", "a1b2c3",
		"41111111111111111111", /* 20 digits */
	}
	for _, s := range no {
		if ContainsPAN(s) {
			t.Errorf("did not expect a PAN in %q", s)
		}
	}
}

func TestPANRefusedInAnyFieldOfAnyKind(t *testing.T) {
	reg := DefaultKinds()
	pan := "4111111111111111"
	cases := map[string]string{
		KindBankAccount:    `{"country":"DE","account_number":"ABC1234","routing_code":"` + pan + `"}`,
		KindCardToken:      `{"token":"tok_1","network":"visa","last4":"1111","psp_card_fingerprint":"` + pan + `"}`,
		KindEwalletAccount: `{"provider":"payz","account_id":"` + pan + `"}`,
		KindCryptoAddress:  `{"network":"eth","address":"0x` + strings.Repeat("a", 30) + `","note":"` + pan + `"}`,
		KindSyntheticTest:  `{"label":"x","extra":"` + pan + `"}`,
	}
	for kind, detail := range cases {
		spec, _ := reg.Spec(kind)
		_, err := spec.Normalize(json.RawMessage(detail))
		if !errors.Is(err, ErrPANRefused) {
			t.Errorf("%s: want ErrPANRefused, got %v", kind, err)
		}
	}
	// A bare JSON number and a PAN inside a key are refused too.
	spec, _ := reg.Spec(KindSyntheticTest)
	for _, d := range []string{`{"label":"x","n":4111111111111111}`, `{"4111111111111111":"x"}`} {
		if _, err := spec.Normalize(json.RawMessage(d)); !errors.Is(err, ErrPANRefused) {
			t.Errorf("%s: want ErrPANRefused, got %v", d, err)
		}
	}
}

func TestMaskRules(t *testing.T) {
	reg := DefaultKinds()
	mask := func(kind, detail string) string {
		t.Helper()
		spec, err := reg.Spec(kind)
		if err != nil {
			t.Fatal(err)
		}
		n, err := spec.Normalize(json.RawMessage(detail))
		if err != nil {
			t.Fatalf("%s %s: %v", kind, detail, err)
		}
		return n.Mask
	}
	if got := mask(KindBankAccount, `{"iban":"GB82 WEST 1234 5698 7654 32"}`); got != "GB****5432" {
		t.Errorf("iban mask = %q (country code + last 4)", got)
	}
	if got := mask(KindBankAccount, `{"country":"br","account_number":"1234-5678-9"}`); got != "BR****6789" {
		t.Errorf("account mask = %q", got)
	}
	if got := mask(KindEwalletAccount, `{"provider":"payz","email":"Alice.Smith@Example.com"}`); got != "a***@example.com" {
		t.Errorf("ewallet email mask = %q (first char of local part + ***@ + domain)", got)
	}
	if got := mask(KindCryptoAddress, `{"network":"eth","address":"0xAbCdEf0123456789AbCdEf0123456789AbCd0123"}`); got != "0xabcd...0123" {
		t.Errorf("crypto mask = %q (first 6 + last 4)", got)
	}
	if got := mask(KindCardToken, `{"token":"tok_abc","network":"visa","last4":"4242"}`); got != "visa ****4242" {
		t.Errorf("card mask = %q (network + last 4)", got)
	}
	for _, m := range []string{
		mask(KindBankAccount, `{"iban":"GB82WEST12345698765432"}`),
		mask(KindSyntheticTest, `{"label":"abc"}`),
	} {
		if len(m) > 64 || len(m) == 0 {
			t.Errorf("mask %q out of bounds", m)
		}
	}
}

func TestNormalizationAndFingerprintInputs(t *testing.T) {
	reg := DefaultKinds()
	norm := func(kind, d string) Normalized {
		spec, _ := reg.Spec(kind)
		n, err := spec.Normalize(json.RawMessage(d))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		return n
	}
	// IBAN spacing / case do not change the fingerprint input.
	a := norm(KindBankAccount, `{"iban":"gb82 west 1234 5698 7654 32"}`)
	b := norm(KindBankAccount, `{"iban":"GB82WEST12345698765432"}`)
	if a.FingerprintInput != b.FingerprintInput {
		t.Error("IBAN normalisation must make the fingerprint input canonical")
	}
	// L-8: with a PSP card fingerprint, two different tokens of the same card collide.
	c1 := norm(KindCardToken, `{"token":"tok_one","network":"visa","last4":"4242","psp_card_fingerprint":"fp_same_card"}`)
	c2 := norm(KindCardToken, `{"token":"tok_two","network":"visa","last4":"4242","psp_card_fingerprint":"fp_same_card"}`)
	if c1.FingerprintInput != c2.FingerprintInput {
		t.Error("a re-tokenised card must produce the same fingerprint input")
	}
	// Without it the token is the input.
	c3 := norm(KindCardToken, `{"token":"tok_one","network":"visa","last4":"4242"}`)
	c4 := norm(KindCardToken, `{"token":"tok_two","network":"visa","last4":"4242"}`)
	if c3.FingerprintInput == c4.FingerprintInput {
		t.Error("different tokens without a PSP fingerprint must differ")
	}
}

func TestKindValidationRefusals(t *testing.T) {
	reg := DefaultKinds()
	bad := map[string][]string{
		KindBankAccount:    {`{}`, `{"iban":"GB82WEST12345698765433"}`, `{"iban":"GB82WEST12345698765432","country":"GB"}`, `{"country":"X","account_number":"1234"}`, `{"iban":"GB82WEST12345698765432","unknown":"x"}`, `not json`, `{"iban":"GB82WEST12345698765432"} {}`},
		KindCardToken:      {`{"token":"t","network":"visa","last4":"4242"}`, `{"token":"tok_1","network":"visa","last4":"42"}`, `{"token":"tok_1","network":"VISA","last4":"4242"}`},
		KindEwalletAccount: {`{"provider":"payz"}`, `{"provider":"payz","email":"a@b.co","account_id":"x1234"}`, `{"provider":"payz","email":"not-an-email"}`},
		KindCryptoAddress:  {`{"network":"eth","address":"short"}`},
		KindSyntheticTest:  {`{"label":""}`, `{"label":"UPPER"}`},
	}
	for kind, list := range bad {
		spec, _ := reg.Spec(kind)
		for _, d := range list {
			if _, err := spec.Normalize(json.RawMessage(d)); err == nil {
				t.Errorf("%s: %s must be refused", kind, d)
			}
		}
	}
	if _, err := reg.Spec("nope"); !errors.Is(err, ErrUnknownKind) {
		t.Error("unknown kind must be refused")
	}
	// Oversized.
	spec, _ := reg.Spec(KindSyntheticTest)
	if _, err := spec.Normalize(json.RawMessage(`{"label":"` + strings.Repeat("a", MaxDetailPlaintext) + `"}`)); err == nil {
		t.Error("oversized detail must be refused")
	}
}

func TestOpenKindSet(t *testing.T) {
	// A new regulated instrument is a new spec, never a code path keyed on one kind.
	r := NewKindRegistry(syntheticTestSpec{})
	if got := r.Codes(); len(got) != 1 || got[0] != KindSyntheticTest {
		t.Fatalf("registry codes = %v", got)
	}
}
