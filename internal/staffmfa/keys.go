package staffmfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// MinKeyBytes is the shortest accepted master key in either family (ADR 0119
// section 5.1).
const MinKeyBytes = 32

// maxKeyBytes bounds a decoded master key. It is a sanity bound against a
// mis-pasted value (for example a whole PEM file), not a cryptographic limit.
const maxKeyBytes = 1024

// HKDF labels (ADR 0119 section 5.2). The HKDF info is label || "|" || kid, so
// a subkey is never shared across kids or across purposes.
const (
	labelFactorAEAD       = "staff-mfa-factor-aead-v1"
	labelRecovery         = "staff-mfa-recovery-v1"
	labelChallenge        = "staff-auth-challenge-v1"
	labelEnrolmentToken   = "staff-enrolment-token-v1"
	labelSessionAssurance = "staff-session-assurance-v1"
)

// Environment variable names of the two key families (ADR 0119 section 5.1).
const (
	EnvFactorKeys      = "STAFF_MFA_FACTOR_KEYS"
	EnvFactorActiveKID = "STAFF_MFA_FACTOR_ACTIVE_KID"
	EnvMACKeys         = "STAFF_MFA_MAC_KEYS"
	EnvMACActiveKID    = "STAFF_MFA_MAC_ACTIVE_KID"
)

var (
	// ErrKeyConfig: the key configuration is unusable. The message names the
	// family and, where useful, the kid; never key material.
	ErrKeyConfig = errors.New("staffmfa: invalid key configuration")
	// ErrUnknownKID: a stored row names a kid this process does not hold
	// (never configured, or retired by removal from the family).
	ErrUnknownKID = errors.New("staffmfa: unknown key id")
	// ErrFactorIntegrity: a sealed factor secret did not open (wrong key,
	// wrong AAD, moved or tampered ciphertext, unknown kid, malformed
	// columns). The caller counts it as a verification failure AND raises the
	// durable alert staff_mfa_integrity (ADR 0119 section 5.3). It is never
	// "factor absent".
	ErrFactorIntegrity = errors.New("staffmfa: factor secret integrity failure")
	// ErrInvalidInput: a caller-supplied value is malformed. Never carries
	// the value.
	ErrInvalidInput = errors.New("staffmfa: invalid input")
)

var (
	kidRE     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	keyPairRE = regexp.MustCompile(`^([A-Za-z0-9._-]{1,32}):([A-Za-z0-9+/_=-]+)$`)
)

// macSubkeys are the four HMAC subkeys of one MAC-family kid.
type macSubkeys struct {
	recovery, challenge, enrolment, session []byte
}

// Keys holds BOTH staff MFA key families, already expanded into HKDF subkeys:
// the FACTOR family (the AES-256-GCM factor-secret key) and the MAC family
// (recovery-code, challenge, enrolment-token and session-assurance HMAC keys).
// The families rotate independently (ADR 0119 section 5.3): the FACTOR family
// by re-enrolment only, the MAC family by ageing out.
//
// Keys has no exported fields and redacts under every fmt verb, slog and JSON
// (value receivers, so a dereferenced copy redacts too).
// It is built only from configuration; there is no random-key fallback and no
// development bypass.
type Keys struct{ km func() *keyMaterial }

// m returns the key material. Keys is only built by NewKeys.
func (k *Keys) m() *keyMaterial { return k.km() }

// keyMaterial is reachable only through an unexported closure, so a Keys value
// held in an UNEXPORTED field of another struct renders as a function address
// under every fmt verb and slog, never as the subkey maps (security review C2).
type keyMaterial struct {
	factorActive string
	factor       map[string][]byte // kid -> AEAD subkey
	macActive    string
	mac          map[string]macSubkeys
	rnd          io.Reader // crypto/rand.Reader; internal tests substitute it
}

const redactedKeys = "staffmfa.Keys{redacted}"

// String never renders key material.
func (Keys) String() string { return redactedKeys }

// GoString never renders key material.
func (Keys) GoString() string { return redactedKeys }

