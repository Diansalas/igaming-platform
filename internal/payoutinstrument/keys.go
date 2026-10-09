package payoutinstrument

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/google/uuid"
)

// MinKeyBytes is the shortest accepted key (both families).
const MinKeyBytes = 32

// HKDF labels (ADR 0111 2.2): distinct labels give independent seal and
// detail-encryption subkeys from one master.
const (
	labelSeal   = "b13-seal-v1"
	labelDetail = "b13-detail-aead-v1"
	// labelImport derives the statement-import seal subkey (ADR 0111 4.3,
	// S-5; T10): a third independent subkey of the same master.
	labelImport = "b13-import-v1"

	fpLabel = "b13-fp-v1"

	// MaxDetailPlaintext bounds the plaintext detail (ADR 0111 2.1).
	MaxDetailPlaintext = 4096
)

var (
	// ErrKeyConfig: the key configuration is unusable.
	ErrKeyConfig = errors.New("payoutinstrument: invalid key configuration")
	// ErrUnknownKID: a row names a key id this process does not hold.
	ErrUnknownKID = errors.New("payoutinstrument: unknown key id")
	// ErrDecrypt: AEAD authentication failed (wrong key, wrong AAD or tampered
	// ciphertext). Never carries key or plaintext material.
	ErrDecrypt = errors.New("payoutinstrument: detail decryption failed")
	// ErrDetailTooLarge: the plaintext detail exceeds MaxDetailPlaintext.
	ErrDetailTooLarge = errors.New("payoutinstrument: detail too large")
)

var kidRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// Keys holds BOTH key families: the master family (seal + detail-AEAD
// subkeys, HKDF-derived) and the fingerprint family. They have independent
// lifecycles (H-2, M-4): the fingerprint key is long-lived and rotates only
// after the re-fingerprint job; the master rotates with retired kids kept
// verify/decrypt-only.
//
// Keys has no exported fields and redacts under every fmt verb, slog and JSON.
// It is constructed ONLY from configuration (NewKeys). There is no
// random-key fallback in this binary; the integration-tag test helper
// (payoutinstrumenttest) is the only place a random key exists.
type Keys struct {
	masterActive string
	seal         map[string][]byte // kid -> seal subkey
	detail       map[string][]byte // kid -> detail subkey
	imp          map[string][]byte // kid -> statement-import seal subkey
	fpActive     string
	fp           map[string][]byte
}

// String never renders key material.
func (k *Keys) String() string { return "payoutinstrument.Keys{redacted}" }

// GoString never renders key material.
func (k *Keys) GoString() string { return k.String() }

// Format redacts under every verb (%v %+v %#v %s %x ...).
func (k *Keys) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }

// MarshalJSON never renders key material.
func (k *Keys) MarshalJSON() ([]byte, error) { return []byte(`"payoutinstrument.Keys{redacted}"`), nil }

// NewKeys builds Keys. Every key must be at least MinKeyBytes; the two
// families must not share a key value; the active kids must exist. Keys are
// copied.
func NewKeys(masterActiveKID string, master map[string][]byte, fpActiveKID string, fp map[string][]byte) (*Keys, error) {
	if !kidRE.MatchString(masterActiveKID) || !kidRE.MatchString(fpActiveKID) {
		return nil, fmt.Errorf("%w: active kid", ErrKeyConfig)
	}
	if _, ok := master[masterActiveKID]; !ok {
		return nil, fmt.Errorf("%w: active master kid has no key", ErrKeyConfig)
	}
	if _, ok := fp[fpActiveKID]; !ok {
		return nil, fmt.Errorf("%w: active fingerprint kid has no key", ErrKeyConfig)
	}
	k := &Keys{masterActive: masterActiveKID, fpActive: fpActiveKID,
		seal: map[string][]byte{}, detail: map[string][]byte{}, imp: map[string][]byte{}, fp: map[string][]byte{}}
	seen := map[string]bool{}
	for kid, m := range master {
		if !kidRE.MatchString(kid) || len(m) < MinKeyBytes {
			return nil, fmt.Errorf("%w: master key %q", ErrKeyConfig, kid)
		}
		if seen[string(m)] {
			return nil, fmt.Errorf("%w: duplicate key value", ErrKeyConfig)
		}
		seen[string(m)] = true
		sk, err := hkdf.Key(sha256.New, m, nil, labelSeal, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: hkdf", ErrKeyConfig)
		}
		dk, err := hkdf.Key(sha256.New, m, nil, labelDetail, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: hkdf", ErrKeyConfig)
		}
		ik, err := hkdf.Key(sha256.New, m, nil, labelImport, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: hkdf", ErrKeyConfig)
		}
		k.seal[kid], k.detail[kid], k.imp[kid] = sk, dk, ik
	}
	for kid, m := range fp {
		if !kidRE.MatchString(kid) || len(m) < MinKeyBytes {
			return nil, fmt.Errorf("%w: fingerprint key %q", ErrKeyConfig, kid)
		}
		if seen[string(m)] {
			return nil, fmt.Errorf("%w: a fingerprint key equals a master key or another fingerprint key", ErrKeyConfig)
		}
		seen[string(m)] = true
		k.fp[kid] = append([]byte(nil), m...)
	}
	return k, nil
}

