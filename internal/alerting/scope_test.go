package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestFreshReadCommittedRunner_UnknownScopeKindRefuses is the scope-kind
// hardening recommendation: an unrecognised ScopeKind must never silently
// fall back to being treated as ScopeTenant (which could open a
// transaction under the wrong identity) - it must refuse outright.
func TestFreshReadCommittedRunner_UnknownScopeKindRefuses(t *testing.T) {
	const unknownScopeKind ScopeKind = 99
	runner := freshReadCommittedRunner(nil, RaiseScope{Kind: unknownScopeKind, TenantID: uuid.New()})

	err := runner.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an unknown ScopeKind to refuse rather than run")
	}
}

// TestFreshReadCommittedRunner_KnownKindsDoNotRefuse pins that every
// currently-defined ScopeKind is handled explicitly (none of them hit the
// refusing default case).
func TestFreshReadCommittedRunner_KnownKindsDoNotRefuse(t *testing.T) {
	var pool *db.Pool // never dereferenced by these constructors - they only store it
	cases := []RaiseScope{
		{Kind: ScopeTenant, TenantID: uuid.New()},
		{Kind: ScopeTenantPrincipal, TenantID: uuid.New(), PrincipalID: uuid.New()},
		{Kind: ScopePlatformAdmin, PlatformAdminID: uuid.New()},
	}
	for _, scope := range cases {
		runner := freshReadCommittedRunner(pool, scope)
		if _, isRefusing := runner.(refusingRunner); isRefusing {
			t.Errorf("scope kind %d unexpectedly hit the refusing default case", scope.Kind)
		}
	}
}
