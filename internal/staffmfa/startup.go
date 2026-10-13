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
// seal/open and keyed-hash self-check so a subkey derivation fault is found
// at deploy time.
//
// S1 status: the gate exists and is unit-tested, but it is NOT yet called from
// cmd/platform-api or cmd/seed-admin. Wiring it in before staff login uses the
// keys (S3) would stop every existing development and test environment that
// has no STAFF_MFA_* values; S3 wires it together with the flows.
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
	sealed, err := keys.SealFactorSecret(aad, secret)
	if err != nil {
		return fmt.Errorf("%w: factor key self-check failed", ErrStartupGate)
	}
	if _, err := keys.OpenFactorSecret(aad, sealed); err != nil {
		return fmt.Errorf("%w: factor key self-check failed", ErrStartupGate)
	}
	for _, kid := range keys.MACKIDs() {
		if _, err := keys.macUnder(kid, MACRecoveryCode, probe, []byte("self-check")); err != nil {
			return fmt.Errorf("%w: MAC key self-check failed for kid %q", ErrStartupGate, kid)
		}
	}
	return nil
}
