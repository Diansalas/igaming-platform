package aml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// L2: the note lint corpus. Every case in `refused` must be refused; every case in `accepted` must pass. The lint is an
// accident guard, disclosed as such; the corpus pins the bypass classes the security review named (fullwidth and
// Arabic-Indic digits, zero-width separators, NFKC).
func TestLintNote_Corpus(t *testing.T) {
	refused := map[string]string{
		"luhn PAN":                       "card 4111 1111 1111 1111 seen",
		"luhn PAN dashes":                "4111-1111-1111-1111",
		"PAN fullwidth digits":           "４１１１ １１１１ １１１１ １１１１",
		"PAN arabic-indic digits":        "٤١١١ ١١١١ ١١١١ ١١١١",
		"PAN extended arabic-indic":      "۴۱۱۱ ۱۱۱۱ ۱۱۱۱ ۱۱۱۱",
		"digits split by zero-width":     "4​1​1​1​1​1​1​1​1​1​1",
		"digits split by word joiner":    "123⁠456⁠789",
		"iban":                           "pay to DE89 3704 0044 0532 0130 00",
		"iban compact":                   "GB29NWBK60161331926819",
		"email":                          "contact john.smith@example.com about it",
		"email fullwidth at":             "john＠example.com",
		"email obfuscated at/dot":        "john at example dot com",
		"email bracket":                  "john[at]example[dot]com",
		"phone international":            "+44 7700 900123",
		"phone local":                    "(020) 7946 0958",
		"phone dotted":                   "020.7946.0958",
		"long digit run":                 "account 123456789",
		"long digit run with separators": "1234-5678-9",
		"passport shaped":                "passport AB1234567",
		"passport shaped dash":           "id X-12345678",
		"wallet evm":                     "sent to 0x52908400098527886E0F7030069857D2E4169EE7",
		"wallet bech32":                  "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq",
		"wallet base58":                  "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2",
		"control char":                   "line\x01break",
		"tab":                            "a\tb",
		"bidi override":                  "approved ‮ by",
		"zero width space":               "re​view",
		"line separator":                 "a b",
		"hangul filler":                  "okㅤnote",
		"empty":                          "",
		"blank":                          "   \n  ",
		"too long":                       strings.Repeat("a", aml.MaxNoteBytes+1),
		"invalid utf8":                   "ok\xff\xfe",
	}
	for name, s := range refused {
		if err := aml.LintNote(s); !errors.Is(err, aml.ErrNoteRefused) {
			t.Errorf("%s must be refused: %q -> %v", name, s, err)
		}
	}
	accepted := []string{
		"reviewed the deposit pattern, nothing further",
		"three deposits within 2 days, EUR 1500 total",
		"opened on 2026-10-12 after the alert",
		"line one\nline two",
		"Überprüfung nach Q1, keine Auffälligkeiten",
		"escalated per procedure 4.2",
		strings.Repeat("a", aml.MaxNoteBytes),
	}
	for _, s := range accepted {
		if err := aml.LintNote(s); err != nil {
			t.Errorf("must be accepted: %q -> %v", s, err)
		}
	}
	if aml.LintVersion == "" {
		t.Fatal("the lint version must be recorded")
	}
}

func TestLintFilingReference(t *testing.T) {
	for _, ok := range []string{"FIU-2026/000123", "REF.2026_77", "a", strings.Repeat("a", 64)} {
		if err := aml.LintFilingReference(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty": "", "space": "FIU 2026", "at sign": "a@b.co", "unicode": "ＦＩＵ-1", "too long": strings.Repeat("a", 65),
		"pan": "4111111111111111", "pan arabic": "٤١١١١١١١١١١١١١١١", "control": "FIU\n1",
	} {
		if err := aml.LintFilingReference(bad); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("%s (%q) must be refused: %v", name, bad, err)
		}
	}
}

func TestOperationVocabulary(t *testing.T) {
	for _, op := range []aml.Operation{aml.OpDeposit, aml.OpWithdrawal, aml.OpPayout, aml.OpCasinoStake, aml.OpSportsbookStake, aml.OpManualAdjustment} {
		if !op.Valid() {
			t.Errorf("%s must be valid", op)
		}
	}
	for _, bad := range []aml.Operation{"", "bet", "DEPOSIT", "deposit "} {
		if bad.Valid() {
			t.Errorf("%q must be invalid", bad)
		}
	}
	for _, ok := range []string{"a", "reason_1", strings.Repeat("a", 64)} {
		if !aml.ValidReasonCode(ok) {
			t.Errorf("%q must be a valid reason code", ok)
		}
	}
	for _, bad := range []string{"", "Not A Token", "x-y", strings.Repeat("a", 65), "é"} {
		if aml.ValidReasonCode(bad) {
			t.Errorf("%q must not be a valid reason code", bad)
		}
	}
}