// MasterKID is the active master kid (new seals and ciphertexts).
func (k *Keys) MasterKID() string { return k.masterActive }

// FingerprintKID is the active fingerprint kid.
func (k *Keys) FingerprintKID() string { return k.fpActive }

// FingerprintKIDs lists every retained fingerprint kid, sorted. Registration
// computes and checks the fingerprint under ALL of them.
func (k *Keys) FingerprintKIDs() []string {
	out := make([]string, 0, len(k.fp))
	for kid := range k.fp {
		out = append(out, kid)
	}
	sort.Strings(out)
	return out
}

// Fingerprint = HMAC(fp_key(kid), canon("b13-fp-v1", tenant_id, kind,
// normalized detail)). tenant_id makes fingerprints non-comparable across
// tenants.
func (k *Keys) Fingerprint(kid string, tenantID uuid.UUID, kind, normalized string) (string, error) {
	key, ok := k.fp[kid]
	if !ok {
		return "", ErrUnknownKID
	}
	return macHex(key, canon(cs(fpLabel), cu(tenantID), cs(kind), cs(normalized))), nil
}

func macHex(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// sealMAC computes a seal under the seal subkey of kid.
func (k *Keys) sealMAC(kid, canonical string) (string, error) {
	key, ok := k.seal[kid]
	if !ok {
		return "", ErrUnknownKID
	}
	return macHex(key, canonical), nil
}

// sealEqual verifies a stored seal in constant time.
func (k *Keys) sealEqual(kid, canonical, stored string) bool {
	want, err := k.sealMAC(kid, canonical)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(want), []byte(stored))
}

// DetailAAD is the AEAD associated data (ADR 0111 2.2): ciphertext moved to
// another row, tenant, kind or schema version fails authentication.
type DetailAAD struct {
	TenantID      uuid.UUID
	InstrumentID  uuid.UUID
	Kind          string
	SchemaVersion int
}

func (a DetailAAD) bytes() []byte {
	return []byte(canon(cu(a.TenantID), cu(a.InstrumentID), cs(a.Kind), cs(strconv.Itoa(a.SchemaVersion))))
}

// EncryptDetail seals plaintext with the active master kid. It returns the
// ciphertext (tag included), the nonce and the kid used.
func (k *Keys) EncryptDetail(aad DetailAAD, plaintext []byte) (ct, nonce []byte, kid string, err error) {
	if len(plaintext) == 0 || len(plaintext) > MaxDetailPlaintext {
		return nil, nil, "", ErrDetailTooLarge
	}
	gcm, err := newGCM(k.detail[k.masterActive])
	if err != nil {
		return nil, nil, "", err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, "", fmt.Errorf("payoutinstrument: nonce: %w", err)
	}
	return gcm.Seal(nil, nonce, plaintext, aad.bytes()), nonce, k.masterActive, nil
}

// DecryptDetail opens a ciphertext under the named kid. Any failure is the
// opaque ErrDecrypt.
func (k *Keys) DecryptDetail(kid string, aad DetailAAD, ct, nonce []byte) ([]byte, error) {
	key, ok := k.detail[kid]
	if !ok {
		return nil, ErrUnknownKID
	}
	gcm, err := newGCM(key)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return nil, ErrDecrypt
	}
	pt, err := gcm.Open(nil, nonce, ct, aad.bytes())
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrUnknownKID
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrDecrypt
	}
	return cipher.NewGCM(b)
}
