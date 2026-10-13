package staffmfa

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
)

// DefaultIssuer is the otpauth issuer when STAFF_MFA_ISSUER is unset (ADR 0119
// section 4.3). It is a platform value, never a brand name.
const DefaultIssuer = "iGaming Platform Back Office"

// maxIssuerLen and maxAccountLen bound the otpauth label parts.
const (
	maxIssuerLen  = 64
	maxAccountLen = 254
)

// ValidateIssuer refuses an issuer that cannot be a safe otpauth label part:
// empty, too long, not valid UTF-8, containing ':' (the label separator) or a
// control character, or with surrounding whitespace.
func ValidateIssuer(issuer string) error {
	if !validLabelPart(issuer, maxIssuerLen) {
		return ErrInvalidInput
	}
	return nil
}

func validLabelPart(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r == ':' || r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

// otpauthURI builds
//
//	otpauth://totp/{issuer}:{account}?secret=...&issuer={issuer}&algorithm=SHA1&digits=6&period=30
//
// with each label part percent-encoded. It fails closed on an invalid issuer
// or account; the error never carries the secret.
func otpauthURI(issuer, account string, secret FactorSecret) (string, error) {
	if !validLabelPart(issuer, maxIssuerLen) || !validLabelPart(account, maxAccountLen) {
		return "", ErrInvalidInput
	}
	if len(secret.d().b) != totp.SecretBytes {
		return "", ErrInvalidInput
	}
	q := "secret=" + base32NoPad.EncodeToString(secret.d().b) +
		"&issuer=" + uriEscape(issuer) +
		"&algorithm=SHA1&digits=" + strconv.Itoa(totp.Digits) +
		"&period=" + strconv.Itoa(totp.Period)
	return "otpauth://totp/" + uriEscape(issuer) + ":" + uriEscape(account) + "?" + q, nil
}

// uriEscape percent-encodes s for both the otpauth label and the query, with
// a space as %20 everywhere (never "+", which some authenticator apps show
// literally). Reserved characters such as '&', '=', '?', '#', '/', '+' and '@'
// are always encoded, so neither part can inject a parameter.
func uriEscape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