// Format redacts under every verb.
func (Keys) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedKeys)) }

// LogValue implements slog.LogValuer.
func (Keys) LogValue() slog.Value { return slog.StringValue(redactedKeys) }

// MarshalJSON never renders key material.
func (Keys) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedKeys + `"`), nil }

// ParseKeySet parses one family value: comma-separated "kid:base64(secret)"
// entries (standard or URL-safe base64, padded or not). It fails closed on an
// empty entry, a malformed entry, a key shorter than MinKeyBytes, a duplicate
// kid or a key value repeated under two kids. Errors name the family and the
// kid only, never the secret.
func ParseKeySet(family, raw string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: %s has no keys", ErrKeyConfig, family)
	}
	seen := map[string]string{}
	for i, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%w: %s entry %d is empty", ErrKeyConfig, family, i+1)
		}
		m := keyPairRE.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("%w: %s entry %d is not kid:base64", ErrKeyConfig, family, i+1)
		}
		kid := m[1]
		b, err := decodeKey(m[2])
		if err != nil {
			return nil, fmt.Errorf("%w: %s key %q is not valid base64", ErrKeyConfig, family, kid)
		}
		if len(b) < MinKeyBytes {
			return nil, fmt.Errorf("%w: %s key %q decodes to fewer than %d bytes", ErrKeyConfig, family, kid, MinKeyBytes)
		}
		if degenerateKey(b) {
			return nil, fmt.Errorf("%w: %s key %q is degenerate (every byte has the same value)", ErrKeyConfig, family, kid)
		}
		if len(b) > maxKeyBytes {
			return nil, fmt.Errorf("%w: %s key %q decodes to more than %d bytes", ErrKeyConfig, family, kid, maxKeyBytes)
		}
		if _, dup := out[kid]; dup {
			return nil, fmt.Errorf("%w: %s has a duplicate kid %q", ErrKeyConfig, family, kid)
		}
		if other, dup := seen[string(b)]; dup {
			return nil, fmt.Errorf("%w: %s keys %q and %q have the same value (a rotation must use a new secret)", ErrKeyConfig, family, other, kid)
		}
		seen[string(b)] = kid
		out[kid] = b
	}
	return out, nil
}

// decodeKey accepts standard or URL-safe base64, padded or raw. The four
// decoders are strict, so trailing garbage bits are refused.
func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.Strict().DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, ErrKeyConfig
}

// degenerateKey reports a key whose bytes are all equal (for example all
// zero): a placeholder or a broken generator, never a real secret.
func degenerateKey(b []byte) bool {
	for _, c := range b {
		if c != b[0] {
			return false
		}
	}
	return true
}

func deriveSubkey(master []byte, label, kid string) ([]byte, error) {
	sk, err := hkdf.Key(sha256.New, master, nil, label+"|"+kid, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: hkdf", ErrKeyConfig)
	}
	return sk, nil
}

