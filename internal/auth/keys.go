package auth

import "fmt"

// KeyRegistry holds the platform's JWT signing keys, keyed by "kid" (key
// id), with one designated as active for new signatures. Verification
// tries whatever key the token's own "kid" header names, not just the
// active one - this is what makes rotation possible: publish a new
// active key, and tokens signed with the previous one keep verifying
// (via its still-registered kid) until they expire naturally, at which
// point the previous key can be retired from the registry entirely.
//
// This is a Stage 2 foundation mechanism (HMAC shared secrets managed by
// this process's own config), not a KMS-backed or asymmetric design -
// see docs/architecture/04-api-architecture.md and
// docs/security/security-architecture.md for the production-oriented
// evaluation the Stage 2 instructions asked for.
type KeyRegistry struct {
	activeKID string
	keys      map[string][]byte
}

const minKeyLen = 32

// NewKeyRegistry builds a registry. Every key must meet a minimum length
// (matching the JWT_SIGNING_SECRET validation already enforced in
// internal/config) so a rotation can never introduce a weak key by
// accident.
func NewKeyRegistry(activeKID string, keys map[string]string) (*KeyRegistry, error) {
	if activeKID == "" {
		return nil, fmt.Errorf("auth: active key id must not be empty")
	}
	if _, ok := keys[activeKID]; !ok {
		return nil, fmt.Errorf("auth: active key id %q has no registered key", activeKID)
	}
	byteKeys := make(map[string][]byte, len(keys))
	for kid, secret := range keys {
		if len(secret) < minKeyLen {
			return nil, fmt.Errorf("auth: key %q must be at least %d characters", kid, minKeyLen)
		}
		byteKeys[kid] = []byte(secret)
	}
	return &KeyRegistry{activeKID: activeKID, keys: byteKeys}, nil
}

// ActiveKID returns the key id new tokens are signed with.
func (r *KeyRegistry) ActiveKID() string { return r.activeKID }

// ActiveSecret returns the signing key for ActiveKID.
func (r *KeyRegistry) ActiveSecret() []byte { return r.keys[r.activeKID] }

// Lookup returns the secret registered for kid, for verification of a
// token signed with a (possibly no-longer-active, but not yet retired)
// key.
func (r *KeyRegistry) Lookup(kid string) ([]byte, bool) {
	secret, ok := r.keys[kid]
	return secret, ok
}
