package httpserver

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// ADR 0119 S3 test fixtures shared by every test server of this package: one deterministic staff MFA key set and
// one shared *StaffMFA built from it. Every test server sets Deps.StaffMFA to testStaffMFA(), so staff login in
// tests runs the real two-step MFA flow (staff_mfa_fixture_integration_test.go drives it). Tests that need a
// commit fault build their own instance with newTestStaffMFA (never mutate the shared one).

// testStaffMFAIssuer is the otpauth issuer of the test key set.
const testStaffMFAIssuer = "iGaming Test Back Office"

var (
	testStaffMFAOnce  sync.Once
	testStaffMFAValue *StaffMFA
	testStaffMFAKeysV *staffmfa.Keys
)

func testStaffMFAKeys() *staffmfa.Keys {
	testStaffMFAOnce.Do(func() {
		k := func(label string) []byte {
			h := sha256.Sum256([]byte("adr0119-s3-httpserver-test|" + label))
			return h[:]
		}
		keys, err := staffmfa.NewKeys("f1", map[string][]byte{"f1": k("factor-1")}, "m1", map[string][]byte{"m1": k("mac-1"), "m0": k("mac-0")})
		if err != nil {
			panic(err)
		}
		testStaffMFAKeysV = keys
		m, err := NewStaffMFA(keys, testStaffMFAIssuer, 15*time.Minute, 30*time.Minute, 12*time.Hour)
		if err != nil {
			panic(err)
		}
		testStaffMFAValue = m
	})
	return testStaffMFAKeysV
}

// testStaffMFA is the shared subsystem every test server uses. Never set commitFault on it.
func testStaffMFA() *StaffMFA {
	testStaffMFAKeys()
	return testStaffMFAValue
}

// newTestStaffMFA builds a private subsystem over the same keys (for a test that sets a commit fault or other TTLs).
func newTestStaffMFA(accessTTL, idleTTL, absoluteTTL time.Duration) *StaffMFA {
	m, err := NewStaffMFA(testStaffMFAKeys(), testStaffMFAIssuer, accessTTL, idleTTL, absoluteTTL)
	if err != nil {
		panic(err)
	}
	return m
}
