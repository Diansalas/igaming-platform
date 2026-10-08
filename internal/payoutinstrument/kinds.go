package payoutinstrument

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Kind codes seeded by migration 0123. The set is OPEN (decision 6): a new
// regulated instrument is a new payout_instrument_kinds row plus a KindSpec
// registered here, never a code path keyed on one kind elsewhere.
const (
	KindBankAccount    = "bank_account"
	KindCardToken      = "card_token"
	KindEwalletAccount = "ewallet_account"
	KindCryptoAddress  = "crypto_address"
	KindSyntheticTest  = "synthetic_test"
)

// ErrInvalidDetail is the opaque validation failure for a kind detail. It
// never echoes the offending value.
var ErrInvalidDetail = errors.New("payoutinstrument: invalid instrument detail")

// ErrPANRefused: a Luhn-valid 12-19 digit string was found (L-8).
var ErrPANRefused = errors.New("payoutinstrument: a card number is never accepted")

// ErrUnknownKind: no KindSpec is registered for the kind.
var ErrUnknownKind = errors.New("payoutinstrument: unknown instrument kind")

// Normalized is the validated form of a detail.
type Normalized struct {
	// Canonical is the re-encoded detail that is encrypted (known fields only,
	// normalised values, fixed key order).
	Canonical []byte
	// FingerprintInput is the kind-specific normalised string the fingerprint
	// HMAC covers.
	FingerprintInput string
	// Mask is display_mask (<= 64 bytes): the only detail-derived value any API
	// returns.
	Mask string
}

// KindSpec validates and normalises one kind's detail.
type KindSpec interface {
	Code() string
	SchemaVersion() int
	Normalize(detail json.RawMessage) (Normalized, error)
}

// KindRegistry maps kind codes to specs.
type KindRegistry struct{ m map[string]KindSpec }

// NewKindRegistry builds a registry from specs.
func NewKindRegistry(specs ...KindSpec) *KindRegistry {
	r := &KindRegistry{m: map[string]KindSpec{}}
	for _, s := range specs {
		r.m[s.Code()] = s
	}
	return r
}

// DefaultKinds is the seeded set.
func DefaultKinds() *KindRegistry {
	return NewKindRegistry(bankAccountSpec{}, cardTokenSpec{}, ewalletSpec{}, cryptoAddressSpec{}, syntheticTestSpec{})
}

// Spec returns the spec for kind.
func (r *KindRegistry) Spec(kind string) (KindSpec, error) {
	s, ok := r.m[kind]
	if !ok {
		return nil, ErrUnknownKind
	}
	return s, nil
}

// Codes lists registered kinds, sorted.
func (r *KindRegistry) Codes() []string {
	out := make([]string, 0, len(r.m))
	for c := range r.m {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ---- shared decoding ------------------------------------------------------

// decodeStrict decodes detail into dst refusing unknown fields, trailing data
// and any string value that is, or contains, a card number (any field of any
// kind, ADR 0111 2.9).
func decodeStrict(detail json.RawMessage, dst any) error {
	if len(detail) == 0 || len(detail) > MaxDetailPlaintext {
		return ErrInvalidDetail
	}
	var generic any
	gd := json.NewDecoder(bytes.NewReader(detail))
	gd.UseNumber()
	if err := gd.Decode(&generic); err != nil {
		return ErrInvalidDetail
	}
	if gd.More() {
		return ErrInvalidDetail
	}
	if walkStrings(generic, ContainsPAN) {
		return ErrPANRefused
	}
	d := json.NewDecoder(bytes.NewReader(detail))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return ErrInvalidDetail
	}
	return nil
}

func walkStrings(v any, pred func(string) bool) bool {
	switch t := v.(type) {
	case string:
		return pred(t)
	case map[string]any:
		for k, x := range t {
			if pred(k) || walkStrings(x, pred) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if walkStrings(x, pred) {
				return true
			}
		}
	case json.Number:
		// A bare JSON number can also be a PAN.
		return pred(t.String())
	}
	return false
}

func cleanText(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

var (
	alnumUpperRE = regexp.MustCompile(`^[A-Z0-9]+$`)
	countryRE    = regexp.MustCompile(`^[A-Z]{2}$`)
	tokenRE      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{3,128}$`)
	idRE         = regexp.MustCompile(`^[A-Za-z0-9_.:@+-]{3,128}$`)
	providerRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	labelRE      = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	emailRE      = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}\.[A-Za-z]{2,24}$`)
	last4RE      = regexp.MustCompile(`^[0-9]{4}$`)
	networkRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,15}$`)
	addressRE    = regexp.MustCompile(`^[A-Za-z0-9:._-]{20,128}$`)
)

func marshalCanonical(v any) []byte {
	b, _ := json.Marshal(v) // struct field order is fixed
	return b
}

// ---- bank_account ---------------------------------------------------------

type bankAccountSpec struct{}

func (bankAccountSpec) Code() string       { return KindBankAccount }
func (bankAccountSpec) SchemaVersion() int { return 1 }

type bankAccountDetail struct {
	IBAN          string `json:"iban,omitempty"`
	Country       string `json:"country,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	RoutingCode   string `json:"routing_code,omitempty"`
}

