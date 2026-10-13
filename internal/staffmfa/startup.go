package staffmfa

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrStartupGate: the process must not start (ADR 0119 section 5.1).
var ErrStartupGate = errors.New("staffmfa: startup gate")

// VerifyStartup is the staff MFA startup gate (ADR 0119 section 5.1). In
// EVERY environment it refuses to start without both valid key families,
// because staff login cannot work without them. It also runs one in-memory
// seal and keyed-hash self-check so a subkey derivation fault is found
// at deploy time.
//
// S3: production code reaches it only through LoadKeys, the single
// constructor cmd/platform-api and cmd/seed-admin call before anything reads
// the keys (ADR 0119 S3 entry gates 21.7 (a) and (b); a static test pins it).
func VerifyStartup(keys *Keys) error {
	if keys == nil {
		return fmt.Errorf("%w: %s / %s and %s / %s are required in every environment (staff login cannot work without them); refusing to start",
			ErrStartupGate, EnvFactorKeys, EnvFactorActiveKID, EnvMACKeys, EnvMACActiveKID)
	}
	probe := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	aad := FactorAAD{FactorID: probe, StaffUserID: probe, FactorType: FactorTOTP}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return fmt.Errorf("%w: random source: %v", ErrStartupGate, err)
	}
	// Sealing exercises the active factor subkey (length, AES and GCM
	// construction). An Open of the same ciphertext cannot fail when the Seal
	// succeeded, so it is not repeated here (mutation U01 in the S1 evidence).
	if _, err := keys.SealFactorSecret(aad, secret); err != nil {
		return fmt.Errorf("%w: factor key self-check failed", ErrStartupGate)
	}
	for _, kid := range keys.MACKIDs() {
		if _, err := keys.macUnder(kid, MACRecoveryCode, probe, []byte("self-check")); err != nil {
			return fmt.Errorf("%w: MAC key self-check failed for kid %q", ErrStartupGate, kid)
		}
	}
	return nil
}

// LoadKeys is the ONE production constructor of *Keys (ADR 0119 S3 entry gate
// 21.7 (b)): it parses the four configuration values and runs VerifyStartup on
// the result, so a process that holds a *Keys has passed the startup gate. An
// absent configuration is refused here (VerifyStartup(nil)) in every
// environment: there is no staff login without the keys, and no development
// bypass. A static test pins that NewKeys and NewKeysFromConfig are called
// nowhere else outside tests.
func LoadKeys(factorActiveKID, factorRaw, macActiveKID, macRaw string) (*Keys, error) {
	keys, err := NewKeysFromConfig(factorActiveKID, factorRaw, macActiveKID, macRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStartupGate, err)
	}
	if err := VerifyStartup(keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// CheckKeyConfig validates the four configuration values WITHOUT returning
// keys (config.Load's validation). It reports whether the families are
// configured at all; a partial or malformed configuration is an error. It is
// not a constructor: nothing can use its result to hash or seal.
func CheckKeyConfig(factorActiveKID, factorRaw, macActiveKID, macRaw string) (configured bool, err error) {
	keys, err := NewKeysFromConfig(factorActiveKID, factorRaw, macActiveKID, macRaw)
	if err != nil {
		return false, err
	}
	return keys != nil, nil
}
