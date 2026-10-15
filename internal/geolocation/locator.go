package geolocation

import (
	"context"
	"strings"
	"sync"
)

// Locator is the NARROW, request-scoped country-evidence seam an
// operating-market gate (internal/marketgate, A8) consumes. It is the
// GetSignal subset of LocationProvider, declared separately so a consumer
// depends on one method only and a test double needs no health/capability
// surface. Every LocationProvider (the mock, a future vendor adapter) is a
// Locator.
//
// Failure modelling is LocationProvider's, unchanged (provider.go,
// binding requirement 3): a Go error, or any Outcome other than
// SignalResolved, means "no usable evidence" and every caller must fail
// closed. A Locator must never fabricate a country.
type Locator interface {
	GetSignal(ctx context.Context, req SignalRequest) (SignalResult, error)
}

var (
	_ Locator = (LocationProvider)(nil)
	_ Locator = (*StaticLocator)(nil)
)

// StaticLocator is the deterministic, no-I/O Locator this codebase ships
// while no geolocation vendor is selected (A8; owner directive "geolocation
// vendor stays MOCK/interface only"). It is a SYNTHETIC component
// (providerkind.Synthetic): the MOCK-ADAPTER-PROD-1 startup guard refuses
// it in production exactly like every other mock, so a production process
// cannot start on static evidence.
//
// Default behaviour (empty country): UNKNOWN - SignalInconclusive for every
// request, which every consumer treats as "no evidence" and refuses. A
// non-empty country is a process-level, operator-set value for local/dev
// and test deployments only (cmd/platform-api wires it from a development-
// only config value); it is NEVER derived from the request, so a client
// cannot influence it. It stores and returns nothing about the request
// (IPAddress is ignored and never retained).
type StaticLocator struct {
	mu      sync.RWMutex
	country string
}

// NewStaticLocator returns a StaticLocator. country is an uppercase
// ISO-3166-1 alpha-2 code, or "" for the default UNKNOWN behaviour. The
// code is not validated here (the consumer validates every resolved
// signal against the ISO registry and treats an invalid one as unknown).
func NewStaticLocator(country string) *StaticLocator {
	return &StaticLocator{country: strings.TrimSpace(country)}
}

// SyntheticComponent implements providerkind.Synthetic (structural marker;
// this package does not import internal/providerkind).
func (s *StaticLocator) SyntheticComponent() {}

// SetCountry changes the static answer ("" restores UNKNOWN). Test and
// local-development hook.
func (s *StaticLocator) SetCountry(country string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.country = strings.TrimSpace(country)
}

// GetSignal implements Locator. It never returns an error.
func (s *StaticLocator) GetSignal(_ context.Context, _ SignalRequest) (SignalResult, error) {
	s.mu.RLock()
	cc := s.country
	s.mu.RUnlock()
	if cc == "" {
		return SignalResult{Outcome: SignalInconclusive, Reason: "static_locator_unknown"}, nil
	}
	return SignalResult{Outcome: SignalResolved, CountryCode: cc, ProviderReference: "static", Reason: "static_locator_configured"}, nil
}
