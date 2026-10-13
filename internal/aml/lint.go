package aml

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// LintVersion is recorded on every stored note (aml_case_notes.lint_version) and bound by the proof of the insert, so a
// note stored under an older lint can be told from one stored under this one.
const LintVersion = "aml-note-lint-v1"

// MaxNoteBytes mirrors the aml_case_notes CHECK.
const MaxNoteBytes = 4000

// maxDigitRun: a run of this many digits (separators ignored) is refused as an account / document / phone number.
const maxDigitRun = 9

var (
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+`)
	obfEmail = regexp.MustCompile(`(?i)[a-z0-9._%+-]+\s*(?:@|\(at\)|\[at\]|\{at\}|\bat\b)\s*[a-z0-9][a-z0-9-]*\s*(?:\.|\(dot\)|\[dot\]|\{dot\}|\bdot\b)\s*[a-z]{2,}`)
	phoneRE  = regexp.MustCompile(`\+\d[\d\s().-]{6,}\d|\(?\d{2,4}\)?[\s.-]\d{3,4}[\s.-]\d{3,4}`)
	ibanRE   = regexp.MustCompile(`\b[A-Z]{2}\d{2}[ ]?[A-Z0-9]{4}[ ]?(?:[A-Z0-9]{2,4}[ ]?){2,8}[A-Z0-9]{1,4}\b`)
	walletRE = regexp.MustCompile(`\b0x[0-9a-fA-F]{40}\b|\b(?:bc1|tb1)[ac-hj-np-z02-9]{11,71}\b|\b[13][a-km-zA-HJ-NP-Z1-9]{25,34}\b|\bT[1-9A-HJ-NP-Za-km-z]{33}\b`)
	// A document-shaped token: one to three letters then six to ten digits (passport / ID card numbers), optionally one
	// trailing letter.
	docRE     = regexp.MustCompile(`(?i)\b[a-z]{1,3}[ -]?\d{6,10}[a-z]?\b`)
	isoDateRE = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
)

func normalise(s string) string {
	return norm.NFKC.String(s)
}

// invisibleFiller reports letter-category characters that render blank (the Hangul fillers).
func invisibleFiller(r rune) bool {
	return r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0
}

// LintNote is the server-side guard for a case note (ADR 0116 s3.2, L2). It is an ACCIDENT GUARD, not a guarantee: it
// refuses text that looks like a card number (a Luhn-valid 13..19 digit run), an IBAN, a wallet address, an e-mail
// address (also "at"/"dot" spellings), a phone number, a document-shaped token or a long digit run, and control or
// format characters other than newline. The text is NFKC-normalised before the pattern rules, so full-width letters and
// digits are matched as their ASCII forms; digits of any Unicode decimal-digit script (Arabic-Indic, ...) count as
// digits; and any zero-width or other format character is refused outright (a separator inserted to split a number).
// Notes are inside the HD-RET-1 pseudonymisation scope: the guard reduces accidental personal data, it does not remove it.
func LintNote(body string) error {
	return lintText("note", body, MaxNoteBytes)
}

// LintFilingReference validates the external filing reference a human enters when closing a case as filed: the shape the
// database enforces, and none of the personal-data shapes of LintNote except the digit rules (a reference is an
// identifier issued by the authority and legitimately contains digits).
func LintFilingReference(ref string) error {
	if !filingRefRE.MatchString(ref) {
		return fmt.Errorf("%w: external filing reference must be 1..64 characters of letters, digits, '.', '_', '/' and '-'", ErrInvalidInput)
	}
	if panShaped(normalise(ref)) {
		return fmt.Errorf("%w: external filing reference looks like a card number", ErrInvalidInput)
	}
	return nil
}

var filingRefRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,64}$`)

func lintText(field, s string, maxBytes int) error {
	if len(s) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrNoteRefused, field, maxBytes)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrNoteRefused, field)
	}
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: %s is empty", ErrNoteRefused, field)
	}
	for _, r := range s {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || invisibleFiller(r) {
			return fmt.Errorf("%w: %s contains a control or format character", ErrNoteRefused, field)
		}
	}
	n := normalise(s)
	switch {
	case emailRE.MatchString(n), obfEmail.MatchString(n):
		return fmt.Errorf("%w: %s looks like it contains an e-mail address", ErrNoteRefused, field)
	case ibanRE.MatchString(n):
		return fmt.Errorf("%w: %s looks like it contains an IBAN", ErrNoteRefused, field)
	case walletRE.MatchString(n):
		return fmt.Errorf("%w: %s looks like it contains a wallet address", ErrNoteRefused, field)
	}
	n = isoDateRE.ReplaceAllString(n, " ")
	if phoneRE.MatchString(n) || docRE.MatchString(n) || longDigitRun(n) || panShaped(n) {
		return fmt.Errorf("%w: %s looks like it contains a phone, card, document or account number", ErrNoteRefused, field)
	}
	return nil
}

// digitValue returns the decimal value of a Unicode decimal digit (Nd) and whether r is one.
func digitValue(r rune) (int, bool) {
	if !unicode.IsDigit(r) {
		return 0, false
	}
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	z := r
	for unicode.IsDigit(z - 1) {
		z--
	}
	return int(r-z) % 10, true
}

// longDigitRun reports a run of >= maxDigitRun digits. Every character that is neither a digit nor a letter or number is a
// separator that does not break the run; a letter breaks it.
func longDigitRun(s string) bool {
	run := 0
	for _, r := range s {
		if _, ok := digitValue(r); ok {
			run++
			if run >= maxDigitRun {
				return true
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			run = 0
		}
	}
	return false
}

// panShaped reports a Luhn-valid run of 13..19 digits; as in longDigitRun, any character that is neither a letter nor a
// number is a separator.
func panShaped(s string) bool {
	var digits []int
	flush := func() bool {
		defer func() { digits = digits[:0] }()
		if len(digits) < 13 || len(digits) > 19 {
			return false
		}
		sum, dbl := 0, false
		for i := len(digits) - 1; i >= 0; i-- {
			d := digits[i]
			if dbl {
				d *= 2
				if d > 9 {
					d -= 9
				}
			}
			sum += d
			dbl = !dbl
		}
		return sum%10 == 0
	}
	for _, r := range s {
		if v, ok := digitValue(r); ok {
			digits = append(digits, v)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if flush() {
				return true
			}
		}
	}
	return flush()
}