func (bankAccountSpec) Normalize(detail json.RawMessage) (Normalized, error) {
	var d bankAccountDetail
	if err := decodeStrict(detail, &d); err != nil {
		return Normalized{}, err
	}
	strip := strings.NewReplacer(" ", "", "-", "")
	if d.IBAN != "" {
		if d.Country != "" || d.AccountNumber != "" || d.RoutingCode != "" {
			return Normalized{}, ErrInvalidDetail
		}
		iban := strings.ToUpper(strip.Replace(d.IBAN))
		if !ibanValid(iban) {
			return Normalized{}, ErrInvalidDetail
		}
		return Normalized{
			Canonical:        marshalCanonical(bankAccountDetail{IBAN: iban}),
			FingerprintInput: "iban:" + iban,
			Mask:             iban[:2] + "****" + last4(iban),
		}, nil
	}
	country := strings.ToUpper(d.Country)
	acct := strings.ToUpper(strip.Replace(d.AccountNumber))
	routing := strings.ToUpper(strip.Replace(d.RoutingCode))
	if !countryRE.MatchString(country) || !alnumUpperRE.MatchString(acct) || len(acct) < 4 || len(acct) > 34 ||
		(routing != "" && (!alnumUpperRE.MatchString(routing) || len(routing) > 20)) {
		return Normalized{}, ErrInvalidDetail
	}
	return Normalized{
		Canonical:        marshalCanonical(bankAccountDetail{Country: country, AccountNumber: acct, RoutingCode: routing}),
		FingerprintInput: "acct:" + country + "|" + routing + "|" + acct,
		Mask:             country + "****" + last4(acct),
	}, nil
}