// NewKeys builds Keys from the two parsed families. Both families are
// required; each active kid must name a key; every kid must match the kid
// syntax; every key must be at least MinKeyBytes; no key value may appear
// twice, within a family or across the two. Master keys are not retained:
// only the derived subkeys are kept.
func NewKeys(factorActiveKID string, factor map[string][]byte, macActiveKID string, mac map[string][]byte) (*Keys, error) {
	if len(factor) == 0 || len(mac) == 0 {
		return nil, fmt.Errorf("%w: both %s and %s are required", ErrKeyConfig, EnvFactorKeys, EnvMACKeys)
	}
	if !kidRE.MatchString(factorActiveKID) {
		return nil, fmt.Errorf("%w: %s is missing or malformed", ErrKeyConfig, EnvFactorActiveKID)
	}
	if !kidRE.MatchString(macActiveKID) {
		return nil, fmt.Errorf("%w: %s is missing or malformed", ErrKeyConfig, EnvMACActiveKID)
	}
	if _, ok := factor[factorActiveKID]; !ok {
		return nil, fmt.Errorf("%w: %s names a kid with no key in %s", ErrKeyConfig, EnvFactorActiveKID, EnvFactorKeys)
	}
	if _, ok := mac[macActiveKID]; !ok {
		return nil, fmt.Errorf("%w: %s names a kid with no key in %s", ErrKeyConfig, EnvMACActiveKID, EnvMACKeys)
	}
	km := &keyMaterial{factorActive: factorActiveKID, macActive: macActiveKID,
		factor: map[string][]byte{}, mac: map[string]macSubkeys{}, rnd: rand.Reader}
	k := &Keys{km: func() *keyMaterial { return km }}
	seen := map[string]string{}
	check := func(family, kid string, m []byte) error {
		if !kidRE.MatchString(kid) {
			return fmt.Errorf("%w: %s kid %q is malformed", ErrKeyConfig, family, kid)
		}
		if len(m) < MinKeyBytes || len(m) > maxKeyBytes {
			return fmt.Errorf("%w: %s key %q must be %d to %d bytes", ErrKeyConfig, family, kid, MinKeyBytes, maxKeyBytes)
		}
		if degenerateKey(m) {
			return fmt.Errorf("%w: %s key %q is degenerate (every byte has the same value)", ErrKeyConfig, family, kid)
		}
		if other, dup := seen[string(m)]; dup {
			return fmt.Errorf("%w: %s key %q has the same value as %s (the families and their kids must be independent)", ErrKeyConfig, family, kid, other)
		}
		seen[string(m)] = family + " key " + fmt.Sprintf("%q", kid)
		return nil
	}
	for _, kid := range sortedKIDs(factor) {
		m := factor[kid]
		if err := check(EnvFactorKeys, kid, m); err != nil {
			return nil, err
		}
		sk, err := deriveSubkey(m, labelFactorAEAD, kid)
		if err != nil {
			return nil, err
		}
		k.m().factor[kid] = sk
	}
	for _, kid := range sortedKIDs(mac) {
		m := mac[kid]
		if err := check(EnvMACKeys, kid, m); err != nil {
			return nil, err
		}
		var s macSubkeys
		var err error
		for _, d := range []struct {
			dst   *[]byte
			label string
		}{{&s.recovery, labelRecovery}, {&s.challenge, labelChallenge}, {&s.enrolment, labelEnrolmentToken}, {&s.session, labelSessionAssurance}} {
			if *d.dst, err = deriveSubkey(m, d.label, kid); err != nil {
				return nil, err
			}
		}
		k.m().mac[kid] = s
	}
	return k, nil
}

func sortedKIDs(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for kid := range m {
		out = append(out, kid)
	}
	sort.Strings(out)
	return out
}

// NewKeysFromConfig builds Keys from the four configuration values. It
// returns (nil, nil) only when all four are empty (the families are absent;
// whether that is acceptable is decided by config.Load and VerifyStartup). Any
// partial configuration is refused.
func NewKeysFromConfig(factorActiveKID, factorRaw, macActiveKID, macRaw string) (*Keys, error) {
	if strings.TrimSpace(factorRaw) == "" && strings.TrimSpace(factorActiveKID) == "" &&
		strings.TrimSpace(macRaw) == "" && strings.TrimSpace(macActiveKID) == "" {
		return nil, nil
	}
	factor, err := ParseKeySet(EnvFactorKeys, factorRaw)
	if err != nil {
		return nil, err
	}
	mac, err := ParseKeySet(EnvMACKeys, macRaw)
	if err != nil {
		return nil, err
	}
	return NewKeys(factorActiveKID, factor, macActiveKID, mac)
}

// FactorKID is the active FACTOR kid: every new factor is sealed under it.
func (k *Keys) FactorKID() string { return k.m().factorActive }

// MACKID is the active MAC kid: every new keyed hash is computed under it.
func (k *Keys) MACKID() string { return k.m().macActive }

