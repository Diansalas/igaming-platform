package reconciliation

import (
	"regexp"
	"strings"
)

// ADR 0114 R2 (slice S4) strict parsers over the free-form diagnostic texts the streams store
// (reconciliation_key, expected_value, actual_value). Those texts embed values a provider or a
// statement controls (references, merchant references, line status), which may contain spaces and
// injected `name=value` tokens. The rule (security C2, ledger-finance F1-F3):
//
//   - only the text BEFORE the first free-text token (provider_reference=, provider_tx_id=,
//     merchant_reference=, settlement_reference=, original=, reference=) is trusted: the emitters
//     write every platform token before the free text they embed (TrustedPrefix);
//   - each token used is accepted only if it appears EXACTLY once in that prefix, else nil;
//   - the asset comes only from the rendered expected/actual segments, never the key;
//   - the amount name is an allow-list.
//
// The callers additionally require the asset to exist in the asset registry and null both amounts
// when it does not.

var freeTextToken = regexp.MustCompile(`(?:^| )(?:provider_reference|provider_tx_id|merchant_reference|settlement_reference|original|reference)=`)

// FreeTextTokenSQL is the same pattern for PostgreSQL (the subject_id filter must agree with the Go parser).
const FreeTextTokenSQL = `(^| )(provider_reference|provider_tx_id|merchant_reference|settlement_reference|original|reference)=.*$`

// TrustedPrefix returns the text before the first free-text token (all of it when there is none).
func TrustedPrefix(s string) string {
	if loc := freeTextToken.FindStringIndex(s); loc != nil {
		return s[:loc[0]]
	}
	return s
}

// tokenOnce returns the value of `name=` in the trusted prefix of text when exactly one field of
// the prefix carries that name.
func tokenOnce(text, name string) (string, bool) {
	val, n := "", 0
	for _, f := range strings.Split(TrustedPrefix(text), " ") {
		if v, ok := strings.CutPrefix(f, name+"="); ok {
			val, n = v, n+1
		}
	}
	return val, n == 1
}

// canonicalUUIDRe is the only UUID spelling accepted anywhere here.
var canonicalUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsCanonicalUUID reports whether s is a lower-case canonical UUID.
func IsCanonicalUUID(s string) bool { return canonicalUUIDRe.MatchString(s) }

// SubjectRef names what a mismatch is about. Never a player identity.
type SubjectRef struct{ Type, ID string }

// subjectTokens are the key/segment tokens that name a subject, per stream, in preference order.
var subjectTokens = map[Stream][]struct{ token, typ string }{
	StreamPaymentStatement:     {{"attempt", "payment_attempt"}, {"deposit_intent", "deposit_intent"}, {"receipt", "payment_receipt"}},
	StreamSportsbookSettlement: {{"bet", "bet"}, {"wallet", "wallet"}, {"ledger_transaction", "ledger_transaction"}},
}

// MismatchSubject derives the subject of a mismatch, or nil. ledger_vs_projection keys are
// platform-written whole UUIDs. For the other streams the id is read from the key's trusted prefix
// and, for payment_statement only, from the expected segment's trusted prefix (the emitters render
// "platform: attempt=<id> ..." there, ahead of the provider reference). The actual segment is
// never used (it renders statement-line fields). A statement:/ledger: key (the sportsbook MOCK
// statement diff) embeds free text and yields nil.
func MismatchSubject(stream Stream, kind MismatchKind, key, expected string) *SubjectRef {
	if stream == StreamLedgerVsProjection {
		if !IsCanonicalUUID(key) {
			return nil
		}
		switch kind {
		case MismatchKindMissingProjection, MismatchKindBalanceMismatch:
			return &SubjectRef{"ledger_account", key}
		case MismatchKindLedgerUnlinkedManualAdjustment:
			return &SubjectRef{"ledger_transaction", key}
		}
		return nil
	}
	if strings.HasPrefix(key, "statement:") || strings.HasPrefix(key, "ledger:") {
		return nil
	}
	segments := []string{key}
	if stream == StreamPaymentStatement {
		segments = append(segments, expected)
	}
	for _, seg := range segments {
		for _, t := range subjectTokens[stream] {
			if v, ok := tokenOnce(seg, t.token); ok && IsCanonicalUUID(v) {
				return &SubjectRef{t.typ, v}
			}
		}
	}
	return nil
}

var (
	integerRe = regexp.MustCompile(`^-?[0-9]{1,40}$`)
	assetRe   = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
)

// amountNames is the allow-list of value names that carry a minor-unit amount (ledger-finance F3):
// the sportsbook locked-balance diff and the `amount=` token the payment renders use.
var amountNames = []string{"amount", "open_bet_stakes", "ledger_locked_net", "sportsbook_locked_bonus_net"}

// AmountKindExposed reports whether a mismatch kind may expose minor-unit amounts at all (ledger-
// finance ruling, security F2: when in doubt, null). Only the two kinds whose expected side is the
// platform render and whose actual side is the statement line render qualify; every other kind's
// actual segment is a platform, ledger or other-attempt value.
func AmountKindExposed(kind MismatchKind) bool {
	return kind == MismatchKindPayAmountMismatch || kind == MismatchKindPayAssetMismatch
}

// segmentBody returns the part of a rendered segment that the platform wrote, or false. A statement
// source label is rendered as a leading "[label] " (security F1); the label is stored data, so the body
// is taken after the LAST "] line=" and the segment is refused when the label contains '=' (a label
// that could carry a token). A bracketed segment with no "] line=" structure is refused.
func segmentBody(seg string) (string, bool) {
	if !strings.HasPrefix(seg, "[") {
		return seg, true
	}
	i := strings.LastIndex(seg, "] line=")
	if i < 0 || strings.Contains(seg[:i], "=") {
		return "", false
	}
	return seg[i+2:], true
}

// MismatchAmount returns the minor-unit amount carried by one rendered segment (expected or
// actual): exactly one allow-listed name, exactly once, in the trusted prefix, with an integer
// value. Anything else is nil.
func MismatchAmount(text string) *string {
	text, ok := segmentBody(text)
	if !ok {
		return nil
	}
	var found *string
	for _, name := range amountNames {
		if v, ok := tokenOnce(text, name); ok && integerRe.MatchString(v) {
			if found != nil {
				return nil // two allow-listed names in one segment: ambiguous
			}
			x := v
			found = &x
		} else if hasToken(text, name) {
			return nil // present but duplicated or malformed
		}
	}
	return found
}

func hasToken(text, name string) bool {
	for _, f := range strings.Split(TrustedPrefix(text), " ") {
		if strings.HasPrefix(f, name+"=") {
			return true
		}
	}
	return false
}

// MismatchAssetCandidate returns the asset code the rendered expected/actual segments agree on
// (each segment has at most one `asset=`; segments that carry one must name the same code), or nil.
// The key is never consulted. The caller must still confirm the code exists in the asset registry.
func MismatchAssetCandidate(expected, actual string) *string {
	var asset string
	for _, seg := range []string{expected, actual} {
		seg, ok := segmentBody(seg)
		if !ok {
			return nil
		}
		if !hasToken(seg, "asset") {
			continue
		}
		v, ok := tokenOnce(seg, "asset")
		if !ok || !assetRe.MatchString(v) {
			return nil
		}
		if asset != "" && !strings.EqualFold(asset, v) {
			return nil
		}
		if asset == "" {
			asset = v
		}
	}
	if asset == "" {
		return nil
	}
	return &asset
}