// ibanValid applies the ISO 13616 length window and the mod-97 check.
func ibanValid(iban string) bool {
	if len(iban) < 15 || len(iban) > 34 || !alnumUpperRE.MatchString(iban) || iban[0] < 'A' || iban[0] > 'Z' || iban[1] < 'A' || iban[1] > 'Z' {
		return false
	}
	rearranged := iban[4:] + iban[:4]
	var sb strings.Builder
	for _, r := range rearranged {
		if r >= 'A' && r <= 'Z' {
			fmt.Fprintf(&sb, "%d", r-'A'+10)
		} else {
			sb.WriteRune(r)
		}
	}
	n, ok := new(big.Int).SetString(sb.String(), 10)
	if !ok {
		return false
	}
	return new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// ---- card_token -----------------------------------------------------------

type cardTokenSpec struct{}

func (cardTokenSpec) Code() string       { return KindCardToken }
func (cardTokenSpec) SchemaVersion() int { return 1 }

type cardTokenDetail struct {
	Token              string `json:"token"`
	Network            string `json:"network"`
	Last4              string `json:"last4"`
	PSPCardFingerprint string `json:"psp_card_fingerprint,omitempty"`
}

func (cardTokenSpec) Normalize(detail json.RawMessage) (Normalized, error) {
	var d cardTokenDetail
	if err := decodeStrict(detail, &d); err != nil {
		return Normalized{}, err
	}
	if !tokenRE.MatchString(d.Token) || !networkRE.MatchString(d.Network) || !last4RE.MatchString(d.Last4) ||
		(d.PSPCardFingerprint != "" && !tokenRE.MatchString(d.PSPCardFingerprint)) {
		return Normalized{}, ErrInvalidDetail
	}
	// L-8: the PSP card fingerprint (when supplied) is the normalised input so
	// a re-tokenised card conflicts; otherwise the token itself.
	fpIn := "tok:" + d.Token
	if d.PSPCardFingerprint != "" {
		fpIn = "pspfp:" + d.PSPCardFingerprint
	}
	return Normalized{Canonical: marshalCanonical(d), FingerprintInput: fpIn, Mask: d.Network + " ****" + d.Last4}, nil
}

// ---- ewallet_account ------------------------------------------------------

type ewalletSpec struct{}

func (ewalletSpec) Code() string       { return KindEwalletAccount }
func (ewalletSpec) SchemaVersion() int { return 1 }

type ewalletDetail struct {
	Provider  string `json:"provider"`
	Email     string `json:"email,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

func (ewalletSpec) Normalize(detail json.RawMessage) (Normalized, error) {
	var d ewalletDetail
	if err := decodeStrict(detail, &d); err != nil {
		return Normalized{}, err
	}
	if !providerRE.MatchString(d.Provider) || (d.Email == "") == (d.AccountID == "") {
		return Normalized{}, ErrInvalidDetail
	}
	if d.Email != "" {
		email := strings.ToLower(d.Email)
		if !emailRE.MatchString(email) {
			return Normalized{}, ErrInvalidDetail
		}
		at := strings.LastIndexByte(email, '@')
		return Normalized{
			Canonical:        marshalCanonical(ewalletDetail{Provider: d.Provider, Email: email}),
			FingerprintInput: d.Provider + "|email:" + email,
			Mask:             email[:1] + "***@" + email[at+1:],
		}, nil
	}
	if !idRE.MatchString(d.AccountID) {
		return Normalized{}, ErrInvalidDetail
	}
	return Normalized{
		Canonical:        marshalCanonical(ewalletDetail{Provider: d.Provider, AccountID: d.AccountID}),
		FingerprintInput: d.Provider + "|id:" + d.AccountID,
		Mask:             "***" + last4(d.AccountID),
	}, nil
}

// ---- crypto_address -------------------------------------------------------

type cryptoAddressSpec struct{}

func (cryptoAddressSpec) Code() string       { return KindCryptoAddress }
func (cryptoAddressSpec) SchemaVersion() int { return 1 }

type cryptoAddressDetail struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

func (cryptoAddressSpec) Normalize(detail json.RawMessage) (Normalized, error) {
	var d cryptoAddressDetail
	if err := decodeStrict(detail, &d); err != nil {
		return Normalized{}, err
	}
	if !networkRE.MatchString(d.Network) || !addressRE.MatchString(d.Address) {
		return Normalized{}, ErrInvalidDetail
	}
	addr := d.Address
	if strings.HasPrefix(addr, "0x") || strings.HasPrefix(addr, "0X") {
		addr = strings.ToLower(addr) // EVM hex addresses are case-insensitive
	}
	return Normalized{
		Canonical:        marshalCanonical(cryptoAddressDetail{Network: d.Network, Address: addr}),
		FingerprintInput: d.Network + "|" + addr,
		Mask:             addr[:6] + "..." + addr[len(addr)-4:],
	}, nil
}

// ---- synthetic_test -------------------------------------------------------

type syntheticTestSpec struct{}

func (syntheticTestSpec) Code() string       { return KindSyntheticTest }
func (syntheticTestSpec) SchemaVersion() int { return 1 }

type syntheticTestDetail struct {
	Label string `json:"label"`
}

func (syntheticTestSpec) Normalize(detail json.RawMessage) (Normalized, error) {
	var d syntheticTestDetail
	if err := decodeStrict(detail, &d); err != nil {
		return Normalized{}, err
	}
	if !labelRE.MatchString(d.Label) {
		return Normalized{}, ErrInvalidDetail
	}
	return Normalized{Canonical: marshalCanonical(d), FingerprintInput: "label:" + d.Label, Mask: "synthetic:" + d.Label}, nil
}