// MACKIDs lists every retained MAC kid (active first, then the verify-only
// kids, sorted). A lookup by keyed hash computes the MAC under each of them.
func (k *Keys) MACKIDs() []string {
	out := []string{k.m().macActive}
	rest := make([]string, 0, len(k.m().mac))
	for kid := range k.m().mac {
		if kid != k.m().macActive {
			rest = append(rest, kid)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// ---- factor secret AEAD (ADR 0119 section 5.3) ------------------------------

// NonceBytes is the AES-GCM nonce length stored in secret_nonce.
const NonceBytes = 12

// FactorAAD is the associated data a factor secret is bound to:
//
//	"sf-v1|" || factor_id || "|" || staff_user_id || "|" || coalesce(tenant_id,'platform') || "|" || factor_type
//
// Ciphertext moved to another row, staff member, tenant or factor type fails
// to open. TenantID uuid.Nil means a platform staff member.
type FactorAAD struct {
	FactorID    uuid.UUID
	StaffUserID uuid.UUID
	TenantID    uuid.UUID
	FactorType  FactorType
}

func (a FactorAAD) bytes() ([]byte, error) {
	if a.FactorID == uuid.Nil || a.StaffUserID == uuid.Nil || !a.FactorType.Valid() {
		return nil, ErrInvalidInput
	}
	tenant := "platform"
	if a.TenantID != uuid.Nil {
		tenant = a.TenantID.String()
	}
	return []byte("sf-v1|" + a.FactorID.String() + "|" + a.StaffUserID.String() + "|" + tenant + "|" + string(a.FactorType)), nil
}

// SealedSecret is the stored form of a factor secret: the secret_kid,
// secret_nonce and secret_ct columns. None of it is secret.
type SealedSecret struct {
	KID        string
	Nonce      []byte
	Ciphertext []byte
}

// SealFactorSecret encrypts secret under the active FACTOR kid with a fresh
// random nonce, bound to aad.
func (k *Keys) SealFactorSecret(aad FactorAAD, secret FactorSecret) (SealedSecret, error) {
	ad, err := aad.bytes()
	if err != nil {
		return SealedSecret{}, err
	}
	if len(secret.d().b) != secretLen(aad.FactorType) {
		return SealedSecret{}, ErrInvalidInput
	}
	gcm, err := newGCM(k.m().factor[k.m().factorActive])
	if err != nil {
		return SealedSecret{}, err
	}
	nonce := make([]byte, NonceBytes)
	if _, err := io.ReadFull(k.m().rnd, nonce); err != nil {
		return SealedSecret{}, fmt.Errorf("staffmfa: nonce: %w", err)
	}
	return SealedSecret{KID: k.m().factorActive, Nonce: nonce, Ciphertext: gcm.Seal(nil, nonce, secret.d().b, ad)}, nil
}

// OpenFactorSecret decrypts a stored factor secret. Every failure, including
// an unknown (retired) kid, malformed columns and a plaintext of the wrong
// length, is ErrFactorIntegrity (the unknown-kid case also matches
// ErrUnknownKID). Malformed AAD input (a nil factor or staff id, an unknown
// factor type) matches BOTH ErrFactorIntegrity and ErrInvalidInput: a caller
// must never read any error from this function as "factor absent" or as a
// reason to offer enrolment; every error is a verification failure.
func (k *Keys) OpenFactorSecret(aad FactorAAD, s SealedSecret) (FactorSecret, error) {
	ad, err := aad.bytes()
	if err != nil {
		return FactorSecret{}, fmt.Errorf("%w: %w", ErrFactorIntegrity, ErrInvalidInput)
	}
	key, ok := k.m().factor[s.KID]
	if !ok {
		return FactorSecret{}, fmt.Errorf("%w: %w", ErrFactorIntegrity, ErrUnknownKID)
	}
	if len(s.Nonce) != NonceBytes {
		return FactorSecret{}, ErrFactorIntegrity
	}
	gcm, err := newGCM(key)
	if err != nil {
		return FactorSecret{}, ErrFactorIntegrity
	}
	pt, err := gcm.Open(nil, s.Nonce, s.Ciphertext, ad)
	if err != nil || len(pt) != secretLen(aad.FactorType) {
		return FactorSecret{}, ErrFactorIntegrity
	}
	return newFactorSecret(pt), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKeyConfig
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrKeyConfig
	}
	return cipher.NewGCM(b)
}

// ---- keyed hashes (ADR 0119 section 5.3) ------------------------------------

// MACKind is the closed set of keyed-hash uses. Each selects a MAC-family
// subkey (section 5.2) and the purpose string inside the message.
type MACKind int

// The keyed-hash kinds. The zero value is invalid.
const (
	_ MACKind = iota
	// MACRecoveryCode: staff_mfa_recovery_codes.code_mac.
	MACRecoveryCode
	// MACChallengeMFA and MACChallengeEnrol: staff_auth_challenges.token_mac
	// for purpose 'mfa' and 'enrol'.
	MACChallengeMFA
	MACChallengeEnrol
	// MACEnrolInvite, MACEnrolMFAReset, MACEnrolPasswordReset and
	// MACEnrolBootstrap: staff_enrolment_tokens.token_mac per purpose.
	MACEnrolInvite
	MACEnrolMFAReset
	MACEnrolPasswordReset
	MACEnrolBootstrap
)

// purpose returns the in-message purpose string, which equals the purpose
// column value of the row the MAC is stored in.
func (m MACKind) purpose() (string, bool) {
	switch m {
	case MACRecoveryCode:
		return "recovery", true
	case MACChallengeMFA:
		return "mfa", true
	case MACChallengeEnrol:
		return "enrol", true
	case MACEnrolInvite:
		return "invite", true
	case MACEnrolMFAReset:
		return "mfa_reset", true
	case MACEnrolPasswordReset:
		return "password_reset", true
	case MACEnrolBootstrap:
		return "bootstrap", true
	}
	return "", false
}

func (s macSubkeys) forKind(m MACKind) []byte {
	switch m {
	case MACRecoveryCode:
		return s.recovery
	case MACChallengeMFA, MACChallengeEnrol:
		return s.challenge
	case MACEnrolInvite, MACEnrolMFAReset, MACEnrolPasswordReset, MACEnrolBootstrap:
		return s.enrolment
	}
	return nil
}

// MACBytes is the length of every keyed hash (HMAC-SHA256).
const MACBytes = 32

// macUnder computes HMAC(subkey(kid), purpose || "|" || staff_user_id || "|" || value).
func (k *Keys) macUnder(kid string, kind MACKind, staff uuid.UUID, value []byte) ([]byte, error) {
	purpose, ok := kind.purpose()
	if !ok || staff == uuid.Nil || len(value) == 0 {
		return nil, ErrInvalidInput
	}
	sub, ok := k.m().mac[kid]
	if !ok {
		return nil, ErrUnknownKID
	}
	m := hmac.New(sha256.New, sub.forKind(kind))
	m.Write([]byte(purpose + "|" + staff.String() + "|"))
	m.Write(value)
	return m.Sum(nil), nil
}

// verifyUnder recomputes the MAC under kid and compares it with stored in
// constant time. Any error is a mismatch.
func (k *Keys) verifyUnder(kid string, kind MACKind, staff uuid.UUID, value, stored []byte) bool {
	want, err := k.macUnder(kid, kind, staff, value)
	if err != nil || len(stored) != MACBytes {
		return false
	}
	return hmac.Equal(want, stored)
}

// KeyedHash is a stored keyed hash: the *_kid and *_mac columns.
type KeyedHash struct {
	KID string
	MAC []byte
}

// lookupHashes returns the keyed hash of value under every retained MAC kid
// (active first), for a lookup by MAC.
func (k *Keys) lookupHashes(kind MACKind, staff uuid.UUID, value []byte) ([]KeyedHash, error) {
	kids := k.MACKIDs()
	out := make([]KeyedHash, 0, len(kids))
	for _, kid := range kids {
		m, err := k.macUnder(kid, kind, staff, value)
		if err != nil {
			return nil, err
		}
		out = append(out, KeyedHash{KID: kid, MAC: m})
	}
	return out, nil
}
