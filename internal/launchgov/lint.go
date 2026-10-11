package launchgov

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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
// classification guarantee): it refuses text containing an e-mail address, a phone shape, an
// IBAN-like or crypto wallet-address shape, or a run of nine or more digits (separators
// ignored), and control characters other than newline. maxBytes is the field bound.
func LintFreeText(field, s string, maxBytes int) error {
	return lint(field, s, maxBytes, false)
}

// LintRegistration is the lint for the operator registration number. A legal-entity registry
// number is the purpose of that field, so the long-digit-run and phone rules do not apply to it;
// e-mail, IBAN, wallet-address and PAN shapes (a Luhn-valid 13..19 digit run) are still refused.
func LintRegistration(s string) error {
	return lint("responsible_operator_registration", s, maxOperatorRegistration, true)
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
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidInput, field)
		}
	}
	switch {
	case emailRE.MatchString(s):
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
	if phoneRE.MatchString(s) || longDigitRun(s) {
		return fmt.Errorf("%w: %s looks like it contains a phone number, card number or account number", ErrInvalidInput, field)
	}
	return nil
}

// longDigitRun reports a run of >= maxDigitRunWithoutPANShape digits, ignoring spaces and hyphens
// between digits.
func longDigitRun(s string) bool {
	run := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			run++
			if run >= maxDigitRunWithoutPANShape {
				return true
			}
		case r == ' ' || r == '-':
			// separators do not break a run
		default:
			run = 0
		}
	}
	return false
}

// panShaped reports a Luhn-valid run of 13..19 digits (separators ignored).
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
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, int(r-'0'))
		case r == ' ' || r == '-':
		default:
			if flush() {
				return true
			}
		}
	}
	return flush()
}

// Reason catalogue (ADR 0112 4.2 "closed reason catalogue; required"). A new code needs a code
// change and review, like any closed vocabulary here. Codes are neutral labels, not legal terms.
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
