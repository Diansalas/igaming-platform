package geolocation

import (
	"context"
	"fmt"
	"sync"
)

// MockLocationProvider is the only LocationProvider implementation this
// phase ships - there is no real vendor selected (Stage 4I Phase B is
// interface-and-mock only; a real vendor is explicitly out of scope,
// gated on its own vendor-selection decision and its own security
// review). Mirrors kyc.MockKYCProvider's identical "magic value" testing
// convention: a test configures a specific SignalResult for a specific
// caller-supplied key via SetSignal, never a real IP-to-country lookup.
type MockLocationProvider struct {
	mu          sync.Mutex
	id          string
	signals     map[string]SignalResult // key -> configured GetSignal result
	unavailable bool
}

// NewMockLocationProvider returns a mock registered under id.
func NewMockLocationProvider(id string) *MockLocationProvider {
	return &MockLocationProvider{
		id:      id,
		signals: make(map[string]SignalResult),
	}
}

func (m *MockLocationProvider) ID() string { return m.id }

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (m *MockLocationProvider) SyntheticComponent() {}

// SetSignal configures the canned result GetSignal returns when called
// with a SignalRequest whose PlayerAccountID stringifies to key - a
// test-only configuration hook, mirroring MockKYCProvider.SetOutcome's
// identical caller-supplied-key convention (there, the key is the
// providerReference argument passed directly to GetVerification; here,
// since GetSignal takes a request struct rather than a bare string
// argument, the equivalent lookup key is req.PlayerAccountID.String() -
// GetSignal never inspects any other request field to decide which
// configured result to return).
func (m *MockLocationProvider) SetSignal(key string, result SignalResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signals[key] = result
}

// SetUnavailable makes every subsequent method call fail, simulating a
// vendor outage (mirrors MockKYCProvider.SetUnavailable exactly).
func (m *MockLocationProvider) SetUnavailable(unavailable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unavailable = unavailable
}

// GetSignal returns the SignalResult configured via SetSignal for
// req.PlayerAccountID. If no result was ever configured for that key,
// GetSignal returns an error rather than fabricating a resolved location -
// mirrors MockKYCProvider.GetVerification's identical "cannot decide, so
// report failure rather than a guessed outcome" default for a reference
// it has no record of, and satisfies this package's own fail-closed
// contract (provider.go, binding requirement 3).
func (m *MockLocationProvider) GetSignal(ctx context.Context, req SignalRequest) (SignalResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return SignalResult{}, fmt.Errorf("geolocation: mock provider unavailable")
	}
	key := req.PlayerAccountID.String()
	if r, ok := m.signals[key]; ok {
		return r, nil
	}
	return SignalResult{}, fmt.Errorf("geolocation: no signal configured for key %q", key)
}

func (m *MockLocationProvider) GetCapabilities() Capabilities {
	return Capabilities{
		SupportsCountry:      true,
		SupportsSubdivision:  false,
		SupportsVPNDetection: false,
	}
}

func (m *MockLocationProvider) HealthStatus(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return fmt.Errorf("geolocation: mock provider unavailable")
	}
	return nil
}
