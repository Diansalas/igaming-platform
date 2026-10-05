package alerting

import (
	"errors"
	"regexp"
)

// recipient_ref rules (security M-7, ADR 0102 section 18):
//
//  1. recipient_ref is an OPAQUE vendor target id (an on-call service or rota
//     reference). It is NEVER a credential: a routing key, integration key or
//     tokenised webhook URL lives only in the secret store.
//  2. It is NEVER a network address: an adapter must not resolve, dial or
//     interpolate it as a host, URL, path, header or template. It is passed
//     to the vendor API as escaped data only.
//  3. A real adapter's endpoint is a compiled-in vendor host allowlist (no
//     redirects, TLS verified, private/link-local/metadata ranges refused
//     after DNS resolution); neither this value nor the database supplies a
//     base URL.
//  4. A generic "webhook" channel kind is out of scope without its own egress
//     allowlist security review.
//
// ValidateRecipientRef is the Go-side check the route-authoring endpoint
// applies on top of migration 0110's shape CHECKs (which accept, for
// example, 169.254.169.254:80 and a 32-hex routing key).

var (
	recipientShape     = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,127}$`)
	recipientPhone     = regexp.MustCompile(`^\+?[0-9][0-9()\s-]{6,}$`)
	recipientIPv4      = regexp.MustCompile(`(^|[^0-9])[0-9]{1,3}(\.[0-9]{1,3}){3}($|[^0-9])`)
	recipientHostPort  = regexp.MustCompile(`:[0-9]{1,5}$`)
	recipientHexSecret = regexp.MustCompile(`[0-9a-f]{32}`)
)

// ErrRecipientRefInvalid is returned for any refused recipient_ref. The
// message never contains the value.
var ErrRecipientRefInvalid = errors.New("alerting: recipient_ref is not an acceptable opaque target id")

// ValidateRecipientRef refuses an email, phone, network-address-shaped or
// credential-shaped value. It performs no I/O and never logs the value.
func ValidateRecipientRef(ref string) error {
	switch {
	case !recipientShape.MatchString(ref),
		recipientPhone.MatchString(ref),
		recipientIPv4.MatchString(ref),
		recipientHostPort.MatchString(ref),
		recipientHexSecret.MatchString(ref):
		return ErrRecipientRefInvalid
	}
	return nil
}
