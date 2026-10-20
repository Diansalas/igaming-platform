package aml

// MOCK. The deterministic synthetic screening provider (ADR 0116 s4.1). LABEL: MOCK. There is no real sanctions / PEP list,
// no real name matching and no vendor behind it; it answers from MAGIC MARKERS in Subject.VendorSubjectRef so tests and
// sandboxes can drive every outcome. It implements providerkind.Synthetic, so the MOCK-ADAPTER-PROD-1 startup guard refuses
// the process in production while it is registered, and the AST completeness scan requires the marker on every type named
// Mock*. Nothing in it persists or logs a subject reference.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Magic markers recognised in Subject.VendorSubjectRef (prefix match, case sensitive).
const (
	MockMarkerSanction    = "SYNTH-SANCTION-"
	MockMarkerPEP         = "SYNTH-PEP-"
	MockMarkerConfirmed   = "SYNTH-CONFIRMED-"
	MockMarkerUnavailable = "SYNTH-UNAVAILABLE"
	MockMarkerRateLimited = "SYNTH-RATELIMITED"
	MockMarkerSlow        = "SYNTH-SLOW-"
	// MockProviderID is the provider id recorded in the audit trail for the mock.
	MockProviderID = "mock-screening"
)

// MockScreeningProvider is the MOCK adapter. The zero value is not usable; use NewMockScreeningProvider.
type MockScreeningProvider struct {
	mu    sync.Mutex
	seen  map[string]mockEntry
	calls int
	now   func() time.Time
}

type mockEntry struct {
	fingerprint string
	result      ScreeningResult
	err         error
}

// NewMockScreeningProvider returns a deterministic mock.
func NewMockScreeningProvider() *MockScreeningProvider {
	return &MockScreeningProvider{seen: map[string]mockEntry{}, now: time.Now}
}

// SyntheticComponent marks the mock as synthetic for the production guard (providerkind.Synthetic).
func (*MockScreeningProvider) SyntheticComponent() {}

// ID implements ScreeningProvider.
func (*MockScreeningProvider) ID() string { return MockProviderID }

// Capabilities implements ScreeningProvider.
func (*MockScreeningProvider) Capabilities() ScreeningCapabilities {
	return ScreeningCapabilities{Sanctions: true, PEP: true, SupportsSubjectReference: true}
}

// Calls is the number of Screen calls that reached the (simulated) vendor, replays excluded. For tests.
func (m *MockScreeningProvider) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func fingerprint(in ScreeningRequest) string {
	return fmt.Sprintf("%s|%s|%s|%s", in.SubjectRef, in.Subject.VendorSubjectRef, strings.Join(sortedLists(in.Lists), ","), "v1")
}

// Screen implements ScreeningProvider.
func (m *MockScreeningProvider) Screen(ctx context.Context, in ScreeningRequest) (ScreeningResult, error) {
	if err := in.Validate(); err != nil {
		return ScreeningResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ScreeningResult{}, err
	}
	fp := fingerprint(in)
	m.mu.Lock()
	if e, ok := m.seen[in.IdempotencyKey]; ok {
		m.mu.Unlock()
		if e.fingerprint != fp {
			return ScreeningResult{}, ErrIdempotencyConflict
		}
		return e.result, e.err
	}
	m.mu.Unlock()

	ref := in.Subject.VendorSubjectRef
	if strings.HasPrefix(ref, MockMarkerSlow) {
		// Simulates a hung vendor: nothing is recorded, the caller's deadline decides.
		<-ctx.Done()
		return ScreeningResult{}, ctx.Err()
	}
	res, err := m.answer(in, ref)

	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.seen[in.IdempotencyKey]; ok { // lost a race: the first stored answer wins
		if e.fingerprint != fp {
			return ScreeningResult{}, ErrIdempotencyConflict
		}
		return e.result, e.err
	}
	m.calls++
	m.seen[in.IdempotencyKey] = mockEntry{fingerprint: fp, result: res, err: err}
	return res, err
}

func (m *MockScreeningProvider) answer(in ScreeningRequest, ref string) (ScreeningResult, error) {
	has := func(l ListType) bool {
		for _, x := range in.Lists {
			if x == l {
				return true
			}
		}
		return false
	}
	base := ScreeningResult{ProviderReference: "mock-" + in.IdempotencyKey, ScreenedAt: m.now().UTC()}
	switch {
	case strings.HasPrefix(ref, MockMarkerUnavailable):
		return ScreeningResult{}, ErrProviderUnavailable
	case strings.HasPrefix(ref, MockMarkerRateLimited):
		return ScreeningResult{}, ErrProviderRateLimited
	case strings.HasPrefix(ref, MockMarkerConfirmed) && has(ListSanctions):
		base.Outcome = OutcomeConfirmed
		base.Matches = []ScreeningMatch{{List: ListSanctions, VendorMatchRef: "mock-match-" + strings.TrimLeft(ref, "ABCDEFGHIJKLMNOPQRSTUVWXYZ-"), Category: "synthetic"}}
	case strings.HasPrefix(ref, MockMarkerSanction) && has(ListSanctions):
		base.Outcome = OutcomePotentialMatch
		base.Matches = []ScreeningMatch{{List: ListSanctions, VendorMatchRef: "mock-match-" + strings.TrimLeft(ref, "ABCDEFGHIJKLMNOPQRSTUVWXYZ-"), Category: "synthetic"}}
	case strings.HasPrefix(ref, MockMarkerPEP) && has(ListPEP):
		base.Outcome = OutcomePotentialMatch
		base.Matches = []ScreeningMatch{{List: ListPEP, VendorMatchRef: "mock-match-" + strings.TrimLeft(ref, "ABCDEFGHIJKLMNOPQRSTUVWXYZ-"), Category: "synthetic"}}
	default:
		base.Outcome = OutcomeClear
	}
	return base, nil
}

// MockSubjectKeys is the MOCK per-tenant key resolver for the pseudonymous subject reference (C7). The real resolver reads the
// per-tenant key from the secret store (PROVIDER DEPENDENT / deploy); this one derives a fixed synthetic key.
type MockSubjectKeys struct{}

// SyntheticComponent marks the mock as synthetic for the production guard.
func (MockSubjectKeys) SyntheticComponent() {}

// Key implements SubjectKeyResolver.
func (MockSubjectKeys) Key(_ context.Context, tenantID uuid.UUID) ([]byte, error) {
	k := make([]byte, 32)
	copy(k, "synthetic-screening-subject-key-")
	for i := range tenantID {
		k[i%32] ^= tenantID[i]
	}
	return k, nil
}
