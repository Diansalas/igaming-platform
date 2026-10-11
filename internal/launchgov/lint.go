package launchgov

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Bounds mirror the migration 0128 CHECKs.
const (
	maxConditionsNote          = 2000
	maxDeterminationReference  = 200
	maxDeterminationByRole     = 100
	maxOperatorName            = 200
	maxOperatorRegistration    = 100
	maxJurisdictions           = 32
	maxDigitRunWithoutPANShape = 9
)

var (
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+`)
	phoneRE  = regexp.MustCompile(`\+\d[\d\s().-]{6,}\d|\(?\d{2,4}\)?[\s.-]\d{3,4}[\s.-]\d{3,4}`)
	ibanRE   = regexp.MustCompile(`\b[A-Z]{2}\d{2}[ ]?[A-Z0-9]{4}[ ]?(?:[A-Z0-9]{2,4}[ ]?){2,8}[A-Z0-9]{1,4}\b`)
	evmRE    = regexp.MustCompile(`\b0x[0-9a-fA-F]{40}\b`)
	bech32RE = regexp.MustCompile(`\b(?:bc1|tb1)[ac-hj-np-z02-9]{11,71}\b`)
	base58RE = regexp.MustCompile(`\b[13][a-km-zA-HJ-NP-Z1-9]{25,34}\b`)
	tronRE   = regexp.MustCompile(`\bT[1-9A-HJ-NP-Za-km-z]{33}\b`)
	// An ISO date is not a phone number; it is removed before the phone / long-digit rules.
	isoDateRE = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
)

// LintFreeText is the accident guard of ADR 0112 8.8 for the bounded free-text fields (not a
// classification guarantee): it refuses text containing an e-mail address (including the common
// "at"/"dot" spellings), a phone shape, an IBAN-like or crypto wallet-address shape, or a run of
// nine or more digits (every non-alphanumeric character between digits is a separator), and
// control characters other than newline. The text is NFKC-normalised first, so full-width
// letters, digits and "@" are matched as their ASCII forms. maxBytes is the field bound.
func LintFreeText(field, s string, maxBytes int) error {
	return lint(field, s, maxBytes, false)
}

// registrationRE is the shape of an operator registration number: letters, digits, spaces, dot,
// slash and hyphen only (no "+", no punctuation that carries a phone or an e-mail), starting
// with an alphanumeric, at most 64 characters.
var registrationRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ./-]{0,63}$`)

// LintRegistration is the lint for the operator registration number. A legal-entity registry
// number is the purpose of that field, so the long-digit-run and phone rules do not apply to it;
// it must match registrationRE, carry at most one alphabetic word of three or more letters (a
// registry prefix, not a name), and still refuses e-mail, IBAN, wallet-address and PAN shapes (a
// Luhn-valid 13..19 digit run).
func LintRegistration(s string) error {
	if s == "" {
		return nil // the registration number is optional
	}
	if err := lint("responsible_operator_registration", s, maxOperatorRegistration, true); err != nil {
		return err
	}
	if !registrationRE.MatchString(s) {
		return fmt.Errorf("%w: responsible_operator_registration may contain only letters, digits, space, '.', '/' and '-'", ErrInvalidInput)
	}
	words := 0
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '.' || r == '/' || r == '-' }) {
		if len(tok) >= 3 && strings.IndexFunc(tok, func(r rune) bool { return !unicode.IsLetter(r) }) < 0 {
			words++
		}
	}
	if words > 1 {
		return fmt.Errorf("%w: responsible_operator_registration looks like free text, not a registry number", ErrInvalidInput)
	}
	return nil
}

// obfuscatedEmailRE matches "user at host dot tld" style spellings ("(at)", "[at]", "[dot]").
var obfuscatedEmailRE = regexp.MustCompile(`(?i)[a-z0-9._%+-]+\s*(?:@|\(at\)|\[at\]|\{at\}|\bat\b)\s*[a-z0-9][a-z0-9-]*\s*(?:\.|\(dot\)|\[dot\]|\{dot\}|\bdot\b)\s*[a-z]{2,}`)

// normalise returns the NFKC form of s without format (Cf) characters such as zero-width spaces.
func normalise(s string) string {
	n := norm.NFKC.String(s)
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, n)
}

