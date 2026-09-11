// Password hashing using Argon2id, OWASP's current recommended default
// for interactive login (ahead of bcrypt/scrypt) because it is tunable
// against both GPU and side-channel attacks. Parameters below follow
// OWASP's baseline recommendation for Argon2id (memory=19MiB minimum;
// this package uses a higher 64MiB to raise the cost of parallel
// cracking attempts on commodity GPUs, since Stage 2 has no other rate
// limit on offline attacks against a stolen hash) and are recorded in
// the stored hash string itself (PHC-string-like format) so they can be
// changed later without breaking verification of already-stored hashes.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

type argon2Params struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
	saltLen     uint32
	keyLen      uint32
}

var defaultArgon2Params = argon2Params{
	memoryKiB:   64 * 1024, // 64 MiB
	iterations:  1,
	parallelism: 4,
	saltLen:     16,
	keyLen:      32,
}

// HashPassword hashes a plaintext password for storage. The returned
// string embeds the algorithm parameters and salt, so
// VerifyPassword needs nothing but the stored string and the candidate
// password to check.
func HashPassword(password string) (string, error) {
	if len(password) == 0 {
		return "", fmt.Errorf("auth: password must not be empty")
	}
	p := defaultArgon2Params

	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.parallelism, p.keyLen)

	encoded := fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memoryKiB, p.iterations, p.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyPassword checks a plaintext password against a hash produced by
// HashPassword, in constant time with respect to the comparison itself
// (the parsing above it is not constant-time, which is fine - the salt
// and parameters embedded in the hash are not secret).
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false, fmt.Errorf("auth: unrecognized password hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[1], "v=%d", &version); err != nil {
		return false, fmt.Errorf("auth: parse hash version: %w", err)
	}
	if version != argon2.Version {
		return false, fmt.Errorf("auth: unsupported argon2 version %d", version)
	}

	var memoryKiB, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memoryKiB, &iterations, &parallelism); err != nil {
		return false, fmt.Errorf("auth: parse hash params: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, fmt.Errorf("auth: decode salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("auth: decode hash: %w", err)
	}

	got := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, uint32(len(want)))

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