// invisibleFiller reports the letter-category characters that render as blank: the Hangul fillers
// U+115F, U+1160, U+3164 and U+FFA0 (they pass Cf/Zl/Zp checks and let a note look empty or hide text).
func invisibleFiller(r rune) bool {
	return r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0
}

func lint(field, s string, maxBytes int, registration bool) error {
	if len(s) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidInput, field, maxBytes)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidInput, field)
	}
	for _, r := range s {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || invisibleFiller(r) { // Cf: bidi overrides, zero-width; Zl/Zp: U+2028/2029; fillers: Hangul fillers
			return fmt.Errorf("%w: %s contains a control or format character", ErrInvalidInput, field)
		}
	}
	s = normalise(s)
	switch {
	case emailRE.MatchString(s), obfuscatedEmailRE.MatchString(s):
		return fmt.Errorf("%w: %s looks like it contains an e-mail address", ErrInvalidInput, field)
	case ibanRE.MatchString(s):
		return fmt.Errorf("%w: %s looks like it contains an IBAN", ErrInvalidInput, field)
	case evmRE.MatchString(s), bech32RE.MatchString(s), base58RE.MatchString(s), tronRE.MatchString(s):
		return fmt.Errorf("%w: %s looks like it contains a wallet address", ErrInvalidInput, field)
	}
	if registration {
		if panShaped(s) {
			return fmt.Errorf("%w: %s looks like it contains a card number", ErrInvalidInput, field)
		}
		return nil
	}
	s = isoDateRE.ReplaceAllString(s, " ")
	if phoneRE.MatchString(s) || longDigitRun(s) || panShaped(s) {
		return fmt.Errorf("%w: %s looks like it contains a phone number, card number or account number", ErrInvalidInput, field)
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

// longDigitRun reports a run of >= maxDigitRunWithoutPANShape digits. Every character that is
// neither a digit nor a letter or number is a separator that does not break the run ("/", "_",
// ".", "(" ...); a letter breaks it.
func longDigitRun(s string) bool {
	run := 0
	for _, r := range s {
		if _, ok := digitValue(r); ok {
			run++
			if run >= maxDigitRunWithoutPANShape {
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

// panShaped reports a Luhn-valid run of 13..19 digits; as in longDigitRun, any character that is
// neither a letter nor a number is a separator.
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

// ReasonOtherDocumented is the catch-all reason code; it requires a non-empty conditions_note.
const ReasonOtherDocumented = "other_documented"

// Reason catalogue (ADR 0112 4.2 "closed reason catalogue; required"). A new code needs a code
// change and review, like any closed vocabulary here. Codes are neutral labels, not legal terms.
// RECOMMENDATION: the catalogues below were authored for this slice; the Blueprint does not
// define them (ADR 0112 records them as RECOMMENDATION pending product / compliance review).
var requestReasons = map[string][]string{
	ActionActivate:   {"initial_launch"},
	ActionReactivate: {"issue_resolved", "relaunch_after_review"},
	ActionSuspend:    {"compliance_hold", "security_incident", "operational_incident", "licence_issue", "operator_request", "other_documented"},
	ActionClose:      {"offboarding", "licence_issue", "operator_request", "compliance_decision"},
	ActionRatify:     {"legacy_ratification"},
	"cancel":         {"requester_withdrew", "superseded_by_new_request", "other_documented"},
}

var decisionReasons = map[string][]string{
	DecisionApprove: {"reviewed_ok"},
	DecisionReject:  {"insufficient_information", "readiness_concerns", "policy_concern", "other_documented"},
}

func reasonAllowed(table map[string][]string, key, code string) bool {
	for _, c := range table[key] {
		if c == code {
			return true
		}
	}
	return false
}

// RequestReasonCodes returns the closed reason codes for an action (for the console).
func RequestReasonCodes(action string) []string {
	return append([]string(nil), requestReasons[action]...)
}

func trimmedNonEmpty(field, s string, max int) error {
	if strings.TrimSpace(s) == "" || len(s) > max {
		return fmt.Errorf("%w: %s must be 1..%d bytes", ErrInvalidInput, field, max)
	}
	return nil
}
